package proxy

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// errRouteInvalid 表示路由项字段非法：缺少代理名、主机名或路径前缀。
var errRouteInvalid = errors.New("HTTP 路由项字段非法")

// errRouteConflict 表示同一主机名与路径前缀组合重复，对应注册校验的冲突环节。
var errRouteConflict = errors.New("HTTP 路由的主机名与路径组合冲突")

// HTTPRoute 是一条 HTTP 路由：主机名 + 路径前缀决定目标代理。
//
// 匹配规则固定为「主机名精确匹配（不区分大小写）+ 路径最长前缀匹配」：
// 这是规格 §3.5 正文写明的两维，不引入通配或正则，也不承载 P2 的其他参数。
type HTTPRoute struct {
	// Proxy 是命中的代理名。
	Proxy string
	// Host 是主机名，精确匹配。
	Host string
	// Path 是路径前缀，必须为空或以 / 开头；空等同 /。
	Path string
}

// HTTPRouter 是一张按主机与路径选择代理的路由表。
//
// 表构造后不可变：多代理共享同一入口端口时路由只随配置快照整体替换，
// 支持 prepare/publish 的原子发布（ADR-0005）。
type HTTPRouter struct {
	routes []HTTPRoute
}

// NewHTTPRouter 构造路由表，按固定顺序校验全部路由项。
//
// 校验两级：先字段合法性，再主机名与路径组合冲突；任一失败返回错误且不产出
// 半成品路由表，对应注册校验「失败不留半注册资源」。
func NewHTTPRouter(routes []HTTPRoute) (*HTTPRouter, error) {
	normalized := make([]HTTPRoute, 0, len(routes))
	for _, route := range routes {
		item, err := normalizeRoute(route)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, item)
	}
	if err := detectRouteConflict(normalized); err != nil {
		return nil, err
	}
	sort.SliceStable(normalized, func(left, right int) bool {
		return len(normalized[left].Path) > len(normalized[right].Path)
	})
	return &HTTPRouter{routes: normalized}, nil
}

// normalizeRoute 校验并规整单条路由：主机名小写、空路径归一为 /。
func normalizeRoute(route HTTPRoute) (HTTPRoute, error) {
	if route.Proxy == "" {
		return HTTPRoute{}, fmt.Errorf("%w：缺少代理名", errRouteInvalid)
	}
	if route.Host == "" {
		return HTTPRoute{}, fmt.Errorf("%w：缺少主机名", errRouteInvalid)
	}
	if route.Path == "" {
		return HTTPRoute{Proxy: route.Proxy, Host: strings.ToLower(route.Host), Path: "/"}, nil
	}
	if !strings.HasPrefix(route.Path, "/") {
		return HTTPRoute{}, fmt.Errorf("%w：路径前缀必须以 / 开头", errRouteInvalid)
	}
	return HTTPRoute{Proxy: route.Proxy, Host: strings.ToLower(route.Host), Path: route.Path}, nil
}

// detectRouteConflict 检测主机名与路径前缀的重复组合。
//
// 键用 NUL 分隔而非直接拼接：主机名与路径都允许含 `/`，直接相加会让不同的组合
// 产生同一个键——例如 `a/b.example.com` + `/c` 与 `a` + `/b.example.com/c`。
// 这类误判会让构造路由表失败，而该端口的全部请求随之一律 404（静默失去路由）。
// NUL 不可能出现在主机名或路径中，因此它不参与组合歧义。
func detectRouteConflict(routes []HTTPRoute) error {
	seen := make(map[string]bool, len(routes))
	for _, route := range routes {
		key := route.Host + "\x00" + route.Path
		if seen[key] {
			return fmt.Errorf("主机 %s 路径 %s 的%w", route.Host, route.Path, errRouteConflict)
		}
		seen[key] = true
	}
	return nil
}

// Select 按主机名与路径选择目标代理；无匹配时返回假。
//
// 按路径长度降序查找，因此命中的必是最长前缀；未命中不回显路由表内容，
// 调用方只得到「无匹配」这一结论（规格 §3.5）。
func (router *HTTPRouter) Select(host, path string) (string, bool) {
	host = strings.ToLower(host)
	if path == "" {
		path = "/"
	}
	for _, route := range router.routes {
		if route.Host != host {
			continue
		}
		if matchesPathPrefix(path, route.Path) {
			return route.Proxy, true
		}
	}
	return "", false
}

// matchesPathPrefix 判定路径是否落在给定前缀下。
//
// 前缀 / 匹配一切；其余前缀要求完整段边界，避免 /api 命中 /apiext。
func matchesPathPrefix(path, prefix string) bool {
	if prefix == "/" {
		return true
	}
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	return len(path) == len(prefix) || path[len(prefix)] == '/'
}
