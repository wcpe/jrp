package httpapi

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// sessionCookiePath 是会话 Cookie 的作用路径：只覆盖管理 API。
const sessionCookiePath = "/api"

// setSessionCookie 发放会话 Cookie。
//
// Cookie 携带 HttpOnly（脚本不可读）、Secure（仅 HTTPS，开发态由 InsecureCookies 显式回退）
// 与 SameSite=Strict（跨站请求不携带）；它只承载会话令牌，不含密码或派生材料。
func (api *sessionAPI) setSessionCookie(context *gin.Context, token string, expiresAt time.Time) {
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	context.SetSameSite(http.SameSiteStrictMode)
	context.SetCookie(sessionCookieName, token, maxAge, sessionCookiePath, "", !api.insecureCookie, true)
}

// clearSessionCookie 注销时立即清除浏览器侧的会话 Cookie。
func (api *sessionAPI) clearSessionCookie(context *gin.Context) {
	context.SetSameSite(http.SameSiteStrictMode)
	context.SetCookie(sessionCookieName, "", -1, sessionCookiePath, "", !api.insecureCookie, true)
}
