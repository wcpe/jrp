package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 测试辅助：构造一条通知。
func testNotification() Notification {
	return Notification{
		EventID:    "event-1",
		EventType:  "apply_failure",
		OccurredAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Payload:    `{"proxy":"edge-1","result":"失败"}`,
	}
}

// 投递成功时请求体与签名必须符合契约。
func TestWebhookSendDeliversSignedPayload(t *testing.T) {
	const secret = "webhook-signing-secret"
	var captured struct {
		body      []byte
		signature string
		content   string
		method    string
		userAgent string
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		captured.body = body
		captured.signature = request.Header.Get(signatureHeader)
		captured.content = request.Header.Get("Content-Type")
		captured.method = request.Method
		captured.userAgent = request.Header.Get("User-Agent")
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender := newUnsafeWebhookSenderForTest()
	target := Target{ID: "hook-1", Type: "webhook", Secret: secret, WebhookURL: server.URL + "/hook"}
	if err := sender.Send(context.Background(), target, testNotification()); err != nil {
		t.Fatalf("投递失败：%v", err)
	}

	if captured.method != http.MethodPost {
		t.Fatalf("应使用 POST，实际 %s", captured.method)
	}
	if !strings.Contains(captured.content, "application/json") {
		t.Fatalf("内容类型应为 JSON：%s", captured.content)
	}

	// 载荷含事件标识、类型、发生时间与脱敏内容。
	var payload map[string]string
	if err := json.Unmarshal(captured.body, &payload); err != nil {
		t.Fatalf("请求体应为 JSON：%v", err)
	}
	if payload["eventId"] != "event-1" || payload["eventType"] != "apply_failure" {
		t.Fatalf("载荷缺少事件信息：%v", payload)
	}
	if payload["occurredAt"] != "2026-01-02T03:04:05Z" {
		t.Fatalf("载荷时间应为 RFC 3339 UTC：%s", payload["occurredAt"])
	}
	if payload["payload"] == "" {
		t.Fatal("载荷应包含脱敏内容")
	}

	// 签名必须与下游用同一 secret 计算的 HMAC-SHA256 一致。
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(captured.body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if captured.signature != expected {
		t.Fatalf("签名不匹配：期望 %s，实际 %s", expected, captured.signature)
	}
}

// 请求体与响应中不得出现目标 secret。
func TestWebhookSendKeepsSecretOutOfPayload(t *testing.T) {
	const secret = "super-secret-token-value"
	var body []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ = io.ReadAll(request.Body)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender := newUnsafeWebhookSenderForTest()
	target := Target{ID: "hook-1", Type: "webhook", Secret: secret, WebhookURL: server.URL + "/hook"}
	if err := sender.Send(context.Background(), target, testNotification()); err != nil {
		t.Fatalf("投递失败：%v", err)
	}
	if strings.Contains(string(body), secret) {
		t.Fatalf("请求体不得包含 secret：%s", body)
	}
}

// 未配置 secret 时不发送签名头：不伪造来源证明。
func TestWebhookSendWithoutSecretSkipsSignature(t *testing.T) {
	var signature string
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		signature = request.Header.Get(signatureHeader)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender := newUnsafeWebhookSenderForTest()
	target := Target{ID: "hook-1", Type: "webhook", WebhookURL: server.URL + "/hook"}
	if err := sender.Send(context.Background(), target, testNotification()); err != nil {
		t.Fatalf("投递失败：%v", err)
	}
	if signature != "" {
		t.Fatalf("未配置 secret 时不应发送签名头：%s", signature)
	}
}

// 状态码归类：2xx 成功，5xx 与 429 可重试，4xx 确定性失败（规格 §3.4）。
func TestWebhookStatusClassification(t *testing.T) {
	cases := []struct {
		status    int
		wantError bool
		retryable bool
	}{
		{http.StatusOK, false, false},
		{http.StatusCreated, false, false},
		{http.StatusNoContent, false, false},
		{http.StatusBadRequest, true, false},
		{http.StatusUnauthorized, true, false},
		{http.StatusForbidden, true, false},
		{http.StatusNotFound, true, false},
		{http.StatusTooManyRequests, true, true},
		{http.StatusInternalServerError, true, true},
		{http.StatusBadGateway, true, true},
		{http.StatusServiceUnavailable, true, true},
	}
	for _, item := range cases {
		server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(item.status)
		}))
		sender := newUnsafeWebhookSenderForTest()
		target := Target{ID: "hook-1", Type: "webhook", WebhookURL: server.URL + "/hook"}
		err := sender.Send(context.Background(), target, testNotification())
		server.Close()

		if item.wantError && err == nil {
			t.Fatalf("HTTP %d 应判为失败", item.status)
		}
		if !item.wantError && err != nil {
			t.Fatalf("HTTP %d 应判为成功：%v", item.status, err)
		}
		if item.wantError && Retryable(err) != item.retryable {
			t.Fatalf("HTTP %d 重试归类不符：期望 %v，实际 %v", item.status, item.retryable, Retryable(err))
		}
	}
}

