package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// 完整链路：发行凭据 → 兑换 → 拿到可用 token → 重复兑换被拒。
func TestEnrollmentRedeemIssuesUsableTokenOnce(t *testing.T) {
	router, database := newNotificationRouter(t, &stubTestNotifier{})
	clientID, credential := createClientWithCredential(t, router, "客户端甲")

	enrolled := doEnroll(t, router, credential)
	if enrolled.Code != http.StatusOK {
		t.Fatalf("兑换应返回 200，实际 %d：%s", enrolled.Code, enrolled.Body.String())
	}
	var response struct {
		ClientID    string `json:"clientId"`
		Token       string `json:"token"`
		MaskedToken string `json:"maskedToken"`
	}
	if err := json.Unmarshal(enrolled.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析兑换响应失败：%v", err)
	}
	if response.ClientID != clientID {
		t.Fatalf("兑换应返回绑定客户端 %s，实际 %s", clientID, response.ClientID)
	}
	if response.Token == "" {
		t.Fatal("兑换响应必须含一次 token 明文")
	}
	if !strings.HasPrefix(response.MaskedToken, "****") {
		t.Fatalf("掩码应以前缀形式出现，实际 %q", response.MaskedToken)
	}
	assertTokenAccepted(t, database, response.Token)

	// 凭据一次性：再次兑换被拒，且与「凭据不存在」返回同一问题详情。
	// 不作逐字节比较——响应含每次不同的 requestId；要比的是对外可观测的类别
	// 与说明，它们必须一致，否则「凭据曾存在」本身就构成了探测面。
	repeated := doEnroll(t, router, credential)
	unknown := doEnroll(t, router, "完全不存在的凭据")
	if repeated.Code != http.StatusUnauthorized || unknown.Code != http.StatusUnauthorized {
		t.Fatalf("重复兑换与未知凭据都应 401，实际 %d / %d", repeated.Code, unknown.Code)
	}
	repeatedProblem := problemFields(t, repeated)
	unknownProblem := problemFields(t, unknown)
	for _, field := range []string{"title", "detail", "code", "status"} {
		if repeatedProblem[field] != unknownProblem[field] {
			t.Fatalf("字段 %s 在两种失败原因下不一致：%v vs %v（区分原因会形成探测面）",
				field, repeatedProblem[field], unknownProblem[field])
		}
	}
}

// problemFields 提取问题详情中的对外可观测字段，剔除每次变化的 requestId。
func problemFields(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析问题详情失败：%v", err)
	}
	delete(payload, "requestId")
	return payload
}

// 兑换响应不得泄漏其他客户端的任何信息。
func TestEnrollmentResponseLeaksNothingElse(t *testing.T) {
	router, _ := newNotificationRouter(t, &stubTestNotifier{})
	_, otherCredential := createClientWithCredential(t, router, "客户端乙")
	clientID, credential := createClientWithCredential(t, router, "客户端甲")

	enrolled := doEnroll(t, router, credential)
	body := enrolled.Body.String()
	if !strings.Contains(body, clientID) {
		t.Fatalf("响应应含本次兑换的客户端标识：%s", body)
	}
	// 另一客户端的凭据明文绝不能出现在响应里。
	if strings.Contains(body, otherCredential) {
		t.Fatalf("响应不得包含其他客户端的凭据：%s", body)
	}
}

// 空凭据与非法请求体返回 400 与中文说明。
func TestEnrollmentRejectsBadRequests(t *testing.T) {
	router, _ := newNotificationRouter(t, &stubTestNotifier{})

	cases := []struct {
		name string
		body string
	}{
		{"空凭据", `{"credential":""}`},
		{"缺少字段", `{}`},
		{"非 JSON", `不是 JSON`},
	}
	for _, item := range cases {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/agent/v1/enrollments", strings.NewReader(item.body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s 应返回 400，实际 %d：%s", item.name, recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "不合法") {
			t.Fatalf("%s 应给中文说明：%s", item.name, recorder.Body.String())
		}
	}
}

// 已吊销的客户端不能被 enrollment 复活。
func TestEnrollmentRejectsRevokedClient(t *testing.T) {
	router, _ := newNotificationRouter(t, &stubTestNotifier{})
	clientID, credential := createClientWithCredential(t, router, "客户端甲")

	// 先吊销，再用之前发行的凭据兑换：吊销必须让既有凭据一并失效。
	revoked := doClientRequest(t, router, http.MethodPost,
		"/api/v1/clients/"+clientID+"/tokens:revoke", "", true)
	if revoked.Code != http.StatusOK {
		t.Fatalf("吊销失败：%d %s", revoked.Code, revoked.Body.String())
	}

	enrolled := doEnroll(t, router, credential)
	if enrolled.Code != http.StatusUnauthorized {
		t.Fatalf("已吊销客户端的凭据应被拒，实际 %d：%s", enrolled.Code, enrolled.Body.String())
	}
}

// 管理通道是独立体系：管理员会话不能用于该通道，客户端 token 也不能用于管理 API。
func TestAgentChannelIsSeparateFromAdminSession(t *testing.T) {
	router, _ := newNotificationRouter(t, &stubTestNotifier{})

	// 管理员会话访问 enrollment 端点：它是公开端点，不该用会话鉴权，因此
	// 不带凭据时应是 400（请求体问题）而不是 401（鉴权问题）。
	login := doLogin(t, router, `{"username":"admin","password":"correct-horse-battery"}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/agent/v1/enrollments", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(login.Result().Cookies()[0])
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("enrollment 是公开端点，应以请求体问题拒绝，实际 %d", recorder.Code)
	}
}

// 创建客户端并取出其一次性 enrollment 凭据。
func createClientWithCredential(t *testing.T, router *gin.Engine, name string) (string, string) {
	t.Helper()
	created := doClientRequest(t, router, http.MethodPost, "/api/v1/clients",
		`{"name":"`+name+`"}`, true)
	if created.Code != http.StatusCreated {
		t.Fatalf("创建客户端失败：%d %s", created.Code, created.Body.String())
	}
	var response struct {
		ID                   string `json:"id"`
		EnrollmentCredential string `json:"enrollmentCredential"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析创建响应失败：%v", err)
	}
	if response.EnrollmentCredential == "" {
		t.Fatalf("创建响应应含一次性凭据：%s", created.Body.String())
	}
	return response.ID, response.EnrollmentCredential
}

// doEnroll 发出一次 enrollment 兑换请求。
func doEnroll(t *testing.T, router *gin.Engine, credential string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"credential":"` + credential + `","platform":"linux","version":"0.2.0"}`
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/agent/v1/enrollments", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder
}
