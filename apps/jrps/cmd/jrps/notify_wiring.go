package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/wcpe/jrp/apps/jrps/internal/notify"
	"github.com/wcpe/jrp/apps/jrps/internal/store"
)

// storeTargetLoader 从 jrps 数据库读取通知目标，转成渠道层所需的配置。
//
// 转换只填充投递所需字段：库内的 CreatedAt、Enabled 等与本层无关，
// 带进来只会扩大敏感数据在内存中的流转面。
type storeTargetLoader struct {
	store *store.Store
}

// LoadTarget 读取目标配置；目标不存在时返回 notify.ErrTargetNotFound。
func (loader storeTargetLoader) LoadTarget(_ context.Context, targetID string) (notify.Target, error) {
	var record store.NotificationTarget
	found := true
	if err := loader.store.View(context.Background(), func(tx *store.Tx) error {
		err := tx.DB().Where("id = ?", targetID).First(&record).Error
		if err != nil {
			found = false
			return nil
		}
		return nil
	}); err != nil {
		return notify.Target{}, err
	}
	if !found {
		return notify.Target{}, notify.ErrTargetNotFound
	}
	if !record.Enabled {
		// 目标被禁用等同于不可投递：调用方会把它转入 discarded。
		return notify.Target{}, notify.ErrTargetNotFound
	}
	return notify.Target{
		ID:           record.ID,
		Type:         record.Type,
		Name:         record.Name,
		Secret:       record.Secret,
		WebhookURL:   record.WebhookURL,
		SMTPHost:     record.SMTPHost,
		SMTPPort:     record.SMTPPort,
		SMTPFrom:     record.SMTPFrom,
		SMTPTo:       splitRecipients(record.SMTPTo),
		SMTPSecurity: record.SMTPSecurity,
	}, nil
}

// LoadEnabledTargets 读取全部启用目标，供未指定目标的广播事件使用。
//
// 单独提供而不是让调用方拼 LoadTarget：逐个按 ID 加载需要先知道 ID 列表，
// 那会把 store 的查询细节泄漏到本层之外。
func (loader storeTargetLoader) LoadEnabledTargets(_ context.Context) ([]notify.Target, error) {
	var records []store.NotificationTarget
	if err := loader.store.View(context.Background(), func(tx *store.Tx) error {
		return tx.DB().Where("enabled = ?", true).Find(&records).Error
	}); err != nil {
		return nil, err
	}
	targets := make([]notify.Target, 0, len(records))
	for _, record := range records {
		targets = append(targets, notify.Target{
			ID:           record.ID,
			Type:         record.Type,
			Name:         record.Name,
			Secret:       record.Secret,
			WebhookURL:   record.WebhookURL,
			SMTPHost:     record.SMTPHost,
			SMTPPort:     record.SMTPPort,
			SMTPFrom:     record.SMTPFrom,
			SMTPTo:       splitRecipients(record.SMTPTo),
			SMTPSecurity: record.SMTPSecurity,
		})
	}
	return targets, nil
}

// splitRecipients 把逗号分隔的收件人拆成列表，忽略空白项。
func splitRecipients(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	recipients := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			recipients = append(recipients, trimmed)
		}
	}
	return recipients
}

// outboxSender 把渠道层分派器适配为 store 的发送接口。
//
// 两层接口的差异仅在于"记录"与"通知"的形态：store 层传整条 outbox 记录，
// 渠道层只关心事件标识、类型、时间与脱敏载荷。适配在此完成，
// 让 store 不依赖 notify、notify 不依赖 store。
type outboxSender struct {
	loader storeTargetLoader
	sender *notify.Sender
	logger *slog.Logger
}

