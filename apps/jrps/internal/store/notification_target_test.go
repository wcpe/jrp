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
			Name: "目标", Type: NotificationTypeWebhook,
			Enabled: false, EnabledProvided: true,
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
			Name: "改名", Type: NotificationTypeWebhook,
			Enabled: true, EnabledProvided: true,
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
			Name: "改名", Type: NotificationTypeWebhook,
			Enabled: true, EnabledProvided: true,
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

// 读取目标时不得回显地址中的查询串。
//
// 回归用例：查询串是凭据的常见载体（`?token=...`），而规格要求读取时隐藏地址的
// 凭据部分。此前直接返回完整 URL，列表与详情响应都会把它带出去。
func TestNotificationTargetViewMasksWebhookQuery(t *testing.T) {
	database := openQueryStore(t)
	const secretQuery = "token=super-secret-value"

	var view NotificationTargetView
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		var err error
		view, err = tx.CreateNotificationTarget(ActorAdmin("admin"), NotificationTargetInput{
			Name: "目标", Type: NotificationTypeWebhook, Enabled: true,
			WebhookURL: "https://hooks.example.com/services/abc?" + secretQuery,
		})
		return err
	}); err != nil {
		t.Fatalf("创建目标失败：%v", err)
	}

	if strings.Contains(view.WebhookURL, "super-secret-value") {
		t.Fatalf("读取视图泄露了查询串凭据：%s", view.WebhookURL)
	}
	// 主机与路径保留：管理员需要靠它们区分目标。
	if !strings.Contains(view.WebhookURL, "hooks.example.com") {
		t.Fatalf("视图应保留主机便于识别：%s", view.WebhookURL)
	}
	if !strings.Contains(view.WebhookURL, "/services/abc") {
		t.Fatalf("视图应保留路径便于识别：%s", view.WebhookURL)
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
		if strings.Contains(item.WebhookURL, "super-secret-value") {
			t.Fatalf("列表视图泄露了查询串凭据：%s", item.WebhookURL)
		}
	}

	// 库内仍保存完整地址：投递必须使用原始 URL。
	var stored NotificationTarget
	if err := database.View(context.Background(), func(tx *Tx) error {
		return tx.db.Where("id = ?", view.ID).First(&stored).Error
	}); err != nil {
		t.Fatalf("读取目标失败：%v", err)
	}
	if !strings.Contains(stored.WebhookURL, "super-secret-value") {
		t.Fatal("库内应保留完整地址，掩码只作用于读取视图")
	}
}

// 更新时省略启用状态必须沿用原值。
//
// 回归用例：`Enabled` 的假零值无法区分"显式停用"与"没提这一项"，按默认值处理
// 会让 PATCH 只改名字的操作把已停用的目标静默重新启用——与"停用即不接收通知"
// 的语义直接冲突，且管理员不会收到任何提示。
func TestUpdateNotificationTargetKeepsEnabledWhenOmitted(t *testing.T) {
	database := openQueryStore(t)

	// 建一个启用的目标，再显式停用。
	target := createWebhookTarget(t, database, "目标", true)
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.UpdateNotificationTarget(ActorAdmin("admin"), target.ID, NotificationTargetInput{
			Name: "目标", Type: NotificationTypeWebhook,
			Enabled: false, EnabledProvided: true,
			WebhookURL: "https://hooks.example.com/hook",
		})
		return err
	}); err != nil {
		t.Fatalf("停用目标失败：%v", err)
	}

	// 只改名字，不提交 enabled。
	var updated NotificationTargetView
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		var err error
		updated, err = tx.UpdateNotificationTarget(ActorAdmin("admin"), target.ID, NotificationTargetInput{
			Name: "改名后", Type: NotificationTypeWebhook,
			WebhookURL: "https://hooks.example.com/hook",
		})
		return err
	}); err != nil {
		t.Fatalf("更新目标失败：%v", err)
	}
	if updated.Enabled {
		t.Fatal("省略启用状态时不得把已停用的目标重新启用")
	}
	if updated.Name != "改名后" {
		t.Fatalf("名称应已更新：%s", updated.Name)
	}

	// 反向场景：已启用的目标在省略 enabled 时必须保持启用。
	// 单靠"停用目标保持停用"无法区分"沿用了原值"与"被设成了零值 false"——
	// 两种实现都会让停用目标保持停用。
	enabled := createWebhookTarget(t, database, "另一个目标", true)
	var kept NotificationTargetView
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		var err error
		kept, err = tx.UpdateNotificationTarget(ActorAdmin("admin"), enabled.ID, NotificationTargetInput{
			Name: "另一个目标改名", Type: NotificationTypeWebhook,
			WebhookURL: "https://hooks.example.com/hook",
		})
		return err
	}); err != nil {
		t.Fatalf("更新目标失败：%v", err)
	}
	if !kept.Enabled {
		t.Fatal("省略启用状态时不得把已启用的目标停用")
	}
}

