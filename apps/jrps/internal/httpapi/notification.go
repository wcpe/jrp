package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jrp/apps/jrps/internal/store"
)

// notificationAPI 持有通知目标端点所需的依赖。
type notificationAPI struct {
	store  *store.Store
	logger *slog.Logger
	// tester 执行测试通知投递；为空时测试通知端点返回 503。
	//
	// 以接口注入而不是直接构造渠道：让 HTTP 层不依赖 notify 包，
	// 测试也无需真实网络即可覆盖端点的鉴权与校验路径。
	tester TestNotificationSender
}

// TestNotificationSender 投递一条测试通知。
type TestNotificationSender interface {
	SendTest(ctx context.Context, target store.NotificationTargetView) error
}

// newNotificationAPI 构造通知端点。
func newNotificationAPI(options RouterOptions) *notificationAPI {
	return &notificationAPI{store: options.Store, logger: options.Logger, tester: options.TestNotifications}
}

// notificationTargetResponse 是目标的脱敏响应。
//
// 秘密只以掩码出现：完整秘密仅在创建与更新时接收，此后再不经 API 输出
// （规格 §3.5）。
type notificationTargetResponse struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	Enabled      bool     `json:"enabled"`
	MaskedSecret string   `json:"maskedSecret"`
	Summary      string   `json:"summary"`
	WebhookURL   string   `json:"webhookUrl,omitempty"`
	SMTPHost     string   `json:"smtpHost,omitempty"`
	SMTPPort     int      `json:"smtpPort,omitempty"`
	SMTPFrom     string   `json:"smtpFrom,omitempty"`
	SMTPTo       []string `json:"smtpTo,omitempty"`
	SMTPSecurity string   `json:"smtpSecurity,omitempty"`
	CreatedAt    string   `json:"createdAt"`
	UpdatedAt    string   `json:"updatedAt"`
}

// notificationTargetsResponse 是目标列表响应。
type notificationTargetsResponse struct {
	Items []notificationTargetResponse `json:"items"`
}

// notificationTargetRequest 是创建与更新请求体。
type notificationTargetRequest struct {
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	Enabled      *bool    `json:"enabled"`
	Secret       string   `json:"secret"`
	WebhookURL   string   `json:"webhookUrl"`
	SMTPHost     string   `json:"smtpHost"`
	SMTPPort     int      `json:"smtpPort"`
	SMTPFrom     string   `json:"smtpFrom"`
	SMTPTo       []string `json:"smtpTo"`
	SMTPSecurity string   `json:"smtpSecurity"`
}

// list 处理 GET /api/v1/notification-targets。
func (api *notificationAPI) list(c *gin.Context) {
	var views []store.NotificationTargetView
	err := api.store.View(c.Request.Context(), func(tx *store.Tx) error {
		var readErr error
		views, readErr = tx.NotificationTargets()
		return readErr
	})
	if err != nil {
		api.logError("读取通知目标失败", err, c)
		writeProblem(c, http.StatusInternalServerError, codeInternalError, "服务内部错误", "读取通知目标失败，请稍后重试")
		return
	}
	items := make([]notificationTargetResponse, 0, len(views))
	for _, view := range views {
		items = append(items, notificationTargetResponseFrom(view))
	}
	c.JSON(http.StatusOK, notificationTargetsResponse{Items: items})
}

// create 处理 POST /api/v1/notification-targets。
func (api *notificationAPI) create(c *gin.Context) {
	request, ok := bindNotificationTargetRequest(c)
	if !ok {
		return
	}
	actor, ok := auditActor(c)
	if !ok {
		return
	}
	input := notificationInputFromRequest(request)

	var view store.NotificationTargetView
	err := api.store.Transaction(c.Request.Context(), func(tx *store.Tx) error {
		var createErr error
		view, createErr = tx.CreateNotificationTarget(actor, input)
		return createErr
	})
	if err != nil {
		api.writeTargetError(c, "创建通知目标失败", err)
		return
	}
	c.JSON(http.StatusCreated, notificationTargetResponseFrom(view))
}

