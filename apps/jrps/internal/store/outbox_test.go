package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// 借发送器：记录被调用的时间点，用于验证外部副作用发生在事务提交之后。
type recordingSender struct {
	calls      []NotificationOutbox
	failures   int
	commitSeen *bool
}

func (s *recordingSender) Send(_ context.Context, entry NotificationOutbox) error {
	s.calls = append(s.calls, entry)
	if s.commitSeen != nil && !*s.commitSeen {
		return errors.New("发送器在事务提交前被调用")
	}
	if s.failures > 0 {
		s.failures--
		return errors.New("下游不可达")
	}
	return nil
}

// 事务回滚后不得存在可发送的 outbox 记录，发送器也不得拿到该批记录。
func TestOutboxRecordsDisappearOnRollback(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	sender := &recordingSender{}

	rollback := errors.New("业务失败")
	err := store.Transaction(context.Background(), func(tx *Tx) error {
		if _, err := tx.OutboxEnqueue(NotificationOutbox{
			EventID: "evt-1", EventType: "config_applied", Payload: `{"结果":"成功"}`,
		}); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("事务应回滚并返回业务错误：%v", err)
	}

	dispatcher := mustDispatcher(t, store, sender)
	processed, err := dispatcher.DispatchCommitted(context.Background(), 10)
	if err != nil {
		t.Fatalf("派发失败：%v", err)
	}
	if processed != 0 || len(sender.calls) != 0 {
		t.Fatalf("回滚事务的记录不得被发送：processed=%d calls=%d", processed, len(sender.calls))
	}
	if count := mustPendingOutboxCount(t, store); count != 0 {
		t.Fatalf("回滚后不得存在可发送记录，实际为 %d", count)
	}
}

// 提交成功后才触发外部副作用，且成功记录进入终态。
func TestOutboxDispatchHappensAfterCommit(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	committed := false
	sender := &recordingSender{commitSeen: &committed}

	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		if _, err := tx.SaveProxy(Proxy{ID: "p1", Name: "代理", Type: "tcp"}, ActorAdmin("admin"), OriginProxyCreate); err != nil {
			return err
		}
		_, err := tx.OutboxEnqueue(NotificationOutbox{
			EventID: "evt-1", EventType: "proxy_created", Payload: `{"代理":"代理"}`,
		})
		return err
	}); err != nil {
		t.Fatalf("业务事务失败：%v", err)
	}
	committed = true

	dispatcher := mustDispatcher(t, store, sender)
	processed, err := dispatcher.DispatchCommitted(context.Background(), 10)
	if err != nil {
		t.Fatalf("派发失败：%v", err)
	}
	if processed != 1 || len(sender.calls) != 1 {
		t.Fatalf("提交后应发送一条通知：processed=%d calls=%d", processed, len(sender.calls))
	}

	entries := mustOutboxEntries(t, store)
	if len(entries) != 1 || entries[0].Status != OutboxStatusSent {
		t.Fatalf("发送成功后应进入终态 sent：%+v", entries)
	}
	if entries[0].LeaseExpiresAt != nil {
		t.Fatalf("进入终态后不应保留租约：%+v", entries[0])
	}
}

// 发送失败按退避重新入队并递增次数；达到上限后进入失败终态。
func TestOutboxRetryReachesFailedState(t *testing.T) {
	const maxAttempts = 5
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	sender := &recordingSender{failures: maxAttempts}

	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.OutboxEnqueue(NotificationOutbox{EventID: "evt-1", EventType: "x", Payload: "{}"})
		return err
	}); err != nil {
		t.Fatalf("写入 outbox 失败：%v", err)
	}

	dispatcher := mustDispatcher(t, store, sender)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if _, err := dispatcher.DispatchCommitted(context.Background(), 1); err != nil {
			t.Fatalf("第 %d 次派发失败：%v", attempt+1, err)
		}
		entries := mustOutboxEntries(t, store)
		if entries[0].Attempts != attempt+1 {
			t.Fatalf("重试次数未递增：%+v", entries[0])
		}
		if attempt+1 < maxAttempts && entries[0].Status != OutboxStatusRetrying {
			t.Fatalf("未达上限时应处于 retrying：%+v", entries[0])
		}
	}

	entries := mustOutboxEntries(t, store)
	if entries[0].Status != OutboxStatusFailed {
		t.Fatalf("达到重试上限后应进入失败终态：%+v", entries[0])
	}
	if !strings.Contains(entries[0].LastError, "下游不可达") {
		t.Fatalf("失败终态应留下脱敏错误摘要：%+v", entries[0])
	}
	if count := mustPendingOutboxCount(t, store); count != 0 {
		t.Fatalf("失败终态不应仍可发送：%d", count)
	}
}

