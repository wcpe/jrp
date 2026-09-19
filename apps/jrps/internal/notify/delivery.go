package notify

import (
	"errors"
)

// DeliveryKind 是投递结果的可重试分类。
//
// outbox 需要据此决定进 retrying 还是直接 failed（规格 §3.4）：
// 超时与 5xx 属临时故障，重试有意义；明确无效目标与鉴权失败属确定性失败，
// 重试只会重复失败并拖长失败终态的到达时间。
type DeliveryKind int

const (
	// DeliveryRetryable 表示临时故障，应当在退避后重试。
	DeliveryRetryable DeliveryKind = iota
	// DeliveryPermanent 表示确定性失败，应直接进入失败终态。
	DeliveryPermanent
)

// DeliveryError 是带分类的投递错误。
//
// 只携带可安全公开的中文摘要：错误信息会进入日志与 outbox 的 LastError，
// 不得包含目标地址的凭据部分、响应体或堆栈。
type DeliveryError struct {
	Kind    DeliveryKind
	Summary string
}

func (err *DeliveryError) Error() string { return err.Summary }

// Retryable 判断错误是否应当在退避后重试。
//
// 未实现该接口的错误按可重试处理：把未知故障当临时问题比当作永久失败更保守，
// 前者最多多试几次，后者会让本该送达的通知永久丢失。
func Retryable(err error) bool {
	var deliveryErr *DeliveryError
	if errors.As(err, &deliveryErr) {
		return deliveryErr.Kind == DeliveryRetryable
	}
	return true
}

// retryableError 构造可重试的投递错误。
func retryableError(summary string) error {
	return &DeliveryError{Kind: DeliveryRetryable, Summary: summary}
}

// permanentError 构造确定性的投递错误。
func permanentError(summary string) error {
	return &DeliveryError{Kind: DeliveryPermanent, Summary: summary}
}

// RetryableDeliveryError 供外壳层构造可重试的投递错误。
//
// 外壳在"记录"与"通知"之间做适配时也会产生投递错误（例如读取目标配置失败），
// 这些错误同样需要带分类，否则会被 store 层按默认可重试处理而丢失语义。
func RetryableDeliveryError(summary string) error { return retryableError(summary) }

// PermanentDeliveryError 供外壳层构造确定性的投递错误。
func PermanentDeliveryError(summary string) error { return permanentError(summary) }

// Retryable 实现 store 侧的重试分类约定。
//
// store 通过最小接口（Retryable() bool）判定可重试性，不直接依赖本包类型。
func (err *DeliveryError) Retryable() bool { return err.Kind == DeliveryRetryable }