// update 处理 PATCH /api/v1/notification-targets/{targetId}。
//
// 用 PATCH 而不是 PUT：请求体缺省秘密表示保留原值，是部分更新语义。
func (api *notificationAPI) update(c *gin.Context) {
	id := c.Param("targetId")
	if strings.TrimSpace(id) == "" {
		writeProblem(c, http.StatusBadRequest, codeInvalidInput, "请求不合法", "缺少目标标识")
		return
	}
	request, ok := bindNotificationTargetRequest(c)
	if !ok {
		return
	}
	actor, ok := auditActor(c)
	if !ok {
		return
	}
	input := notificationInputFromRequest(request)

	var view store.NotificationTargetView
	err := api.store.Transaction(c.Request.Context(), func(tx *store.Tx) error {
		var updateErr error
		view, updateErr = tx.UpdateNotificationTarget(actor, id, input)
		return updateErr
	})
	if err != nil {
		api.writeTargetError(c, "更新通知目标失败", err)
		return
	}
	c.JSON(http.StatusOK, notificationTargetResponseFrom(view))
}

// remove 处理 DELETE /api/v1/notification-targets/{targetId}。
func (api *notificationAPI) remove(c *gin.Context) {
	id := c.Param("targetId")
	if strings.TrimSpace(id) == "" {
		writeProblem(c, http.StatusBadRequest, codeInvalidInput, "请求不合法", "缺少目标标识")
		return
	}
	actor, ok := auditActor(c)
	if !ok {
		return
	}
	err := api.store.Transaction(c.Request.Context(), func(tx *store.Tx) error {
		return tx.DeleteNotificationTarget(actor, id)
	})
	if err != nil {
		api.writeTargetError(c, "删除通知目标失败", err)
		return
	}
	c.Status(http.StatusNoContent)
}

// test 处理 POST /api/v1/notification-targets/{targetId}:test。
//
// 发送的是明确的测试事件，不伪造业务事件（规格 §3.6）：下游据此可区分
// "管理员在验证配置"与"系统真出了问题"。
//
// 路径由 catch-all 捕获后在此切分后缀：对外 URL 保持契约的 `{targetId}:test`
// 形式，而 gin 的路径语法无法直接表达它（见 router.go 的注册说明）。
func (api *notificationAPI) test(c *gin.Context) {
	id, ok := parseTestTargetID(c)
	if !ok {
		return
	}
	if api.tester == nil {
		writeProblem(c, http.StatusServiceUnavailable, codeUninitialized, "通知渠道不可用", "测试通知渠道未启用")
		return
	}
	actor, ok := auditActor(c)
	if !ok {
		return
	}

	var view store.NotificationTargetView
	err := api.store.View(c.Request.Context(), func(tx *store.Tx) error {
		var readErr error
		view, readErr = tx.NotificationTargetByID(id)
		return readErr
	})
	if errors.Is(err, store.ErrNotificationTargetMissing) {
		writeProblem(c, http.StatusNotFound, codeNotFound, "目标不存在", "通知目标不存在或已被删除")
		return
	}
	if err != nil {
		api.logError("读取通知目标失败", err, c)
		writeProblem(c, http.StatusInternalServerError, codeInternalError, "服务内部错误", "读取通知目标失败，请稍后重试")
		return
	}
	// 已停用的目标拒绝测试通知：停用的含义就是不接收通知，对它能测试会让管理员
	// 误以为目标工作正常，而业务通知实际不会发往它——业务投递路径同样拒绝停用目标。
	if !view.Enabled {
		// 被拒绝的操作同样留痕：规格 §2.1 要求失败与被拒绝都必须记录，
		// 不得以"没做成就不算操作"跳过。
		api.writeTestAudit(c, actor, view, store.AuditResultDenied,
			"测试通知被拒绝：目标处于停用状态（渠道 "+view.Type+"）")
		writeProblem(c, http.StatusConflict, codeConflict, "目标已停用",
			"该通知目标处于停用状态，不会接收任何通知；如确需验证请先启用")
		return
	}

	sendErr := api.tester.SendTest(c.Request.Context(), view)
	if sendErr != nil {
		api.writeTestAudit(c, actor, view, store.AuditResultFailure,
			"测试通知发送失败（渠道 "+view.Type+"）")
		// 只回传可安全公开的失败类别，不转发底层错误文本。
		writeProblem(c, http.StatusBadGateway, codeInternalError, "测试通知发送失败",
			"目标未能接收测试通知，请检查地址、凭据与网络可达性")
		return
	}
	api.writeTestAudit(c, actor, view, store.AuditResultSuccess,
		"发送测试通知（渠道 "+view.Type+"）")
	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "测试通知已发送"})
}

