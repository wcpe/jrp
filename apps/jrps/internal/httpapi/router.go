package httpapi

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jrp/apps/jrps/internal/buildinfo"
	"github.com/wcpe/jrp/apps/jrps/internal/webui"
)

const (
	assetCacheControl = "public, max-age=31536000, immutable"
	indexCacheControl = "no-cache"
)

type statusResponse struct {
	Status       string   `json:"status"`
	Version      string   `json:"version"`
	ProtocolID   string   `json:"protocol"`
	CoreBaseline string   `json:"core_baseline"`
	WireVersions []string `json:"wire_versions"`
}

func NewRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.GET("/healthz", statusHandler)
	router.GET("/readyz", statusHandler)
	registerSPA(router, webui.FileSystem())
	return router
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
			context.JSON(http.StatusNotFound, gin.H{"error": "资源不存在"})
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
