package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/wcpe/jrp/apps/jrps/internal/buildinfo"
	"github.com/wcpe/jrp/apps/jrps/internal/httpapi"
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
	server := http.Server{
		Addr:              listen,
		Handler:           httpapi.NewRouter(httpapi.RouterOptions{Store: database, Logger: logger}),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	logger.Info("管理服务开始监听", "地址", listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("管理服务启动失败", "错误", err)
		return 1
	}
	return 0
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