// Send 按 outbox 记录投递通知。
//
// 记录未指定目标时广播给全部启用目标：删除类事件无法指向任何目标——被删的
// 目标此刻已不存在，指向它会让记录以"投递失败"收场，而真实原因是目标已消失。
// 广播时只有投递全部失败才返回错误，避免部分目标故障让整条记录反复重试、
// 把同一条通知重复塞给已经收过的目标。
func (adapter outboxSender) Send(ctx context.Context, entry store.NotificationOutbox) error {
	notification := notify.Notification{
		EventID:    entry.EventID,
		EventType:  entry.EventType,
		OccurredAt: entry.CreatedAt,
		Payload:    entry.Payload,
	}
	if entry.TargetID == "" {
		return adapter.broadcast(ctx, notification)
	}
	target, err := adapter.loader.LoadTarget(ctx, entry.TargetID)
	if err != nil {
		if errors.Is(err, notify.ErrTargetNotFound) {
			// 目标缺失属确定性失败：重试不会凭空产生目标。
			return notify.PermanentDeliveryError("通知目标已不存在或已停用")
		}
		return notify.RetryableDeliveryError("读取通知目标失败")
	}
	return adapter.sender.Send(ctx, target, notification)
}

// broadcast 把通知投递给全部启用目标。
func (adapter outboxSender) broadcast(ctx context.Context, notification notify.Notification) error {
	targets, err := adapter.loader.LoadEnabledTargets(ctx)
	if err != nil {
		return notify.RetryableDeliveryError("读取通知目标失败")
	}
	if len(targets) == 0 {
		// 没有任何接收方：这不是故障，重试也不会凭空产生目标。
		if adapter.logger != nil {
			adapter.logger.Warn("广播通知时无启用目标，该事件无人接收",
				"eventType", notification.EventType, "eventID", notification.EventID)
		}
		return nil
	}
	var lastError error
	delivered := 0
	for index := range targets {
		if err := adapter.sender.Send(ctx, targets[index], notification); err != nil {
			lastError = err
			continue
		}
		delivered++
	}
	if delivered == 0 {
		return lastError
	}
	return nil
}

// testNotifier 执行测试通知投递。
//
// 与业务通知走同一套渠道实现，只是事件类型固定为"测试"：若测试通知另走一条
// 简化路径，它验证的就不是真实投递链路，测通了也不能说明业务通知能送达。
type testNotifier struct {
	store  *store.Store
	sender *notify.Sender
}

// SendTest 向指定目标发送一条测试通知。
func (notifier testNotifier) SendTest(ctx context.Context, target store.NotificationTargetView) error {
	converted := notify.Target{
		ID:           target.ID,
		Type:         target.Type,
		Name:         target.Name,
		WebhookURL:   target.WebhookURL,
		SMTPHost:     target.SMTPHost,
		SMTPPort:     target.SMTPPort,
		SMTPFrom:     target.SMTPFrom,
		SMTPTo:       target.SMTPTo,
		SMTPSecurity: target.SMTPSecurity,
	}
	// 测试通知必须携带真实秘密：否则鉴权与签名路径得不到验证。
	secret, err := notifier.loadSecret(ctx, target.ID)
	if err != nil {
		return notify.PermanentDeliveryError("读取目标秘密失败")
	}
	converted.Secret = secret

	return notifier.sender.Send(ctx, converted, notify.Notification{
		EventID:    "test-" + target.ID,
		EventType:  "test",
		OccurredAt: time.Now().UTC(),
		Payload:    "这是一条测试通知，由管理员手动触发；收到即表示该目标可正常接收通知。",
	})
}

// loadSecret 读取目标秘密明文，仅供投递使用。
func (notifier testNotifier) loadSecret(ctx context.Context, targetID string) (string, error) {
	var record store.NotificationTarget
	found := false
	err := notifier.store.View(ctx, func(tx *store.Tx) error {
		if err := tx.DB().Where("id = ?", targetID).First(&record).Error; err != nil {
			return nil
		}
		found = true
		return nil
	})
	if err != nil || !found {
		return "", errors.New("目标不存在")
	}
	return record.Secret, nil
}
