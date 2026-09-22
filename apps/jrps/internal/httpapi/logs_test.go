package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jrp/apps/jrps/internal/store"
)

// 本文件是 FR-12「请求与运行日志」的首批失败测试（先红后绿）。
//
// 规格路径：docs/specs/request-and-runtime-logging.md。覆盖 §4 第一条（脱敏五类
// 值出现在输出即失败）、§5 的会话保护与审计留痕、以及运行日志通道的降级语义。
// 请求日志的生成路径随 FR-13 接线；本文件按规格 §2 断言「采集关闭时零输出」。

// TestLogsRequireSession 验证未认证访问 GET /api/v1/logs 返回 401（规格 §3.5）。
func TestLogsRequireSession(t *testing.T) {
	router, _ := newLogsTestRouter(t)

	recorder := doRequest(router, http.MethodGet, "/api/v1/logs", "", nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("未认证访问日志查询应返回 401，实际 %d", recorder.Code)
	}
}

// TestLogsQueryWithSession 验证已登录管理员可以查询日志并拿到脱敏 JSON。
func TestLogsQueryWithSession(t *testing.T) {
	router, database := newLogsTestRouter(t)
	writeTestLogEvents(t, database,
		store.LogEvent{Level: "INFO", Component: "server", Event: "client-connected",
			Message: "客户端连接成功", ClientID: "acc-client"},
	)

	recorder := doAuthenticated(t, router, http.MethodGet, "/api/v1/logs", "", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("已认证查询应返回 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	var response logsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if len(response.Items) != 1 {
		t.Fatalf("应返回 1 条日志，实际 %d", len(response.Items))
	}
	if response.Items[0].Message != "客户端连接成功" {
		t.Fatalf("日志消息不符：%q", response.Items[0].Message)
	}
	if response.Items[0].ClientID != "acc-client" {
		t.Fatalf("客户端标识不符：%q", response.Items[0].ClientID)
	}
}

// TestLogsQueryFilterAndPagination 验证等级/组件过滤与游标分页（规格 §3.5）。
func TestLogsQueryFilterAndPagination(t *testing.T) {
	router, database := newLogsTestRouter(t)
	writeTestLogEvents(t, database,
		store.LogEvent{Level: "INFO", Component: "server", Event: "e1", Message: "一"},
		store.LogEvent{Level: "ERROR", Component: "server", Event: "e2", Message: "二"},
		store.LogEvent{Level: "INFO", Component: "apply", Event: "e3", Message: "三"},
	)

	// 按等级过滤。
	recorder := doAuthenticated(t, router, http.MethodGet, "/api/v1/logs?level=ERROR", "", false)
	var filtered logsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &filtered); err != nil {
		t.Fatalf("过滤响应不是合法 JSON：%v", err)
	}
	if len(filtered.Items) != 1 || filtered.Items[0].Level != "ERROR" {
		t.Fatalf("等级过滤应只剩 1 条 ERROR，实际 %+v", filtered.Items)
	}

	// 分页：limit=2 取第一页，cursor 取第二页。
	first := doAuthenticated(t, router, http.MethodGet, "/api/v1/logs?limit=2", "", false)
	var pageOne logsResponse
	if err := json.Unmarshal(first.Body.Bytes(), &pageOne); err != nil {
		t.Fatalf("分页响应不是合法 JSON：%v", err)
	}
	if len(pageOne.Items) != 2 || pageOne.NextCursor == "" {
		t.Fatalf("第一页应有 2 条与非空游标，实际 %d 条游标 %q", len(pageOne.Items), pageOne.NextCursor)
	}
	second := doAuthenticated(t, router, http.MethodGet, "/api/v1/logs?limit=2&cursor="+pageOne.NextCursor, "", false)
	var pageTwo logsResponse
	if err := json.Unmarshal(second.Body.Bytes(), &pageTwo); err != nil {
		t.Fatalf("第二页响应不是合法 JSON：%v", err)
	}
	if len(pageTwo.Items) != 1 {
		t.Fatalf("第二页应剩 1 条，实际 %d", len(pageTwo.Items))
	}
}

