package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// v1Migrations 返回只含 v1 的迁移集，用于构造真实的旧版本数据库。
//
// 直接复用生产 v1 的 apply 函数而不是手写建表：这样"旧库"与线上旧库形态一致，
// 升级测试才有意义。
func v1Migrations() []migration {
	return []migration{
		{version: 1, name: "建立 P1 初始表结构", apply: applyInitialSchema},
	}
}

// openV1Store 建立一个停留在 v1 的数据库并写入一条审计事件，返回其路径。
func openV1Store(t *testing.T) (string, AuditEvent) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jrps.db")
	legacy, err := Open(Config{
		Path:        path,
		BusyTimeout: time.Second,
		Logger:      quietLogger(),
		migrations:  v1Migrations(),
	})
	if err != nil {
		t.Fatalf("建立 v1 数据库失败：%v", err)
	}
	if legacy.SchemaVersion() != 1 {
		t.Fatalf("旧库架构版本应为 1，实际 %d", legacy.SchemaVersion())
	}

	event := AuditEvent{
		ActorType:  ActorTypeAdmin,
		ActorID:    "admin",
		Action:     ActionAdminLogin,
		ObjectType: "session",
		ObjectID:   "digest-prefix",
		Result:     AuditResultSuccess,
		Context:    "升级前写入的审计事件",
	}
	if err := legacy.Transaction(context.Background(), func(tx *Tx) error {
		return tx.writeAudit(event)
	}); err != nil {
		t.Fatalf("写入旧库审计事件失败：%v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("关闭旧库失败：%v", err)
	}
	return path, event
}

// v1 库升级到 v2 后应保留既有审计事件，并建立默认保留策略。
func TestMigrationUpgradesV1AndKeepsAuditEvents(t *testing.T) {
	path, seeded := openV1Store(t)

	upgraded := openServerStore(t, path)
	if upgraded.SchemaVersion() != currentSchemaVersion {
		t.Fatalf("升级后架构版本不匹配：%d", upgraded.SchemaVersion())
	}

	// 旧数据必须原样保留：升级不得清表或重建审计表。
	var events []AuditEvent
	if err := upgraded.View(context.Background(), func(tx *Tx) error {
		var err error
		events, err = tx.AuditEvents()
		return err
	}); err != nil {
		t.Fatalf("读取升级后审计事件失败：%v", err)
	}
	if len(events) != 1 {
		t.Fatalf("升级后应保留 1 条审计事件，实际 %d 条", len(events))
	}
	if events[0].Action != seeded.Action || events[0].Context != seeded.Context {
		t.Fatalf("升级后审计事件内容被改动：%+v", events[0])
	}

	// 默认策略随之建立，取自规格规定的默认值。
	var policy CapturePolicy
	if err := upgraded.View(context.Background(), func(tx *Tx) error {
		return tx.db.Where("id = ?", capturePolicyRowID).First(&policy).Error
	}); err != nil {
		t.Fatalf("读取默认保留策略失败：%v", err)
	}
	if policy.RetentionDays != DefaultRetentionDays || policy.MaxTotalBytes != DefaultMaxTotalBytes {
		t.Fatalf("默认保留策略不匹配：%+v", policy)
	}
	if policy.CaptureEnabled {
		t.Fatal("采集默认必须关闭")
	}
}

// 升级后审计事件仍不可就地修改：放开删除不等于允许改写。
func TestMigrationKeepsAuditEventsImmutable(t *testing.T) {
	path, _ := openV1Store(t)
	upgraded := openServerStore(t, path)

	err := upgraded.Transaction(context.Background(), func(tx *Tx) error {
		return tx.db.Model(&AuditEvent{}).Where("id = ?", 1).Update("context", "事后改写").Error
	})
	if err == nil {
		t.Fatal("审计事件不得被就地修改")
	}
	if !strings.Contains(err.Error(), "不可修改") {
		t.Fatalf("错误信息应说明审计不可修改：%v", err)
	}
}

// 升级必须放开发起删除的能力，否则按时间维度的审计保留策略无法执行（FR-16 §3.7）。
func TestMigrationAllowsAuditCleanupAfterUpgrade(t *testing.T) {
	path, _ := openV1Store(t)
	upgraded := openServerStore(t, path)

	err := upgraded.Transaction(context.Background(), func(tx *Tx) error {
		return tx.db.Exec("DELETE FROM audit_events WHERE id = ?", 1).Error
	})
	if err != nil {
		t.Fatalf("升级后应允许按保留策略清理审计事件，实际失败：%v", err)
	}
}

// 迁移可重复执行：已是最新版本时不重建策略行，也不冲掉管理员保存的值。
func TestMigrationDoesNotReseedExistingPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jrps.db")
	first := openServerStore(t, path)
	if err := first.Transaction(context.Background(), func(tx *Tx) error {
		return tx.db.Model(&CapturePolicy{}).Where("id = ?", capturePolicyRowID).
			Updates(map[string]any{"retention_days": 7, "capture_enabled": true}).Error
	}); err != nil {
		t.Fatalf("改写保留策略失败：%v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}

	reopened := openServerStore(t, path)
	var policy CapturePolicy
	if err := reopened.View(context.Background(), func(tx *Tx) error {
		return tx.db.Where("id = ?", capturePolicyRowID).First(&policy).Error
	}); err != nil {
		t.Fatalf("重新打开后读取保留策略失败：%v", err)
	}
	if policy.RetentionDays != 7 || !policy.CaptureEnabled {
		t.Fatalf("重新打开不应覆盖已保存的策略：%+v", policy)
	}
}

// 保底断言：v2 迁移后审计更新触发器仍然存在。
func TestMigrationRetainsAuditUpdateGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jrps.db")
	database := openServerStore(t, path)

	var count int64
	if err := database.View(context.Background(), func(tx *Tx) error {
		return tx.db.Raw(
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?",
			"audit_events_append_only_update",
		).Scan(&count).Error
	}); err != nil {
		t.Fatalf("查询触发器失败：%v", err)
	}
	if count != 1 {
		t.Fatal("审计更新触发器不得被移除")
	}
}

// 迁移后的审计表仍拒绝更新，避免 v2 误删更新触发器。
func TestMigrationDropsOnlyDeleteGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jrps.db")
	database := openServerStore(t, path)

	var count int64
	if err := database.View(context.Background(), func(tx *Tx) error {
		return tx.db.Raw(
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?",
			"audit_events_append_only_delete",
		).Scan(&count).Error
	}); err != nil {
		t.Fatalf("查询触发器失败：%v", err)
	}
	if count != 0 {
		t.Fatal("审计删除触发器应在 v2 中被移除")
	}
}
