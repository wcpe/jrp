package notify

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeSMTPServer 是一个用于测试的最小 SMTP 服务器。
//
// 只实现投递所需的命令序列（EHLO、MAIL、RCPT、DATA），并把收到的邮件
// 内容记录下来供断言。刻意不用第三方 SMTP 库：测试不应为验证标准库行为
// 而引入新依赖。
type fakeSMTPServer struct {
	listener net.Listener
	// messages 记录每封邮件的收件人与内容。
	messages []fakeSMTPMessage
	// rejectRecipient 非空时，对该收件人返回 550。
	rejectRecipient string
	// requireAuth 为真时要求 AUTH 成功，否则返回 535。
	requireAuth bool
	// authOK 表示认证是否应成功。
	authOK bool
}

type fakeSMTPMessage struct {
	recipients []string
	body       string
}

// startFakeSMTPServer 启动测试服务器。
func startFakeSMTPServer(t *testing.T) *fakeSMTPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动测试 SMTP 服务器失败：%v", err)
	}
	server := &fakeSMTPServer{listener: listener, authOK: true}
	go server.serve()
	t.Cleanup(func() { _ = listener.Close() })
	return server
}

// port 返回服务器监听端口。
func (server *fakeSMTPServer) port() int {
	return server.listener.Addr().(*net.TCPAddr).Port
}

// serve 处理连接。
func (server *fakeSMTPServer) serve() {
	for {
		connection, err := server.listener.Accept()
		if err != nil {
			return
		}
		go server.handle(connection)
	}
}

// handle 处理单个会话。
func (server *fakeSMTPServer) handle(connection net.Conn) {
	defer func() { _ = connection.Close() }()
	reader := bufio.NewReader(connection)
	writer := bufio.NewWriter(connection)
	reply := func(text string) {
		_, _ = writer.WriteString(text + "\r\n")
		_ = writer.Flush()
	}
	reply("220 fake.local ESMTP ready")

	var recipients []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		command := strings.TrimSpace(line)
		upper := strings.ToUpper(command)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			reply("250-fake.local")
			if server.requireAuth {
				reply("250-AUTH PLAIN")
			}
			reply("250 STARTTLS")
		case strings.HasPrefix(upper, "AUTH"):
			if server.authOK {
				reply("235 认证成功")
			} else {
				reply("535 认证失败")
			}
		case strings.HasPrefix(upper, "MAIL FROM"):
			reply("250 发件人已接受")
		case strings.HasPrefix(upper, "RCPT TO"):
			recipient := extractAddress(command)
			if server.rejectRecipient != "" && strings.EqualFold(recipient, server.rejectRecipient) {
				reply("550 收件人不存在")
				continue
			}
			recipients = append(recipients, recipient)
			reply("250 收件人已接受")
		case strings.HasPrefix(upper, "DATA"):
			reply("354 开始输入内容")
			body := readData(reader)
			server.messages = append(server.messages, fakeSMTPMessage{recipients: recipients, body: body})
			recipients = nil
			reply("250 邮件已接收")
		case strings.HasPrefix(upper, "QUIT"):
			reply("221 再见")
			return
		case strings.HasPrefix(upper, "RSET"):
			recipients = nil
			reply("250 已重置")
		default:
			reply("250 已接受")
		}
	}
}

// readData 读取 DATA 段直到结束标记。
func readData(reader *bufio.Reader) string {
	var builder strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.TrimSpace(line) == "." {
			break
		}
		builder.WriteString(line)
	}
	return builder.String()
}

// extractAddress 从 RCPT TO:<addr> 中取出地址。
func extractAddress(command string) string {
	start := strings.Index(command, "<")
	end := strings.LastIndex(command, ">")
	if start < 0 || end <= start {
		return ""
	}
	return command[start+1 : end]
}