// TestLogsQueryInvalidParams 验证非法过滤参数返回 400 问题详情（规格 §3.5）。
func TestLogsQueryInvalidParams(t *testing.T) {
	router, _ := newLogsTestRouter(t)
	for _, query := range []string{
		"/api/v1/logs?limit=abc",
		"/api/v1/logs?from=not-a-time",
		"/api/v1/logs?level=SEVERE",
		"/api/v1/logs?cursor=%2F%2Fbad",
	} {
		recorder := doAuthenticated(t, router, http.MethodGet, query, "", false)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s 应返回 400，实际 %d", query, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "查询参数") {
			t.Fatalf("%s 的响应应含中文问题详情：%s", query, recorder.Body.String())
		}
	}
}

// TestLogsViewWritesAudit 验证日志查看动作写入审计且只含条件摘要与条数
// （规格 §3.5：不记录被查日志的敏感内容）。
func TestLogsViewWritesAudit(t *testing.T) {
	router, database := newLogsTestRouter(t)
	writeTestLogEvents(t, database,
		store.LogEvent{Level: "INFO", Component: "server", Event: "e1", Message: "普通消息"},
	)

	doAuthenticated(t, router, http.MethodGet, "/api/v1/logs?level=INFO", "", false)

	recorder := doAuthenticated(t, router, http.MethodGet, "/api/v1/audit-events?action=log_view", "", false)
	var audits auditEventsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &audits); err != nil {
		t.Fatalf("审计响应不是合法 JSON：%v", err)
	}
	if len(audits.Items) != 1 {
		t.Fatalf("日志查看应留下 1 条审计，实际 %d", len(audits.Items))
	}
	event := audits.Items[0]
	if event.Action != "log_view" {
		t.Fatalf("审计动作应为 log_view，实际 %q", event.Action)
	}
	if strings.Contains(event.Context, "普通消息") {
		t.Fatalf("审计上下文不得包含被查日志内容：%q", event.Context)
	}
}

// TestLogRedactionHelpers 验证脱敏辅助入口对五类敏感值的处理（规格 §3.4）。
func TestLogRedactionHelpers(t *testing.T) {
	const token = "client-token-abcdef0123456789"
	const password = "admin-password-plaintext"
	const cookieValue = "session-cookie-value-xyz"
	const authorization = "Bearer eyJhbGciOiJIUzI1NiJ9.payload.signature"
	const body = "request-body-原文-不得出现"

	// token：只允许摘要前缀，完整值禁止。
	masked := RedactToken(token)
	if strings.Contains(masked, token) || strings.Contains(masked, "abcdef0123456789") {
		t.Fatalf("token 掩码泄露原值：%q", masked)
	}
	if masked == "" {
		t.Fatal("token 掩码不得为空：丢失标识会让排障无从下手")
	}

	// 密码：完全禁止，掩码不含原值任何片段。
	if got := RedactSecret(password); strings.Contains(got, "admin-password") {
		t.Fatalf("密码掩码泄露原值片段：%q", got)
	}

	// Cookie：只允许名称与存在性，值禁止。
	if got := RedactCookieValue("session", cookieValue); strings.Contains(got, cookieValue) {
		t.Fatalf("Cookie 掩码泄露原值：%q", got)
	}

	// Authorization：只允许方案类别。
	authMasked := RedactAuthorization(authorization)
	if strings.Contains(authMasked, "eyJhbGciOiJIUzI1NiJ9") {
		t.Fatalf("Authorization 掩码泄露原值：%q", authMasked)
	}
	if !strings.Contains(strings.ToLower(authMasked), "bearer") {
		t.Fatalf("Authorization 掩码应保留方案类别：%q", authMasked)
	}

	// 路径：查询串按键名掩码，键名可保留。
	if got := SummaryPath("/api/v1/clients?secret=value&page=2"); strings.Contains(got, "value") {
		t.Fatalf("路径摘要泄露查询串值：%q", got)
	}
	if !strings.Contains(SummaryPath("/api/v1/clients?secret=value&page=2"), "secret") {
		t.Fatal("路径摘要应保留查询串键名以便排障")
	}
	if !strings.HasPrefix(SummaryPath("/api/v1/clients?secret=value"), "/api/v1/clients") {
		t.Fatal("路径摘要应保留路径本身")
	}
}

