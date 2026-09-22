package store

import (
	"context"
	"strings"
	"testing"
)

// 配置应用失败必须在同一业务事务内广播 outbox 通知（FR-15 §6.1 的 FR-10 写入点）。
//
// 载荷只含 revision、阶段与脱敏错误摘要（规格 §3.5 白名单：revision、阶段、
// 结果类别与安全错误摘要），不含凭证或正文。
func TestRecordApplyFailureBroadcastsNotification(t *testing.T) {
	database := openServerStore(t, t.TempDir()+"/jrps.db")
	actor := ActorAdmin("admin")
	createWebhookTarget(t, database, "验收目标", true)

	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		if _, err := tx.SaveProxy(Proxy{ID: "p1", Name: "代理", Type: "tcp"}, actor, OriginProxyCreate); err != nil {
			return err
		}
		return tx.RecordApplyResult(ApplyResultInput{
			Revision: 1, Phase: PhaseHealthCheck, Succeeded: false,
			ErrorDetail: "端口绑定失败", Actor: actor, RequestID: "req-1",
		})
	}); err != nil {
		t.Fatalf("记录应用失败结果失败：%v", err)
	}

	events := mustOutboxEvents(t, database, EventTypeConfigApplyFailed)
	if len(events) != 1 {
		t.Fatalf("失败应用应向唯一启用目标写一条通知，实际 %d 条", len(events))
	}
	payload := events[0].Payload
	if !strings.Contains(payload, "health_check") || !strings.Contains(payload, "端口绑定失败") {
		t.Fatalf("载荷应含阶段与脱敏错误摘要：%s", payload)
	}
	if strings.Contains(payload, "token") || strings.Contains(payload, "password") {
		t.Fatalf("载荷不得含凭证字段：%s", payload)
	}
}

// 成功的应用不产生失败通知：通知是给管理员"出事了"的信号，成功发通知是噪声。
func TestRecordApplySuccessDoesNotBroadcast(t *testing.T) {
	database := openServerStore(t, t.TempDir()+"/jrps.db")
	actor := ActorAdmin("admin")

	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		if _, err := tx.SaveProxy(Proxy{ID: "p1", Name: "代理", Type: "tcp"}, actor, OriginProxyCreate); err != nil {
			return err
		}
		return tx.RecordApplyResult(ApplyResultInput{
			Revision: 1, Phase: PhasePublish, Succeeded: true, Actor: actor,
		})
	}); err != nil {
		t.Fatalf("记录应用成功结果失败：%v", err)
	}

	events := mustOutboxEvents(t, database, EventTypeConfigApplyFailed)
	if len(events) != 0 {
		t.Fatalf("成功的应用不应产生失败通知，实际 %d 条", len(events))
	}
}

// 每次失败的应用只发一条通知，而不是每个阶段一条：prepare 失败后流程终止，
// 后续阶段没有执行；同一轮应用里 validate+prepare 两阶段都失败时（快照转换
// 失败与引擎失败不会同时发生），通知按"每次 Apply 至多一条"去重。
//
// 该语义由编排层保证（失败即终止，只记录一个失败阶段）；store 层逐条记录
// 落库，广播只对"失败阶段"发生。本用例验证两次独立失败产生两条通知。
func TestEachFailedApplyBroadcastsOnce(t *testing.T) {
	database := openServerStore(t, t.TempDir()+"/jrps.db")
	actor := ActorAdmin("admin")
	createWebhookTarget(t, database, "验收目标", true)

	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		if _, err := tx.SaveProxy(Proxy{ID: "p1", Name: "代理", Type: "tcp"}, actor, OriginProxyCreate); err != nil {
			return err
		}
		if err := tx.RecordApplyResult(ApplyResultInput{
			Revision: 1, Phase: PhasePrepare, Succeeded: false,
			ErrorDetail: "第一次失败", Actor: actor,
		}); err != nil {
			return err
		}
		return tx.RecordApplyResult(ApplyResultInput{
			Revision: 2, Phase: PhaseHealthCheck, Succeeded: false,
			ErrorDetail: "第二次失败", Actor: actor,
		})
	}); err != nil {
		t.Fatalf("记录两次失败失败：%v", err)
	}

	events := mustOutboxEvents(t, database, EventTypeConfigApplyFailed)
	if len(events) != 2 {
		t.Fatalf("两次独立失败应产生两条通知，实际 %d 条", len(events))
	}
}

// mustOutboxEvents 读取指定事件类型的全部 outbox 记录（测试辅助）。
func mustOutboxEvents(t *testing.T, database *Store, eventType string) []NotificationOutbox {
	t.Helper()
	var events []NotificationOutbox
	if err := database.View(context.Background(), func(tx *Tx) error {
		return tx.db.Where("event_type = ?", eventType).Order("id ASC").Find(&events).Error
	}); err != nil {
		t.Fatalf("读取 outbox 事件失败：%v", err)
	}
	return events
}
