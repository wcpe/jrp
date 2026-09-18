package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// run 子命令应在数据目录下创建独立 SQLite 并成功退出。
func TestRunCreatesDatabaseInDataDirectory(t *testing.T) {
	dataDirectory := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	if code := Run([]string{"run", "--data-dir", dataDirectory}, &stdout, &stderr); code != 0 {
		t.Fatalf("run 退出码不匹配：%d，错误：%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dataDirectory, "jrpc.db")); err != nil {
		t.Fatalf("数据目录下应生成 jrpc.db：%v", err)
	}
	if !strings.Contains(stderr.String(), "本地配置数据库已就绪") {
		t.Fatalf("启动日志应为中文并说明数据库就绪：%q", stderr.String())
	}
}

// 指向 jrps 的数据库文件时，run 必须拒绝启动并给出中文错误。
func TestRunRejectsForeignDatabase(t *testing.T) {
	dataDirectory := t.TempDir()
	databasePath := filepath.Join(dataDirectory, "foreign.db")

	var stderr bytes.Buffer
	if code := Run([]string{"run", "--data-dir", dataDirectory, "--database", databasePath}, &stderr, &stderr); code != 0 {
		t.Fatalf("首次启动应成功：%d，错误：%s", code, stderr.String())
	}
	rewriteRoleMarker(t, databasePath, "jrps")

	stderr.Reset()
	if code := Run([]string{"run", "--data-dir", dataDirectory, "--database", databasePath}, &stderr, &stderr); code != 1 {
		t.Fatalf("指向 jrps 数据库时应拒绝启动：%d", code)
	}
	if !strings.Contains(stderr.String(), "独立") {
		t.Fatalf("拒绝原因应为中文并说明必须独立：%q", stderr.String())
	}
}

// 未知子命令仍返回 2，保持既有 CLI 约定。
func TestUnknownCommandStillFails(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := Run([]string{"enroll"}, &stdout, &stderr); code != 2 {
		t.Fatalf("未知命令退出码不匹配：%d", code)
	}
	if !strings.Contains(stderr.String(), "未知命令") {
		t.Fatalf("未知命令错误不匹配：%q", stderr.String())
	}
}

// 帮助信息应列出 run 子命令与引导参数。
func TestUsageMentionsRunCommand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := Run([]string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("帮助退出码不匹配：%d", code)
	}
	for _, expected := range []string{"jrpc run", "--data-dir", "--database"} {
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
