package main

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/wcpe/jrp/apps/jrps/internal/buildinfo"
	"github.com/wcpe/jrp/apps/jrps/internal/httpapi"
	"github.com/wcpe/jrp/apps/jrps/internal/notify"
	"github.com/wcpe/jrp/apps/jrps/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if handled, code := handleImmediateCommand(args, stdout); handled {
		return code
	}
	if len(args) > 0 && args[0] == "init" {
		return runInitCommand(args[1:], stdout, stderr, os.Stdin)
	}

	flags := flag.NewFlagSet("jrps", flag.ContinueOnError)
	flags.SetOutput(stderr)
	listen := flags.String("listen", "127.0.0.1:7500", "管理服务监听地址")
	dataDirectory := flags.String("data-dir", defaultDataDirectory(), "数据目录，SQLite 与运行数据存放位置")
	databasePath := flags.String("database", "", "SQLite 主文件路径，缺省为数据目录下的 jrps.db")
	showVersion := flags.Bool("version", false, "显示版本信息")
	tlsCert := flags.String("tls-cert", "", "管理服务 TLS 证书路径；与 --tls-key 同时提供即启用 HTTPS")
	tlsKey := flags.String("tls-key", "", "管理服务 TLS 私钥路径；与 --tls-cert 同时提供即启用 HTTPS")
	flags.Usage = func() { _ = writeUsage(stderr) }
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		if err := writeText(stdout, buildinfo.VersionText("jrps")); err != nil {
			return 1
		}
		return 0
	}
	if flags.NArg() != 0 {
		if err := writeText(stderr, "未知命令："+flags.Arg(0)+"\n"); err != nil {
			return 1
		}
		return 2
	}
	path := *databasePath
	if path == "" {
		path = store.DatabasePath(*dataDirectory)
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	database, err := openStore(path, logger)
	if err != nil {
		logger.Error("打开配置数据库失败，拒绝启动", "错误", err)
		return 1
	}
	defer func() { _ = database.Close() }()
	logger.Info("配置数据库已就绪", "路径", database.Path(), "架构版本", database.SchemaVersion())

	// 启动前恢复已完成（assembleApplyService）：run() 中不再重复调用。
	return serve(*listen, *tlsCert, *tlsKey, database, logger)
}

// openStore 打开独占的配置数据库：迁移失败、数据库归属错误或已有实例占用时返回错误。
func openStore(path string, logger *slog.Logger) (*store.Store, error) {
	return store.Open(store.Config{Path: path, Logger: logger})
}

func handleImmediateCommand(args []string, stdout io.Writer) (bool, int) {
	if len(args) != 1 {
		return false, 0
	}
	if args[0] == "version" {
		if err := writeText(stdout, buildinfo.VersionText("jrps")); err != nil {
			return true, 1
		}
		return true, 0
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		if err := writeUsage(stdout); err != nil {
			return true, 1
		}
		return true, 0
	}
	return false, 0
}

