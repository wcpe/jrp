package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jrp/apps/jrps/internal/store"
)

// 问题详情的稳定机器码。
const (
	codeUninitialized   = "not_initialized"
	codeUnauthenticated = "unauthenticated"
	codeCSRFInvalid     = "csrf_invalid"
	codeInvalidInput    = "invalid_input"
	codeRateLimited     = "rate_limited"
	codeNotFound        = "not_found"
	codeInternalError   = "internal_error"
)

// 请求上下文键。
const (
	contextKeyRequestID = "requestID"
	contextKeySession   = "session"
)

// requestIDHeader 是响应中回显请求标识的头名。
const requestIDHeader = "X-Request-ID"

// problem 是 RFC 7807 风格的问题详情，媒体类型为 application/problem+json。
//
// detail 只承载可安全公开的中文说明，不含堆栈、SQL、文件路径或凭证（API 契约 §2）。
type problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	Code      string `json:"code"`
	RequestID string `json:"requestId"`
}

// requestIDMiddleware 为每个请求生成不透明标识，供日志与问题详情关联。
func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := newRequestID()
		if err != nil {
			// 随机数不可用是运行环境异常，标识退化为固定值也不影响可用性。
			id = "unknown"
		}
		c.Set(contextKeyRequestID, id)
		c.Header(requestIDHeader, id)
		c.Next()
	}
}

func newRequestID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

// requestID 取出当前请求标识；缺失时返回空串，不中断处理。
func requestID(c *gin.Context) string {
	value, exists := c.Get(contextKeyRequestID)
	if !exists {
		return ""
	}
	id, _ := value.(string)
	return id
}

// writeProblem 以 application/problem+json 写入错误响应。
func writeProblem(c *gin.Context, status int, code, title, detail string) {
	c.Header("Content-Type", "application/problem+json")
	c.JSON(status, problem{
		Type:      "https://jrp.invalid/problems/" + code,
		Title:     title,
		Status:    status,
		Detail:    detail,
		Code:      code,
		RequestID: requestID(c),
	})
}

// ErrSessionMissing 表示请求上下文缺少已认证的会话。
var ErrSessionMissing = errors.New("请求上下文缺少会话")

// currentSession 取出中间件写入的会话；缺失时返回哨兵错误。
func currentSession(c *gin.Context) (store.Session, error) {
	value, exists := c.Get(contextKeySession)
	if !exists {
		return store.Session{}, ErrSessionMissing
	}
	session, ok := value.(store.Session)
	if !ok {
		return store.Session{}, ErrSessionMissing
	}
	return session, nil
}
