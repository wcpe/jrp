package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcpe/jrp/apps/jrps/internal/store"
)

// 测试辅助：构造写入内存缓冲的运行环境。
type runEnv struct {
	stdout bytes.Buffer
	stderr bytes.Buffer
	input  *bytes.Reader
}

func newRunEnv(input string) *runEnv {
	return &runEnv{input: bytes.NewReader([]byte(input))}
}

// 测试辅助：执行 init 子命令并打开同一数据库检查结果。
func runInit(t *testing.T, env *runEnv, args ...string) int {
	t.Helper()
	return runInitWithEnv(t, env, nil, args...)
}

// 测试辅助：带环境变量执行 init 子命令。
func runInitWithEnv(t *testing.T, env *runEnv, extraEnv []string, args ...string) int {
	t.Helper()
	if len(extraEnv) > 0 {
		t.Setenv("JRP_ADMIN_PASSWORD", "")
		for _, entry := range extraEnv {
			parts := strings.SplitN(entry, "=", 2)
			t.Setenv(parts[0], parts[1])
		}
	}
	return runInitCommand(args, env.stdoutWriter(), env.stderrWriter(), env.inputReader())
}

func (e *runEnv) stdoutWriter() io.Writer { return &e.stdout }

func (e *runEnv) stderrWriter() io.Writer { return &e.stderr }

func (e *runEnv) inputReader() io.Reader { return e.input }

// init 必须通过标准输入读取密码完成初始化。
func TestInitAcceptsPasswordFromStdin(t *testing.T) {
	dataDirectory := t.TempDir()
	env := newRunEnv("correct-horse-battery\n")

	code := runInit(t, env, "--data-dir", dataDirectory)
	if code != 0 {
		t.Fatalf("init 退出码不匹配：%d，标准错误 %s", code, env.stderr.String())
	}
	if !strings.Contains(env.stdout.String(), "初始化完成") {
		t.Fatalf("init 成功应给出中文提示：%q", env.stdout.String())
	}

	database := openTestStore(t, store.DatabasePath(dataDirectory))
	assertInitialized(t, database, true)
	if _, err := authenticateAdmin(database, "correct-horse-battery"); err != nil {
		t.Fatalf("初始化后的密码应可登录：%v", err)
	}
}

// init 禁止把密码作为普通命令参数传入。
func TestInitRejectsPasswordAsArgument(t *testing.T) {
	env := newRunEnv("")
	code := runInit(t, env, "--data-dir", t.TempDir(), "--password", "correct-horse-battery")
	if code == 0 {
		t.Fatal("init 不得接受 --password 参数形态")
	}
	if !strings.Contains(env.stderr.String(), "禁止") {
		t.Fatalf("拒绝原因应说明禁止把密码放入命令参数：%q", env.stderr.String())
	}
}

// init 禁止把密码作为位置参数传入。
func TestInitRejectsPasswordAsPositionalArgument(t *testing.T) {
	env := newRunEnv("")
	if code := runInit(t, env, "--data-dir", t.TempDir(), "correct-horse-battery"); code == 0 {
		t.Fatal("init 不得接受位置参数形态的密码")
	}
}

// 自动化方式：通过一次性环境变量提供密码。
func TestInitAcceptsPasswordFromEnvironment(t *testing.T) {
	dataDirectory := t.TempDir()
	env := newRunEnv("")
	code := runInitWithEnv(t, env,
		[]string{"JRP_ADMIN_PASSWORD=correct-horse-battery"},
		"--data-dir", dataDirectory)
	if code != 0 {
		t.Fatalf("环境变量方式初始化失败：%d，标准错误 %s", code, env.stderr.String())
	}
	database := openTestStore(t, store.DatabasePath(dataDirectory))
	assertInitialized(t, database, true)
	// 环境变量读取后必须从进程环境中清除，避免被子进程或后续日志透出。
	if value := os.Getenv("JRP_ADMIN_PASSWORD"); value != "" {
		t.Fatalf("一次性环境变量必须在读取后清除，实际仍为 %q", value)
	}
}

