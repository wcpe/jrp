package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Webhook 渠道的固定上限。
const (
	// webhookTimeout 是单次投递的端到端超时。
	//
	// 它同时约束连接、TLS 握手与响应读取：目标无响应时必须在有限时间内失败，
	// 否则发送器会被慢目标拖住，进而拖住 outbox 的后续记录。
	webhookTimeout = 10 * time.Second

	// maxWebhookResponseBytes 限制读取的响应体大小。
	//
	// 响应内容只用于判断状态码，不解析正文；限制读取量避免恶意或异常目标
	// 返回超大响应耗尽内存。
	maxWebhookResponseBytes = 4 << 10

	// maxWebhookPayloadBytes 限制请求体大小。
	maxWebhookPayloadBytes = 64 << 10
)

// signatureHeader 是 HMAC 签名头名。
const signatureHeader = "X-JRP-Signature"

// webhookSender 通过 HTTPS 投递通知。
type webhookSender struct {
	client *http.Client

	// skipAddressCheck 仅供进程内测试使用：httptest 服务器只能监听回环地址，
	// 而生产校验会拒绝回环。生产构造器 newWebhookSender 恒将其置为 false，
	// 测试必须显式调用 newUnsafeWebhookSenderForTest 才能开启，避免误用。
	skipAddressCheck bool
}

// webhookPayload 是投递的 JSON 体。
//
// 字段与规格 §3.4 一致：事件类型、发生时间、脱敏载荷与事件标识。
// 不包含目标地址、秘密或任何来自目标配置的信息。
type webhookPayload struct {
	EventID    string `json:"eventId"`
	EventType  string `json:"eventType"`
	OccurredAt string `json:"occurredAt"`
	Payload    string `json:"payload"`
}

// newWebhookSender 构造生产用的 Webhook 发送器，地址校验恒开启。
func newWebhookSender() *webhookSender {
	return &webhookSender{client: newWebhookClient(), skipAddressCheck: false}
}

// newWebhookClient 构造出站 HTTP 客户端。
//
// 客户端刻意不跟随重定向：规格要求"重定向不得被自动跟随到被禁止范围"，
// 而校验每一跳的目标比直接拒绝重定向更容易出错——一个校验疏漏就让 SSRF
// 防线失效。拒绝重定向把该风险降为零，代价是目标不能使用重定向。
//
// 拨号层再挂一道地址校验，防止域名解析后指向内网（DNS 重绑定）。
func newWebhookClient() *http.Client {
	transport := &http.Transport{
		DialContext:           newOutboundDialer().DialContext,
		TLSHandshakeTimeout:   webhookTimeout,
		ResponseHeaderTimeout: webhookTimeout,
		ExpectContinueTimeout: time.Second,
		// 不复用连接：通知是低频操作，连接复用带来的收益有限，
		// 而每个目标保留空闲连接会让"目标已删除"后的连接继续存活。
		DisableKeepAlives: true,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   webhookTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// newUnsafeWebhookSenderForTest 构造仅供进程内测试使用的发送器。
//
// 它跳过地址范围校验并允许拨号到回环，因为 httptest 服务器只能监听回环。
// 命名刻意带上 Unsafe 与 ForTest：生产代码不得引用本函数，任何对它的
// 调用都应当被视为绕过 SSRF 防线的缺陷。
func newUnsafeWebhookSenderForTest() *webhookSender {
	transport := &http.Transport{
		DialContext: (&net.Dialer{Timeout: webhookTimeout}).DialContext,
		// httptest 的 TLS 服务器使用自签证书，测试需跳过校验。
		// 这是测试专用客户端，生产客户端不设置该项。
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		TLSHandshakeTimeout:   webhookTimeout,
		ResponseHeaderTimeout: webhookTimeout,
		DisableKeepAlives:     true,
	}
	return &webhookSender{
		client: &http.Client{
			Transport: transport,
			Timeout:   webhookTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		skipAddressCheck: true,
	}
}

// Send 投递一条 Webhook 通知。
//
// secret 为空时跳过签名：规格允许目标不配置签名，此时下游无法验证来源，
// 但请求仍走 HTTPS。secret 只用于生成签名头，不出现在请求体或错误信息中。
func (sender *webhookSender) Send(ctx context.Context, target Target, notification Notification) error {
	parsed, err := validateWebhookURL(target.WebhookURL, sender.skipAddressCheck)
	if err != nil {
		return permanentError(err.Error())
	}
	body, err := encodeWebhookPayload(notification)
	if err != nil {
		return permanentError(err.Error())
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String(), bytes.NewReader(body))
	if err != nil {
		return permanentError("构造通知请求失败")
	}
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("User-Agent", "jrp-notify/1")
	if target.Secret != "" {
		request.Header.Set(signatureHeader, signWebhookBody(target.Secret, body))
	}

	response, err := sender.client.Do(request)
	if err != nil {
		return classifyTransportError(ctx, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxWebhookResponseBytes))
		_ = response.Body.Close()
	}()
	return classifyWebhookStatus(response.StatusCode)
}

// encodeWebhookPayload 序列化请求体。
func encodeWebhookPayload(notification Notification) ([]byte, error) {
	body, err := json.Marshal(webhookPayload{
		EventID:    notification.EventID,
		EventType:  notification.EventType,
		OccurredAt: notification.OccurredAt.UTC().Format(time.RFC3339),
		Payload:    notification.Payload,
	})
	if err != nil {
		return nil, fmt.Errorf("序列化通知载荷失败：%w", err)
	}
	if len(body) > maxWebhookPayloadBytes {
		return nil, fmt.Errorf("通知载荷超出上限")
	}
	return body, nil
}

// signWebhookBody 用目标 secret 生成请求体签名。
//
// 采用 HMAC-SHA256 的十六进制表示，前缀标注算法便于下游按算法分派。
func signWebhookBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// classifyWebhookStatus 按状态码归类投递结果。
//
// 归类依据是"重试是否可能改变结果"：4xx 表示请求本身不被接受（目标地址错的、
// 鉴权不对、方法不允许），重试不会变；5xx 与 429 是目标侧临时状态，重试有意义。
//
// 3xx 归为确定性失败：本渠道刻意不跟随重定向（见 newWebhookClient），
// 收到 3xx 说明目标配置指向了需要跳转的地址，重试仍会得到同样的跳转响应。
func classifyWebhookStatus(status int) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusTooManyRequests:
		return retryableError(fmt.Sprintf("目标限流（HTTP %d）", status))
	case status >= 500:
		return retryableError(fmt.Sprintf("目标服务异常（HTTP %d）", status))
	case status >= 300 && status < 400:
		return permanentError(fmt.Sprintf("目标返回重定向（HTTP %d），本渠道不跟随重定向", status))
	case status >= 400:
		return permanentError(fmt.Sprintf("目标拒绝请求（HTTP %d）", status))
	default:
		return retryableError(fmt.Sprintf("目标返回未预期状态（HTTP %d）", status))
	}
}

// classifyTransportError 归类传输层错误。
//
// 错误信息只给可安全公开的类别说明，不转发底层错误文本：后者可能包含
// 完整地址（含查询串凭据）或内部解析细节。
func classifyTransportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return retryableError("投递超时或被取消")
	}
	// 地址被 SSRF 防线拒绝属确定性失败：目标配置不会自行变合法。
	// 按哨兵错误判定而不是匹配错误文本，避免文案调整后归类失效。
	if errors.Is(err, ErrTargetBlocked) || errors.Is(err, ErrTargetMalformed) {
		return permanentError("目标地址未通过安全校验")
	}
	return retryableError("无法连接目标（DNS、TLS 或网络故障）")
}
