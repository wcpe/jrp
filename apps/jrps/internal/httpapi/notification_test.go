package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jrp/apps/jrps/internal/store"
)

// stubTestNotifier 记录测试通知调用，供断言端点的鉴权与审计路径。
type stubTestNotifier struct {
	calls   []store.NotificationTargetView
	sendErr error
}

func (notifier *stubTestNotifier) SendTest(_ context.Context, target store.NotificationTargetView) error {
	notifier.calls = append(notifier.calls, target)
	return notifier.sendErr
}

// 测试辅助：构造带通知端点的路由。
func newNotificationRouter(t *testing.T, notifier TestNotificationSender) (*gin.Engine, *store.Store) {
	t.Helper()
	database := openInitializedStore(t, "correct-horse-battery")
	return NewRouter(RouterOptions{
		Store: database, InsecureCookies: true, TestNotifications: notifier,
	}), database
}

// 测试辅助：以已登录会话执行带 JSON 体的请求。
func doNotificationRequest(
	t *testing.T, router *gin.Engine, method, path, body string, withCSRF bool,
) *httptest.ResponseRecorder {
	t.Helper()
	login := doLogin(t, router, `{"username":"admin","password":"correct-horse-battery"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("登录失败：%d", login.Code)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.AddCookie(login.Result().Cookies()[0])
	request.Header.Set("Content-Type", "application/json")
	if withCSRF {
		request.Header.Set(csrfHeaderName, decodeCSRFToken(t, login.Body.Bytes()))
	}
	router.ServeHTTP(recorder, request)
	return recorder
}

// 测试辅助：构造合法的 Webhook 目标请求体。
func webhookTargetBody(name, url, secret string) string {
	payload := map[string]any{
		"name": name, "type": "webhook", "webhookUrl": url,
	}
	if secret != "" {
		payload["secret"] = secret
	}
	encoded, _ := json.Marshal(payload)
	return string(encoded)
}

// 未认证访问通知端点一律 401。
func TestNotificationTargetsRequireSession(t *testing.T) {
	router, _ := newNotificationRouter(t, nil)
	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/notification-targets"},
		{http.MethodPost, "/api/v1/notification-targets"},
		{http.MethodPatch, "/api/v1/notification-targets/nt_1"},
		{http.MethodDelete, "/api/v1/notification-targets/nt_1"},
		{http.MethodPost, "/api/v1/notification-targets/nt_1:test"},
	}
	for _, item := range cases {
		t.Run(item.method+" "+item.path, func(t *testing.T) {
			recorder := doAuthenticatedWithoutSession(t, router, item.method, item.path, "{}")
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("未认证应返回 401，实际 %d", recorder.Code)
			}
		})
	}
}

// 修改类请求缺少 CSRF 返回 403 且无副作用。
func TestNotificationTargetsRequireCSRF(t *testing.T) {
	router, database := newNotificationRouter(t, &stubTestNotifier{})
	body := webhookTargetBody("运维群", "https://hooks.example.com/hook", "s3cret-value")

	recorder := doNotificationRequest(t, router, http.MethodPost, "/api/v1/notification-targets", body, false)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("缺少 CSRF 应返回 403，实际 %d：%s", recorder.Code, recorder.Body.String())
	}

	var count int64
	if err := database.View(context.Background(), func(tx *store.Tx) error {
		return tx.DB().Model(&store.NotificationTarget{}).Count(&count).Error
	}); err != nil {
		t.Fatalf("统计目标失败：%v", err)
	}
	if count != 0 {
		t.Fatal("CSRF 失败不得创建目标")
	}
}

// 创建目标成功并写入审计，响应中的秘密必须掩码。
func TestNotificationTargetCreateMasksSecret(t *testing.T) {
	router, database := newNotificationRouter(t, &stubTestNotifier{})
	const secret = "webhook-secret-abcdef123456"
	body := webhookTargetBody("运维群", "https://hooks.example.com/hook", secret)

	recorder := doNotificationRequest(t, router, http.MethodPost, "/api/v1/notification-targets", body, true)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("创建应返回 201，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), secret) {
		t.Fatalf("响应泄露了完整秘密：%s", recorder.Body.String())
	}

	var response notificationTargetResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if !strings.HasPrefix(response.MaskedSecret, "*") {
		t.Fatalf("秘密应掩码展示：%s", response.MaskedSecret)
	}
	if !strings.HasSuffix(response.MaskedSecret, secret[len(secret)-4:]) {
		t.Fatalf("掩码应保留末四位：%s", response.MaskedSecret)
	}
	if response.ID == "" {
		t.Fatal("目标标识应由服务端生成")
	}

	// 创建写入审计，且审计内容不含秘密。
	var events []store.AuditEvent
	if err := database.View(context.Background(), func(tx *store.Tx) error {
		var err error
		events, err = tx.AuditEvents()
		return err
	}); err != nil {
		t.Fatalf("读取审计失败：%v", err)
	}
	found := false
	for _, event := range events {
		if event.Action == store.ActionNotificationTargetCreate {
			found = true
			if strings.Contains(event.Context, secret) {
				t.Fatalf("审计泄露了秘密：%s", event.Context)
			}
		}
	}
	if !found {
		t.Fatal("创建目标必须写入审计")
	}
}

// 非法输入返回 400 问题详情，且一次给出全部违规项。
func TestNotificationTargetRejectsInvalidInput(t *testing.T) {
	router, _ := newNotificationRouter(t, &stubTestNotifier{})
	cases := []struct {
		name string
		body string
	}{
		{"缺名称", `{"type":"webhook","webhookUrl":"https://hooks.example.com/hook"}`},
		{"缺地址", `{"name":"目标","type":"webhook"}`},
		{"明文协议", `{"name":"目标","type":"webhook","webhookUrl":"http://hooks.example.com/hook"}`},
		{"地址内嵌凭据", `{"name":"目标","type":"webhook","webhookUrl":"https://u:p@hooks.example.com/hook"}`},
		{"未知渠道", `{"name":"目标","type":"sms"}`},
		{"邮件缺主机", `{"name":"目标","type":"email","smtpFrom":"a@b.c","smtpTo":["x@y.z"]}`},
		{"邮件缺收件人", `{"name":"目标","type":"email","smtpHost":"smtp.example.com","smtpFrom":"a@b.c"}`},
		{"邮件安全选项非法", `{"name":"目标","type":"email","smtpHost":"smtp.example.com","smtpFrom":"a@b.c","smtpTo":["x@y.z"],"smtpSecurity":"ssl"}`},
		{"非 JSON", `不是 JSON`},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			recorder := doNotificationRequest(t, router, http.MethodPost, "/api/v1/notification-targets", item.body, true)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("非法输入应返回 400，实际 %d：%s", recorder.Code, recorder.Body.String())
			}
			var problem problem
			if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
				t.Fatalf("响应应为问题详情：%v", err)
			}
			if problem.Code != codeInvalidInput {
				t.Fatalf("问题码应为 %s，实际 %s", codeInvalidInput, problem.Code)
			}
		})
	}
}

// 列表返回全部目标的脱敏视图。
func TestNotificationTargetListMasksSecrets(t *testing.T) {
	router, _ := newNotificationRouter(t, &stubTestNotifier{})
	const secret = "webhook-secret-abcdef123456"
	doNotificationRequest(t, router, http.MethodPost, "/api/v1/notification-targets",
		webhookTargetBody("运维群", "https://hooks.example.com/hook", secret), true)

	recorder := doNotificationRequest(t, router, http.MethodGet, "/api/v1/notification-targets", "", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("列表应返回 200，实际 %d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), secret) {
		t.Fatalf("列表泄露了完整秘密：%s", recorder.Body.String())
	}
	var response notificationTargetsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析列表失败：%v", err)
	}
	if len(response.Items) != 1 {
		t.Fatalf("应返回 1 个目标，实际 %d 个", len(response.Items))
	}
}

// 更新目标时秘密留空表示保留原值。
func TestNotificationTargetUpdateKeepsSecretWhenOmitted(t *testing.T) {
	router, database := newNotificationRouter(t, &stubTestNotifier{})
	const secret = "webhook-secret-abcdef123456"
	created := doNotificationRequest(t, router, http.MethodPost, "/api/v1/notification-targets",
		webhookTargetBody("运维群", "https://hooks.example.com/hook", secret), true)
	var target notificationTargetResponse
	_ = json.Unmarshal(created.Body.Bytes(), &target)

	// 只改名字，不带秘密。
	updated := doNotificationRequest(t, router, http.MethodPatch, "/api/v1/notification-targets/"+target.ID,
		webhookTargetBody("运维群改名", "https://hooks.example.com/hook", ""), true)
	if updated.Code != http.StatusOK {
		t.Fatalf("更新应返回 200，实际 %d：%s", updated.Code, updated.Body.String())
	}

	var stored store.NotificationTarget
	if err := database.View(context.Background(), func(tx *store.Tx) error {
		return tx.DB().Where("id = ?", target.ID).First(&stored).Error
	}); err != nil {
		t.Fatalf("读取目标失败：%v", err)
	}
	if stored.Secret != secret {
		t.Fatal("未提供秘密时不得清空原有秘密")
	}
	if stored.Name != "运维群改名" {
		t.Fatalf("名称应已更新：%s", stored.Name)
	}
}

// 更新目标写入审计。
func TestNotificationTargetUpdateWritesAudit(t *testing.T) {
	router, database := newNotificationRouter(t, &stubTestNotifier{})
	created := doNotificationRequest(t, router, http.MethodPost, "/api/v1/notification-targets",
		webhookTargetBody("运维群", "https://hooks.example.com/hook", "secret-value"), true)
	var target notificationTargetResponse
	_ = json.Unmarshal(created.Body.Bytes(), &target)

	doNotificationRequest(t, router, http.MethodPatch, "/api/v1/notification-targets/"+target.ID,
		webhookTargetBody("新名字", "https://hooks.example.com/hook", ""), true)

	assertAuditAction(t, database, store.ActionNotificationTargetUpdate)
}

// 删除目标返回 204，把在途记录转入 discarded，并写入审计。
func TestNotificationTargetDeleteDiscardsPending(t *testing.T) {
	router, database := newNotificationRouter(t, &stubTestNotifier{})
	created := doNotificationRequest(t, router, http.MethodPost, "/api/v1/notification-targets",
		webhookTargetBody("运维群", "https://hooks.example.com/hook", "secret-value"), true)
	var target notificationTargetResponse
	_ = json.Unmarshal(created.Body.Bytes(), &target)

	// 制造一条该目标的待发送记录。
	if err := database.Transaction(context.Background(), func(tx *store.Tx) error {
		_, err := tx.OutboxEnqueue(store.NotificationOutbox{
			EventID: "evt-1", TargetID: target.ID, EventType: "apply_failure", Payload: "{}",
		})
		return err
	}); err != nil {
		t.Fatalf("写入待发送记录失败：%v", err)
	}

	recorder := doNotificationRequest(t, router, http.MethodDelete,
		"/api/v1/notification-targets/"+target.ID, "", true)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("删除应返回 204，实际 %d：%s", recorder.Code, recorder.Body.String())
	}

	var entries []store.NotificationOutbox
	if err := database.View(context.Background(), func(tx *store.Tx) error {
		var err error
		entries, err = tx.OutboxEntries()
		return err
	}); err != nil {
		t.Fatalf("读取 outbox 失败：%v", err)
	}
	if len(entries) != 1 || entries[0].Status != store.OutboxStatusDiscarded {
		t.Fatalf("删除目标后在途记录应转入 discarded：%+v", entries)
	}
	assertAuditAction(t, database, store.ActionNotificationTargetDelete)
}

// 操作不存在的目标返回 404。
func TestNotificationTargetMissingReturnsNotFound(t *testing.T) {
	router, _ := newNotificationRouter(t, &stubTestNotifier{})
	cases := []struct{ method, path, body string }{
		{http.MethodPatch, "/api/v1/notification-targets/nt_missing", webhookTargetBody("x", "https://hooks.example.com/hook", "")},
		{http.MethodDelete, "/api/v1/notification-targets/nt_missing", ""},
		{http.MethodPost, "/api/v1/notification-targets/nt_missing:test", ""},
	}
	for _, item := range cases {
		t.Run(item.method+" "+item.path, func(t *testing.T) {
			recorder := doNotificationRequest(t, router, item.method, item.path, item.body, true)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("不存在的目标应返回 404，实际 %d：%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

// 测试通知端点接受契约中的 `{targetId}:test` 形式并写入审计。
func TestNotificationTargetTestSendsAndAudits(t *testing.T) {
	notifier := &stubTestNotifier{}
	router, database := newNotificationRouter(t, notifier)
	created := doNotificationRequest(t, router, http.MethodPost, "/api/v1/notification-targets",
		webhookTargetBody("运维群", "https://hooks.example.com/hook", "secret-value"), true)
	var target notificationTargetResponse
	_ = json.Unmarshal(created.Body.Bytes(), &target)

	recorder := doNotificationRequest(t, router, http.MethodPost,
		"/api/v1/notification-targets/"+target.ID+":test", "", true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("测试通知应返回 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("应触发一次测试投递，实际 %d 次", len(notifier.calls))
	}
	if notifier.calls[0].ID != target.ID {
		t.Fatalf("测试通知应发给指定目标：%s", notifier.calls[0].ID)
	}
	assertAuditAction(t, database, store.ActionNotificationTargetTest)
}

// 测试通知发送失败时返回失败状态，但仍写入审计。
func TestNotificationTargetTestFailureIsAudited(t *testing.T) {
	notifier := &stubTestNotifier{sendErr: errors.New("下游不可达")}
	router, database := newNotificationRouter(t, notifier)
	created := doNotificationRequest(t, router, http.MethodPost, "/api/v1/notification-targets",
		webhookTargetBody("运维群", "https://hooks.example.com/hook", "secret-value"), true)
	var target notificationTargetResponse
	_ = json.Unmarshal(created.Body.Bytes(), &target)

	recorder := doNotificationRequest(t, router, http.MethodPost,
		"/api/v1/notification-targets/"+target.ID+":test", "", true)
	if recorder.Code == http.StatusOK {
		t.Fatal("投递失败时不应返回成功")
	}
	// 失败原因不得转发底层错误文本。
	if strings.Contains(recorder.Body.String(), "下游不可达") {
		t.Fatalf("响应不应回显底层错误：%s", recorder.Body.String())
	}

	var events []store.AuditEvent
	if err := database.View(context.Background(), func(tx *store.Tx) error {
		var err error
		events, err = tx.AuditEvents()
		return err
	}); err != nil {
		t.Fatalf("读取审计失败：%v", err)
	}
	found := false
	for _, event := range events {
		if event.Action == store.ActionNotificationTargetTest {
			found = true
			if event.Result != store.AuditResultFailure {
				t.Fatalf("失败的测试通知应记录为失败结果：%+v", event)
			}
		}
	}
	if !found {
		t.Fatal("失败的测试通知同样必须写入审计")
	}
}

// 非 `:test` 后缀的路径按资源不存在处理。
func TestNotificationTargetTestRejectsWrongSuffix(t *testing.T) {
	router, _ := newNotificationRouter(t, &stubTestNotifier{})
	for _, path := range []string{
		"/api/v1/notification-targets/nt_1:unknown",
		"/api/v1/notification-targets/nt_1",
		"/api/v1/notification-targets/:test",
	} {
		t.Run(path, func(t *testing.T) {
			recorder := doNotificationRequest(t, router, http.MethodPost, path, "", true)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("非测试后缀应返回 404，实际 %d", recorder.Code)
			}
		})
	}
}

// 未启用测试通知渠道时返回 503。
func TestNotificationTargetTestUnavailable(t *testing.T) {
	router, _ := newNotificationRouter(t, nil)
	created := doNotificationRequest(t, router, http.MethodPost, "/api/v1/notification-targets",
		webhookTargetBody("运维群", "https://hooks.example.com/hook", "secret-value"), true)
	var target notificationTargetResponse
	_ = json.Unmarshal(created.Body.Bytes(), &target)

	recorder := doNotificationRequest(t, router, http.MethodPost,
		"/api/v1/notification-targets/"+target.ID+":test", "", true)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("渠道未启用应返回 503，实际 %d", recorder.Code)
	}
}

// 对已停用目标的测试通知必须被拒绝，且不留副作用、仍写入审计。
//
// 回归用例：测试通知曾不检查启用状态，可对已停用目标投递成功，使管理员
// 误以为该目标工作正常——而业务通知实际不会发往它。
func TestNotificationTargetTestRejectsDisabledTarget(t *testing.T) {
	notifier := &stubTestNotifier{}
	router, database := newNotificationRouter(t, notifier)

	// 创建时即停用：这一步同时覆盖 F-01 的修复（显式 false 必须如实落库）。
	created := doNotificationRequest(t, router, http.MethodPost, "/api/v1/notification-targets",
		`{"name":"停用目标","type":"webhook","webhookUrl":"https://hooks.example.com/hook","enabled":false,"secret":"secret-value"}`, true)
	if created.Code != http.StatusCreated {
		t.Fatalf("创建失败：%d %s", created.Code, created.Body.String())
	}
	var target notificationTargetResponse
	if err := json.Unmarshal(created.Body.Bytes(), &target); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if target.Enabled {
		t.Fatal("创建时显式停用的目标不得呈现为启用状态")
	}

	recorder := doNotificationRequest(t, router, http.MethodPost,
		"/api/v1/notification-targets/"+target.ID+":test", "", true)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("停用目标应返回 409，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	var problem problem
	if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
		t.Fatalf("响应应为问题详情：%v", err)
	}
	if problem.Code != codeConflict {
		t.Fatalf("问题码应为 %s，实际 %s", codeConflict, problem.Code)
	}
	if !containsChinese(problem.Detail) {
		t.Fatalf("问题详情应为中文说明：%q", problem.Detail)
	}
	// 拒绝必须发生在投递之前。
	if len(notifier.calls) != 0 {
		t.Fatalf("停用目标不得触发投递，实际投递 %d 次", len(notifier.calls))
	}

	// 被拒绝的操作同样留痕：规格 §2.1 要求失败与被拒绝都必须记录。
	var events []store.AuditEvent
	if err := database.View(context.Background(), func(tx *store.Tx) error {
		var err error
		events, err = tx.AuditEvents()
		return err
	}); err != nil {
		t.Fatalf("读取审计失败：%v", err)
	}
	found := false
	for _, event := range events {
		if event.Action == store.ActionNotificationTargetTest {
			found = true
			if event.Result != store.AuditResultDenied {
				t.Fatalf("被拒绝的测试通知应记录为 denied：%+v", event)
			}
		}
	}
	if !found {
		t.Fatal("被拒绝的测试通知必须写入审计")
	}
}

// 列表与详情不得输出完整秘密，即使是掩码字段。
func TestNotificationTargetsNeverExposeRawSecret(t *testing.T) {
	router, _ := newNotificationRouter(t, &stubTestNotifier{})
	const secret = "webhook-secret-abcdef123456"
	doNotificationRequest(t, router, http.MethodPost, "/api/v1/notification-targets",
		webhookTargetBody("运维群", "https://hooks.example.com/hook", secret), true)

	recorder := doNotificationRequest(t, router, http.MethodGet, "/api/v1/notification-targets", "", false)
	body := recorder.Body.String()
	// 完整秘密与"secret"明文字段都不应出现；掩码字段名是 maskedSecret。
	if strings.Contains(body, secret) {
		t.Fatalf("列表泄露完整秘密：%s", body)
	}
	if strings.Contains(body, `"secret"`) {
		t.Fatalf("响应不应包含明文字段 secret：%s", body)
	}
}

// 测试辅助：断言存在指定动作的审计事件。
func assertAuditAction(t *testing.T, database *store.Store, action string) {
	t.Helper()
	var events []store.AuditEvent
	if err := database.View(context.Background(), func(tx *store.Tx) error {
		var err error
		events, err = tx.AuditEvents()
		return err
	}); err != nil {
		t.Fatalf("读取审计失败：%v", err)
	}
	for _, event := range events {
		if event.Action == action {
			return
		}
	}
	t.Fatalf("未找到动作 %s 的审计事件", action)
}
