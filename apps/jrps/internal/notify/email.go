package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// SMTP 渠道的固定上限。
const (
	// smtpTimeout 是单次投递的端到端超时。
	//
	// 它覆盖连接、TLS 握手、认证与投递全过程：SMTP 交互有多轮往返，
	// 只给连接设超时不足以防止慢服务器把发送器拖住。
	smtpTimeout = 15 * time.Second

	// maxRecipients 是单封邮件的收件人上限（规格 §3.4 要求有上限）。
	//
	// 取 20 是与"通知"的语义匹配的规模：通知是发给少数运维联系人的，
	// 不是邮件列表。上限同时约束了单次投递的连接时长。
	maxRecipients = 20

	// maxSubjectRunes 与 maxBodyRunes 限制邮件主题与正文长度。
	maxSubjectRunes = 120
	maxBodyRunes    = 2000
)

// emailSender 通过 SMTP 投递通知。
type emailSender struct {
	// skipAddressCheck 仅供进程内测试使用，含义与 webhookSender 一致。
	skipAddressCheck bool
}

// newEmailSender 构造生产用的邮件发送器。
func newEmailSender() *emailSender {
	return &emailSender{skipAddressCheck: false}
}

// newUnsafeEmailSenderForTest 构造仅供测试使用的邮件发送器。
//
// 命名刻意带上 Unsafe 与 ForTest：生产代码不得引用本函数。
func newUnsafeEmailSenderForTest() *emailSender {
	return &emailSender{skipAddressCheck: true}
}

// Send 投递一封通知邮件。
//
// SMTP 协议本身不提供"取消"语义：context 取消只通过提前截止连接生效，
// 已经发出的命令无法撤回。因此取消后立即关闭连接，不等待服务器响应。
func (sender *emailSender) Send(ctx context.Context, target Target, notification Notification) error {
	if err := sender.validate(target); err != nil {
		return err
	}
	message, recipients, err := buildEmail(target, notification)
	if err != nil {
		return permanentError(err.Error())
	}

	deadline := time.Now().Add(smtpTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	address := net.JoinHostPort(target.SMTPHost, fmt.Sprintf("%d", target.SMTPPort))
	connection, err := sender.dial(ctx, address, deadline)
	if err != nil {
		// 连接失败与超时是临时故障，按可重试归类。
		if ctx.Err() != nil {
			return retryableError("邮件投递超时或被取消")
		}
		return retryableError("无法连接 SMTP 服务器")
	}
	defer func() { _ = connection.Close() }()

	return deliverSMTP(connection, target, recipients, message)
}

// validate 校验目标配置的完整性与地址安全。
func (sender *emailSender) validate(target Target) error {
	if strings.TrimSpace(target.SMTPHost) == "" {
		return permanentError("邮件目标缺少 SMTP 主机")
	}
	if target.SMTPPort <= 0 || target.SMTPPort > 65535 {
		return permanentError("邮件目标的 SMTP 端口不合法")
	}
	if strings.TrimSpace(target.SMTPFrom) == "" {
		return permanentError("邮件目标缺少发件人")
	}
	if len(target.SMTPTo) == 0 {
		return permanentError("邮件目标缺少收件人")
	}
	if len(target.SMTPTo) > maxRecipients {
		return permanentError(fmt.Sprintf("收件人数量超出上限 %d", maxRecipients))
	}
	if err := sender.checkHost(target.SMTPHost); err != nil {
		return err
	}
	switch target.SMTPSecurity {
	case SMTPSecurityStartTLS:
		return nil
	case SMTPSecurityNone:
		// 明文 SMTP 会把通知内容暴露在链路上。允许它是为了兼容不支持
		// STARTTLS 的内网中继，但这是显式的取舍而非默认值。
		return nil
	default:
		return permanentError("安全传输选项不合法，只能是 starttls 或 none")
	}
}

// checkHost 校验 SMTP 主机的地址安全。
//
// 与 Webhook 同源：拒绝回环、链路本地与私有地址，避免把 SMTP 连接
// 变成探测内网服务的手段。
func (sender *emailSender) checkHost(host string) error {
	if sender.skipAddressCheck {
		return nil
	}
	addresses, err := net.LookupIP(host)
	if err != nil {
		// 解析失败可能是临时故障，按可重试处理而不是当场判死。
		return retryableError("SMTP 主机名无法解析")
	}
	if err := CheckResolvedAddresses(addresses); err != nil {
		// 地址落在禁止范围是配置问题：重试不会让它变合法。
		return permanentError("SMTP 主机地址未通过安全校验")
	}
	return nil
}

// dial 建立到 SMTP 服务器的连接，并按需完成 TLS 升级。
func (sender *emailSender) dial(ctx context.Context, address string, deadline time.Time) (net.Conn, error) {
	dialer := newOutboundDialer()
	if sender.skipAddressCheck {
		dialer = &net.Dialer{Timeout: smtpTimeout}
	}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	if err := connection.SetDeadline(deadline); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return connection, nil
}

// buildEmail 组装邮件正文与收件人列表。
func buildEmail(target Target, notification Notification) ([]byte, []string, error) {
	subject := emailSubject(notification)
	body := emailBody(notification)
	if len([]rune(subject)) > maxSubjectRunes {
		return nil, nil, fmt.Errorf("邮件主题超出长度上限")
	}
	if len([]rune(body)) > maxBodyRunes {
		return nil, nil, fmt.Errorf("邮件正文超出长度上限")
	}

	recipients := make([]string, 0, len(target.SMTPTo))
	for _, recipient := range target.SMTPTo {
		trimmed := strings.TrimSpace(recipient)
		if trimmed == "" {
			return nil, nil, fmt.Errorf("收件人不能为空")
		}
		recipients = append(recipients, trimmed)
	}

	// 头部使用 UTF-8 编码主题，正文声明 UTF-8，保证中文正确显示。
	var builder strings.Builder
	builder.WriteString("From: " + sanitizeHeader(target.SMTPFrom) + "\r\n")
	builder.WriteString("To: " + sanitizeHeader(strings.Join(recipients, ", ")) + "\r\n")
	builder.WriteString("Subject: " + encodeHeader(subject) + "\r\n")
	builder.WriteString("MIME-Version: 1.0\r\n")
	builder.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	builder.WriteString("\r\n")
	builder.WriteString(body)
	return []byte(builder.String()), recipients, nil
}

// emailSubject 生成中文主题，只含事件类型与时间。
func emailSubject(notification Notification) string {
	return "[JRP] " + notification.EventType + " " +
		notification.OccurredAt.UTC().Format(time.RFC3339)
}

// emailBody 生成中文正文，只含脱敏摘要。
func emailBody(notification Notification) string {
	var builder strings.Builder
	builder.WriteString("事件类型：" + notification.EventType + "\r\n")
	builder.WriteString("发生时间：" + notification.OccurredAt.UTC().Format(time.RFC3339) + "\r\n")
	builder.WriteString("事件标识：" + notification.EventID + "\r\n")
	builder.WriteString("\r\n摘要：\r\n")
	builder.WriteString(notification.Payload)
	return builder.String()
}

// sanitizeHeader 移除头部值中的换行，防止头部注入。
func sanitizeHeader(value string) string {
	replacer := strings.NewReplacer("\r", "", "\n", "")
	return replacer.Replace(value)
}

// encodeHeader 按 RFC 2047 编码非 ASCII 头部值。
func encodeHeader(value string) string {
	if isASCII(value) {
		return sanitizeHeader(value)
	}
	return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(sanitizeHeader(value))) + "?="
}

