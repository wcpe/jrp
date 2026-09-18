package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/wcpe/jrp/apps/jrps/internal/store"
)

// adminPasswordEnv 是一次性环境变量名：自动化方式下用它提供密码，读取后立即清除。
//
// 环境变量不得写入日志或 unit，进程命令行也不得包含密码（FR-02 规格 §3.4）。
const adminPasswordEnv = "JRP_ADMIN_PASSWORD"

// ErrPasswordAsArgument 表示调用形态把密码放进了命令参数，这是被禁止的形态。
var ErrPasswordAsArgument = errors.New("禁止把管理员密码作为命令参数传入，请改用交互式标准输入或一次性环境变量")

// initFlags 是 init 子命令接受的引导参数。
//
// 它只包含数据目录与 SQLite 路径两项引导参数；密码绝不作为参数出现。
type initFlags struct {
	dataDirectory string
	databasePath  string
}

// runInitCommand 执行 jrps init：建立唯一管理员凭据并永久关闭初始化路径。
//
// 密码只经标准输入或一次性环境变量读取，读完后立即从进程环境中清除，
// 不写入日志、不出现在命令行中（FR-02 规格 §3.2）。
func runInitCommand(args []string, stdout, stderr io.Writer, stdin io.Reader) int {
	flags := flag.NewFlagSet("jrps init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	options := initFlags{}
	flags.StringVar(&options.dataDirectory, "data-dir", defaultDataDirectory(), "数据目录，SQLite 与运行数据存放位置")
	flags.StringVar(&options.databasePath, "database", "", "SQLite 主文件路径，缺省为数据目录下的 jrps.db")
	flags.Usage = func() { _, _ = io.WriteString(stderr, initUsageText()) }
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		return writeInitFailure(stderr, ErrPasswordAsArgument)
	}

	logger := slog.New(slog.NewTextHandler(stderr, nil))
	path := options.databasePath
	if path == "" {
		path = store.DatabasePath(options.dataDirectory)
	}
	password, err := readAdminPassword(stdin, stdout)
	if err != nil {
		return writeInitFailure(stderr, err)
	}
	// 密码只在内存中按最小生存期保留：派生完成后立即失效。
	defer func() { password = "" }()

	database, err := store.Open(store.Config{Path: path, Logger: logger})
	if err != nil {
		logger.Error("打开配置数据库失败，初始化中止", "错误", err)
		return 1
	}
	defer func() { _ = database.Close() }()

	err = database.Transaction(context.Background(), func(tx *store.Tx) error {
		return tx.InitializeAdmin(store.InitializeAdminInput{Password: password})
	})
	if err != nil {
		return writeInitFailure(stderr, translateInitError(err))
	}
	logger.Info("管理员初始化完成，初始化路径已永久关闭")
	if err := writeText(stdout, "管理员初始化完成：管理员凭据已建立，初始化路径已永久关闭。\n"); err != nil {
		return 1
	}
	return 0
}

// initUsageText 返回 init 的用法说明，明确禁止把密码作为参数。
func initUsageText() string {
	return "使用方法：jrps init [--data-dir 目录] [--database 路径]\n" +
		"管理员密码通过交互式标准输入读取，或使用一次性环境变量 " + adminPasswordEnv + "；\n" +
		"禁止把密码作为命令参数传入。\n"
}

// readAdminPassword 读取管理员密码：优先一次性环境变量，否则交互式标准输入。
func readAdminPassword(stdin io.Reader, stdout io.Writer) (string, error) {
	if value, exists := os.LookupEnv(adminPasswordEnv); exists {
		// 一次性：读取后立刻从进程环境中清除，避免被子进程或后续输出透出。
		_ = os.Unsetenv(adminPasswordEnv)
		if value == "" {
			return "", errors.New("环境变量中的管理员密码为空")
		}
		return value, nil
	}
	if err := writeText(stdout, "请输入管理员密码（至少 12 个字符）："); err != nil {
		return "", err
	}
	reader := bufio.NewReader(stdin)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("读取管理员密码失败：%w", err)
	}
	if err != nil && errors.Is(err, io.EOF) && line == "" {
		return "", errors.New("未读取到管理员密码")
	}
	password := strings.TrimRight(line, "\r\n")
	if !utf8.ValidString(password) {
		return "", errors.New("管理员密码必须是有效的 UTF-8 文本")
	}
	return password, nil
}

// translateInitError 把存储层错误转换为面向运维的中文提示。
func translateInitError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrAdminAlreadyInitialized) {
		return errors.New("管理员已完成初始化，jrps init 不可重复执行；如需重置请清空并重建数据目录")
	}
	return err
}

// writeInitFailure 把初始化失败原因写入标准错误并返回失败退出码。
func writeInitFailure(stderr io.Writer, err error) int {
	if writeErr := writeText(stderr, "初始化失败："+err.Error()+"\n"); writeErr != nil {
		return 1
	}
	return 1
}
