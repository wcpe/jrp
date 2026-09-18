package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// openStore 应在数据目录下创建数据库，并在归属为 jrpc 时拒绝打开。
func TestOpenStoreChecksDatabaseOwnership(t *testing.T) {
	dataDirectory := t.TempDir()
	path := filepath.Join(dataDirectory, "shared.db")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	database, err := openStore(path, logger)
	if err != nil {
		t.Fatalf("首次打开数据库失败：%v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("数据目录下应生成数据库文件：%v", err)
	}

	rewriteRoleMarker(t, path, "jrpc")
	if _, err := openStore(path, logger); err == nil {
		t.Fatal("指向 jrpc 数据库时应拒绝打开")
	} else if !strings.Contains(err.Error(), "独立") {
		t.Fatalf("拒绝原因应为中文并说明必须独立：%v", err)
	}
}

// 帮助信息应列出三项引导参数。
func TestUsageMentionsBootstrapFlags(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("帮助退出码不匹配：%d", code)
	}
	for _, expected := range []string{"--listen", "--data-dir", "--database"} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("帮助信息缺少 %s：%q", expected, stdout.String())
		}
	}
}

// 测试辅助：直接改写数据库归属标记，模拟另一侧外壳的数据库文件。
func rewriteRoleMarker(t *testing.T, path string, role string) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+filepath.ToSlash(path)), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("测试改写标记时打开数据库失败：%v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("测试改写标记时获取连接失败：%v", err)
	}
	defer func() { _ = sqlDB.Close() }()

	if err := db.Exec("UPDATE store_meta SET value = ? WHERE key = ?", role, "role").Error; err != nil {
		t.Fatalf("测试改写角色标记失败：%v", err)
	}
}