// 测试辅助：构造一个指向测试服务器的邮件目标。
func testEmailTarget(server *fakeSMTPServer) Target {
	return Target{
		ID:           "mail-1",
		Type:         "email",
		Name:         "运维邮箱",
		SMTPHost:     "127.0.0.1",
		SMTPPort:     server.port(),
		SMTPFrom:     "jrp@example.invalid",
		SMTPTo:       []string{"ops@example.invalid"},
		SMTPSecurity: SMTPSecurityNone,
	}
}

// 正常投递：收件人与中文内容正确送达。
func TestEmailSendDeliversChineseMessage(t *testing.T) {
	server := startFakeSMTPServer(t)
	sender := newUnsafeEmailSenderForTest()
	target := testEmailTarget(server)

	if err := sender.Send(context.Background(), target, testNotification()); err != nil {
		t.Fatalf("投递失败：%v", err)
	}
	if len(server.messages) != 1 {
		t.Fatalf("应投递 1 封邮件，实际 %d 封", len(server.messages))
	}
	message := server.messages[0]
	if len(message.recipients) != 1 || message.recipients[0] != "ops@example.invalid" {
		t.Fatalf("收件人不匹配：%v", message.recipients)
	}
	if !strings.Contains(message.body, "apply_failure") {
		t.Fatalf("正文应含事件类型：%s", message.body)
	}
	// 主题按 RFC 2047 编码，正文为 UTF-8 中文。
	if !strings.Contains(message.body, "事件类型") {
		t.Fatalf("正文应含中文说明：%s", message.body)
	}
	if !strings.Contains(message.body, "UTF-8") {
		t.Fatalf("正文应声明 UTF-8 编码：%s", message.body)
	}
}

// 多收件人时逐个投递，且不抄送到未配置地址。
func TestEmailSendsToAllConfiguredRecipients(t *testing.T) {
	server := startFakeSMTPServer(t)
	sender := newUnsafeEmailSenderForTest()
	target := testEmailTarget(server)
	target.SMTPTo = []string{"a@example.invalid", "b@example.invalid"}

	if err := sender.Send(context.Background(), target, testNotification()); err != nil {
		t.Fatalf("投递失败：%v", err)
	}
	if len(server.messages[0].recipients) != 2 {
		t.Fatalf("应投递给 2 个收件人，实际 %v", server.messages[0].recipients)
	}
}

// 收件人超过上限必须拒绝：规格要求收件人数量有上限。
func TestEmailRejectsTooManyRecipients(t *testing.T) {
	server := startFakeSMTPServer(t)
	sender := newUnsafeEmailSenderForTest()
	target := testEmailTarget(server)
	target.SMTPTo = make([]string, maxRecipients+1)
	for index := range target.SMTPTo {
		target.SMTPTo[index] = "user@example.invalid"
	}

	err := sender.Send(context.Background(), target, testNotification())
	if err == nil {
		t.Fatal("超出收件人上限必须被拒绝")
	}
	if Retryable(err) {
		t.Fatalf("收件人超限属配置问题，应为确定性失败：%v", err)
	}
	if len(server.messages) != 0 {
		t.Fatal("超限时不得投递任何邮件")
	}
}

// 目标配置缺字段时明确拒绝，而不是尝试投递。
func TestEmailRejectsIncompleteTarget(t *testing.T) {
	server := startFakeSMTPServer(t)
	sender := newUnsafeEmailSenderForTest()

	cases := []struct {
		name   string
		modify func(*Target)
	}{
		{"缺主机", func(target *Target) { target.SMTPHost = "" }},
		{"缺发件人", func(target *Target) { target.SMTPFrom = "" }},
		{"缺收件人", func(target *Target) { target.SMTPTo = nil }},
		{"端口为零", func(target *Target) { target.SMTPPort = 0 }},
		{"端口越界", func(target *Target) { target.SMTPPort = 70000 }},
		{"安全选项非法", func(target *Target) { target.SMTPSecurity = "ssl" }},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			target := testEmailTarget(server)
			item.modify(&target)
			err := sender.Send(context.Background(), target, testNotification())
			if err == nil {
				t.Fatal("配置不完整必须被拒绝")
			}
			if Retryable(err) {
				t.Fatalf("配置问题应为确定性失败：%v", err)
			}
		})
	}
}

