package apply

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/wcpe/jrp/apps/jrps/internal/store"
	"github.com/wcpe/jrp/core"
)

// eventSubscriber 是事件订阅端口的窄接口：真实引擎与测试替身均可实现。
type eventSubscriber interface {
	Subscribe(options core.Options) *core.Subscription
}

// StartEventAdapter 启动事件适配器：建立订阅并开始消费。
//
// 消费在独立 goroutine 中进行，随 ctx 取消退出；订阅在退出时关闭。
// 适配器是事件消费者（FR-12 §3.7），其消费速度不影响数据面：SubmitLogEvent
// 的有界通道保证慢日志只降级丢弃、绝不阻塞消费循环。
func StartEventAdapter(ctx context.Context, engine eventSubscriber, database *store.Store, logger *slog.Logger) {
	subscription := engine.Subscribe(core.Options{Capacity: 32})
	go func() {
		defer subscription.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-subscription.Events():
				if !ok {
					return
				}
				submitEventLog(database, event)
			}
		}
	}()
}

// 事件适配器把 FR-27 事件映射为运行日志（FR-12 规格 §3.7）。
//
// 映射规则：ClientConnected → INFO（含客户端标识）；ApplyResultEvent 成功 →
// INFO、失败 → WARN；EngineStopped 成功停止 → INFO、异常停止 → ERROR；
// ResyncRequired → WARN（记录溢出与丢弃数，重建视图由订阅方按 State() 执行，
// 不逐条猜写缺失事件）。所有日志提交经 store.SubmitLogEvent 有界通道。

// submitEventLog 把单个 Core 事件映射为一条运行日志。
func submitEventLog(database *store.Store, event core.Event) {
	var level, eventName, message, clientID string
	switch ev := event.(type) {
	case core.ClientConnected:
		level = "INFO"
		eventName = "client-connected"
		clientID = ev.ClientID
		message = fmt.Sprintf("客户端 %s 已连接", ev.ClientID)
	case core.ApplyResultEvent:
		if ev.Err != nil {
			level = "WARN"
			eventName = "event-apply-failed"
			message = fmt.Sprintf("配置应用 %d 失败：%s", ev.Revision, ev.Err)
		} else {
			level = "INFO"
			eventName = "event-apply-complete"
			message = fmt.Sprintf("配置应用 %d 完成（阶段 %s）", ev.Revision, ev.Stage)
		}
	case core.EngineStopped:
		if ev.Err != nil {
			level = "ERROR"
			eventName = "engine-stopped-abnormal"
			message = fmt.Sprintf("引擎异常停止：%s", ev.Err)
		} else {
			level = "INFO"
			eventName = "engine-stopped"
			message = "引擎已正常停止"
		}
	case core.ResyncRequired:
		// 规格的降级路径在 resync 专项测试覆盖（重建视图）；
		// 此处仅记录溢出事实。
		level = "WARN"
		eventName = "event-resync-required"
		message = fmt.Sprintf("事件通道溢出（丢弃 %d 条），视图需重建", ev.Dropped)
	default:
		return
	}
	database.SubmitLogEvent(store.LogEvent{
		Level:     level,
		Component: "events",
		Event:     eventName,
		Message:   message,
		ClientID:  clientID,
	})
}