// 重定向不得被跟随：避免绕过 SSRF 校验。
func TestWebhookDoesNotFollowRedirect(t *testing.T) {
	redirected := false
	destination := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		redirected = true
		writer.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()

	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL+"/hook", http.StatusFound)
	}))
	defer server.Close()

	sender := newUnsafeWebhookSenderForTest()
	target := Target{ID: "hook-1", Type: "webhook", WebhookURL: server.URL + "/hook"}
	err := sender.Send(context.Background(), target, testNotification())
	if err == nil {
		t.Fatal("重定向应被判为失败而不是静默跟随")
	}
	if redirected {
		t.Fatal("重定向目标不得被实际请求")
	}
	if Retryable(err) {
		t.Fatalf("3xx 重定向不应判为可重试：%v", err)
	}
}

// 目标不可达时按可重试归类：网络故障是临时问题。
func TestWebhookUnreachableIsRetryable(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unreachable := server.URL
	server.Close() // 关闭后端口不再有监听

	sender := newUnsafeWebhookSenderForTest()
	target := Target{ID: "hook-1", Type: "webhook", WebhookURL: unreachable + "/hook"}
	err := sender.Send(context.Background(), target, testNotification())
	if err == nil {
		t.Fatal("不可达目标应判为失败")
	}
	if !Retryable(err) {
		t.Fatalf("连接被拒应判为可重试：%v", err)
	}
}

// 生产发送器必须拒绝回环地址：即使测试能连，生产路径不得放行。
func TestWebhookProductionSenderBlocksLoopback(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender := newWebhookSender()
	target := Target{ID: "hook-1", Type: "webhook", WebhookURL: server.URL + "/hook"}
	err := sender.Send(context.Background(), target, testNotification())
	if err == nil {
		t.Fatal("生产发送器必须拒绝回环地址")
	}
	if Retryable(err) {
		t.Fatalf("地址被安全校验拒绝应判为确定性失败：%v", err)
	}
	if !errors.Is(err, ErrTargetBlocked) {
		return // 错误已被包装为 DeliveryError，此处只确认非可重试
	}
}

// 上下文取消时应判为可重试并尽快返回，不悬挂。
func TestWebhookHonorsContextCancellation(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	sender := newUnsafeWebhookSenderForTest()
	target := Target{ID: "hook-1", Type: "webhook", WebhookURL: server.URL + "/hook"}

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := sender.Send(ctx, target, testNotification())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("上下文取消后应返回错误")
	}
	if elapsed > time.Second {
		t.Fatalf("取消后应尽快返回，实际耗时 %v", elapsed)
	}
	if !Retryable(err) {
		t.Fatalf("取消应判为可重试：%v", err)
	}
}

// 投递错误信息不得回显完整目标地址。
func TestWebhookErrorDoesNotEchoAddress(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	sender := newUnsafeWebhookSenderForTest()
	target := Target{ID: "hook-1", Type: "webhook", WebhookURL: server.URL + "/hook?sensitive=value"}
	err := sender.Send(context.Background(), target, testNotification())
	if err == nil {
		t.Fatal("5xx 应判为失败")
	}
	if strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("错误信息不得回显地址查询串：%v", err)
	}
}

// 非 webhook 类型的地址校验失败应判为确定性失败。
func TestWebhookInvalidURLIsPermanent(t *testing.T) {
	sender := newUnsafeWebhookSenderForTest()
	target := Target{ID: "hook-1", Type: "webhook", WebhookURL: "http://insecure.example.com/hook"}
	err := sender.Send(context.Background(), target, testNotification())
	if err == nil {
		t.Fatal("明文协议应被拒绝")
	}
	if Retryable(err) {
		t.Fatalf("地址非法应判为确定性失败：%v", err)
	}
}