// 认证失败必须判为确定性失败：重试不会让错误的口令变对。
func TestEmailAuthFailureIsPermanent(t *testing.T) {
	server := startFakeSMTPServer(t)
	server.authOK = false
	sender := newUnsafeEmailSenderForTest()
	target := testEmailTarget(server)
	target.Secret = "wrong-password"

	err := sender.Send(context.Background(), target, testNotification())
	if err == nil {
		t.Fatal("认证失败必须报错")
	}
	if Retryable(err) {
		t.Fatalf("认证失败应为确定性失败：%v", err)
	}
}

// 收件人被拒必须判为确定性失败。
func TestEmailRejectedRecipientIsPermanent(t *testing.T) {
	server := startFakeSMTPServer(t)
	server.rejectRecipient = "missing@example.invalid"
	sender := newUnsafeEmailSenderForTest()
	target := testEmailTarget(server)
	target.SMTPTo = []string{"missing@example.invalid"}

	err := sender.Send(context.Background(), target, testNotification())
	if err == nil {
		t.Fatal("收件人被拒必须报错")
	}
	if Retryable(err) {
		t.Fatalf("收件人不存在应为确定性失败：%v", err)
	}
}

// 服务器不可达时按可重试归类。
func TestEmailUnreachableIsRetryable(t *testing.T) {
	server := startFakeSMTPServer(t)
	target := testEmailTarget(server)
	_ = server.listener.Close()

	sender := newUnsafeEmailSenderForTest()
	err := sender.Send(context.Background(), target, testNotification())
	if err == nil {
		t.Fatal("不可达服务器必须报错")
	}
	if !Retryable(err) {
		t.Fatalf("连接失败应判为可重试：%v", err)
	}
}

// 邮件正文不得包含 SMTP 密码。
//
// 服务器声明 AUTH 以通过"配置了密码就必须能认证"的校验：
// 该用例要验证的是密码不进正文，不是认证路径本身。
func TestEmailBodyOmitsPassword(t *testing.T) {
	server := startFakeSMTPServer(t)
	server.requireAuth = true
	sender := newUnsafeEmailSenderForTest()
	target := testEmailTarget(server)
	target.Secret = "smtp-password-value"

	if err := sender.Send(context.Background(), target, testNotification()); err != nil {
		t.Fatalf("投递失败：%v", err)
	}
	if strings.Contains(server.messages[0].body, "smtp-password-value") {
		t.Fatalf("邮件正文不得包含密码：%s", server.messages[0].body)
	}
}

// 头部注入必须被阻断：换行符不得进入邮件头部。
func TestEmailHeaderInjectionIsBlocked(t *testing.T) {
	server := startFakeSMTPServer(t)
	sender := newUnsafeEmailSenderForTest()
	target := testEmailTarget(server)
	target.SMTPTo = []string{"victim@example.invalid\r\nBcc: attacker@example.invalid"}

	// 注入尝试不应导致额外的收件人。
	err := sender.Send(context.Background(), target, testNotification())
	if err != nil {
		// 部分注入形态会被服务器拒绝，这也是可接受的结果。
		if len(server.messages) > 0 {
			t.Fatal("注入尝试不得产生已投递邮件")
		}
		return
	}
	for _, recipient := range server.messages[0].recipients {
		if strings.Contains(recipient, "attacker") {
			t.Fatalf("头部注入导致额外收件人：%v", server.messages[0].recipients)
		}
	}
}