// 重复执行 init 必须被拒绝，且不覆盖既有密码。
func TestInitIsRejectedWhenAlreadyInitialized(t *testing.T) {
	dataDirectory := t.TempDir()
	first := newRunEnv("correct-horse-battery\n")
	if code := runInit(t, first, "--data-dir", dataDirectory); code != 0 {
		t.Fatalf("首次初始化失败：%d，标准错误 %s", code, first.stderr.String())
	}

	second := newRunEnv("another-password\n")
	code := runInit(t, second, "--data-dir", dataDirectory)
	if code == 0 {
		t.Fatal("重复 init 应被拒绝")
	}
	if !strings.Contains(second.stderr.String(), "已完成初始化") {
		t.Fatalf("重复 init 应给出中文明确错误：%q", second.stderr.String())
	}

	database := openTestStore(t, store.DatabasePath(dataDirectory))
	if _, err := authenticateAdmin(database, "correct-horse-battery"); err != nil {
		t.Fatalf("重复 init 后原密码应仍可用：%v", err)
	}
	if _, err := authenticateAdmin(database, "another-password"); err == nil {
		t.Fatal("重复 init 不得覆盖密码")
	}
}

// 进程命令行与日志中不得出现密码。
func TestInitDoesNotLeakPasswordToOutput(t *testing.T) {
	dataDirectory := t.TempDir()
	password := "correct-horse-battery"
	env := newRunEnv(password + "\n")
	if code := runInit(t, env, "--data-dir", dataDirectory); code != 0 {
		t.Fatalf("初始化失败：%d，标准错误 %s", code, env.stderr.String())
	}
	if strings.Contains(env.stdout.String(), password) || strings.Contains(env.stderr.String(), password) {
		t.Fatalf("init 输出不得包含密码：stdout=%q stderr=%q", env.stdout.String(), env.stderr.String())
	}
}

// 密码过短或空输入必须有确定行为与中文提示。
func TestInitRejectsWeakPassword(t *testing.T) {
	dataDirectory := t.TempDir()
	env := newRunEnv("short\n")
	code := runInit(t, env, "--data-dir", dataDirectory)
	if code == 0 {
		t.Fatal("过短密码应被拒绝")
	}
	if !strings.Contains(env.stderr.String(), "密码") {
		t.Fatalf("拒绝原因应说明密码问题：%q", env.stderr.String())
	}
	database := openTestStore(t, store.DatabasePath(dataDirectory))
	assertInitialized(t, database, false)
}

// 空输入（无密码）必须有确定行为。
func TestInitRejectsEmptyInput(t *testing.T) {
	dataDirectory := t.TempDir()
	env := newRunEnv("\n")
	if code := runInit(t, env, "--data-dir", dataDirectory); code == 0 {
		t.Fatal("空密码应被拒绝")
	}
	database := openTestStore(t, store.DatabasePath(dataDirectory))
	assertInitialized(t, database, false)
}

// init 必须支持与 serve 一致的引导参数。
func TestInitAcceptsDatabaseBootstrapFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.db")
	env := newRunEnv("correct-horse-battery\n")
	if code := runInit(t, env, "--database", path); code != 0 {
		t.Fatalf("指定数据库路径失败：%d，标准错误 %s", code, env.stderr.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("应在指定路径创建数据库：%v", err)
	}
}

// 测试辅助：打开指定路径的数据库并在测试结束时关闭。
func openTestStore(t *testing.T, path string) *store.Store {
	t.Helper()
	database, err := store.Open(store.Config{
		Path:        path,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		BusyTimeout: 0,
	})
	if err != nil {
		t.Fatalf("打开数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// 测试辅助：断言数据库的初始化状态。
func assertInitialized(t *testing.T, database *store.Store, want bool) {
	t.Helper()
	var got bool
	if err := database.View(context.Background(), func(tx *store.Tx) error {
		var err error
		got, err = tx.IsInitialized()
		return err
	}); err != nil {
		t.Fatalf("读取初始化状态失败：%v", err)
	}
	if got != want {
		t.Fatalf("初始化状态不匹配：期望 %v，实际 %v", want, got)
	}
}

// 测试辅助：以给定密码鉴权。
func authenticateAdmin(database *store.Store, password string) (store.AdminCredential, error) {
	var credential store.AdminCredential
	err := database.View(context.Background(), func(tx *store.Tx) error {
		var err error
		credential, err = tx.AuthenticateAdmin(store.AdminUsername, password)
		return err
	})
	return credential, err
}
