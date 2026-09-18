package main

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

		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v 退出码不匹配：%d", args, code)
		}
		if !strings.Contains(stdout.String(), "jrps 0.1.0") {
			t.Fatalf("%v 版本输出不匹配：%q", args, stdout.String())
		}
	}
}

func TestHelpCommand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	if code := run([]string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("帮助命令退出码不匹配：%d", code)
	}
	if !strings.Contains(stdout.String(), "使用方法") {
		t.Fatalf("帮助输出不匹配：%q", stdout.String())
	}
}

func TestVersionOutputFailure(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"--version"}, failingWriter{}, &stderr); code != 1 {
		t.Fatalf("版本输出失败退出码不匹配：%d", code)
	}
}

func TestUnknownCommand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	if code := run([]string{"start"}, &stdout, &stderr); code != 2 {
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
