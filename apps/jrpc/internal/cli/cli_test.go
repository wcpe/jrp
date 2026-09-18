package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestVersionCommands(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"version"}} {
		var stdout bytes.Buffer
		var stderr bytes.Buffer

		if code := Run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v 退出码不匹配：%d，错误：%s", args, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "jrpc 0.1.0") || !strings.Contains(stdout.String(), "Core 基线 jrp@e1f1d6ab1b591ec9b596f4b1b6a81cd2960c6314") {
			t.Fatalf("%v 版本输出不匹配：%q", args, stdout.String())
		}
	}
}

func TestHelpCommands(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"--help"}} {
		var stdout bytes.Buffer
		var stderr bytes.Buffer

		if code := Run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v 退出码不匹配：%d", args, code)
		}
		if !strings.Contains(stdout.String(), "使用方法") {
			t.Fatalf("%v 帮助输出不匹配：%q", args, stdout.String())
		}
	}
}

func TestVersionOutputFailure(t *testing.T) {
	var stderr bytes.Buffer
	if code := Run([]string{"--version"}, failingWriter{}, &stderr); code != 1 {
		t.Fatalf("版本输出失败退出码不匹配：%d", code)
	}
}

func TestUnknownCommand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	if code := Run([]string{"enroll"}, &stdout, &stderr); code != 2 {
		t.Fatalf("未知命令退出码不匹配：%d", code)
	}
	if !strings.Contains(stderr.String(), "未知命令") {
		t.Fatalf("未知命令错误不匹配：%q", stderr.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}
