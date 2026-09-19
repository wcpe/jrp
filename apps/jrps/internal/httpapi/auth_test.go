package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jrp/apps/jrps/internal/store"
)

// 测试辅助：打开一个已初始化的 jrps 数据库。
func openInitializedStore(t *testing.T, password string) *store.Store {
	t.Helper()
	database, err := store.Open(store.Config{
		Path:        t.TempDir() + "/jrps.db",
		BusyTimeout: time.Second,
		Logger:      slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	if err != nil {
		t.Fatalf("打开数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Transaction(context.Background(), func(tx *store.Tx) error {
		return tx.InitializeAdmin(store.InitializeAdminInput{Password: password})
	}); err != nil {
		t.Fatalf("初始化管理员失败：%v", err)
	}
	return database
}

// 测试辅助：打开一个未初始化的 jrps 数据库。
func openEmptyStore(t *testing.T) *store.Store {
	t.Helper()
	database, err := store.Open(store.Config{
		Path:        t.TempDir() + "/jrps.db",
		BusyTimeout: time.Second,
		Logger:      slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	if err != nil {
		t.Fatalf("打开数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// 测试辅助：构造挂载了认证路由的引擎。
//
// 测试使用明文 HTTP，因此显式开启 InsecureCookies 的本地回退，
// 与生产默认值（Secure）相区别（FR-02 规格 §6）。
func newTestRouter(database *store.Store) *gin.Engine {
	return NewRouter(RouterOptions{Store: database, InsecureCookies: true})
}

type discardWriter struct{}

func (discardWriter) Write([]byte) (int, error) { return 0, nil }

// sameProblemIgnoringRequestID 比较两个问题详情是否一致；requestId 每次请求都不同，需忽略。
//
// 忽略 requestId 是合理的：它是不透明关联标识，不属于"响应泄漏了凭据信息"的判定范围。
func sameProblemIgnoringRequestID(left, right []byte) bool {
	var first, second problem
	if err := json.Unmarshal(left, &first); err != nil {
		return false
	}
	if err := json.Unmarshal(right, &second); err != nil {
		return false
	}
	first.RequestID, second.RequestID = "", ""
	return first == second
}

// 测试辅助：从登录响应中取出 CSRF token。
func decodeCSRFToken(t *testing.T, body []byte) string {
	t.Helper()
	var response sessionResponse
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("解析会话响应失败：%v", err)
	}
	if response.CSRFToken == "" {
		t.Fatalf("会话响应缺少 CSRF token：%s", body)
	}
	return response.CSRFToken
}

// 未初始化时健康检查仍可用，其余 /api/v1 端点返回 503。
func TestUninitializedAPIRejectsWithServiceUnavailable(t *testing.T) {
	router := newTestRouter(openEmptyStore(t))

	for _, path := range []string{"/healthz", "/readyz"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s 在未初始化时应保持可用，实际为 %d", path, recorder.Code)
		}
	}

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/session"},
		{http.MethodGet, "/api/v1/session"},
		{http.MethodDelete, "/api/v1/session"},
		{http.MethodGet, "/api/v1/clients"},
		{http.MethodPost, "/api/v1/clients"},
	}
	for _, item := range cases {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(item.method, item.path, strings.NewReader("{}"))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s 在未初始化时应返回 503，实际为 %d", item.method, item.path, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "未初始化") {
			t.Fatalf("%s %s 应给出中文未初始化说明：%s", item.method, item.path, recorder.Body.String())
		}
	}
}

// 登录成功响应不得包含密码、派生材料、完整 Cookie 或服务端会话密钥。
func TestLoginResponseExposesNoCredentialMaterial(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/session",
		strings.NewReader(`{"username":"admin","password":"correct-horse-battery"}`))
	request.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("登录状态码不匹配：%d，响应 %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, forbidden := range []string{"correct-horse-battery", "password_digest", "password_salt", "token_digest"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("登录响应泄露凭据材料 %q：%s", forbidden, body)
		}
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("登录应只发放一个会话 Cookie，实际 %d 个", len(cookies))
	}
	if !cookies[0].HttpOnly {
		t.Fatal("会话 Cookie 必须携带 HttpOnly")
	}
	if cookies[0].SameSite == 0 {
		t.Fatal("会话 Cookie 必须显式设置 SameSite")
	}
	if !strings.Contains(body, "csrfToken") {
		t.Fatalf("登录响应必须返回 CSRF 协议所需状态：%s", body)
	}
}

// 登录失败不区分用户名与密码错误。
func TestLoginFailureIsUniform(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))

	wrongUser := doLogin(t, router, `{"username":"not-admin","password":"correct-horse-battery"}`)
	wrongPassword := doLogin(t, router, `{"username":"admin","password":"wrong-password"}`)

	if wrongUser.Code != http.StatusUnauthorized || wrongPassword.Code != http.StatusUnauthorized {
		t.Fatalf("登录失败应返回 401，实际为 %d 与 %d", wrongUser.Code, wrongPassword.Code)
	}
	if !sameProblemIgnoringRequestID(wrongUser.Body.Bytes(), wrongPassword.Body.Bytes()) {
		t.Fatalf("两种失败必须返回同一问题详情：%s 与 %s", wrongUser.Body.String(), wrongPassword.Body.String())
	}
	if strings.Contains(wrongUser.Body.String(), "不存在") {
		t.Fatalf("失败提示不得暴露用户名是否存在：%s", wrongUser.Body.String())
	}
	if len(wrongUser.Result().Cookies()) != 0 {
		t.Fatal("登录失败不得发放会话 Cookie")
	}
}

