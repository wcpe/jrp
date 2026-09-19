package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jrp/apps/jrps/internal/store"
)

// policyAPI 持有保留策略端点所需的依赖。
type policyAPI struct {
	store  *store.Store
	logger *slog.Logger
}

// newPolicyAPI 构造策略端点。
func newPolicyAPI(options RouterOptions) *policyAPI {
	return &policyAPI{store: options.Store, logger: options.Logger}
}

// capturePolicyResponse 是策略读取与写入的响应。
//
// 字段名与规格 §3.4 的三项策略值一一对应；采集关闭是默认值而非缺省项，
// 始终出现在响应里，避免调用方把"未返回"误读为"未设置"。
type capturePolicyResponse struct {
	CaptureEnabled     bool   `json:"captureEnabled"`
	RetentionDays      int    `json:"retentionDays"`
	MaxTotalBytes      int64  `json:"maxTotalBytes"`
	AuditRetentionDays int    `json:"auditRetentionDays"`
	UpdatedAt          string `json:"updatedAt"`
}

// capturePolicyRequest 是策略变更请求体。
//
// 每个字段都是必填：策略是整体对象，"只改一个字段"的语义会让并发写入更容易
// 互相覆盖，也让审计的"变更前后值"难以表达。调用方先读再整体写。
type capturePolicyRequest struct {
	CaptureEnabled     *bool  `json:"captureEnabled"`
	RetentionDays      *int   `json:"retentionDays"`
	MaxTotalBytes      *int64 `json:"maxTotalBytes"`
	AuditRetentionDays *int   `json:"auditRetentionDays"`
}

// show 处理 GET /api/v1/capture-policy。
func (api *policyAPI) show(c *gin.Context) {
	var policy store.CapturePolicy
	err := api.store.View(c.Request.Context(), func(tx *store.Tx) error {
		var readErr error
		policy, readErr = tx.CapturePolicy()
		return readErr
	})
	if err != nil {
		api.logError("读取保留策略失败", err, c)
		writeProblem(c, http.StatusInternalServerError, codeInternalError, "服务内部错误", "读取保留策略失败，请稍后重试")
		return
	}
	c.JSON(http.StatusOK, policyResponse(policy.View()))
}

// update 处理 PUT /api/v1/capture-policy。
//
// 越界值返回 400 且不写入任何变更（规格 §2.3）：校验在 store 层完成，
// 保证"读—校验—写"在同一事务内，不会出现部分字段已生效的状态。
func (api *policyAPI) update(c *gin.Context) {
	var request capturePolicyRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		writeProblem(c, http.StatusBadRequest, codeInvalidInput, "请求体不合法", "请求体必须是包含全部策略字段的 JSON 对象")
		return
	}
	input, ok := policyInputFromRequest(c, request)
	if !ok {
		return
	}

	actor, ok := auditActor(c)
	if !ok {
		return
	}

	var updated store.CapturePolicyView
	err := api.store.Transaction(c.Request.Context(), func(tx *store.Tx) error {
		var updateErr error
		updated, updateErr = tx.UpdateCapturePolicy(actor, input)
		return updateErr
	})
	if err != nil {
		var violationErr store.PolicyViolationError
		if errors.As(err, &violationErr) {
			writeProblem(c, http.StatusBadRequest, codeInvalidInput, "策略值不合法", violationErr.Error())
			return
		}
		api.logError("写入保留策略失败", err, c)
		writeProblem(c, http.StatusInternalServerError, codeInternalError, "服务内部错误", "写入保留策略失败，请稍后重试")
		return
	}
	c.JSON(http.StatusOK, policyResponse(updated))
}

// policyInputFromRequest 把请求体转为策略输入；字段缺失时已写入响应并返回 false。
func policyInputFromRequest(c *gin.Context, request capturePolicyRequest) (store.CapturePolicyInput, bool) {
	if request.CaptureEnabled == nil || request.RetentionDays == nil ||
		request.MaxTotalBytes == nil || request.AuditRetentionDays == nil {
		writeProblem(c, http.StatusBadRequest, codeInvalidInput, "策略值不合法",
			"必须提供 captureEnabled、retentionDays、maxTotalBytes 与 auditRetentionDays 四项")
		return store.CapturePolicyInput{}, false
	}
	return store.CapturePolicyInput{
		CaptureEnabled:     *request.CaptureEnabled,
		RetentionDays:      *request.RetentionDays,
		MaxTotalBytes:      *request.MaxTotalBytes,
		AuditRetentionDays: *request.AuditRetentionDays,
	}, true
}

// auditActor 构造审计主体。
//
// P1 只有一个管理员，主体标识取固定值，与 store 层既有写入点保持一致
// （admin.go 的登录、登出与初始化均用 "admin"）。主体来自已认证会话这个事实
// 由 authenticate 中间件保证：本函数只应在会话中间件之后调用。
func auditActor(c *gin.Context) (store.Actor, bool) {
	if _, err := currentSession(c); err != nil {
		writeUnauthenticated(c)
		return store.Actor{}, false
	}
	return store.ActorAdmin(adminActorID), true
}

// adminActorID 是 P1 唯一管理员的审计主体标识。
const adminActorID = "admin"

// policyResponse 把视图转为响应体。
func policyResponse(view store.CapturePolicyView) capturePolicyResponse {
	return capturePolicyResponse{
		CaptureEnabled:     view.CaptureEnabled,
		RetentionDays:      view.RetentionDays,
		MaxTotalBytes:      view.MaxTotalBytes,
		AuditRetentionDays: view.AuditRetentionDays,
		UpdatedAt:          view.UpdatedAt.UTC().Format(timeRFC3339),
	}
}

// logError 记录中文错误日志；请求标识用于关联，不含任何秘密。
func (api *policyAPI) logError(message string, err error, c *gin.Context) {
	if api.logger == nil {
		return
	}
	api.logger.Error(message, "错误", err, "请求", requestID(c))
}
