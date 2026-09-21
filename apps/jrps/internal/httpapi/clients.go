package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jrp/apps/jrps/internal/store"
)

// clientAPI 持有客户端与 token 管理端点所需的依赖。
type clientAPI struct {
	store  *store.Store
	logger *slog.Logger
}

// newClientAPI 构造客户端管理端点。
func newClientAPI(options RouterOptions) *clientAPI {
	return &clientAPI{store: options.Store, logger: options.Logger}
}

// clientItem 是客户端列表与详情的响应项。
//
// 只含掩码与元数据：完整 token 与凭据明文只在创建、兑换或轮换响应中出现一次
// （FR-07 §3.3）。
type clientItem struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	MaskedToken     string `json:"maskedToken"`
	EnrollmentState string `json:"enrollmentState"`
	ConnectionState string `json:"connectionState"`
	DesiredRevision uint64 `json:"desiredRevision"`
	ActiveRevision  uint64 `json:"activeRevision"`
}

// clientListResponse 是客户端列表的分页响应。
type clientListResponse struct {
	Items []clientItem `json:"items"`
}

// createClientRequest 是创建客户端的请求体。
type createClientRequest struct {
	Name string `json:"name"`
}

// clientSecretResponse 是唯一会携带明文秘密的响应。
//
// token 字段只在创建、兑换与轮换时出现；其余接口一律返回掩码。
type clientSecretResponse struct {
	clientItem
	// Token 是明文，只此一次；UI 应标记为一次性展示。
	Token string `json:"token"`
}

// createClientResponse 是创建客户端的响应。
//
// 创建即产出独立 token 并同事务写审计：客户端没有 token 就无法注册，
// 分成两步会让"创建了但没有凭据"成为可观测的中间态。
type createClientResponse struct {
	clientSecretResponse
	// EnrollmentCredential 是一次性凭据明文，同样只返回一次。
	EnrollmentCredential string `json:"enrollmentCredential"`
}

// list 处理 GET /api/v1/clients。
func (api *clientAPI) list(c *gin.Context) {
	var views []store.ClientView
	err := api.store.View(c.Request.Context(), func(tx *store.Tx) error {
		var readErr error
		views, readErr = tx.Clients()
		return readErr
	})
	if err != nil {
		api.logError("读取客户端列表失败", err, c)
		writeProblem(c, http.StatusInternalServerError, codeInternalError, "服务内部错误", "读取客户端列表失败，请稍后重试")
		return
	}
	items := make([]clientItem, 0, len(views))
	for _, view := range views {
		items = append(items, clientItemFrom(view))
	}
	c.JSON(http.StatusOK, clientListResponse{Items: items})
}

// show 处理 GET /api/v1/clients/{clientId}。
func (api *clientAPI) show(c *gin.Context) {
	clientID := c.Param("clientId")
	var view store.ClientView
	err := api.store.View(c.Request.Context(), func(tx *store.Tx) error {
		var readErr error
		view, readErr = tx.Client(clientID)
		return readErr
	})
	if err != nil {
		if errors.Is(err, store.ErrClientNotFound) {
			writeProblem(c, http.StatusNotFound, codeNotFound, "客户端不存在", "请求的客户端不存在")
			return
		}
		api.logError("读取客户端失败", err, c)
		writeProblem(c, http.StatusInternalServerError, codeInternalError, "服务内部错误", "读取客户端失败，请稍后重试")
		return
	}
	c.JSON(http.StatusOK, clientItemFrom(view))
}

// create 处理 POST /api/v1/clients。
//
// 同时返回独立 token 与一次性 enrollment 凭据：前者供已有身份的场景直接使用，
// 后者供 jrpc 首次注册换取身份（FR-07 §3.2）。
func (api *clientAPI) create(c *gin.Context) {
	var request createClientRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		writeProblem(c, http.StatusBadRequest, codeInvalidInput, "请求体不合法", "请求体必须是包含 name 的 JSON")
		return
	}
	if request.Name == "" {
		writeProblem(c, http.StatusBadRequest, codeInvalidInput, "请求体不合法", "name：名称不能为空")
		return
	}
	clientID, err := store.NewClientID()
	if err != nil {
		api.logError("生成客户端标识失败", err, c)
		writeProblem(c, http.StatusInternalServerError, codeInternalError, "服务内部错误", "创建客户端失败，请稍后重试")
		return
	}
	token, err := store.NewClientToken()
	if err != nil {
		api.logError("生成客户端 token 失败", err, c)
		writeProblem(c, http.StatusInternalServerError, codeInternalError, "服务内部错误", "创建客户端失败，请稍后重试")
		return
	}

	var view store.ClientView
	var credential string
	err = api.store.Transaction(c.Request.Context(), func(tx *store.Tx) error {
		if _, err := tx.CreateClient(store.ClientInput{ID: clientID, Name: request.Name, Token: token}); err != nil {
			return err
		}
		issued, err := tx.IssueEnrollmentCredential(clientID)
		if err != nil {
			return err
		}
		credential = issued
		view, err = tx.Client(clientID)
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrClientNameInvalid) {
			writeProblem(c, http.StatusBadRequest, codeInvalidInput, "请求体不合法", nameViolationDetail(err))
			return
		}
		api.logError("创建客户端失败", err, c)
		writeProblem(c, http.StatusInternalServerError, codeInternalError, "服务内部错误", "创建客户端失败，请稍后重试")
		return
	}

	c.JSON(http.StatusCreated, createClientResponse{
		clientSecretResponse: clientSecretResponse{
			clientItem: clientItemFrom(view),
			Token:      token,
		},
		EnrollmentCredential: credential,
	})
}