// writeTestAudit 写入测试通知的审计事件。
//
// 只记录渠道与结果，不含目标地址与秘密；写入失败不改变已产生的业务结果，
// 只记 ERROR 日志——审计写入失败的完整回滚语义属于业务事务，此处是一次
// 已完成的投递的事后留痕。
func (api *notificationAPI) writeTestAudit(
	c *gin.Context, actor store.Actor, view store.NotificationTargetView, result, context string,
) {
	err := api.store.Transaction(c.Request.Context(), func(tx *store.Tx) error {
		return tx.WriteAudit(store.AuditEvent{
			ActorType:  actor.Type,
			ActorID:    actor.ID,
			Action:     store.ActionNotificationTargetTest,
			ObjectType: store.ObjectTypeNotificationMsg,
			ObjectID:   view.ID,
			Result:     result,
			Context:    context,
		})
	})
	if err != nil {
		api.logError("写入测试通知审计失败", err, c)
	}
}

// parseTestTargetID 从 catch-all 捕获值中切出目标标识。
//
// 捕获值形如 `/nt_abc:test`；只接受 `:test` 后缀，其他形式按资源不存在处理，
// 避免把拼写错误的 URL 误当作有效请求。
func parseTestTargetID(c *gin.Context) (string, bool) {
	action := c.Param("action")
	trimmed := strings.TrimPrefix(action, "/")
	const suffix = ":test"
	if !strings.HasSuffix(trimmed, suffix) {
		writeProblem(c, http.StatusNotFound, codeNotFound, "资源不存在", "请求的管理端点不存在")
		return "", false
	}
	id := strings.TrimSuffix(trimmed, suffix)
	if strings.TrimSpace(id) == "" || strings.Contains(id, "/") {
		writeProblem(c, http.StatusNotFound, codeNotFound, "资源不存在", "请求的管理端点不存在")
		return "", false
	}
	return id, true
}

// bindNotificationTargetRequest 解析请求体；失败时已写入响应并返回 false。
func bindNotificationTargetRequest(c *gin.Context) (notificationTargetRequest, bool) {
	var request notificationTargetRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		writeProblem(c, http.StatusBadRequest, codeInvalidInput, "请求体不合法", "请求体必须是合法的 JSON 对象")
		return notificationTargetRequest{}, false
	}
	return request, true
}

// notificationInputFromRequest 把请求体转为 store 输入。
//
// Enabled 用指针区分"未提供"与"显式关闭"：省略时按启用处理，
// 因为创建目标后期望它立刻生效是更常见的意图。
func notificationInputFromRequest(request notificationTargetRequest) store.NotificationTargetInput {
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	return store.NotificationTargetInput{
		Name:         request.Name,
		Type:         request.Type,
		Enabled:      enabled,
		Secret:       request.Secret,
		WebhookURL:   request.WebhookURL,
		SMTPHost:     request.SMTPHost,
		SMTPPort:     request.SMTPPort,
		SMTPFrom:     request.SMTPFrom,
		SMTPTo:       request.SMTPTo,
		SMTPSecurity: request.SMTPSecurity,
	}
}

// writeTargetError 把 store 层错误转为合适的问题详情。
func (api *notificationAPI) writeTargetError(c *gin.Context, logMessage string, err error) {
	var validationErr store.NotificationTargetValidationError
	if errors.As(err, &validationErr) {
		writeProblem(c, http.StatusBadRequest, codeInvalidInput, "通知目标不合法", validationErr.Error())
		return
	}
	if errors.Is(err, store.ErrNotificationTargetMissing) {
		writeProblem(c, http.StatusNotFound, codeNotFound, "目标不存在", "通知目标不存在或已被删除")
		return
	}
	api.logError(logMessage, err, c)
	writeProblem(c, http.StatusInternalServerError, codeInternalError, "服务内部错误", logMessage+"，请稍后重试")
}

// notificationTargetResponseFrom 构造脱敏响应。
func notificationTargetResponseFrom(view store.NotificationTargetView) notificationTargetResponse {
	return notificationTargetResponse{
		ID:           view.ID,
		Name:         view.Name,
		Type:         view.Type,
		Enabled:      view.Enabled,
		MaskedSecret: view.MaskedSecret,
		Summary:      view.Summary,
		WebhookURL:   view.WebhookURL,
		SMTPHost:     view.SMTPHost,
		SMTPPort:     view.SMTPPort,
		SMTPFrom:     view.SMTPFrom,
		SMTPTo:       view.SMTPTo,
		SMTPSecurity: view.SMTPSecurity,
		CreatedAt:    view.CreatedAt.UTC().Format(timeRFC3339),
		UpdatedAt:    view.UpdatedAt.UTC().Format(timeRFC3339),
	}
}

// logError 记录中文错误日志；不含秘密与完整地址。
func (api *notificationAPI) logError(message string, err error, c *gin.Context) {
	if api.logger == nil {
		return
	}
	api.logger.Error(message, "错误", err, "请求", requestID(c))
}
