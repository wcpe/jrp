//go:build webui

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

func TestProductionSPAAssets(t *testing.T) {
	// 本用例只验证生产 SPA 资源，不涉及管理端点，因此传空选项：
	// Store 为空时管理端点按未初始化处理，不影响静态资源断言。
	router := NewRouter(RouterOptions{})
	indexRecorder := httptest.NewRecorder()
	indexRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	router.ServeHTTP(indexRecorder, indexRequest)

	if indexRecorder.Code != http.StatusOK {
		t.Fatalf("生产 SPA 入口状态码不匹配：%d", indexRecorder.Code)
	}
	if indexRecorder.Header().Get("Cache-Control") != indexCacheControl {
		t.Fatalf("生产 SPA 入口缓存策略不匹配：%q", indexRecorder.Header().Get("Cache-Control"))
	}

	assetPattern := regexp.MustCompile(`(?:src|href)="(/assets/[^"]+)"`)
	matches := assetPattern.FindAllStringSubmatch(indexRecorder.Body.String(), -1)
	if len(matches) == 0 {
		t.Fatalf("生产 SPA 入口未引用哈希资源：%q", indexRecorder.Body.String())
	}

	visited := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		assetPath := match[1]
		if _, ok := visited[assetPath]; ok {
			continue
		}
		visited[assetPath] = struct{}{}

		assetRecorder := httptest.NewRecorder()
		assetRequest := httptest.NewRequest(http.MethodGet, assetPath, nil)
		router.ServeHTTP(assetRecorder, assetRequest)

		if assetRecorder.Code != http.StatusOK {
			t.Fatalf("生产资源 %s 状态码不匹配：%d", assetPath, assetRecorder.Code)
		}
		if assetRecorder.Header().Get("Cache-Control") != assetCacheControl {
			t.Fatalf("生产资源 %s 缓存策略不匹配：%q", assetPath, assetRecorder.Header().Get("Cache-Control"))
		}
	}
}