// isASCII 判断文本是否只含 ASCII 字符。
func isASCII(value string) bool {
	for index := 0; index < len(value); index += 1 {
		if value[index] > 127 {
			return false
		}
	}
	return true
}

// deliverSMTP 完成 SMTP 会话：握手、可选认证与投递。
//
// 错误在产生处就地分类，而不是事后按错误文本猜测：SMTP 服务器返回的
// 文本随实现而异（中文、英文、不同措辞），按文本判定归类必然误判。
func deliverSMTP(connection net.Conn, target Target, recipients []string, message []byte) error {
	client, err := smtp.NewClient(connection, target.SMTPHost)
	if err != nil {
		return retryableError("SMTP 握手失败")
	}
	defer func() { _ = client.Close() }()

	if target.SMTPSecurity == SMTPSecurityStartTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return permanentError("SMTP 服务器不支持 STARTTLS")
		}
		if err := client.StartTLS(&tls.Config{ServerName: target.SMTPHost, MinVersion: tls.VersionTLS12}); err != nil {
			return retryableError("SMTP 的 TLS 握手失败")
		}
	}
	// 密码为空时不认证：部分内网中继按来源地址授权。
	if target.Secret != "" {
		if ok, _ := client.Extension("AUTH"); !ok {
			return permanentError("SMTP 服务器不支持认证，但目标配置了密码")
		}
		auth := smtp.PlainAuth("", target.SMTPFrom, target.Secret, target.SMTPHost)
		if err := client.Auth(auth); err != nil {
			// 凭据错误是确定性失败：重试不会让错误的口令变对。
			return permanentError("SMTP 认证失败")
		}
	}
	if err := client.Mail(target.SMTPFrom); err != nil {
		// 发件人被拒通常是配置问题（地址不被服务器接受）。
		return permanentError("SMTP 服务器拒绝了发件人")
	}
	for _, recipient := range recipients {
		if err := client.Rcpt(recipient); err != nil {
			// 收件人被拒是确定性失败：地址不存在或不被接受。
			return permanentError("SMTP 服务器拒绝了收件人")
		}
	}
	writer, err := client.Data()
	if err != nil {
		return retryableError("SMTP 服务器拒绝接收邮件内容")
	}
	if _, err := writer.Write(message); err != nil {
		_ = writer.Close()
		return retryableError("写入邮件内容失败")
	}
	if err := writer.Close(); err != nil {
		return retryableError("提交邮件内容失败")
	}
	return nil
}
