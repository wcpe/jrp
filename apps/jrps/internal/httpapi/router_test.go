package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthAndReadiness(t *testing.T) {
	router := NewRouter()

	for _, path := range []string{"/healthz", "/readyz"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		router.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("%s 状态码不匹配：%d", path, recorder.Code)
		}

		var response statusResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("%s 响应不是有效 JSON：%v", path, err)
		}
		if response.Status != "ok" || response.Version != "0.1.0" || response.ProtocolID != "jrp" {
			t.Fatalf("%s 健康信息不匹配：%+v", path, response)
		}
		if response.CoreBaseline != "jrp@e1f1d6ab1b591ec9b596f4b1b6a81cd2960c6314" {
			t.Fatalf("%s Core 基线不匹配：%s", path, response.CoreBaseline)
		}
	}
}

func TestSPAFallback(t *testing.T) {
	router := NewRouter()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/dashboard/proxies", nil)

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("SPA fallback 状态码不匹配：%d", recorder.Code)
	}
	if !strings.Contains(strings.ToLower(recorder.Body.String()), "<html") {
		t.Fatalf("SPA fallback 未返回 HTML：%q", recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != indexCacheControl {
		t.Fatalf("SPA 入口缓存策略不匹配：%q", recorder.Header().Get("Cache-Control"))
	}
}

func TestSPAFallbackDoesNotHandleReservedPaths(t *testing.T) {
	router := NewRouter()

	for _, path := range []string{"/api", "/api/proxies", "/agent", "/agent/v1/connect", "/healthz/missing", "/readyz/missing"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		router.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s 应返回 404，实际为 %d", path, recorder.Code)
		}
		if strings.Contains(strings.ToLower(recorder.Body.String()), "<html") {
			t.Fatalf("%s 被 SPA fallback 吞掉", path)
		}
	}
}
