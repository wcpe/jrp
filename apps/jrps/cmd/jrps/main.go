package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
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

	// 恢复流程以 SQLite 中的 desired 为输入重建应用状态；此处尚无 Core 门面，保持 active 为空。
	if err := database.Recover(context.Background(), nil, store.ActorAdmin("server")); err != nil {
		logger.Error("配置恢复失败，拒绝启动", "错误", err)
		return 1
	}
	return serve(*listen, database, logger)
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

func serve(listen string, database *store.Store, logger *slog.Logger) int {
	// 优雅退出的根上下文：SIGINT/SIGTERM 触发取消，据此依次停止 HTTP 服务
	// 与后台发送循环。这是本程序第一处信号处理——此前直接 ListenAndServe，
	// 收到信号即被杀死，正在进行的投递会停在 sending 状态。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
		}),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("管理服务开始监听", "地址", listen)
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

func writeUsage(output io.Writer) error {
	return writeText(output, "使用方法：jrps [--listen 地址] [--data-dir 目录] [--database 路径] [--version]\n")
}

func writeText(output io.Writer, value string) error {
	_, err := io.WriteString(output, value)
	return err
}