func serve(listen, tlsCert, tlsKey string, database *store.Store, logger *slog.Logger) int {
	// 两个 TLS 参数必须成对出现：只给一个是配置错误，按启动失败处理而不是
	// 静默降级为明文——静默降级会让管理员以为自己在用 HTTPS。
	if (tlsCert == "") != (tlsKey == "") {
		logger.Error("TLS 参数不完整：--tls-cert 与 --tls-key 必须同时提供")
		return 2
	}
	useTLS := tlsCert != "" && tlsKey != ""
	var fingerprint string
	if useTLS {
		computed, err := certificateFingerprint(tlsCert)
		if err != nil {
			logger.Error("读取 TLS 证书失败，拒绝启动", "错误", err)
			return 1
		}
		fingerprint = computed
	}

	// 优雅退出的根上下文：SIGINT/SIGTERM 触发取消，据此依次停止 HTTP 服务
	// 与后台发送循环。这是本程序第一处信号处理——此前直接 ListenAndServe，
	// 收到信号即被杀死，正在进行的投递会停在 sending 状态。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 数据面引擎装配（FR-10）：控制监听失败或引擎启动失败都按致命错误处理，
	// 不降级为"只有管理面没有数据面"的半可用状态——后者让管理员误以为服务正常。
	engine, err := startEngine(ctx, logger)
	if err != nil {
		logger.Error("数据面引擎启动失败，拒绝启动", "错误", err)
		return 1
	}
	defer shutdownEngine(engine, logger)

	// 启动恢复：以 desired 为输入走一次完整四阶段，重建 active 与 last-good。
	applyService, err := assembleApplyService(context.Background(), database, engine, logger)
	if err != nil {
		logger.Error("配置恢复失败，拒绝启动", "错误", err)
		return 1
	}

	loop, err := startOutboxLoop(ctx, database, logger)
	if err != nil {
		logger.Error("启动通知发送循环失败，拒绝启动", "错误", err)
		return 1
	}

	server := http.Server{
		Addr: listen,
		Handler: httpapi.NewRouter(httpapi.RouterOptions{
			Store:  database,
			Logger: logger,
			// 测试通知与业务通知共用同一套渠道实现，保证测通即可用。
			TestNotifications: testNotifier{store: database, sender: notify.NewSender()},
			ApplyService:      applyService,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		if useTLS {
			// 指纹随启动日志输出：自签场景下管理员据此核对证书，
			// 规格 §5 要求「自签名场景记录并核对证书指纹」。
			logger.Info("管理服务开始监听", "地址", listen, "协议", "https", "证书指纹", fingerprint)
			if err := server.ListenAndServeTLS(tlsCert, tlsKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErr <- err
				return
			}
			serveErr <- nil
			return
		}
		logger.Info("管理服务开始监听", "地址", listen, "协议", "http")
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			logger.Error("管理服务启动失败", "错误", err)
			shutdownOutboxLoop(loop, logger)
			return 1
		}
		shutdownOutboxLoop(loop, logger)
		return 0
	case <-ctx.Done():
		logger.Info("收到退出信号，开始优雅停止")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("管理服务停止超时", "错误", err)
	}
	shutdownOutboxLoop(loop, logger)
	return 0
}

// shutdownTimeout 是优雅停止各阶段的超时上限。
const shutdownTimeout = 10 * time.Second

// startOutboxLoop 装配并启动通知发送循环。
//
// 发送器在事务之外独立运行：它只通过数据库读取取得待发送记录，因此未提交
// 或已回滚的记录对它永远不可见（ADR-0004、FR-15 §3.2）。
func startOutboxLoop(ctx context.Context, database *store.Store, logger *slog.Logger) (*store.OutboxLoop, error) {
	adapter := outboxSender{
		loader: storeTargetLoader{store: database},
		sender: notify.NewSender(),
		logger: logger,
	}
	dispatcher, err := store.NewOutboxDispatcher(store.OutboxDispatcherConfig{
		Store:  database,
		Sender: adapter,
	})
	if err != nil {
		return nil, err
	}
	loop, err := store.NewOutboxLoop(store.OutboxLoopConfig{
		Dispatcher: dispatcher,
		Logger:     logger,
		OnTargetMissing: func(ctx context.Context, targetID string) error {
			return discardOutboxForTarget(ctx, database, targetID, logger)
		},
	})
	if err != nil {
		return nil, err
	}
	loop.Start()
	return loop, nil
}

// discardOutboxForTarget 把已缺失目标的在途记录转入 discarded 并写审计（FR-15 §3.6）。
func discardOutboxForTarget(ctx context.Context, database *store.Store, targetID string, logger *slog.Logger) error {
	return database.Transaction(ctx, func(tx *store.Tx) error {
		count, err := tx.DiscardOutboxForTarget(targetID)
		if err != nil || count == 0 {
			return err
		}
		return tx.WriteAudit(store.AuditEvent{
			ActorType:  store.ActorTypeAdmin,
			ActorID:    "server",
			Action:     store.ActionNotificationDiscard,
			ObjectType: store.ObjectTypeNotificationMsg,
			ObjectID:   targetID,
			Result:     store.AuditResultSuccess,
			Context:    "通知目标已不存在，在途通知转入不再投递",
		})
	})
}

// shutdownOutboxLoop 停止后台发送循环并等待当前轮结束。
func shutdownOutboxLoop(loop *store.OutboxLoop, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := loop.Close(ctx); err != nil {
		logger.Error("停止通知发送循环超时", "错误", err)
		return
	}
	logger.Info("通知发送循环已停止", "累计投递", loop.Dispatched(), "累计失败", loop.Failed())
}

// defaultDataDirectory 返回默认数据目录。
//
// 平台约定与 OPERATIONS §1.2 及 FR-29 规格一致：Linux 为 /var/lib/jrp/jrps，
// Windows 为 %ProgramData%\JRP\jrps；两者都可用 --data-dir 显式覆盖。
func defaultDataDirectory() string {
	if runtime.GOOS == "windows" {
		if programData := os.Getenv("ProgramData"); programData != "" {
			return filepath.Join(programData, "JRP", "jrps")
		}
	}
	return "/var/lib/jrp/jrps"
}

// certificateFingerprint 计算证书文件的 SHA-256 指纹，供管理员核对。
//
// 输出按字节分组的十六进制：与浏览器和 openssl 的常见展示形态一致，
// 便于逐段比对。只取文件中的第一张证书——服务端证书在前是通行约定。
func certificateFingerprint(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取证书文件失败：%w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return "", errors.New("证书文件不含 PEM 块")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("解析证书失败：%w", err)
	}
	sum := sha256.Sum256(certificate.Raw)
	octets := make([]string, 0, len(sum))
	for _, value := range sum {
		octets = append(octets, fmt.Sprintf("%02X", value))
	}
	return strings.Join(octets, ":"), nil
}

func writeUsage(output io.Writer) error {
	return writeText(output, "使用方法：jrps [--listen 地址] [--data-dir 目录] [--database 路径] [--tls-cert 证书] [--tls-key 私钥] [--version]\n")
}

func writeText(output io.Writer, value string) error {
	_, err := io.WriteString(output, value)
	return err
}
