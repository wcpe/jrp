package httpapi

import (
	"io/fs"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jrp/apps/jrps/internal/buildinfo"
	"github.com/wcpe/jrp/apps/jrps/internal/store"
	"github.com/wcpe/jrp/apps/jrps/internal/webui"
)

const (
	assetCacheControl = "public, max-age=31536000, immutable"
	indexCacheControl = "no-cache"

	// timeRFC3339 是响应中使用的时间格式（API 契约 §1.2：RFC 3339 UTC）。
	timeRFC3339 = "2006-01-02T15:04:05Z07:00"
)

// RouterOptions 是构造管理路由所需的依赖。
type RouterOptions struct {
	// Store 是 jrps 私有 SQLite 入口；为空时所有管理端点按未初始化处理。
	Store *store.Store
	// InsecureCookies 仅在本地明文开发场景开启：它会关闭会话 Cookie 的 Secure 属性。
	// 生产默认必须使用 Secure，不得因开发便利削弱默认值（FR-02 规格 §6）。
	InsecureCookies bool
	// Logger 是外壳日志器；为空时静默。
	Logger *slog.Logger
}

type statusResponse struct {
	Status       string   `json:"status"`
	Version      string   `json:"version"`
	ProtocolID   string   `json:"protocol"`
	CoreBaseline string   `json:"core_baseline"`
	WireVersions []string `json:"wire_versions"`
}

// NewRouter 构造管理路由。
//
// /api/v1 是本工程首个管理端点组（FR-02）；后续 FR-15 通知与 FR-16 审计
// 只需在同一个 api 组上追加路由组，中间件挂载点已经就位。
func NewRouter(options RouterOptions) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(requestIDMiddleware())

	// 健康检查不要求管理员会话，也不受初始化状态影响。
	router.GET("/healthz", statusHandler)
	router.GET("/readyz", statusHandler)

	registerAPI(router, options)
	registerSPA(router, webui.FileSystem())
	return router
}

// registerAPI 注册 /api/v1 端点组与统一中间件挂载点。
//
// 就绪边界做成 /api 前缀的路由器级中间件：gin 对未注册路径不执行组中间件，
// 只有在路由匹配时才执行，因此仅靠组中间件会让未初始化的未知端点绕过守卫返回 404。
func registerAPI(router *gin.Engine, options RouterOptions) {
	session := newSessionAPI(options)
	guard := requireInitialized(options.Store)
	router.Use(func(context *gin.Context) {
		if strings.HasPrefix(context.Request.URL.Path, "/api") {
			guard(context)
			return
		}
		context.Next()
	})

	api := router.Group("/api/v1")
	api.POST("/session", session.login)
	api.GET("/session", session.authenticate(false), session.query)
	api.DELETE("/session", session.authenticate(true), session.logout)
}

func notImplementedHandler(context *gin.Context) {
	writeProblem(context, http.StatusNotFound, codeNotFound, "资源不存在", "请求的管理端点不存在")
}

func statusHandler(context *gin.Context) {
	info := buildinfo.Current()
	context.JSON(http.StatusOK, statusResponse{
		Status:       "ok",
		Version:      info.Version,
		ProtocolID:   info.ProtocolID,
		CoreBaseline: info.CoreBaseline,
		WireVersions: info.WireVersions,
	})
}

func registerSPA(router *gin.Engine, assets fs.FS) {
	router.NoRoute(func(context *gin.Context) {
		path := context.Request.URL.Path
		if context.Request.Method != http.MethodGet || isReservedPath(path) {
			writeProblem(context, http.StatusNotFound, codeNotFound, "资源不存在", "请求的资源不存在")
			return
		}

		assetPath := strings.TrimPrefix(path, "/")
		if assetPath != "" {
			if info, err := fs.Stat(assets, assetPath); err == nil && !info.IsDir() {
				if strings.HasPrefix(assetPath, "assets/") {
					context.Header("Cache-Control", assetCacheControl)
				}
				http.ServeFileFS(context.Writer, context.Request, assets, assetPath)
				return
			}
		}
		context.Header("Cache-Control", indexCacheControl)
		http.ServeFileFS(context.Writer, context.Request, assets, "index.html")
	})
}

func isReservedPath(path string) bool {
	for _, prefix := range []string{"/api", "/agent", "/healthz", "/readyz"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}