// 上下文取消后应尽快返回。
//
// 服务器接受连接但从不回送问候语，使取消必定发生在等待服务器响应期间：
// 若用一个正常应答的服务器，取消与 SMTP 会话进度会竞争，取消可能迟到而
// 让用例走到"发件人被拒"等无关分支，断言随之失去意义。
func TestEmailHonorsContextCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动静默服务器失败：%v", err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			// 保持连接打开但不发送任何内容；由用例超时后的清理关闭监听器，
			// 连接随进程结束释放。此处不主动关闭，避免 Send 因连接断开
			// 而非超时返回，混淆断言目标。
			_ = connection
		}
	}()

	sender := newUnsafeEmailSenderForTest()
	target := Target{
		ID: "mail-1", Type: "email",
		SMTPHost: "127.0.0.1", SMTPPort: listener.Addr().(*net.TCPAddr).Port,
		SMTPFrom: "jrp@example.invalid", SMTPTo: []string{"ops@example.invalid"},
		SMTPSecurity: SMTPSecurityNone,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	sendErr := sender.Send(ctx, target, testNotification())
	elapsed := time.Since(start)

	if sendErr == nil {
		t.Fatal("上下文超时后应返回错误")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("超时后应尽快返回，实际耗时 %v", elapsed)
	}
	if !Retryable(sendErr) {
		t.Fatalf("超时应判为可重试：%v", sendErr)
	}
}

// 主题与正文超长时必须拒绝，避免无界内容进入投递。
func TestEmailRejectsOverlongContent(t *testing.T) {
	server := startFakeSMTPServer(t)
	sender := newUnsafeEmailSenderForTest()
	target := testEmailTarget(server)

	notification := testNotification()
	notification.Payload = strings.Repeat("超", maxBodyRunes+100)
	err := sender.Send(context.Background(), target, notification)
	if err == nil {
		t.Fatal("超长正文必须被拒绝")
	}
	if Retryable(err) {
		t.Fatalf("内容超限应为确定性失败：%v", err)
	}
}

// 明文 SMTP 加密码的组合必须被明确拒绝。
//
// 回归用例：Go 标准库的 PlainAuth 只在 TLS 或 localhost 上发送凭据，因此
// `none` + 非回环主机 + 密码的配置在运行期必然失败，而失败原因会被归为
// "认证失败"——那会把排查方向引向凭据本身，而真实原因是传输方式不允许发送凭据。
func TestEmailRejectsPlainTextWithCredentials(t *testing.T) {
	sender := newUnsafeEmailSenderForTest()

	t.Run("非回环主机被拒绝", func(t *testing.T) {
		target := Target{
			ID: "mail-1", Type: "email",
			SMTPHost: "smtp.example.com", SMTPPort: 25,
			SMTPFrom: "jrp@example.invalid", SMTPTo: []string{"ops@example.invalid"},
			SMTPSecurity: SMTPSecurityNone, Secret: "password-value",
		}
		err := sender.validate(target)
		if err == nil {
			t.Fatal("明文 SMTP 加密码的组合必须被拒绝")
		}
		if Retryable(err) {
			t.Fatalf("该组合是配置问题，应为确定性失败：%v", err)
		}
		// 错误应指明真实原因，而不是笼统的"认证失败"。
		if !strings.Contains(err.Error(), "starttls") {
			t.Fatalf("错误应给出可执行的修正方向：%v", err)
		}
	})

	t.Run("回环主机放行", func(t *testing.T) {
		target := Target{
			ID: "mail-1", Type: "email",
			SMTPHost: "127.0.0.1", SMTPPort: 25,
			SMTPFrom: "jrp@example.invalid", SMTPTo: []string{"ops@example.invalid"},
			SMTPSecurity: SMTPSecurityNone, Secret: "password-value",
		}
		// 回环是标准库允许明文发凭据的范围，配置校验应放行；
		// 投递本身会因无监听而失败，那属于运行期问题。
		if err := sender.validate(target); err != nil {
			t.Fatalf("回环主机加明文应通过配置校验：%v", err)
		}
	})
}
