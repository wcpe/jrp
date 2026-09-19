package notify

import (
	"context"
	"errors"
	"fmt"
)

// Sender 是渠道分派器：按目标类型选择 Webhook 或邮件渠道投递。
//
// 它实现 store 侧的发送接口约定（Send 接收已提交的 outbox 记录），
// 把"记录"翻译为"渠道 + 目标配置"。
type Sender struct {
	webhook *webhookSender
	email   *emailSender
}

// NewSender 构造生产用渠道分派器。
func NewSender() *Sender {
	return &Sender{webhook: newWebhookSender(), email: newEmailSender()}
}

// TargetLoader 按标识读取目标配置。
//
// 以接口注入而不直接依赖 store：notify 包只关心"能不能拿到投递所需的配置"，
// 不关心它来自 SQLite 还是别处。这也让渠道测试无需构造整个存储层。
type TargetLoader interface {
	LoadTarget(ctx context.Context, targetID string) (Target, error)
}

// ErrTargetNotFound 表示目标不存在；调用方据此把在途记录转入 discarded。
var ErrTargetNotFound = errors.New("通知目标不存在")

// Send 投递一条通知。
//
// 目标标识为空表示该记录未指定目标，属配置错误，直接判为确定性失败：
// 没有目标的记录无法投递，重试也不会凭空产生目标。
func (sender *Sender) Send(ctx context.Context, target Target, notification Notification) error {
	switch target.Type {
	case "webhook":
		return sender.webhook.Send(ctx, target, notification)
	case "email":
		return sender.email.Send(ctx, target, notification)
	default:
		return permanentError(fmt.Sprintf("未知的通知渠道类型：%s", target.Type))
	}
}
