package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jrp/apps/jrps/internal/store"
)

// sessionCookieName 是会话 Cookie 名称；Cookie 只承载会话令牌，不承载密码或派生材料。
const sessionCookieName = "jrp_session"

// csrfHeaderName 是 CSRF token 的请求头名。
const csrfHeaderName = "X-CSRF-Token"

// sessionResponse 是会话的脱敏响应：只暴露用户名、过期时间与 CSRF 协议所需状态。
type sessionResponse struct {
	Username  string `json:"username"`
	ExpiresAt string `json:"expiresAt"`
	CSRFToken string `json:"csrfToken"`
}

// sessionAPI 持有会话端点所需的依赖。
type sessionAPI struct {
	store          *store.Store
	insecureCookie bool
	limiter        *loginLimiter
	logger         *slog.Logger
}

// newSessionAPI 构造会话端点。
func newSessionAPI(options RouterOptions) *sessionAPI {
	return &sessionAPI{
		store:          options.Store,
		insecureCookie: options.InsecureCookies,
		limiter:        newLoginLimiter(),
		logger:         options.Logger,
	}
}

// requireInitialized 是初始化完成前的就绪边界。
//
// 未初始化时管理 API 除健康检查外一律返回 503，避免未初始化状态被远程利用（FR-02 §2.1）。
func requireInitialized(database *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		initialized, err := isInitialized(c, database)
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable,
				codeInternalError, "初始化状态不可用", "读取初始化状态失败，请稍后重试")
			c.Abort()
			return
		}
		if !initialized {
			writeProblem(c, http.StatusServiceUnavailable,
				codeUninitialized, "服务尚未初始化", "请先在本机执行 jrps init 完成管理员初始化")
			c.Abort()
			return
		}
		c.Next()
	}
}

func isInitialized(c *gin.Context, database *store.Store) (bool, error) {
	if database == nil {
		return false, nil
	}
	var initialized bool
	err := database.View(c.Request.Context(), func(tx *store.Tx) error {
		var err error
		initialized, err = tx.IsInitialized()
		return err
	})
	return initialized, err
}

// login 校验凭据并建立会话。
//
// 失败时对"用户名不存在"与"密码错误"返回同一问题详情，不泄露账号是否存在。
func (api *sessionAPI) login(c *gin.Context) {
	var request struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		writeProblem(c, http.StatusBadRequest, codeInvalidInput,
			"登录请求格式非法", "请求正文必须为 JSON 且包含用户名与密码")
		return
	}
	if api.limiter.IsLimited(request.Username) {
		writeProblem(c, http.StatusTooManyRequests, codeRateLimited,
			"登录尝试过于频繁", "连续登录失败次数过多，请稍后再试")
		return
	}

	var credential store.AdminCredential
	err := api.store.View(c.Request.Context(), func(tx *store.Tx) error {
		var err error
		credential, err = tx.AuthenticateAdmin(request.Username, request.Password)
		return err
	})
	if err != nil {
		api.handleLoginFailure(c, request.Username, err)
		return
	}

	var issued store.SessionToken
	err = api.store.Transaction(c.Request.Context(), func(tx *store.Tx) error {
		var err error
		issued, err = tx.CreateSession(store.CreateSessionInput{Username: credential.Username})
		return err
	})
	if err != nil {
		api.logError("建立会话失败", err, c)
		writeProblem(c, http.StatusInternalServerError, codeInternalError,
			"建立会话失败", "服务端建立会话失败，请稍后重试")
		return
	}
	api.limiter.Reset(request.Username)
	api.setSessionCookie(c, issued.Token, issued.ExpiresAt)
	c.JSON(http.StatusOK, sessionResponse{
		Username:  credential.Username,
		ExpiresAt: issued.ExpiresAt.UTC().Format(timeRFC3339),
		CSRFToken: issued.CSRFToken,
	})
}

