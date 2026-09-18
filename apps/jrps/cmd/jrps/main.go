package main

import (
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/wcpe/jrp/apps/jrps/internal/buildinfo"
	"github.com/wcpe/jrp/apps/jrps/internal/httpapi"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if handled, code := handleImmediateCommand(args, stdout); handled {
		return code
	}

	flags := flag.NewFlagSet("jrps", flag.ContinueOnError)
	flags.SetOutput(stderr)
	listen := flags.String("listen", "127.0.0.1:7500", "管理服务监听地址")
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
	return serve(*listen, stderr)
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

func serve(listen string, stderr io.Writer) int {
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	server := http.Server{
		Addr:              listen,
		Handler:           httpapi.NewRouter(),
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

func writeUsage(output io.Writer) error {
	return writeText(output, "使用方法：jrps [--listen 地址] [--version]\n")
}

func writeText(output io.Writer, value string) error {
	_, err := io.WriteString(output, value)
	return err
}
