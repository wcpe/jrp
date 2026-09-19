package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 测试辅助：创建一个 Webhook 目标。
func createWebhookTarget(t *testing.T, database *Store, name string, enabled bool) NotificationTargetView {
	t.Helper()
	var view NotificationTargetView
	err := database.Transaction(context.Background(), func(tx *Tx) error {
		var createErr error
		view, createErr = tx.CreateNotificationTarget(ActorAdmin("admin"), NotificationTargetInput{
			Name: name, Type: NotificationTypeWebhook, Enabled: enabled,
			WebhookURL: "https://hooks.example.com/hook", Secret: "webhook-secret-value",
		})
		return createErr
	})
	if err != nil {
		t.Fatalf("创建目标失败：%v", err)
	}
	return view
}

// 创建时显式停用的目标必须真的处于停用状态。
//
// 回归用例：GORM 对带 default 标签的零值字段会改用数据库默认值，而 Enabled 的
// 零值恰是 false，曾导致显式停用被 default:true 静默覆盖成启用——管理员以为
// 目标不会接收通知，实际它仍在接收。
func TestCreateNotificationTargetHonorsDisabledFlag(t *testing.T) {
	database := openQueryStore(t)

	disabled := createWebhookTarget(t, database, "停用目标", false)
	if disabled.Enabled {
		t.Fatalf("显式停用的目标不得落库为启用状态：%+v", disabled)
	}

	enabled := createWebhookTarget(t, database, "启用目标", true)
	if !enabled.Enabled {
		t.Fatalf("显式启用的目标应为启用状态：%+v", enabled)
	}

	// 直接读库确认，排除视图构造层的干扰。
	var stored NotificationTarget
	if err := database.View(context.Background(), func(tx *Tx) error {
		return tx.db.Where("id = ?", disabled.ID).First(&stored).Error
	}); err != nil {
		t.Fatalf("读取目标失败：%v", err)
	}
	if stored.Enabled {
		t.Fatal("落库值不得被默认值覆盖")
	}
}

// 更新时停用目标必须生效，并使其在途记录转入 discarded。
func TestUpdateNotificationTargetDisablesAndDiscards(t *testing.T) {
	database := openQueryStore(t)
	target := createWebhookTarget(t, database, "目标", true)

	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.OutboxEnqueue(NotificationOutbox{
			EventID: "evt-1", TargetID: target.ID, EventType: "x", Payload: "{}",
		})
		return err
	}); err != nil {
		t.Fatalf("写入待发送记录失败：%v", err)
	}

	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.UpdateNotificationTarget(ActorAdmin("admin"), target.ID, NotificationTargetInput{
			Name: "目标", Type: NotificationTypeWebhook, Enabled: false,
			WebhookURL: "https://hooks.example.com/hook",
		})
		return err
	}); err != nil {
		t.Fatalf("停用目标失败：%v", err)
	}

	entries := mustOutboxEntries(t, database)
	if len(entries) != 1 || entries[0].Status != OutboxStatusDiscarded {
		t.Fatalf("停用目标后其待发送记录应转入 discarded：%+v", entries)
	}
}

// 更新时省略秘密表示保留原值。
func TestUpdateNotificationTargetKeepsSecretWhenOmitted(t *testing.T) {
	database := openQueryStore(t)
	target := createWebhookTarget(t, database, "目标", true)

	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.UpdateNotificationTarget(ActorAdmin("admin"), target.ID, NotificationTargetInput{
			Name: "改名", Type: NotificationTypeWebhook, Enabled: true,
			WebhookURL: "https://hooks.example.com/hook", // 未提供 Secret
		})
		return err
	}); err != nil {
		t.Fatalf("更新目标失败：%v", err)
	}

	var stored NotificationTarget
	if err := database.View(context.Background(), func(tx *Tx) error {
		return tx.db.Where("id = ?", target.ID).First(&stored).Error
	}); err != nil {
		t.Fatalf("读取目标失败：%v", err)
	}
	if stored.Secret != "webhook-secret-value" {
		t.Fatal("未提供秘密时不得清空原有秘密")
	}
	if stored.Name != "改名" {
		t.Fatalf("名称应已更新：%s", stored.Name)
	}
}

// 目标的更新不得改写主键与创建时间。
func TestUpdateNotificationTargetKeepsIdentityAndCreationTime(t *testing.T) {
	database := openQueryStore(t)
	target := createWebhookTarget(t, database, "目标", true)

	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.UpdateNotificationTarget(ActorAdmin("admin"), target.ID, NotificationTargetInput{
			Name: "改名", Type: NotificationTypeWebhook, Enabled: true,
			WebhookURL: "https://hooks.example.com/hook",
		})
		return err
	}); err != nil {
		t.Fatalf("更新目标失败：%v", err)
	}

	var stored NotificationTarget
	if err := database.View(context.Background(), func(tx *Tx) error {
		return tx.db.Where("id = ?", target.ID).First(&stored).Error
	}); err != nil {
		t.Fatalf("读取目标失败：%v", err)
	}
	if stored.ID != target.ID {
		t.Fatalf("主键不得被改写：%s", stored.ID)
	}
	if !stored.CreatedAt.Equal(target.CreatedAt) {
		t.Fatalf("创建时间不得被改写：%v → %v", target.CreatedAt, stored.CreatedAt)
	}
}

