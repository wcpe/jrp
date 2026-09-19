package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// 测试辅助：不带任何会话信息执行请求，用于断言未认证路径。
func doAuthenticatedWithoutSession(t *testing.T, router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder
}

// 测试辅助：判断文本是否含中文。
//
// 断言"是中文说明"而不是匹配固定措辞：措辞会随文案调整，而"给用户的说明
// 必须是中文"才是契约要求（项目规则：面向用户的说明一律中文）。
func containsChinese(text string) bool {
	for _, r := range text {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

// 测试辅助：以已登录会话执行请求。
func doAuthenticated(t *testing.T, router *gin.Engine, method, path, body string, withCSRF bool) *httptest.ResponseRecorder {
	t.Helper()
	login := doLogin(t, router, `{"username":"admin","password":"correct-horse-battery"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("登录失败：%d %s", login.Code, login.Body.String())
	}
	sessionCookie := login.Result().Cookies()[0]

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.AddCookie(sessionCookie)
	request.Header.Set("Content-Type", "application/json")
	if withCSRF {
		request.Header.Set(csrfHeaderName, decodeCSRFToken(t, login.Body.Bytes()))
	}
	router.ServeHTTP(recorder, request)
	return recorder
}

// 审计查询必须要求会话：未认证返回 401（FR-16 §5 错误路径）。
func TestAuditEventsRequireSession(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/audit-events", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("未认证访问审计端点应返回 401，实际 %d", recorder.Code)
	}
}

// 已登录管理员可查询审计事件，响应为脱敏 JSON。
func TestAuditEventsListReturnsEvents(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))

	recorder := doAuthenticated(t, router, http.MethodGet, "/api/v1/audit-events", "", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("查询审计事件失败：%d %s", recorder.Code, recorder.Body.String())
	}

	var response auditEventsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析审计响应失败：%v", err)
	}
	if len(response.Items) == 0 {
		t.Fatal("初始化与登录已产生审计事件，查询结果不应为空")
	}
	for _, item := range response.Items {
		if item.OccurredAt == "" {
			t.Fatalf("审计项缺少时间：%+v", item)
		}
		if item.Action == "" || item.ObjectType == "" {
			t.Fatalf("审计项缺少动作或对象类型：%+v", item)
		}
		// 六项字段齐全：主体、动作、对象、结果、时间、上下文。
		if item.ActorType == "" || item.ActorID == "" || item.Result == "" {
			t.Fatalf("审计项字段不全：%+v", item)
		}
	}
}

// 审计响应不得包含完整口令或会话令牌。
func TestAuditEventsResponseLeaksNoSecrets(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))

	recorder := doAuthenticated(t, router, http.MethodGet, "/api/v1/audit-events", "", false)
	body := recorder.Body.String()
	for _, forbidden := range []string{"correct-horse-battery", "password_digest", "password_salt", "TokenDigest"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("审计响应泄露敏感内容 %q：%s", forbidden, body)
		}
	}
}

// 非法过滤参数返回 400 问题详情，而不是静默忽略。
func TestAuditEventsRejectInvalidFilters(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))
	cases := []string{
		"/api/v1/audit-events?action=未登记动作",
		"/api/v1/audit-events?objectType=未登记对象",
		"/api/v1/audit-events?result=大概成功",
		"/api/v1/audit-events?limit=不是数字",
		"/api/v1/audit-events?limit=9999",
		"/api/v1/audit-events?from=不是时间",
		"/api/v1/audit-events?cursor=这不是游标",
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			recorder := doAuthenticated(t, router, http.MethodGet, path, "", false)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("非法参数应返回 400，实际 %d：%s", recorder.Code, recorder.Body.String())
			}
			var problem problem
			if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
				t.Fatalf("响应应为问题详情：%v", err)
			}
			if problem.Code != codeInvalidInput {
				t.Fatalf("问题码应为 %s，实际 %s", codeInvalidInput, problem.Code)
			}
			if !containsChinese(problem.Detail) {
				t.Fatalf("问题详情应为中文说明：%q", problem.Detail)
			}
		})
	}
}

// 合法过滤参数应被接受。
func TestAuditEventsAcceptValidFilters(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))

	recorder := doAuthenticated(t, router, http.MethodGet,
		"/api/v1/audit-events?limit=10&result=success", "", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("合法过滤参数应返回 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
}

// 分页契约：limit 与 cursor，响应含 items 与可选 nextCursor（API 契约 §1.5）。
func TestAuditEventsPaginates(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))

	// 通过多次登录产生足够多的审计事件。
	for index := 0; index < 4; index += 1 {
		doAuthenticated(t, router, http.MethodGet, "/api/v1/session", "", false)
	}

	first := doAuthenticated(t, router, http.MethodGet, "/api/v1/audit-events?limit=2", "", false)
	if first.Code != http.StatusOK {
		t.Fatalf("分页查询失败：%d", first.Code)
	}
	var firstPage auditEventsResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstPage); err != nil {
		t.Fatalf("解析分页响应失败：%v", err)
	}
	if len(firstPage.Items) != 2 {
		t.Fatalf("首页应返回 2 条，实际 %d 条", len(firstPage.Items))
	}
	if firstPage.NextCursor == "" {
		t.Fatal("还有更多事件时应返回游标")
	}

	second := doAuthenticated(t, router, http.MethodGet,
		"/api/v1/audit-events?limit=2&cursor="+firstPage.NextCursor, "", false)
	if second.Code != http.StatusOK {
		t.Fatalf("第二页查询失败：%d %s", second.Code, second.Body.String())
	}
	var secondPage auditEventsResponse
	if err := json.Unmarshal(second.Body.Bytes(), &secondPage); err != nil {
		t.Fatalf("解析第二页失败：%v", err)
	}
	for _, item := range secondPage.Items {
		for _, previous := range firstPage.Items {
			if item.ID == previous.ID {
				t.Fatalf("游标翻页出现重复记录：%d", item.ID)
			}
		}
	}
}

// 空的查询结果应有确定行为：items 为空数组而非 null，且无游标。
func TestAuditEventsEmptyResultIsDeterministic(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))

	recorder := doAuthenticated(t, router, http.MethodGet,
		"/api/v1/audit-events?action=proxy_delete", "", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("查询失败：%d", recorder.Code)
	}
	var response auditEventsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if len(response.Items) != 0 {
		t.Fatalf("不应命中任何记录，实际 %d 条", len(response.Items))
	}
	if response.NextCursor != "" {
		t.Fatalf("空结果不应有游标：%s", response.NextCursor)
	}
	if !strings.Contains(recorder.Body.String(), `"items":[]`) {
		t.Fatalf("空结果应为空数组而非 null：%s", recorder.Body.String())
	}
}

// API 不提供审计导出与删除接口（FR-16 §3.6）。
func TestAuditEventsExposeNoDeleteOrExport(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))

	for _, method := range []string{http.MethodDelete, http.MethodPost, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			recorder := doAuthenticated(t, router, method, "/api/v1/audit-events", "{}", true)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("审计端点不应支持 %s，实际 %d", method, recorder.Code)
			}
		})
	}
}

// 未初始化时审计端点返回 503，不由就绪边界绕过。
func TestAuditEventsBlockedBeforeInitialization(t *testing.T) {
	router := newTestRouter(openEmptyStore(t))

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/audit-events", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("未初始化时审计端点应返回 503，实际 %d", recorder.Code)
	}
}
