package cli

import (
	"flag"
	"io"

	"github.com/wcpe/jrp/apps/jrpc/internal/buildinfo"
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

func isHelpCommand(args []string) bool {
	if len(args) != 1 {
		return false
	}
	return args[0] == "help" || args[0] == "--help" || args[0] == "-h"
}

func writeUsage(output io.Writer) error {
	return writeText(output, "使用方法：\n  jrpc --version\n  jrpc version\n  jrpc help\n")
}

func writeText(output io.Writer, value string) error {
	_, err := io.WriteString(output, value)
	return err
}