// 校验失败必须返回全部违规项，且不创建任何目标。
func TestCreateNotificationTargetReportsAllViolations(t *testing.T) {
	database := openQueryStore(t)

	err := database.Transaction(context.Background(), func(tx *Tx) error {
		_, createErr := tx.CreateNotificationTarget(ActorAdmin("admin"), NotificationTargetInput{
			Name: "", Type: NotificationTypeWebhook, WebhookURL: "http://insecure.example.com/hook",
		})
		return createErr
	})
	var validationErr NotificationTargetValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("应返回目标校验错误：%v", err)
	}
	if len(validationErr.Violations) < 2 {
		t.Fatalf("应一次返回全部违规项，实际 %d 项", len(validationErr.Violations))
	}

	var count int64
	if err := database.View(context.Background(), func(tx *Tx) error {
		return tx.db.Model(&NotificationTarget{}).Count(&count).Error
	}); err != nil {
		t.Fatalf("统计目标失败：%v", err)
	}
	if count != 0 {
		t.Fatal("校验失败不得创建目标")
	}
}

// 目标的秘密不得以明文出现在读取视图中。
func TestNotificationTargetViewNeverExposesRawSecret(t *testing.T) {
	database := openQueryStore(t)
	const secret = "webhook-secret-abcdef123456"

	var view NotificationTargetView
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		var err error
		view, err = tx.CreateNotificationTarget(ActorAdmin("admin"), NotificationTargetInput{
			Name: "目标", Type: NotificationTypeWebhook, Enabled: true,
			WebhookURL: "https://hooks.example.com/hook", Secret: secret,
		})
		return err
	}); err != nil {
		t.Fatalf("创建目标失败：%v", err)
	}

	if strings.Contains(view.MaskedSecret, secret) {
		t.Fatalf("视图泄露完整秘密：%s", view.MaskedSecret)
	}
	if !strings.HasSuffix(view.MaskedSecret, secret[len(secret)-4:]) {
		t.Fatalf("掩码应保留末四位：%s", view.MaskedSecret)
	}

	// 列表视图同样不得泄露。
	var views []NotificationTargetView
	if err := database.View(context.Background(), func(tx *Tx) error {
		var err error
		views, err = tx.NotificationTargets()
		return err
	}); err != nil {
		t.Fatalf("读取目标列表失败：%v", err)
	}
	for _, item := range views {
		if strings.Contains(item.MaskedSecret, secret) {
			t.Fatalf("列表视图泄露完整秘密：%s", item.MaskedSecret)
		}
	}
}

// 目标摘要不得包含 Webhook 地址的路径与查询串：它们可能含凭据。
func TestNotificationTargetSummaryHidesPath(t *testing.T) {
	database := openQueryStore(t)

	var view NotificationTargetView
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		var err error
		view, err = tx.CreateNotificationTarget(ActorAdmin("admin"), NotificationTargetInput{
			Name: "目标", Type: NotificationTypeWebhook, Enabled: true,
			WebhookURL: "https://hooks.example.com/services/secret-path?token=abc123",
		})
		return err
	}); err != nil {
		t.Fatalf("创建目标失败：%v", err)
	}

	if strings.Contains(view.Summary, "secret-path") || strings.Contains(view.Summary, "abc123") {
		t.Fatalf("摘要不得包含路径与查询串：%s", view.Summary)
	}
	if !strings.Contains(view.Summary, "hooks.example.com") {
		t.Fatalf("摘要应保留主机便于识别：%s", view.Summary)
	}
}

// 目标数量达到上限后拒绝创建。
func TestCreateNotificationTargetEnforcesLimit(t *testing.T) {
	database := openQueryStore(t)
	for index := 0; index < maxEnabledTargets; index += 1 {
		createWebhookTarget(t, database, "目标", true)
	}

	err := database.Transaction(context.Background(), func(tx *Tx) error {
		_, createErr := tx.CreateNotificationTarget(ActorAdmin("admin"), NotificationTargetInput{
			Name: "超限目标", Type: NotificationTypeWebhook, Enabled: true,
			WebhookURL: "https://hooks.example.com/hook",
		})
		return createErr
	})
	if err == nil {
		t.Fatal("达到上限后应拒绝创建")
	}
	if !strings.Contains(err.Error(), "上限") {
		t.Fatalf("错误信息应说明上限：%v", err)
	}
}
