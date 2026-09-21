package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jrp/apps/jrps/internal/store"
)

// agentAPI 持有 /agent/v1 管理通道所需的依赖。
//
// 该通道使用客户端 token 鉴权，与管理员会话是两套独立体系（FR-07 §3.4）：
// 客户端 token 不能登录管理 API，管理员会话也不能访问该通道。
type agentAPI struct {
	store  *store.Store
	logger *slog.Logger
}

// newAgentAPI 构造管理通道端点。
func newAgentAPI(options RouterOptions) *agentAPI {
	return &agentAPI{store: options.Store, logger: options.Logger}
}

// enrollRequest 是兑换 enrollment 凭据的请求体。
//
// 客户端提交自己的身份材料：身份由客户端生成并在此登记，服务端不再单独分配，
// 使客户端首次启动即可用同一份身份材料完成注册与后续连接（FR-08 §3）。
type enrollRequest struct {
	Credential string `json:"credential"`
	// Platform 与 Version 是客户端上报的运行环境摘要，进审计便于排障。
	Platform string `json:"platform"`
	Version  string `json:"version"`
}

// enrollResponse 是兑换成功的响应。
//
// token 明文只在此出现一次；后续所有管理通道请求都靠它鉴权。
type enrollResponse struct {
	ClientID string `json:"clientId"`
	Token    string `json:"token"`
	// MaskedToken 便于客户端确认响应来自本次兑换而非缓存。
	MaskedToken string `json:"maskedToken"`
}

// enroll 处理 POST /agent/v1/enrollments。
//
// 它是当前唯一的管理通道端点，也是唯一不需要客户端 token 的端点：凭据本身
// 就是入场券。凭据是一次性的，兑换即作废；并发兑换由存储层的条件更新保证
// 只有一个成功。
//
// 受 token 保护的管理通道端点（desired-state、apply-results、connect）属
// FR-08，随该 FR 交付时补上 Bearer 鉴权中间件；此处不预先实现没有挂载点的
// 中间件——那只会留下无人调用的代码。
func (api *agentAPI) enroll(c *gin.Context) {
	var request enrollRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		writeProblem(c, http.StatusBadRequest, codeInvalidInput, "请求体不合法", "请求体必须是包含 credential 的 JSON")
		return
	}
	if request.Credential == "" {
		writeProblem(c, http.StatusBadRequest, codeInvalidInput, "请求体不合法", "credential：凭据不能为空")
		return
	}

	var view store.ClientView
	var token string
	err := api.store.Transaction(c.Request.Context(), func(tx *store.Tx) error {
		redeemed, issued, err := tx.RedeemEnrollmentCredential(request.Credential)
		view, token = redeemed, issued
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrCredentialRejected) {
			// 凭据无效、过期与已使用返回同一响应：区分它们会让攻击者据此
			// 判断凭据是否曾经存在（FR-07 §3.4）。
			writeProblem(c, http.StatusUnauthorized, codeUnauthenticated, "凭据无效", "enrollment 凭据无效、已过期或已被使用")
			return
		}
		api.logError("兑换 enrollment 凭据失败", err, c)
		writeProblem(c, http.StatusInternalServerError, codeInternalError, "服务内部错误", "兑换 enrollment 凭据失败，请稍后重试")
		return
	}

	c.JSON(http.StatusOK, enrollResponse{
		ClientID:    view.ID,
		MaskedToken: view.MaskedToken(),
		Token:       token,
	})
}

// logError 记录服务端错误；日志只含请求标识与错误，不含 token 值。
func (api *agentAPI) logError(message string, err error, c *gin.Context) {
	if api.logger == nil {
		// 与同包其他端点一致：未注入 logger 时静默，而不是 panic。
		return
	}
	api.logger.Error(message, "错误", err, "请求标识", requestID(c))
}