// handleLoginFailure 记录失败尝试并写入审计，对外只返回统一问题详情。
func (api *sessionAPI) handleLoginFailure(c *gin.Context, username string, err error) {
	if errors.Is(err, store.ErrInvalidCredentials) {
		api.limiter.RecordFailure(username)
		_ = api.store.Transaction(c.Request.Context(), func(tx *store.Tx) error {
			return tx.RecordLoginFailure(username)
		})
		writeProblem(c, http.StatusUnauthorized, codeUnauthenticated, "登录失败", "用户名或密码错误")
		return
	}
	api.logError("登录凭据校验失败", err, c)
	writeProblem(c, http.StatusInternalServerError, codeInternalError,
		"登录失败", "服务端校验凭据失败，请稍后重试")
}

// authenticate 是会话校验中间件。
//
// requireCSRF 为真时额外校验 CSRF token：修改类请求必须携带，
// 校验失败返回 403 且不产生副作用（FR-02 规格 §3.3）。
func (api *sessionAPI) authenticate(requireCSRF bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := c.Cookie(sessionCookieName)
		if err != nil || token == "" {
			writeProblem(c, http.StatusUnauthorized, codeUnauthenticated, "未认证", "缺少会话 Cookie，请先登录")
			c.Abort()
			return
		}
		var session store.Session
		viewErr := api.store.View(c.Request.Context(), func(tx *store.Tx) error {
			var err error
			session, err = tx.ActiveSession(token)
			return err
		})
		if viewErr != nil {
			writeProblem(c, http.StatusUnauthorized, codeUnauthenticated,
				"会话已失效", "会话不存在、已过期或已注销，请重新登录")
			c.Abort()
			return
		}
		if requireCSRF && !api.verifyCSRF(c, token) {
			writeProblem(c, http.StatusForbidden, codeCSRFInvalid,
				"CSRF 校验失败", "修改请求必须携带有效的 CSRF token")
			c.Abort()
			return
		}
		c.Set(contextKeySession, session)
		c.Next()
	}
}

// verifyCSRF 校验请求头中的 CSRF token 是否与会话状态匹配。
func (api *sessionAPI) verifyCSRF(c *gin.Context, sessionToken string) bool {
	csrfToken := c.GetHeader(csrfHeaderName)
	if csrfToken == "" {
		return false
	}
	var matched bool
	err := api.store.View(c.Request.Context(), func(tx *store.Tx) error {
		matched = tx.VerifySessionCSRF(sessionToken, csrfToken)
		return nil
	})
	return err == nil && matched
}

// logout 注销当前会话并使其立即失效。
func (api *sessionAPI) logout(c *gin.Context) {
	token, err := c.Cookie(sessionCookieName)
	if err != nil {
		writeUnauthenticated(c)
		return
	}
	err = api.store.Transaction(c.Request.Context(), func(tx *store.Tx) error {
		return tx.InvalidateSession(token)
	})
	if err != nil {
		writeUnauthenticated(c)
		return
	}
	api.clearSessionCookie(c)
	c.JSON(http.StatusOK, gin.H{"status": "已登出"})
}

// query 返回当前会话的脱敏信息与 CSRF 协议所需状态。
//
// 响应不含密码、派生材料或服务端会话密钥（API §4.2）。
func (api *sessionAPI) query(c *gin.Context) {
	session, err := currentSession(c)
	if err != nil {
		writeUnauthenticated(c)
		return
	}
	token, err := c.Cookie(sessionCookieName)
	if err != nil {
		writeUnauthenticated(c)
		return
	}
	var csrfToken string
	err = api.store.View(c.Request.Context(), func(tx *store.Tx) error {
		active, err := tx.ActiveSession(token)
		if err != nil {
			return err
		}
		csrfToken = active.CSRFToken
		return nil
	})
	if err != nil {
		writeUnauthenticated(c)
		return
	}
	c.JSON(http.StatusOK, sessionResponse{
		Username:  store.AdminUsername,
		ExpiresAt: session.ExpiresAt.UTC().Format(timeRFC3339),
		CSRFToken: csrfToken,
	})
}

func writeUnauthenticated(c *gin.Context) {
	writeProblem(c, http.StatusUnauthorized, codeUnauthenticated, "会话已失效", "请重新登录")
}

// logError 记录中文错误日志；请求标识用于关联，不含密码或 Cookie。
func (api *sessionAPI) logError(message string, err error, c *gin.Context) {
	if api.logger == nil {
		return
	}
	api.logger.Error(message, "错误", err, "请求", requestID(c))
}
