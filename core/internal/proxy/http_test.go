package proxy_test

import (
	"testing"

	"github.com/wcpe/jrp/core/internal/proxy"
)

// 本文件覆盖 FR-06a §3.5 的 HTTP 路由：主机名精确匹配 + 路径最长前缀匹配，
// 无匹配回执不回显内部路由表。先把路由表做成活跃 capillary：环境影响最小化。

// TestHTTPRouterSelectsLongestPrefix 覆盖主机名精确匹配与路径最长前缀匹配。
func TestHTTPRouterSelectsLongestPrefix(t *testing.T) {
	router, err := proxy.NewHTTPRouter([]proxy.HTTPRoute{
		{Proxy: "app-root", Host: "app.example.com", Path: "/"},
		{Proxy: "app-api", Host: "app.example.com", Path: "/api"},
		{Proxy: "other", Host: "other.example.com", Path: "/"},
	})
	if err != nil {
		t.Fatalf("构造路由表失败：%v", err)
	}

	cases := []struct {
		host string
		path string
		want string
	}{
		{host: "app.example.com", path: "/", want: "app-root"},
		{host: "app.example.com", path: "/api", want: "app-api"},
		{host: "app.example.com", path: "/api/v1/users", want: "app-api"},
		{host: "app.example.com", path: "/apiext", want: "app-root"},
		{host: "other.example.com", path: "/", want: "other"},
		{host: "APP.example.com", path: "/", want: "app-root"},
	}
	for _, item := range cases {
		matched, ok := router.Select(item.host, item.path)
		if !ok {
			t.Fatalf("主机 %s 路径 %s 应当匹配到代理，实际未匹配", item.host, item.path)
		}
		if matched != item.want {
			t.Fatalf("主机 %s 路径 %s 期望选到 %s，实际 %s", item.host, item.path, item.want, matched)
		}
	}
}

// TestHTTPRouterUnmatched 覆盖无匹配：返回明确失败，不回显内部路由表。
func TestHTTPRouterUnmatched(t *testing.T) {
	router, err := proxy.NewHTTPRouter([]proxy.HTTPRoute{
		{Proxy: "secret-admin", Host: "internal.example.com", Path: "/admin"},
	})
	if err != nil {
		t.Fatalf("构造路由表失败：%v", err)
	}
	if _, ok := router.Select("unknown.example.com", "/"); ok {
		t.Fatalf("未知主机不应匹配到代理")
	}
	if _, ok := router.Select("internal.example.com", "/"); ok {
		t.Fatalf("路径前缀不符时不应匹配到代理")
	}
}

// TestHTTPRouterRejectsDuplicate 覆盖路由冲突：同一主机与路径组合重复即拒绝构造。
func TestHTTPRouterRejectsDuplicate(t *testing.T) {
	if _, err := proxy.NewHTTPRouter([]proxy.HTTPRoute{
		{Proxy: "first", Host: "app.example.com", Path: "/api"},
		{Proxy: "second", Host: "app.example.com", Path: "/api"},
	}); err == nil {
		t.Fatalf("主机与路径组合冲突应当返回错误")
	}
}

// TestHTTPRouterRequiresHostAndPath 覆盖路由项的字段校验：主机名与路径前缀必填。
func TestHTTPRouterRequiresHostAndPath(t *testing.T) {
	if _, err := proxy.NewHTTPRouter([]proxy.HTTPRoute{{Proxy: "app", Path: "/"}}); err == nil {
		t.Fatalf("缺少主机名应当返回错误")
	}
	if _, err := proxy.NewHTTPRouter([]proxy.HTTPRoute{{Host: "app.example.com", Path: "/"}}); err == nil {
		t.Fatalf("缺少代理名应当返回错误")
	}
	if _, err := proxy.NewHTTPRouter([]proxy.HTTPRoute{
		{Proxy: "app", Host: "app.example.com", Path: "api"},
	}); err == nil {
		t.Fatalf("路径前缀必须以 / 开头")
	}
}

// TestHTTPRouterEmptyPathMatchesAll 覆盖空路径前缀：等同根前缀，匹配该主机下全部路径。
func TestHTTPRouterEmptyPathMatchesAll(t *testing.T) {
	router, err := proxy.NewHTTPRouter([]proxy.HTTPRoute{{Proxy: "app", Host: "app.example.com"}})
	if err != nil {
		t.Fatalf("构造路由表失败：%v", err)
	}
	matched, ok := router.Select("app.example.com", "/anything/deep")
	if !ok || matched != "app" {
		t.Fatalf("空路径前缀应当等同根前缀：命中 %s 成功 %v", matched, ok)
	}
}

// TestHTTPRouterSamePathDifferentHosts 覆盖多主机共存：同一路径在不同主机下互不干扰。
func TestHTTPRouterSamePathDifferentHosts(t *testing.T) {
	router, err := proxy.NewHTTPRouter([]proxy.HTTPRoute{
		{Proxy: "tenant-a", Host: "a.example.com", Path: "/api"},
		{Proxy: "tenant-b", Host: "b.example.com", Path: "/api"},
	})
	if err != nil {
		t.Fatalf("构造路由表失败：%v", err)
	}
	matched, ok := router.Select("b.example.com", "/api")
	if !ok || matched != "tenant-b" {
		t.Fatalf("同路径不同主机应当各自命中：命中 %s 成功 %v", matched, ok)
	}
}

// 键拼接不得让不同的主机名与路径组合被判为冲突。
//
// 回归用例：冲突检测的键是 `主机名 + 路径` 直接相加，而两者都允许含 `/`，
// 于是 `a/b.example.com` + `/c` 与 `a` + `/b.example.com/c` 产生同一个键。
// 这类误判会让路由表构造失败，而该端口的全部请求随之一律 404——配置校验
// 判定合法、运行期却静默失去路由。
func TestHTTPRouterAcceptsAmbiguousLookingCombinations(t *testing.T) {
	if _, err := proxy.NewHTTPRouter([]proxy.HTTPRoute{
		{Proxy: "first", Host: "a/b.example.com", Path: "/c"},
		{Proxy: "second", Host: "a", Path: "/b.example.com/c"},
	}); err != nil {
		t.Fatalf("这两条路由的主机与路径组合不同，不应被判为冲突：%v", err)
	}
}