// 生产默认必须发放 Secure Cookie；只有显式开启本地回退才关闭它（FR-02 规格 §6）。
func TestSessionCookieSecureByDefault(t *testing.T) {
	database := openInitializedStore(t, "correct-horse-battery")
	router := NewRouter(RouterOptions{Store: database})
	login := doLogin(t, router, `{"username":"admin","password":"correct-horse-battery"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("登录失败：%d", login.Code)
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应只发放一个会话 Cookie，实际 %d 个", len(cookies))
	}
	if !cookies[0].Secure {
		t.Fatal("生产默认必须设置 Secure，不得因开发便利削弱默认值")
	}
}

// 生产默认必须发放 Secure Cookie（本地明文回退需显式开启）。
func TestSessionCookieInsecureOnlyWhenExplicitlyEnabled(t *testing.T) {
	database := openInitializedStore(t, "correct-horse-battery")
	router := NewRouter(RouterOptions{Store: database, InsecureCookies: true})
	login := doLogin(t, router, `{"username":"admin","password":"correct-horse-battery"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("登录失败：%d", login.Code)
	}
	cookies := login.Result().Cookies()
	if len(cookies) == 1 && cookies[0].Secure {
		t.Fatal("显式开启本地回退后应关闭 Secure，否则明文开发场景无法登录")
	}
}

// 测试辅助：执行一次登录并返回响应记录器。
func doLogin(t *testing.T, router *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/session", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder
}

// 会话查询只返回脱敏信息与 CSRF 状态。
func TestSessionQueryReturnsMaskedState(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))
	login := doLogin(t, router, `{"username":"admin","password":"correct-horse-battery"}`)
	sessionCookie := login.Result().Cookies()[0]

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	request.AddCookie(sessionCookie)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("会话查询状态码不匹配：%d，响应 %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, forbidden := range []string{"correct-horse-battery", "password_digest", "token_digest"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("会话查询泄露凭据材料 %q：%s", forbidden, body)
		}
	}
	if !strings.Contains(body, decodeCSRFToken(t, login.Body.Bytes())) {
		t.Fatalf("会话查询必须回显 CSRF 状态：%s", body)
	}
}

// 未携带会话访问管理端点返回 401。
func TestProtectedEndpointRequiresSession(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("未认证访问应返回 401，实际为 %d", recorder.Code)
	}
}