// clientAction 处理 POST /api/v1/clients/{clientId}/* 下的全部子动作。
//
// 对外 URL 是契约要求的形式（`tokens:rotate`、`tokens:revoke`、
// `enrollment-credentials`），但 gin 不允许同一路径段既有静态段又有通配符，
// 两个冒号动作也会被判定为冲突通配符，因此统一捕获后在这里切分。
// 未识别的子动作按 404 处理：它等价于"该动作不存在"。
func (api *clientAPI) clientAction(c *gin.Context) {
	action := strings.TrimPrefix(c.Param("action"), "/")
	switch action {
	case "tokens:rotate":
		api.rotate(c)
	case "tokens:revoke":
		api.revoke(c)
	case "enrollment-credentials":
		api.issueCredential(c)
	default:
		writeProblem(c, http.StatusNotFound, codeNotFound, "资源不存在", "请求的资源不存在")
	}
}

// rotate 处理 POST /api/v1/clients/{clientId}/tokens:rotate。
func (api *clientAPI) rotate(c *gin.Context) {
	clientID := c.Param("clientId")
	var view store.ClientView
	var token string
	err := api.store.Transaction(c.Request.Context(), func(tx *store.Tx) error {
		rotated, issued, err := tx.RotateClientToken(clientID)
		view, token = rotated, issued
		return err
	})
	if err != nil {
		api.writeTokenActionError(c, "轮换 token 失败", err)
		return
	}
	c.JSON(http.StatusOK, clientSecretResponse{clientItem: clientItemFrom(view), Token: token})
}

// revoke 处理 POST /api/v1/clients/{clientId}/tokens:revoke。
func (api *clientAPI) revoke(c *gin.Context) {
	clientID := c.Param("clientId")
	var view store.ClientView
	err := api.store.Transaction(c.Request.Context(), func(tx *store.Tx) error {
		revoked, err := tx.RevokeClientToken(clientID)
		view = revoked
		return err
	})
	if err != nil {
		api.writeTokenActionError(c, "吊销 token 失败", err)
		return
	}
	c.JSON(http.StatusOK, clientItemFrom(view))
}

// issueCredential 处理 POST /api/v1/clients/{clientId}/enrollment-credentials。
//
// 供"凭据丢失或需要重新注册"的场景重新发行；发行不影响既有凭据之外的状态。
func (api *clientAPI) issueCredential(c *gin.Context) {
	clientID := c.Param("clientId")
	var credential string
	err := api.store.Transaction(c.Request.Context(), func(tx *store.Tx) error {
		issued, err := tx.IssueEnrollmentCredential(clientID)
		credential = issued
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrClientNotFound) {
			writeProblem(c, http.StatusNotFound, codeNotFound, "客户端不存在", "请求的客户端不存在")
			return
		}
		api.logError("发行 enrollment 凭据失败", err, c)
		writeProblem(c, http.StatusInternalServerError, codeInternalError, "服务内部错误", "发行 enrollment 凭据失败，请稍后重试")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"enrollmentCredential": credential})
}

// writeTokenActionError 把 token 生命周期动作的错误映射为问题详情。
//
// 客户端不存在返回 404；其余按内部错误处理。错误说明不回显 token 值。
func (api *clientAPI) writeTokenActionError(c *gin.Context, message string, err error) {
	if errors.Is(err, store.ErrClientNotFound) {
		writeProblem(c, http.StatusNotFound, codeNotFound, "客户端不存在", "请求的客户端不存在")
		return
	}
	api.logError(message, err, c)
	writeProblem(c, http.StatusInternalServerError, codeInternalError, "服务内部错误", message+"，请稍后重试")
}

// logError 记录服务端错误；日志只含请求标识与错误，不含 token 值。
func (api *clientAPI) logError(message string, err error, c *gin.Context) {
	if api.logger == nil {
		// 与同包其他端点一致：未注入 logger 时静默，而不是 panic。
		return
	}
	api.logger.Error(message, "错误", err, "请求标识", requestID(c))
}

// nameViolationDetail 提取名称校验失败的中文说明。
//
// 从哨兵错误的包装文本里取冒号后的部分，不回显用户输入的名称本身。
func nameViolationDetail(err error) string {
	message := err.Error()
	if index := strings.Index(message, "："); index >= 0 {
		return message[index+len("："):]
	}
	return "名称不合法"
}

// clientItemFrom 把脱敏视图转为响应项。
func clientItemFrom(view store.ClientView) clientItem {
	return clientItem{
		ActiveRevision:  view.ActiveRevision,
		ConnectionState: view.ConnectionState,
		DesiredRevision: view.DesiredRevision,
		EnrollmentState: view.EnrollmentState,
		ID:              view.ID,
		MaskedToken:     view.MaskedToken(),
		Name:            view.Name,
	}
}
