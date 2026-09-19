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

// v2 库升级到 v3 后，既有通知目标不被改动，新列可用。
//
// 升级不得为旧目标编造投递配置：v1/v2 时期的目标只有摘要与秘密，
// 没有任何地址可回填，留空比猜一个默认地址安全。
func TestMigrationUpgradesV2AndKeepsNotificationTargets(t *testing.T) {
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
	// 用 v1 表结构写入一条目标：此时还没有投递配置列。
	if err := legacy.Transaction(context.Background(), func(tx *Tx) error {
		return tx.db.Create(&NotificationTarget{
			ID:            "legacy-target",
			Type:          NotificationTypeWebhook,
			Name:          "历史目标",
			TargetSummary: "https://example.invalid/hook",
		}).Error
	}); err != nil {
		t.Fatalf("写入历史目标失败：%v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("关闭旧库失败：%v", err)
	}

	upgraded := openServerStore(t, path)
	if upgraded.SchemaVersion() != currentSchemaVersion {
		t.Fatalf("升级后架构版本不匹配：%d", upgraded.SchemaVersion())
	}

	var target NotificationTarget
	if err := upgraded.View(context.Background(), func(tx *Tx) error {
		return tx.db.Where("id = ?", "legacy-target").First(&target).Error
	}); err != nil {
		t.Fatalf("读取升级后目标失败：%v", err)
	}
	if target.Name != "历史目标" || target.TargetSummary != "https://example.invalid/hook" {
		t.Fatalf("升级不得改动既有目标：%+v", target)
	}
	if target.WebhookURL != "" || target.SMTPHost != "" {
		t.Fatalf("升级不得为旧目标编造投递配置：%+v", target)
	}
}

// 升级后的表结构支持两种渠道的完整配置。
func TestNotificationTargetStoresBothChannels(t *testing.T) {
	database := openServerStore(t, filepath.Join(t.TempDir(), "jrps.db"))

	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		return tx.db.Create(&NotificationTarget{
			ID: "hook-1", Type: NotificationTypeWebhook, Name: "告警钩子",
			TargetSummary: "https://example.invalid/hook", Secret: "s3cr3t",
			WebhookURL: "https://example.invalid/hook",
		}).Error
	}); err != nil {
		t.Fatalf("写入 Webhook 目标失败：%v", err)
	}
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		return tx.db.Create(&NotificationTarget{
			ID: "mail-1", Type: NotificationTypeEmail, Name: "运维邮箱",
			TargetSummary: "smtp.example.invalid:587",
			SMTPHost:      "smtp.example.invalid", SMTPPort: 587,
			SMTPFrom: "jrp@example.invalid", SMTPTo: "ops@example.invalid",
			SMTPSecurity: SMTPSecurityStartTLS, Secret: "mail-password",
		}).Error
	}); err != nil {
		t.Fatalf("写入邮件目标失败：%v", err)
	}

	var hook NotificationTarget
	if err := database.View(context.Background(), func(tx *Tx) error {
		return tx.db.Where("id = ?", "hook-1").First(&hook).Error
	}); err != nil {
		t.Fatalf("读取 Webhook 目标失败：%v", err)
	}
	if hook.WebhookURL != "https://example.invalid/hook" {
		t.Fatalf("Webhook 地址未落库：%+v", hook)
	}

	var mail NotificationTarget
	if err := database.View(context.Background(), func(tx *Tx) error {
		return tx.db.Where("id = ?", "mail-1").First(&mail).Error
	}); err != nil {
		t.Fatalf("读取邮件目标失败：%v", err)
	}
	if mail.SMTPPort != 587 || mail.SMTPSecurity != SMTPSecurityStartTLS {
		t.Fatalf("SMTP 配置未落库：%+v", mail)
	}
}

// outbox 的失败终态时间可写入，用于记录"何时停止重试"。
func TestOutboxStoresStoppedAt(t *testing.T) {
	database := openServerStore(t, filepath.Join(t.TempDir(), "jrps.db"))
	stopped := time.Now().UTC()

	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		return tx.db.Create(&NotificationOutbox{
			EventID: "event-1", TargetID: "hook-1", EventType: "apply_failure",
			Payload: "{}", Status: OutboxStatusFailed, Attempts: 5,
			StoppedAt: &stopped,
		}).Error
	}); err != nil {
		t.Fatalf("写入失败终态记录失败：%v", err)
	}

	var entry NotificationOutbox
	if err := database.View(context.Background(), func(tx *Tx) error {
		return tx.db.Where("event_id = ?", "event-1").First(&entry).Error
	}); err != nil {
		t.Fatalf("读取 outbox 记录失败：%v", err)
	}
	if entry.StoppedAt == nil {
		t.Fatal("失败终态时间应被保留")
	}
	if entry.Attempts != 5 {
		t.Fatalf("失败次数应为 5，实际 %d", entry.Attempts)
	}
}
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