// 登出后原会话立即失效。
func TestLogoutInvalidatesSession(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))
	login := doLogin(t, router, `{"username":"admin","password":"correct-horse-battery"}`)
	sessionCookie := login.Result().Cookies()[0]
	csrfToken := decodeCSRFToken(t, login.Body.Bytes())

	logoutRecorder := httptest.NewRecorder()
	logoutRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/session", nil)
	logoutRequest.AddCookie(sessionCookie)
	logoutRequest.Header.Set(csrfHeaderName, csrfToken)
	router.ServeHTTP(logoutRecorder, logoutRequest)
	if logoutRecorder.Code != http.StatusOK {
		t.Fatalf("登出状态码不匹配：%d，响应 %s", logoutRecorder.Code, logoutRecorder.Body.String())
	}

	afterRecorder := httptest.NewRecorder()
	afterRequest := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	afterRequest.AddCookie(sessionCookie)
	router.ServeHTTP(afterRecorder, afterRequest)
	if afterRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("登出后原会话应返回 401，实际为 %d", afterRecorder.Code)
	}
}

// 修改类请求缺少或携带错误 CSRF token 时返回 403 且不产生副作用。
func TestCSRFRequiredForStateChangingRequests(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))
	login := doLogin(t, router, `{"username":"admin","password":"correct-horse-battery"}`)
	sessionCookie := login.Result().Cookies()[0]

	// 缺少 CSRF token：返回 403，会话仍然有效。
	missing := httptest.NewRecorder()
	missingRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/session", nil)
	missingRequest.AddCookie(sessionCookie)
	router.ServeHTTP(missing, missingRequest)
	if missing.Code != http.StatusForbidden {
		t.Fatalf("缺少 CSRF token 应返回 403，实际为 %d", missing.Code)
	}
	if !strings.Contains(missing.Body.String(), "csrf") && !strings.Contains(missing.Body.String(), "CSRF") {
		t.Fatalf("CSRF 失败应说明原因：%s", missing.Body.String())
	}
	if !sessionStillValid(t, router, sessionCookie) {
		t.Fatal("CSRF 校验失败不得产生副作用：会话被注销")
	}

	// 错误 CSRF token：同样返回 403 且无副作用。
	wrong := httptest.NewRecorder()
	wrongRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/session", nil)
	wrongRequest.AddCookie(sessionCookie)
	wrongRequest.Header.Set(csrfHeaderName, "invalid-csrf-token")
	router.ServeHTTP(wrong, wrongRequest)
	if wrong.Code != http.StatusForbidden {
		t.Fatalf("错误 CSRF token 应返回 403，实际为 %d", wrong.Code)
	}
	if !sessionStillValid(t, router, sessionCookie) {
		t.Fatal("CSRF 校验失败不得产生副作用：会话被注销")
	}
}

// 测试辅助：判断会话是否仍然有效。
func sessionStillValid(t *testing.T, router *gin.Engine, cookie *http.Cookie) bool {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	request.AddCookie(cookie)
	router.ServeHTTP(recorder, request)
	return recorder.Code == http.StatusOK
}

// 连续登录失败达到阈值后应被限流。
func TestLoginRateLimitedAfterThreshold(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))
	var lastCode int
	for attempt := 0; attempt < loginFailureLimit+2; attempt++ {
		recorder := doLogin(t, router, `{"username":"admin","password":"wrong-password"}`)
		lastCode = recorder.Code
		if attempt < loginFailureLimit {
			continue
		}
		if recorder.Code != http.StatusTooManyRequests {
			t.Fatalf("第 %d 次失败应被限流返回 429，实际为 %d", attempt+1, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "429") {
			t.Fatalf("限流响应应给出中文问题详情：%s", recorder.Body.String())
		}
	}
	if lastCode != http.StatusTooManyRequests {
		t.Fatalf("持续失败应保持在限流状态，实际为 %d", lastCode)
	}
}

// 限流不得影响已成功建立的会话。
func TestRateLimitDoesNotAffectActiveSession(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))
	login := doLogin(t, router, `{"username":"admin","password":"correct-horse-battery"}`)
	for attempt := 0; attempt < loginFailureLimit+1; attempt++ {
		doLogin(t, router, `{"username":"admin","password":"wrong-password"}`)
	}
	if !sessionStillValid(t, router, login.Result().Cookies()[0]) {
		t.Fatal("限流不得使已建立的会话失效")
	}
}

