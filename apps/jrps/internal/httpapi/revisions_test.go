package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jrp/apps/jrps/internal/store"
)

// newRevisionsTestRouter 构造带认证路由的引擎（无 apply 服务的默认形态）。
func newRevisionsTestRouter(database *store.Store) *gin.Engine {
	return NewRouter(RouterOptions{Store: database, InsecureCookies: true})
}

// revisionsTestEnv 是配置版本端点的测试环境：已登录管理员 + 已写入的代理版本。
type revisionsTestEnv struct {
	router        *gin.Engine
	database      *store.Store
	sessionCookie *http.Cookie
	csrfToken     string
}

// newRevisionsTestEnv 打开数据库、登录管理员并写入一个代理版本。
func newRevisionsTestEnv(t *testing.T) *revisionsTestEnv {
	t.Helper()
	database := openInitializedStore(t, "correct-horse-battery")
	router := newRevisionsTestRouter(database)

	login := doLogin(t, router, `{"username":"admin","password":"correct-horse-battery"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("登录失败：%d，响应 %s", login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("登录响应缺少会话 Cookie")
	}

	if err := database.Transaction(context.Background(), func(tx *store.Tx) error {
		_, err := tx.SaveProxy(store.Proxy{
			ID: "p1", ClientID: "c1", Name: "ssh", Type: "tcp",
			RemotePort: 26022, Target: "127.0.0.1:22",
		}, store.ActorAdmin("admin"), store.OriginProxyCreate)
		return err
	}); err != nil {
		t.Fatalf("写入测试代理失败：%v", err)
	}

	return &revisionsTestEnv{
		router:        router,
		database:      database,
		sessionCookie: cookies[0],
		csrfToken:     decodeCSRFToken(t, login.Body.Bytes()),
	}
}

// 未认证访问配置版本端点返回 401。
func TestConfigRevisionsRequireSession(t *testing.T) {
	env := newRevisionsTestEnv(t)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/config-revisions", nil)
	env.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("未认证访问应返回 401，实际为 %d", recorder.Code)
	}
}

// 列表返回三 revision 状态与版本摘要；版本摘要不含内容本体。
func TestConfigRevisionsListReturnsStateAndVersions(t *testing.T) {
	env := newRevisionsTestEnv(t)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/config-revisions", nil)
	request.AddCookie(env.sessionCookie)
	env.router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("列表状态码不匹配：%d，响应 %s", recorder.Code, recorder.Body.String())
	}
	var response revisionStateResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析列表响应失败：%v", err)
	}
	if response.DesiredRevision != 1 {
		t.Fatalf("desired revision 应为 1，实际 %d", response.DesiredRevision)
	}
	if len(response.Versions) != 1 || response.Versions[0].Revision != 1 {
		t.Fatalf("版本列表不符：%+v", response.Versions)
	}
	if len(response.Results) != 0 {
		t.Fatalf("尚无应用结果，实际 %+v", response.Results)
	}
}

// 按 revision 查询应用结果；非法 revision 返回 400。
func TestConfigRevisionsResultsByRevision(t *testing.T) {
	env := newRevisionsTestEnv(t)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/config-revisions?revision=not-a-number", nil)
	request.AddCookie(env.sessionCookie)
	env.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("非法 revision 应返回 400，实际为 %d", recorder.Code)
	}

	okRecorder := httptest.NewRecorder()
	okRequest := httptest.NewRequest(http.MethodGet, "/api/v1/config-revisions?revision=1", nil)
	okRequest.AddCookie(env.sessionCookie)
	env.router.ServeHTTP(okRecorder, okRequest)
	if okRecorder.Code != http.StatusOK {
		t.Fatalf("合法 revision 查询应返回 200，实际为 %d", okRecorder.Code)
	}
}

// restore 以历史内容创建新版本：201 返回新 revision，历史内容不变，写审计。
func TestConfigRevisionsRestoreCreatesNewRevision(t *testing.T) {
	env := newRevisionsTestEnv(t)

	// 再写入一个版本，使 restore 的源版本（1）过期。
	if err := env.database.Transaction(context.Background(), func(tx *store.Tx) error {
		_, err := tx.SaveProxy(store.Proxy{
			ID: "p2", ClientID: "c1", Name: "web", Type: "tcp",
			RemotePort: 26080, Target: "127.0.0.1:80",
		}, store.ActorAdmin("admin"), store.OriginProxyCreate)
		return err
	}); err != nil {
		t.Fatalf("写入第二个代理失败：%v", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/config-revisions/1:restore", nil)
	request.AddCookie(env.sessionCookie)
	request.Header.Set(csrfHeaderName, env.csrfToken)
	env.router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("restore 应返回 201，实际为 %d，响应 %s", recorder.Code, recorder.Body.String())
	}
	var response applyAcceptedResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析 restore 响应失败：%v", err)
	}
	if response.Revision != 3 {
		t.Fatalf("restore 应创建版本 3，实际 %d", response.Revision)
	}

	// 历史版本内容不变。
	var source store.ConfigRevision
	if err := env.database.View(context.Background(), func(tx *store.Tx) error {
		var err error
		source, err = tx.Revision(1)
		return err
	}); err != nil {
		t.Fatalf("读取源版本失败：%v", err)
	}
	var restored store.ConfigRevision
	if err := env.database.View(context.Background(), func(tx *store.Tx) error {
		var err error
		restored, err = tx.Revision(3)
		return err
	}); err != nil {
		t.Fatalf("读取恢复版本失败：%v", err)
	}
	if source.Content != restored.Content {
		t.Fatalf("恢复版本内容应与源版本一致")
	}

	// 审计留痕。
	var events []store.AuditEvent
	if err := env.database.View(context.Background(), func(tx *store.Tx) error {
		var err error
		events, err = tx.AuditEvents()
		return err
	}); err != nil {
		t.Fatalf("读取审计失败：%v", err)
	}
	found := false
	for _, event := range events {
		if event.Action == store.ActionRestoreApply {
			found = true
		}
	}
	if !found {
		t.Fatalf("restore 应写审计事件")
	}
}

// restore 缺 CSRF 返回 403；不存在的版本返回 404；未知子动作 404。
func TestConfigRevisionsRestoreGuards(t *testing.T) {
	env := newRevisionsTestEnv(t)

	noCSRF := httptest.NewRecorder()
	noCSRFRequest := httptest.NewRequest(http.MethodPost, "/api/v1/config-revisions/1:restore", nil)
	noCSRFRequest.AddCookie(env.sessionCookie)
	env.router.ServeHTTP(noCSRF, noCSRFRequest)
	if noCSRF.Code != http.StatusForbidden {
		t.Fatalf("缺 CSRF 应返回 403，实际为 %d", noCSRF.Code)
	}

	notFound := httptest.NewRecorder()
	notFoundRequest := httptest.NewRequest(http.MethodPost, "/api/v1/config-revisions/999:restore", nil)
	notFoundRequest.AddCookie(env.sessionCookie)
	notFoundRequest.Header.Set(csrfHeaderName, env.csrfToken)
	env.router.ServeHTTP(notFound, notFoundRequest)
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("不存在的版本应返回 404，实际为 %d", notFound.Code)
	}

	unknown := httptest.NewRecorder()
	unknownRequest := httptest.NewRequest(http.MethodPost, "/api/v1/config-revisions/1:explode", nil)
	unknownRequest.AddCookie(env.sessionCookie)
	unknownRequest.Header.Set(csrfHeaderName, env.csrfToken)
	env.router.ServeHTTP(unknown, unknownRequest)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("未知子动作应返回 404，实际为 %d", unknown.Code)
	}
}

// apply 无服务时返回 503（引擎未装配的可选依赖形态）。
func TestConfigRevisionsApplyWithoutService(t *testing.T) {
	env := newRevisionsTestEnv(t)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/config-revisions/1:apply", nil)
	request.AddCookie(env.sessionCookie)
	request.Header.Set(csrfHeaderName, env.csrfToken)
	env.router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("无 apply 服务应返回 503，实际为 %d，响应 %s", recorder.Code, recorder.Body.String())
	}
}

// 编译期使用检查：以下导入在测试断言中使用。
var (
	_ = strings.Contains
)