// TestLogSubmitBatchAndDegrade 验证提交进入缓冲并按批量落库（规格 §3.2 运行日志通道）。
func TestLogSubmitBatchAndDegrade(t *testing.T) {
	router, database := newLogsTestRouter(t)
	writeTestLogEvents(t, database,
		store.LogEvent{Level: "INFO", Component: "server", Event: "a", Message: "消息一"},
		store.LogEvent{Level: "WARN", Component: "server", Event: "b", Message: "消息二"},
	)

	recorder := doAuthenticated(t, router, http.MethodGet, "/api/v1/logs", "", false)
	var response logsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if len(response.Items) != 2 {
		t.Fatalf("两条提交都应落库，实际 %d", len(response.Items))
	}
	levels := map[string]bool{}
	for _, item := range response.Items {
		levels[item.Level] = true
	}
	if !levels["INFO"] || !levels["WARN"] {
		t.Fatalf("两条日志的等级应齐全，实际 %+v", response.Items)
	}
}

// TestLogDegradeKeepsWarnError 验证缓冲满时 DEBUG/INFO 被丢弃、WARN/ERROR 保留
// 且丢弃计数可见（规格 §3.2 降级策略 + §5 边界第二条）。
func TestLogDegradeKeepsWarnError(t *testing.T) {
	router, database := newLogsTestRouter(t)
	flushLogs(t, database)

	// 先确认日志通道的容量上界行为可从外部观测：塞满缓冲后 WARN 必须存活。
	for index := 0; index < database.LogChannelCapacity()+10; index++ {
		level := "DEBUG"
		if index%2 == 0 {
			level = "INFO"
		}
		database.SubmitLogEvent(store.LogEvent{
			Level: level, Component: "server", Event: "noise", Message: "填充",
		})
	}
	database.SubmitLogEvent(store.LogEvent{
		Level: "WARN", Component: "server", Event: "kept", Message: "必须保留",
	})
	flushLogs(t, database)

	recorder := doAuthenticated(t, router, http.MethodGet, "/api/v1/logs?level=WARN&limit=100", "", false)
	var response logsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if len(response.Items) != 1 || response.Items[0].Event != "kept" {
		t.Fatalf("缓冲满后 WARN 应保留，实际 %+v", response.Items)
	}

	if dropped := database.LogDropped(); dropped == 0 {
		t.Fatal("低等级日志被丢弃后应有可观测的丢弃计数")
	}
}

// newLogsTestRouter 构造带已初始化存储的测试路由器，并返回存储句柄供提交事件。
//
// 注入 stderr logger：审计写入失败等观测错误在此可见，而不是被静默吞掉。
func newLogsTestRouter(t *testing.T) (*gin.Engine, *store.Store) {
	t.Helper()
	database := openInitializedStore(t, "correct-horse-battery")
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return NewRouter(RouterOptions{Store: database, InsecureCookies: true, Logger: logger}), database
}

// writeTestLogEvents 向存储提交若干日志事件并等待落库。
func writeTestLogEvents(t *testing.T, database *store.Store, events ...store.LogEvent) {
	t.Helper()
	for _, event := range events {
		if !database.SubmitLogEvent(event) {
			t.Fatalf("提交日志事件被拒绝：%+v", event)
		}
	}
	flushLogs(t, database)
}

// flushLogs 等待日志通道内的缓冲全部落库。
func flushLogs(t *testing.T, database *store.Store) {
	t.Helper()
	database.FlushLogEvents()
}

// doRequest 发起一个裸请求（无会话）。
func doRequest(router *gin.Engine, method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	for key, value := range header {
		request.Header.Set(key, value)
	}
	router.ServeHTTP(recorder, request)
	return recorder
}