// 连续登录失败跨过限流阈值时产生一条通知。
//
// 回归用例：FR-15 的 outbox 此前没有任何生产写入点。登录失败是本阶段第一个
// 接入的业务动作，且按"只在阈值点发一次"接入——单次失败是噪声，达到阈值才
// 意味着有人在尝试爆破管理员凭据。
func TestLoginFailureLimitEnqueuesNotification(t *testing.T) {
	database := openInitializedStore(t, "correct-horse-battery")
	// 需要一个启用目标作为接收方，否则广播无人可发。
	if err := database.Transaction(context.Background(), func(tx *store.Tx) error {
		_, err := tx.CreateNotificationTarget(store.ActorAdmin("admin"), store.NotificationTargetInput{
			Name: "运维群", Type: store.NotificationTypeWebhook, Enabled: true,
			WebhookURL: "https://hooks.example.com/hook", Secret: "webhook-secret-value",
		})
		return err
	}); err != nil {
		t.Fatalf("创建通知目标失败：%v", err)
	}
	router := newTestRouter(database)

	countOutbox := func(t *testing.T, eventType string) int {
		t.Helper()
		var total int
		if err := database.View(context.Background(), func(tx *store.Tx) error {
			entries, err := tx.OutboxEntries()
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if entry.EventType == eventType {
					total++
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("读取 outbox 失败：%v", err)
		}
		return total
	}

	// 前 threshold-1 次失败不应产生通知。
	for attempt := 1; attempt < loginFailureLimit; attempt++ {
		doLogin(t, router, `{"username":"admin","password":"wrong-password"}`)
		if got := countOutbox(t, store.EventTypeLoginFailureLimit); got != 0 {
			t.Fatalf("第 %d 次失败不应产生通知，实际已产生 %d 条", attempt, got)
		}
	}

	// 第 threshold 次跨过阈值，产生且仅产生一条。
	doLogin(t, router, `{"username":"admin","password":"wrong-password"}`)
	if got := countOutbox(t, store.EventTypeLoginFailureLimit); got != 1 {
		t.Fatalf("达到阈值应产生 1 条通知，实际 %d 条", got)
	}
}

// 登录成功不产生登录失败通知，且会重置阈值。
func TestSuccessfulLoginResetsFailureNotification(t *testing.T) {
	database := openInitializedStore(t, "correct-horse-battery")
	if err := database.Transaction(context.Background(), func(tx *store.Tx) error {
		_, err := tx.CreateNotificationTarget(store.ActorAdmin("admin"), store.NotificationTargetInput{
			Name: "运维群", Type: store.NotificationTypeWebhook, Enabled: true,
			WebhookURL: "https://hooks.example.com/hook", Secret: "webhook-secret-value",
		})
		return err
	}); err != nil {
		t.Fatalf("创建通知目标失败：%v", err)
	}
	router := newTestRouter(database)

	// 制造两次失败（未达阈值），随后成功登录。
	doLogin(t, router, `{"username":"admin","password":"wrong-password"}`)
	doLogin(t, router, `{"username":"admin","password":"wrong-password"}`)
	recorder := doLogin(t, router, `{"username":"admin","password":"correct-horse-battery"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("正确凭据应登录成功，实际 %d：%s", recorder.Code, recorder.Body.String())
	}

	// 成功后阈值被重置，再失败两次仍不应触发通知。
	for attempt := 0; attempt < loginFailureLimit-1; attempt++ {
		doLogin(t, router, `{"username":"admin","password":"wrong-password"}`)
	}
	var total int
	if err := database.View(context.Background(), func(tx *store.Tx) error {
		entries, err := tx.OutboxEntries()
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.EventType == store.EventTypeLoginFailureLimit {
				total++
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("读取 outbox 失败：%v", err)
	}
	if total != 0 {
		t.Fatalf("登录成功后阈值应重置，不应产生通知，实际 %d 条", total)
	}
}
