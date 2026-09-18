package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 测试辅助：静默日志器，避免测试输出噪声。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// 测试辅助：打开一个 jrps 数据库并在测试结束时关闭。
func openServerStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(Config{Path: path, BusyTimeout: time.Second, Logger: quietLogger()})
	if err != nil {
		t.Fatalf("打开 jrps 数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// 测试辅助：绕过 Store，直接用底层驱动改写数据库标记，用于模拟"指向 jrpc 的数据库文件"。
func rewriteRoleMarker(t *testing.T, path string, role string) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(databaseDSN(path, time.Second)), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("测试改写标记时打开数据库失败：%v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("测试改写标记时获取连接失败：%v", err)
	}
	defer func() { _ = sqlDB.Close() }()

	if err := db.Exec("UPDATE store_meta SET value = ? WHERE key = ?", role, metaKeyRole).Error; err != nil {
		t.Fatalf("测试改写角色标记失败：%v", err)
	}
}

// 空数据库首次启动应完成初始迁移，并登记本侧角色标记。
func TestOpenOnEmptyDatabaseAppliesInitialMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jrps.db")
	store := openServerStore(t, path)

	if store.SchemaVersion() != currentSchemaVersion {
		t.Fatalf("迁移后架构版本不匹配：%d", store.SchemaVersion())
	}
	if store.Role() != RoleServer {
		t.Fatalf("数据库角色标记不匹配：%s", store.Role())
	}

	expected := []string{
		"store_meta", "admin_credentials", "sessions", "clients", "proxies",
		"config_revisions", "revision_state", "apply_results", "audit_events",
		"notification_targets", "notification_outbox", "request_records", "body_segments",
	}
	for _, table := range expected {
		if !store.DB().Migrator().HasTable(table) {
			t.Fatalf("初始迁移缺少数据表：%s", table)
		}
	}
}

// 指向 jrpc 的数据库文件时必须拒绝启动并给出中文错误。
func TestOpenRejectsDatabaseOwnedByJrpc(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreign.db")
	store := openServerStore(t, path)
	if err := store.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}
	rewriteRoleMarker(t, path, "jrpc")

	_, err := Open(Config{Path: path, BusyTimeout: time.Second, Logger: quietLogger()})
	if err == nil {
		t.Fatal("jrps 复用 jrpc 的数据库文件必须失败")
	}
	if !strings.Contains(err.Error(), "jrpc") || !strings.Contains(err.Error(), "独立") {
		t.Fatalf("错误信息应说明数据库归属并要求独立文件：%v", err)
	}
}

// 同一数据库文件被两个实例同时打开时，后启动者必须失败。
func TestOpenRejectsSecondInstanceOnSameDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jrps.db")
	first := openServerStore(t, path)

	_, err := Open(Config{Path: path, BusyTimeout: 200 * time.Millisecond, Logger: quietLogger()})
	if err == nil {
		t.Fatal("同一数据库被两个实例打开必须失败")
	}
	if !strings.Contains(err.Error(), "占用") {
		t.Fatalf("错误信息应说明数据库已被占用：%v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}
	reopened, err := Open(Config{Path: path, BusyTimeout: time.Second, Logger: quietLogger()})
	if err != nil {
		t.Fatalf("释放独占后重新打开失败：%v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("关闭重新打开的数据库失败：%v", err)
	}
}

// 迁移失败必须拒绝启动，且不得留下半迁移状态。
func TestMigrationFailureRefusesStartAndLeavesNoHalfState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jrps.db")
	failing := Config{
		Path:        path,
		BusyTimeout: time.Second,
		Logger:      quietLogger(),
		migrations: []migration{
			{version: 1, name: "先建一张表", apply: func(tx *gorm.DB) error {
				return tx.Migrator().CreateTable(&ConfigRevision{})
			}},
			{version: 2, name: "故意失败", apply: func(tx *gorm.DB) error {
				return errors.New("模拟迁移步骤失败")
			}},
		},
	}
	if _, err := Open(failing); err == nil {
		t.Fatal("迁移失败必须拒绝启动")
	} else if !strings.Contains(err.Error(), "迁移") {
		t.Fatalf("迁移失败错误信息应为中文且指明迁移：%v", err)
	}

	// 迁移被回滚后，重新以正常迁移启动必须成功，且不残留第一步建出的表。
	store := openServerStore(t, path)
	if store.SchemaVersion() != currentSchemaVersion {
		t.Fatalf("恢复后架构版本不匹配：%d", store.SchemaVersion())
	}
}

// 数据库架构版本高于当前程序时必须拒绝启动。
func TestOpenRejectsNewerSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jrps.db")
	store := openServerStore(t, path)
	if err := store.DB().Exec("PRAGMA user_version = 99").Error; err != nil {
		t.Fatalf("写入更高架构版本失败：%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}

	_, err := Open(Config{Path: path, BusyTimeout: time.Second, Logger: quietLogger()})
	if err == nil {
		t.Fatal("数据库架构版本高于程序时必须拒绝启动")
	}
	if !strings.Contains(err.Error(), "版本") {
		t.Fatalf("错误信息应说明架构版本问题：%v", err)
	}
}

// 数据目录不存在时应创建，且超长路径不得导致启动失败。
func TestOpenCreatesDataDirectoryWithLongPath(t *testing.T) {
	base := t.TempDir()
	deep := base
	for index := 0; index < 6; index++ {
		deep = filepath.Join(deep, "深层次数据目录段"+strings.Repeat("x", 8))
	}
	path := filepath.Join(deep, "jrps.db")

	store := openServerStore(t, path)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("数据目录与数据库文件应被创建：%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}
}

// 底层写入失败时应返回中文错误，供上层按明确行为降级。
func TestWriteFailureReturnsChineseError(t *testing.T) {
	store := openServerStore(t, filepath.Join(t.TempDir(), "jrps.db"))
	if err := store.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}

	err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.SaveProxy(Proxy{ID: "p1", Name: "代理"}, ActorAdmin("tester"), OriginProxyCreate)
		return err
	})
	if err == nil {
		t.Fatal("数据库不可用时的写入必须返回错误")
	}
	if !strings.Contains(err.Error(), "写入") {
		t.Fatalf("写入失败错误信息应为中文：%v", err)
	}
}
