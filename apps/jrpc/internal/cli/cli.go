package cli

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/wcpe/jrp/apps/jrpc/internal/buildinfo"
	"github.com/wcpe/jrp/apps/jrpc/internal/store"
)

func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpCommand(args) {
		if err := writeUsage(stdout); err != nil {
			return 1
		}
		return 0
	}
	if len(args) == 1 && args[0] == "version" {
		if err := writeText(stdout, buildinfo.VersionText("jrpc")); err != nil {
			return 1
		}
		return 0
	}
	if args[0] == "run" {
		return runCommand(args[1:], stderr)
	}

	flags := flag.NewFlagSet("jrpc", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "显示版本信息")
	flags.Usage = func() { _ = writeUsage(stderr) }
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *showVersion && flags.NArg() == 0 {
		if err := writeText(stdout, buildinfo.VersionText("jrpc")); err != nil {
			return 1
		}
		return 0
	}

	command := flags.Arg(0)
	if command == "" {
		command = args[0]
	}
	if err := writeText(stderr, "未知命令："+command+"\n"); err != nil {
		return 1
	}
	return 2
}

// runCommand 启动客户端：先打开本地 SQLite 并执行迁移，再以本地 desired 走恢复流程。
//
// 数据目录与 SQLite 路径是引导参数，变更需要重启。
func runCommand(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("jrpc run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dataDirectory := flags.String("data-dir", defaultDataDirectory(), "数据目录，SQLite 与运行数据存放位置")
	databasePath := flags.String("database", "", "SQLite 主文件路径，缺省为数据目录下的 jrpc.db")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	path := *databasePath
	if path == "" {
		path = store.DatabasePath(*dataDirectory)
	}

	database, err := store.Open(store.Config{Path: path, Logger: logger})
	if err != nil {
		logger.Error("打开本地配置数据库失败，拒绝启动", "错误", err)
		return 1
	}
	defer func() { _ = database.Close() }()
	logger.Info("本地配置数据库已就绪", "路径", database.Path(), "架构版本", database.SchemaVersion())

	// 恢复流程以本地 desired 为输入重建应用状态；此处尚无 Core 门面，保持 active 为空。
	if err := database.Recover(context.Background(), nil); err != nil {
		logger.Error("本地配置恢复失败，拒绝启动", "错误", err)
		return 1
	}
	return 0
}

// defaultDataDirectory 返回默认数据目录。
//
// 平台约定与 OPERATIONS §1.2 及 FR-29 规格一致：Linux 为 /var/lib/jrp/jrpc，
// Windows 为 %ProgramData%\JRP\jrpc；两者都可用 --data-dir 显式覆盖。
func defaultDataDirectory() string {
	if runtime.GOOS == "windows" {
		if programData := os.Getenv("ProgramData"); programData != "" {
			return filepath.Join(programData, "JRP", "jrpc")
		}
	}
	return "/var/lib/jrp/jrpc"
}

func isHelpCommand(args []string) bool {
	if len(args) != 1 {
		return false
	}
	return args[0] == "help" || args[0] == "--help" || args[0] == "-h"
}

func writeUsage(output io.Writer) error {
	return writeText(output, "使用方法：\n  jrpc run [--data-dir 目录] [--database 路径]\n  jrpc --version\n  jrpc version\n  jrpc help\n")
}

func writeText(output io.Writer, value string) error {
	_, err := io.WriteString(output, value)
	return err
}