// 目标变更动作必须为每个其他启用目标各写一条 outbox 记录。
//
// 回归用例：FR-15 的发送侧此前已完整，但 outbox 没有任何生产写入点——
// 业务动作未接入，通知在真实运行中永远不会产生。本用例守住"业务动作写入
// outbox"这一环，且验证是同事务的：动作失败时不得留下孤立的通知。
func TestTargetChangeEnqueuesOutboxForOtherTargets(t *testing.T) {
	database := openQueryStore(t)
	first := createWebhookTarget(t, database, "第一个目标", true)

	// 第二个目标的创建应给第一个目标写一条通知，但不写给自己。
	second := createWebhookTarget(t, database, "第二个目标", true)
	entries := mustOutboxEntries(t, database)
	if len(entries) != 1 {
		t.Fatalf("第二个目标创建应产生 1 条通知（只给第一个目标），实际 %d：%+v", len(entries), entries)
	}
	if entries[0].TargetID != first.ID {
		t.Fatalf("通知应投递给第一个目标，实际 %q", entries[0].TargetID)
	}
	if entries[0].EventType != EventTypeTargetCreated {
		t.Fatalf("事件类型应为 %q，实际 %q", EventTypeTargetCreated, entries[0].EventType)
	}
	if entries[0].Status != OutboxStatusPending {
		t.Fatalf("新写入的通知应处于待发送，实际 %q", entries[0].Status)
	}

	// 更新第二个目标同样只通知第一个。
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.UpdateNotificationTarget(ActorAdmin("admin"), second.ID, NotificationTargetInput{
			Name: "第二个目标改名", Type: NotificationTypeWebhook,
			WebhookURL: "https://hooks.example.com/hook",
		})
		return err
	}); err != nil {
		t.Fatalf("更新目标失败：%v", err)
	}
	entries = mustOutboxEntries(t, database)
	if len(entries) != 2 {
		t.Fatalf("更新后应共 2 条通知，实际 %d：%+v", len(entries), entries)
	}
}

// 停用的目标不接收通知。
func TestTargetChangeSkipsDisabledTargets(t *testing.T) {
	database := openQueryStore(t)
	createWebhookTarget(t, database, "停用目标", false)
	createWebhookTarget(t, database, "第二个目标", true)

	// 唯一启用的是第二个目标，而它是本次创建的对象，被排除在接收方之外，
	// 因此没有任何接收方，不产生通知。
	if entries := mustOutboxEntries(t, database); len(entries) != 0 {
		t.Fatalf("无其他启用目标时不应产生通知，实际 %d：%+v", len(entries), entries)
	}
}

// 删除目标产生的事件不指向任何目标。
//
// 回归用例：删除是硬删除，目标行会消失。若事件带着 TargetID，发送器必然
// 找不到目标并把它记为投递失败——而 failed 会被读成"投递出了问题"，
// 真实原因却是目标已不存在（规格 §3.6 要求不得静默丢失，也不得误报）。
func TestDeleteTargetEnqueuesEventWithoutTarget(t *testing.T) {
	database := openQueryStore(t)
	target := createWebhookTarget(t, database, "待删除", true)

	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		return tx.DeleteNotificationTarget(ActorAdmin("admin"), target.ID)
	}); err != nil {
		t.Fatalf("删除目标失败：%v", err)
	}

	entries := mustOutboxEntries(t, database)
	if len(entries) != 1 {
		t.Fatalf("删除应产生 1 条事件，实际 %d：%+v", len(entries), entries)
	}
	if entries[0].TargetID != "" {
		t.Fatalf("删除事件不应指向目标，实际 %q", entries[0].TargetID)
	}
	if entries[0].EventType != EventTypeTargetDeleted {
		t.Fatalf("事件类型应为 %q，实际 %q", EventTypeTargetDeleted, entries[0].EventType)
	}
}

// 业务动作失败时不得留下通知。
//
// 回归用例：outbox 写入必须与业务结果同事务。若通知已写而业务回滚，
// 管理员会收到一条对应"从未发生过的变更"的通知。
func TestFailedTargetChangeLeavesNoOutbox(t *testing.T) {
	database := openQueryStore(t)
	createWebhookTarget(t, database, "存在目标", true)

	// 更新一个不存在的目标：事务整体回滚。
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.UpdateNotificationTarget(ActorAdmin("admin"), "nt_missing", NotificationTargetInput{
			Name: "改名", Type: NotificationTypeWebhook,
			WebhookURL: "https://hooks.example.com/hook",
		})
		return err
	}); err == nil {
		t.Fatal("更新不存在的目标应失败")
	}

	if entries := mustOutboxEntries(t, database); len(entries) != 0 {
		t.Fatalf("业务失败时不得留下通知，实际 %d：%+v", len(entries), entries)
	}
}
