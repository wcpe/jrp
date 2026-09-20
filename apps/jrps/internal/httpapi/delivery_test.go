package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jrp/apps/jrps/internal/store"
)

// 测试辅助：以已登录会话发出只读请求；notifier 为空时不需要 CSRF。
func doDeliveryRequest(t *testing.T, router *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	login := doLogin(t, router, `{"username":"admin","password":"correct-horse-battery"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("登录失败：%d", login.Code)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.AddCookie(login.Result().Cookies()[0])
	router.ServeHTTP(recorder, request)
	return recorder
}

// 投递结果查询是只读端点，仍需管理员会话。
func TestDeliveryListRequiresSession(t *testing.T) {
	router, _ := newNotificationRouter(t, &stubTestNotifier{})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/notification-deliveries", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("未认证应返回 401，实际 %d", recorder.Code)
	}
}

// 只读端点不要求 CSRF：GET 无副作用，契约也未要求携带 token。
func TestDeliveryListDoesNotRequireCSRF(t *testing.T) {
	router, _ := newNotificationRouter(t, &stubTestNotifier{})

	recorder := doDeliveryRequest(t, router, "/api/v1/notification-deliveries")
	if recorder.Code != http.StatusOK {
		t.Fatalf("只读查询应放行，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
}

// 失败终态必须留下失败次数、脱敏错误摘要与最终停止时间——这既是规格 §5
// 的实机条款要求，也是 Web 通知页"发送结果"的数据来源。
func TestDeliveryListExposesFailureTerminalEvidence(t *testing.T) {
	router, database := newNotificationRouter(t, &stubTestNotifier{})
	seedDelivery(t, database, "evt-failed", store.OutboxStatusFailed, 4, "投递失败：连接被拒绝")

	recorder := doDeliveryRequest(t, router, "/api/v1/notification-deliveries")
	if recorder.Code != http.StatusOK {
		t.Fatalf("查询应返回 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}

	var response deliveriesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if len(response.Items) != 1 {
		t.Fatalf("应返回 1 条，实际 %d", len(response.Items))
	}
	item := response.Items[0]
	if item.Attempts != 4 {
		t.Fatalf("应保留失败次数 4，实际 %d", item.Attempts)
	}
	if item.StoppedAt == "" {
		t.Fatalf("失败终态必须带停止时间")
	}
	if item.LastError != "投递失败：连接被拒绝" {
		t.Fatalf("应保留脱敏错误摘要，实际 %q", item.LastError)
	}
	if item.Status != store.OutboxStatusFailed {
		t.Fatalf("状态应为 failed，实际 %q", item.Status)
	}
}

// stopped=true 只返回已停止重试的记录：仍在重试的若混入，页面会把
// "正在重试"误报成"最终失败"。
func TestDeliveryListStoppedFilterExcludesRetrying(t *testing.T) {
	router, database := newNotificationRouter(t, &stubTestNotifier{})
	seedDelivery(t, database, "evt-retrying", store.OutboxStatusRetrying, 2, "暂时失败")
	seedDelivery(t, database, "evt-failed", store.OutboxStatusFailed, 4, "最终失败")

	recorder := doDeliveryRequest(t, router, "/api/v1/notification-deliveries?stopped=true")
	var response deliveriesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if len(response.Items) != 1 || response.Items[0].EventID != "evt-failed" {
		t.Fatalf("stopped 过滤应只留终态记录：%+v", response.Items)
	}
}

// 查询参数非法时返回 400 与中文说明，且不回显非法输入值。
func TestDeliveryListRejectsInvalidQuery(t *testing.T) {
	router, _ := newNotificationRouter(t, &stubTestNotifier{})

	for _, path := range []string{
		"/api/v1/notification-deliveries?status=不存在的状态",
		"/api/v1/notification-deliveries?limit=abc",
		"/api/v1/notification-deliveries?limit=999",
		"/api/v1/notification-deliveries?cursor=不是游标",
	} {
		recorder := doDeliveryRequest(t, router, path)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s 应返回 400，实际 %d", path, recorder.Code)
		}
		body := recorder.Body.String()
		if !strings.Contains(body, "查询参数不合法") {
			t.Fatalf("%s 的响应应含中文说明：%s", path, body)
		}
		if strings.Contains(body, "不存在的状态") {
			t.Fatalf("%s 的响应不得回显非法输入值：%s", path, body)
		}
	}
}

// seedDelivery 写入一条投递记录并置为指定状态。
func seedDelivery(
	t *testing.T, database *store.Store, eventID, status string, attempts int, lastError string,
) {
	t.Helper()
	err := database.Transaction(context.Background(), func(tx *store.Tx) error {
		if _, err := tx.OutboxEnqueue(store.NotificationOutbox{
			EventID:   eventID,
			EventType: store.EventTypeTargetCreated,
			Payload:   `{"摘要":"测试"}`,
			TargetID:  "nt_seed",
		}); err != nil {
			return err
		}
		return tx.MarkOutboxTerminalForTest(eventID, status, attempts, lastError, time.Now().UTC())
	})
	if err != nil {
		t.Fatalf("写入投递记录失败：%v", err)
	}
}