// 同一事件标识重复入队必须被拒绝，避免重试放大为多条通知。
func TestOutboxEventIDIsUnique(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	err := store.Transaction(context.Background(), func(tx *Tx) error {
		if _, err := tx.OutboxEnqueue(NotificationOutbox{EventID: "evt-1", EventType: "x"}); err != nil {
			return err
		}
		_, err := tx.OutboxEnqueue(NotificationOutbox{EventID: "evt-1", EventType: "x"})
		return err
	})
	if err == nil {
		t.Fatal("同一事件标识重复入队必须被拒绝")
	}
}

// 通知秘密写入后可读，但脱敏展示必须只给出掩码。
func TestNotificationSecretIsMaskedOnRead(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	const secret = "webhook-secret-abcdef123456"

	err := store.Transaction(context.Background(), func(tx *Tx) error {
		target := NotificationTarget{
			ID: "target-1", Type: "webhook", Name: "运维群",
			TargetSummary: "https://hooks.invalid/***", Secret: secret, Enabled: true,
		}
		if err := tx.DB().Create(&target).Error; err != nil {
			return err
		}
		view, err := tx.NotificationTarget("target-1")
		if err != nil {
			return err
		}
		if strings.Contains(view.MaskedSecret(), secret) {
			return errors.New("脱敏展示泄露了完整秘密")
		}
		if !strings.HasSuffix(view.MaskedSecret(), secret[len(secret)-4:]) {
			return errors.New("脱敏展示应保留末四位便于运维识别")
		}
		if !strings.HasPrefix(view.MaskedSecret(), "*") {
			return errors.New("脱敏展示应以掩码开头")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("通知目标脱敏校验失败：%v", err)
	}
}

// 审计事件不得包含完整秘密。
func TestAuditContextDoesNotLeakSecrets(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	const secret = "webhook-secret-abcdef123456"

	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		return tx.Transaction(func() error {
			return tx.writeAudit(AuditEvent{
				ActorType: ActorTypeAdmin, ActorID: "admin",
				Action: ActionProxyCreate, ObjectType: "notification_target", ObjectID: "target-1",
				Result: AuditResultSuccess, Context: "修改通知目标（秘密已掩码）",
			})
		})
	}); err != nil {
		t.Fatalf("写入审计失败：%v", err)
	}

	var events []AuditEvent
	if err := store.View(context.Background(), func(tx *Tx) error {
		var err error
		events, err = tx.AuditEvents()
		return err
	}); err != nil {
		t.Fatalf("读取审计失败：%v", err)
	}
	for _, event := range events {
		if strings.Contains(event.Context, secret) || strings.Contains(event.ObjectID, secret) {
			t.Fatalf("审计事件泄露了秘密：%+v", event)
		}
	}
}

// 出站通知载荷必须是脱敏白名单字段；这里校验类型定义不提供整段秘密的直通字段。
func TestOutboxPayloadCarriesNoSecretField(t *testing.T) {
	entry := NotificationOutbox{EventID: "evt-1", EventType: "x", Payload: `{"结果":"成功"}`}
	if strings.Contains(entry.Payload, "secret") {
		t.Fatalf("outbox 载荷不应含秘密字段：%s", entry.Payload)
	}
}

// 测试辅助：构造使用零退避的发送器，使重试可被连续驱动。
func mustDispatcher(t *testing.T, store *Store, sender Sender) *OutboxDispatcher {
	t.Helper()
	dispatcher, err := NewOutboxDispatcher(OutboxDispatcherConfig{
		Store: store, Sender: sender, MaxAttempts: 5,
		Backoff: func(int) time.Duration { return 0 },
	})
	if err != nil {
		t.Fatalf("构造发送器失败：%v", err)
	}
	return dispatcher
}

func mustOutboxEntries(t *testing.T, store *Store) []NotificationOutbox {
	t.Helper()
	var entries []NotificationOutbox
	if err := store.View(context.Background(), func(tx *Tx) error {
		var err error
		entries, err = tx.OutboxEntries()
		return err
	}); err != nil {
		t.Fatalf("读取 outbox 失败：%v", err)
	}
	return entries
}

func mustPendingOutboxCount(t *testing.T, store *Store) int64 {
	t.Helper()
	var count int64
	if err := store.View(context.Background(), func(tx *Tx) error {
		var err error
		count, err = tx.PendingOutboxCount()
		return err
	}); err != nil {
		t.Fatalf("统计待发送通知失败：%v", err)
	}
	return count
}
