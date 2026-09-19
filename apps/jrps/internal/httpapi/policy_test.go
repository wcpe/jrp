package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// 策略读取需要会话。
func TestCapturePolicyShowRequiresSession(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))

	recorder := doAuthenticatedWithoutSession(t, router, http.MethodGet, "/api/v1/capture-policy", "")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("未认证读取策略应返回 401，实际 %d", recorder.Code)
	}
}

// 策略读取返回默认值：采集关闭、正文 30 天、5 GiB、审计 180 天（FR-16 §2.3）。
func TestCapturePolicyShowReturnsDefaults(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))

	recorder := doAuthenticated(t, router, http.MethodGet, "/api/v1/capture-policy", "", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("读取策略失败：%d %s", recorder.Code, recorder.Body.String())
	}
	var response capturePolicyResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析策略响应失败：%v", err)
	}
	if response.CaptureEnabled {
		t.Fatal("采集默认必须关闭")
	}
	if response.RetentionDays != 30 {
		t.Fatalf("正文保留天数默认应为 30，实际 %d", response.RetentionDays)
	}
	if response.MaxTotalBytes != 5<<30 {
		t.Fatalf("正文总量上限默认应为 5 GiB，实际 %d", response.MaxTotalBytes)
	}
	if response.AuditRetentionDays != 180 {
		t.Fatalf("审计保留天数默认应为 180，实际 %d", response.AuditRetentionDays)
	}
}

// 策略修改需要 CSRF：缺少时返回 403 且无副作用。
func TestCapturePolicyUpdateRequiresCSRF(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))
	body := `{"captureEnabled":true,"retentionDays":7,"maxTotalBytes":1073741824,"auditRetentionDays":90}`

	recorder := doAuthenticated(t, router, http.MethodPut, "/api/v1/capture-policy", body, false)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("缺少 CSRF 应返回 403，实际 %d：%s", recorder.Code, recorder.Body.String())
	}

	// 无副作用：策略仍是默认值。
	after := doAuthenticated(t, router, http.MethodGet, "/api/v1/capture-policy", "", false)
	var response capturePolicyResponse
	if err := json.Unmarshal(after.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析策略响应失败：%v", err)
	}
	if response.CaptureEnabled || response.RetentionDays != 30 {
		t.Fatalf("CSRF 失败不得改动策略：%+v", response)
	}
}

// 携带有效 CSRF 时可修改策略，且变更被审计。
func TestCapturePolicyUpdateSucceedsWithCSRF(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))
	body := `{"captureEnabled":true,"retentionDays":7,"maxTotalBytes":1073741824,"auditRetentionDays":90}`

	recorder := doAuthenticated(t, router, http.MethodPut, "/api/v1/capture-policy", body, true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("修改策略失败：%d %s", recorder.Code, recorder.Body.String())
	}
	var updated capturePolicyResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &updated); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if !updated.CaptureEnabled || updated.RetentionDays != 7 || updated.AuditRetentionDays != 90 {
		t.Fatalf("响应应回显新策略：%+v", updated)
	}

	// 变更写入审计。
	auditRecorder := doAuthenticated(t, router, http.MethodGet,
		"/api/v1/audit-events?action=policy_update", "", false)
	var events auditEventsResponse
	if err := json.Unmarshal(auditRecorder.Body.Bytes(), &events); err != nil {
		t.Fatalf("解析审计响应失败：%v", err)
	}
	if len(events.Items) != 1 {
		t.Fatalf("策略变更应产生 1 条审计，实际 %d 条", len(events.Items))
	}
	if !strings.Contains(events.Items[0].Context, "30→7") {
		t.Fatalf("审计应记录变更前后值：%s", events.Items[0].Context)
	}
}

// 越界值返回 400 问题详情，不静默收敛为默认值。
func TestCapturePolicyUpdateRejectsOutOfRange(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))
	cases := []struct {
		name string
		body string
	}{
		{"保留天数为零", `{"captureEnabled":false,"retentionDays":0,"maxTotalBytes":1073741824,"auditRetentionDays":90}`},
		{"保留天数超上限", `{"captureEnabled":false,"retentionDays":9999,"maxTotalBytes":1073741824,"auditRetentionDays":90}`},
		{"总量为负", `{"captureEnabled":false,"retentionDays":30,"maxTotalBytes":-1,"auditRetentionDays":90}`},
		{"审计保留天数过小", `{"captureEnabled":false,"retentionDays":30,"maxTotalBytes":1073741824,"auditRetentionDays":1}`},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			recorder := doAuthenticated(t, router, http.MethodPut, "/api/v1/capture-policy", item.body, true)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("越界值应返回 400，实际 %d：%s", recorder.Code, recorder.Body.String())
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

// 越界请求不产生部分变更：策略保持默认值。
func TestCapturePolicyRejectsPartialChange(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))
	body := `{"captureEnabled":true,"retentionDays":9999,"maxTotalBytes":1073741824,"auditRetentionDays":90}`

	recorder := doAuthenticated(t, router, http.MethodPut, "/api/v1/capture-policy", body, true)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("越界值应返回 400，实际 %d", recorder.Code)
	}

	after := doAuthenticated(t, router, http.MethodGet, "/api/v1/capture-policy", "", false)
	var response capturePolicyResponse
	if err := json.Unmarshal(after.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析策略响应失败：%v", err)
	}
	if response.CaptureEnabled {
		t.Fatal("越界请求不得改动任何字段（采集开关被改动了）")
	}
}

// 请求体缺少字段时返回 400：策略是整体对象，不接受部分提交。
func TestCapturePolicyRejectsIncompleteBody(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))
	cases := []string{
		`{}`,
		`{"captureEnabled":true}`,
		`{"captureEnabled":true,"retentionDays":30}`,
		`{"captureEnabled":true,"retentionDays":30,"maxTotalBytes":1073741824}`,
		`不是 JSON`,
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			recorder := doAuthenticated(t, router, http.MethodPut, "/api/v1/capture-policy", body, true)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("不完整请求体应返回 400，实际 %d：%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

// 未初始化时策略端点返回 503。
func TestCapturePolicyBlockedBeforeInitialization(t *testing.T) {
	router := newTestRouter(openEmptyStore(t))

	recorder := doAuthenticatedWithoutSession(t, router, http.MethodGet, "/api/v1/capture-policy", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("未初始化时策略端点应返回 503，实际 %d", recorder.Code)
	}
}

// 策略响应不得包含任何秘密字段。
func TestCapturePolicyResponseLeaksNothing(t *testing.T) {
	router := newTestRouter(openInitializedStore(t, "correct-horse-battery"))

	recorder := doAuthenticated(t, router, http.MethodGet, "/api/v1/capture-policy", "", false)
	body := recorder.Body.String()
	for _, forbidden := range []string{"correct-horse-battery", "password", "secret", "token"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Fatalf("策略响应泄露敏感内容 %q：%s", forbidden, body)
		}
	}
}
