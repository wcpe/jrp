package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jrp/apps/jrps/internal/store"
)

// 测试辅助：以已登录会话发出客户端管理请求。
func doClientRequest(
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

// 未认证访问客户端端点一律 401。
func TestClientsRequireSession(t *testing.T) {
	router, _ := newNotificationRouter(t, &stubTestNotifier{})

	for _, path := range []string{"/api/v1/clients", "/api/v1/clients/cli_x"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s 未认证应返回 401，实际 %d", path, recorder.Code)
		}
	}
}

// 生命周期动作缺少 CSRF 一律 403。
func TestClientTokenActionsRequireCSRF(t *testing.T) {
	router, _ := newNotificationRouter(t, &stubTestNotifier{})

	for _, path := range []string{
		"/api/v1/clients",
		"/api/v1/clients/cli_x/tokens:rotate",
		"/api/v1/clients/cli_x/tokens:revoke",
		"/api/v1/clients/cli_x/enrollment-credentials",
	} {
		recorder := doClientRequest(t, router, http.MethodPost, path, `{"name":"甲"}`, false)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("%s 缺 CSRF 应返回 403，实际 %d", path, recorder.Code)
		}
	}
}

// 创建客户端返回一次明文 token 与一次性凭据；列表与详情只返回掩码。
func TestClientCreateReturnsSecretOnceThenMasks(t *testing.T) {
	router, _ := newNotificationRouter(t, &stubTestNotifier{})

	created := doClientRequest(t, router, http.MethodPost, "/api/v1/clients", `{"name":"客户端甲"}`, true)
	if created.Code != http.StatusCreated {
		t.Fatalf("创建应返回 201，实际 %d：%s", created.Code, created.Body.String())
	}
	var createResponse struct {
		ID                   string `json:"id"`
		Name                 string `json:"name"`
		MaskedToken          string `json:"maskedToken"`
		Token                string `json:"token"`
		EnrollmentCredential string `json:"enrollmentCredential"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createResponse); err != nil {
		t.Fatalf("解析创建响应失败：%v", err)
	}
	if createResponse.Token == "" || createResponse.EnrollmentCredential == "" {
		t.Fatalf("创建响应应含一次性 token 与凭据：%s", created.Body.String())
	}
	if !strings.HasPrefix(createResponse.MaskedToken, "****") {
		t.Fatalf("掩码应以前缀形式出现，实际 %q", createResponse.MaskedToken)
	}

	// 列表与详情不得出现完整 token 或凭据明文。
	list := doClientRequest(t, router, http.MethodGet, "/api/v1/clients", "", false)
	show := doClientRequest(t, router, http.MethodGet, "/api/v1/clients/"+createResponse.ID, "", false)
	for _, recorder := range []*httptest.ResponseRecorder{list, show} {
		body := recorder.Body.String()
		if strings.Contains(body, createResponse.Token) {
			t.Fatalf("读取接口不得返回完整 token：%s", body)
		}
		if strings.Contains(body, createResponse.EnrollmentCredential) {
			t.Fatalf("读取接口不得返回凭据明文：%s", body)
		}
		if !strings.Contains(body, "****") {
			t.Fatalf("读取接口应返回掩码：%s", body)
		}
	}
}

// 轮换后旧 token 立即失效、新 token 生效；吊销后一律失效。
func TestClientTokenRotateAndRevoke(t *testing.T) {
	router, database := newNotificationRouter(t, &stubTestNotifier{})
	clientID, oldToken := createClient(t, router, "客户端甲")

	rotated := doClientRequest(t, router, http.MethodPost,
		"/api/v1/clients/"+clientID+"/tokens:rotate", "", true)
	if rotated.Code != http.StatusOK {
		t.Fatalf("轮换应返回 200，实际 %d：%s", rotated.Code, rotated.Body.String())
	}
	var rotateResponse struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rotated.Body.Bytes(), &rotateResponse)
	if rotateResponse.Token == "" || rotateResponse.Token == oldToken {
		t.Fatalf("轮换必须产出新 token，实际 %q", rotateResponse.Token)
	}
	assertTokenRejected(t, database, oldToken)
	assertTokenAccepted(t, database, rotateResponse.Token)

	revoked := doClientRequest(t, router, http.MethodPost,
		"/api/v1/clients/"+clientID+"/tokens:revoke", "", true)
	if revoked.Code != http.StatusOK {
		t.Fatalf("吊销应返回 200，实际 %d：%s", revoked.Code, revoked.Body.String())
	}
	assertTokenRejected(t, database, rotateResponse.Token)
}

// 未知动作返回 404，且不影响已注册动作。
func TestClientTokenActionRejectsUnknownSuffix(t *testing.T) {
	router, _ := newNotificationRouter(t, &stubTestNotifier{})

	recorder := doClientRequest(t, router, http.MethodPost,
		"/api/v1/clients/cli_x/tokens:nonsense", "", true)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("未知动作应返回 404，实际 %d", recorder.Code)
	}
}

// 对不存在的客户端执行动作返回 404，不泄露其他信息。
func TestClientTokenActionUnknownClient(t *testing.T) {
	router, _ := newNotificationRouter(t, &stubTestNotifier{})

	for _, path := range []string{
		"/api/v1/clients/cli_missing/tokens:rotate",
		"/api/v1/clients/cli_missing/tokens:revoke",
		"/api/v1/clients/cli_missing/enrollment-credentials",
	} {
		recorder := doClientRequest(t, router, http.MethodPost, path, "", true)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s 应返回 404，实际 %d：%s", path, recorder.Code, recorder.Body.String())
		}
	}
}

// 客户端 token 不能用于访问管理员端点（两类凭据互不通用）。
func TestClientTokenCannotAccessAdminEndpoints(t *testing.T) {
	router, _ := newNotificationRouter(t, &stubTestNotifier{})
	_, token := createClient(t, router, "客户端甲")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/clients", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("客户端 token 不得用于管理员端点，实际 %d", recorder.Code)
	}
}

// createClient 创建客户端并返回其标识与 token 明文。
func createClient(t *testing.T, router *gin.Engine, name string) (string, string) {
	t.Helper()
	created := doClientRequest(t, router, http.MethodPost, "/api/v1/clients",
		`{"name":"`+name+`"}`, true)
	if created.Code != http.StatusCreated {
		t.Fatalf("创建客户端失败：%d %s", created.Code, created.Body.String())
	}
	var response struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析创建响应失败：%v", err)
	}
	return response.ID, response.Token
}

// assertTokenRejected 断言 token 已不可用于鉴权。
func assertTokenRejected(t *testing.T, database *store.Store, token string) {
	t.Helper()
	err := database.View(t.Context(), func(tx *store.Tx) error {
		_, err := tx.AuthenticateClientToken(token)
		return err
	})
	if err == nil {
		t.Fatal("token 应已失效但鉴权成功")
	}
}

// assertTokenAccepted 断言 token 可用于鉴权。
func assertTokenAccepted(t *testing.T, database *store.Store, token string) {
	t.Helper()
	err := database.View(t.Context(), func(tx *store.Tx) error {
		_, err := tx.AuthenticateClientToken(token)
		return err
	})
	if err != nil {
		t.Fatalf("token 应可用但鉴权失败：%v", err)
	}
}
