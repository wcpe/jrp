package server

import (
	"encoding/json"
	"errors"
	"net/netip"
	"strings"

	"github.com/wcpe/jrp/core/internal/proxy"
	"github.com/wcpe/jrp/core/internal/transport"
	"github.com/wcpe/jrp/core/internal/wire"
)

// proxyOperationResponse 是 new-proxy 的响应载荷（官方形状）。
//
// 官方对端以「error 为空」判定成功，并从此读取远端地址；该响应不用于
// close-proxy——官方协议不为关闭定义响应消息。
type proxyOperationResponse struct {
	ProxyName  string `json:"proxy_name,omitempty"`
	RemoteAddr string `json:"remote_addr,omitempty"`
	Error      string `json:"error,omitempty"`
}

// runtimeProxyRequest 是 new-proxy 的请求载荷（P1 字段子集）。
//
// 字段名取自官方兼容消息族；官方注册载荷还包含域名、插件、压缩、限速等字段，
// 解析容忍未知字段——拒绝会让携带可选字段的合法客户端无法注册。
//
// 官方协议不携带本地目标地址：目标由客户端在其自身配置中决定，服务端不预知、
// 因此运行时代理没有「目标允许集合」这一约束（该约束只适用于以 desired 快照
// 声明的代理）。运行时代理的安全边界是 token 鉴权与会话归属校验。
type runtimeProxyRequest struct {
	ProxyName     string   `json:"proxy_name"`
	ProxyType     string   `json:"proxy_type"`
	RemotePort    int      `json:"remote_port"`
	CustomDomains []string `json:"custom_domains"`
	SubDomain     string   `json:"subdomain"`
	Locations     []string `json:"locations"`
}

// 稳定失败类别（FR-03 规格 §3.6）。
var (
	ErrProxyTypeUnsupported = errors.New("proxy_rejected: type_unsupported")
	ErrProxyNameConflict    = errors.New("proxy_rejected: name_conflict")
	ErrProxyPortConflict    = errors.New("proxy_rejected: port_conflict")
	ErrProxyFieldInvalid    = errors.New("proxy_rejected: field_invalid")
)

// P1 支持的运行时注册代理类型；之外一律可判定拒绝。
var runtimeProxyTypes = map[string]bool{
	"tcp":   true,
	"udp":   true,
	"http":  true,
	"https": true,
}

// handleNewProxy 处理 new-proxy 消息：运行时注册一个代理。
//
// 四段校验顺序固定（FR-06a §3.2）：字段 → 权限 → 端口/名称冲突 → P1 范围。
// 全部通过后创建入口监听器并登记到当前代——成功响应只在资源可用后返回，
// 失败不得留下半注册监听器。
//
// 运行时注册的代理是会话级的：不写 desired、不参与快照换代；控制会话结束时
// 由代清理路径统一释放。关闭走 close-proxy 消息。
func (engine *Engine) handleNewProxy(gen *generation, session sessionWriter, clientID string, payload []byte) {
	var request runtimeProxyRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		engine.writeProxyResponse(session, proxyOperationResponse{Error: ErrProxyFieldInvalid.Error()})
		return
	}
	engine.log().Info("收到代理注册请求", "客户端", clientID,
		"代理", request.ProxyName, "类型", request.ProxyType, "远端端口", request.RemotePort)
	if request.ProxyName == "" {
		engine.writeProxyResponse(session, proxyOperationResponse{ProxyName: request.ProxyName, Error: ErrProxyFieldInvalid.Error()})
		return
	}
	if !runtimeProxyTypes[request.ProxyType] {
		engine.writeProxyResponse(session, proxyOperationResponse{ProxyName: request.ProxyName, Error: ErrProxyTypeUnsupported.Error()})
		return
	}
	if request.ProxyType != "http" && request.ProxyType != "https" &&
		(request.RemotePort <= 0 || request.RemotePort > 65535) {
		engine.writeProxyResponse(session, proxyOperationResponse{ProxyName: request.ProxyName, Error: ErrProxyFieldInvalid.Error()})
		return
	}
	routes, routeErr := runtimeHTTPRoutes(request)
	if routeErr != nil {
		engine.writeProxyResponse(session, proxyOperationResponse{ProxyName: request.ProxyName, Error: ErrProxyFieldInvalid.Error()})
		return
	}
	if request.ProxyType == "http" {
		engine.log().Info("运行时 HTTP 路由已解析", "代理", request.ProxyName, "路由数", len(routes))
	}
	engine.mu.Lock()
	conflictErr := engine.checkRuntimeProxyConflicts(gen, clientID, request, routes)
	engine.mu.Unlock()
	if conflictErr != nil {
		engine.writeProxyResponse(session, proxyOperationResponse{ProxyName: request.ProxyName, Error: conflictErr.Error()})
		return
	}

	listener, udpEntry, entryName, created, openErr := engine.openRuntimeProxy(gen, request, session.version)
	if openErr != nil {
		engine.writeProxyResponse(session, proxyOperationResponse{ProxyName: request.ProxyName, Error: ErrProxyPortConflict.Error()})
		return
	}
	if listener != nil && (request.ProxyType == "http" || request.ProxyType == "https") && request.RemotePort == 0 {
		address, parseErr := netip.ParseAddrPort(listener.Addr().String())
		if parseErr != nil {
			releaseRuntimeProxyResource(listener, udpEntry, created)
			engine.writeProxyResponse(session, proxyOperationResponse{ProxyName: request.ProxyName, Error: ErrProxyFieldInvalid.Error()})
			return
		}
		request.RemotePort = int(address.Port())
	}
	engine.mu.Lock()
	conflictErr = engine.checkRuntimeProxyConflicts(gen, clientID, request, routes)
	var discardListener *transport.Listener
	if conflictErr == nil && request.ProxyType == "http" {
		if existing := gen.guestLns[entryName]; existing != nil && existing != listener {
			discardListener = listener
			listener = existing
			created = false
		}
	}
	if conflictErr != nil {
		engine.mu.Unlock()
		releaseRuntimeProxyResource(listener, udpEntry, created)
		engine.writeProxyResponse(session, proxyOperationResponse{ProxyName: request.ProxyName, Error: conflictErr.Error()})
		return
	}
	merged := make(proxy.Registry)
	for name, binding := range gen.engine.registry.Current() {
		merged[name] = binding
	}
	merged[request.ProxyName] = &proxy.Binding{
		Name:                request.ProxyName,
		OwnerClientID:       clientID,
		UnrestrictedTargets: true,
	}
	gen.runtimeProxies[request.ProxyName] = &runtimeProxy{
		name:       request.ProxyName,
		clientID:   clientID,
		ownerConn:  session.conn,
		proxyType:  request.ProxyType,
		entryName:  entryName,
		listener:   listener,
		udpEntry:   udpEntry,
		httpRoutes: routes,
	}
	if request.ProxyType == "http" && gen.guestLns[entryName] == nil {
		gen.guestLns[entryName] = listener
		gen.guestAddr[entryName] = listener.Addr()
	}
	if request.ProxyType == "http" {
		gen.guestAddr[request.ProxyName] = listener.Addr()
	} else if udpEntry != nil {
		gen.guestAddr[request.ProxyName] = udpEntry.Addr()
	} else {
		gen.guestLns[request.ProxyName] = listener
		gen.guestAddr[request.ProxyName] = listener.Addr()
	}
	gen.engine.registry.Publish(merged)
	gen.rebuildHTTPRoutesLocked()
	engine.mu.Unlock()
	if discardListener != nil {
		_ = discardListener.Release()
	}

	if created && listener != nil {
		gen.acceptWG.Add(1)
		if request.ProxyType == "http" {
			go engine.serveHTTPGuest(gen, request.RemotePort, listener)
		} else {
			go engine.serveGuest(gen, request.ProxyName, listener)
		}
	} else if created && udpEntry != nil {
		gen.acceptWG.Add(1)
		go engine.serveUDPEntry(gen, request.ProxyName, udpEntry)
	}
	addr := gen.engine.GuestAddr(request.ProxyName)
	engine.writeProxyResponse(session, proxyOperationResponse{ProxyName: request.ProxyName, RemoteAddr: addr.String()})
	engine.log().Info("运行时代理已注册", "代理", request.ProxyName, "客户端", clientID, "入口", addr.String())
}

// openRuntimeProxy 创建运行时代理所需的数据面资源；网络 IO 在引擎锁外执行。
func (engine *Engine) openRuntimeProxy(gen *generation, request runtimeProxyRequest, version wire.Version) (*transport.Listener, *proxy.UDPProxy, string, bool, error) {
	if request.ProxyType == "udp" {
		entry, err := engine.openUDPEntryWithProtocol(gen.config, request.ProxyName, request.RemotePort, true, version)
		return nil, entry, request.ProxyName, true, err
	}
	entryName := request.ProxyName
	if request.ProxyType == "http" {
		if request.RemotePort > 0 {
			entryName = httpEntryName(request.RemotePort)
			engine.mu.Lock()
			existing := gen.guestLns[entryName]
			engine.mu.Unlock()
			if existing != nil {
				return existing, nil, entryName, false, nil
			}
		} else {
			engine.mu.Lock()
			for _, runtime := range gen.runtimeProxies {
				if runtime.proxyType == "http" {
					entryName = runtime.entryName
					if existing := gen.guestLns[entryName]; existing != nil {
						engine.mu.Unlock()
						return existing, nil, entryName, false, nil
					}
					break
				}
			}
			engine.mu.Unlock()
		}
	}
	entry, err := engine.listenGuest(gen.config, request.RemotePort)
	if err != nil {
		return nil, nil, entryName, true, err
	}
	if request.ProxyType == "http" && request.RemotePort == 0 {
		address, parseErr := netip.ParseAddrPort(entry.Addr().String())
		if parseErr != nil {
			_ = entry.Release()
			return nil, nil, entryName, true, parseErr
		}
		entryName = httpEntryName(int(address.Port()))
	}
	return entry, nil, entryName, true, nil
}

// releaseRuntimeProxyResource 释放本次创建且未登记的运行时资源。
func releaseRuntimeProxyResource(listener *transport.Listener, udpEntry *proxy.UDPProxy, created bool) {
	if !created {
		return
	}
	if udpEntry != nil {
		_ = udpEntry.Close()
		return
	}
	if listener != nil {
		_ = listener.Release()
	}
}

// runtimeHTTPRoutes 解析官方 HTTP 注册载荷中的主机与路径字段。
func runtimeHTTPRoutes(request runtimeProxyRequest) ([]proxy.HTTPRoute, error) {
	if request.ProxyType != "http" {
		return nil, nil
	}
	hosts := append([]string(nil), request.CustomDomains...)
	if request.SubDomain != "" {
		hosts = append(hosts, request.SubDomain)
	}
	paths := append([]string(nil), request.Locations...)
	if len(paths) == 0 {
		paths = []string{"/"}
	}
	if len(hosts) == 0 {
		return nil, ErrProxyFieldInvalid
	}
	routes := make([]proxy.HTTPRoute, 0, len(hosts)*len(paths))
	for _, host := range hosts {
		host = strings.TrimSpace(host)
		if host == "" {
			return nil, ErrProxyFieldInvalid
		}
		for _, path := range paths {
			path = strings.TrimSpace(path)
			if path == "" {
				path = "/"
			}
			if !strings.HasPrefix(path, "/") {
				return nil, ErrProxyFieldInvalid
			}
			routes = append(routes, proxy.HTTPRoute{Proxy: request.ProxyName, Host: host, Path: path})
		}
	}
	return routes, nil
}

// rebuildHTTPRoutesLocked 重建配置与运行时代理合并后的共享 HTTP 路由表。
func (gen *generation) rebuildHTTPRoutesLocked() {
	grouped := make(map[int][]proxy.HTTPRoute)
	for _, binding := range gen.config.HTTPBindings() {
		for _, host := range binding.Hosts {
			grouped[binding.RemotePort] = append(grouped[binding.RemotePort], proxy.HTTPRoute{
				Proxy: binding.Name,
				Host:  host,
				Path:  binding.Path,
			})
		}
	}
	for _, runtime := range gen.runtimeProxies {
		if runtime.proxyType != "http" {
			continue
		}
		port, ok := httpEntryPort(runtime.entryName)
		if !ok {
			continue
		}
		grouped[port] = append(grouped[port], runtime.httpRoutes...)
	}
	routes := make(map[int]*proxy.HTTPRouter, len(grouped))
	for port, items := range grouped {
		router, err := proxy.NewHTTPRouter(items)
		if err != nil {
			continue
		}
		routes[port] = router
	}
	gen.engine.httpRoutes = routes
}

// handleCloseProxy 处理 close-proxy 消息：停止入口接收并清理登记。
//
// 活动连接按代排水语义自然结束；只允许代理属主关闭自己的代理。
//
// 官方协议不为 close-proxy 定义响应：这里只在服务端侧清理，不向对端回写任何帧
// ——官方 frpc 的读循环遇到预期之外的消息会报错，回写响应会直接打断它的会话。
func (engine *Engine) handleCloseProxy(gen *generation, clientID string, payload []byte) {
	var request struct {
		ProxyName string `json:"proxy_name"`
	}
	if err := json.Unmarshal(payload, &request); err != nil || request.ProxyName == "" {
		engine.log().Warn("关闭代理载荷非法", "客户端", clientID)
		return
	}

	engine.mu.Lock()
	proxyEntry, ok := gen.runtimeProxies[request.ProxyName]
	if !ok || proxyEntry.clientID != clientID {
		engine.mu.Unlock()
		engine.log().Warn("关闭代理被拒绝：代理不存在或不属于该客户端", "代理", request.ProxyName, "客户端", clientID)
		return
	}
	delete(gen.runtimeProxies, request.ProxyName)
	delete(gen.guestAddr, request.ProxyName)
	listener := proxyEntry.listener
	udpEntry := proxyEntry.udpEntry
	removeShared := proxyEntry.proxyType == "http" && !gen.hasHTTPEntryUsers(proxyEntry.entryName)
	if proxyEntry.proxyType != "http" {
		delete(gen.guestLns, request.ProxyName)
	}
	if removeShared {
		delete(gen.guestLns, proxyEntry.entryName)
		delete(gen.guestAddr, proxyEntry.entryName)
	}
	merged := make(proxy.Registry)
	for name, binding := range gen.engine.registry.Current() {
		if name != request.ProxyName {
			merged[name] = binding
		}
	}
	gen.engine.registry.Publish(merged)
	gen.rebuildHTTPRoutesLocked()
	engine.mu.Unlock()

	if udpEntry != nil {
		_ = udpEntry.Close()
	} else if removeShared || proxyEntry.proxyType != "http" {
		_ = listener.Release()
	}
	engine.log().Info("运行时代理已关闭", "代理", request.ProxyName, "客户端", clientID)
}

// checkRuntimeProxyConflicts 校验名称、端口与 HTTP 路由的运行期冲突。
// 调用方持有 engine.mu；本函数不执行网络 IO。
func (engine *Engine) checkRuntimeProxyConflicts(gen *generation, clientID string, request runtimeProxyRequest, routes []proxy.HTTPRoute) error {
	current := gen.engine.registry.Current()
	if _, exists := current[request.ProxyName]; exists {
		return ErrProxyNameConflict
	}
	for name, addr := range gen.guestAddr {
		resolved, err := netip.ParseAddrPort(addr.String())
		if err != nil || resolved.Port() != uint16(request.RemotePort) {
			continue
		}
		if request.ProxyType == "http" && (strings.HasPrefix(name, httpEntryPrefix) || gen.isHTTPProxyName(name)) {
			continue
		}
		return ErrProxyPortConflict
	}
	if request.ProxyType == "http" {
		grouped := make([]proxy.HTTPRoute, 0, len(routes))
		for _, binding := range gen.config.HTTPBindings() {
			if binding.RemotePort != request.RemotePort {
				continue
			}
			for _, host := range binding.Hosts {
				grouped = append(grouped, proxy.HTTPRoute{Proxy: binding.Name, Host: host, Path: binding.Path})
			}
		}
		for _, runtime := range gen.runtimeProxies {
			if runtime.proxyType != "http" {
				continue
			}
			port, ok := httpEntryPort(runtime.entryName)
			if ok && port == request.RemotePort {
				grouped = append(grouped, runtime.httpRoutes...)
			}
		}
		grouped = append(grouped, routes...)
		if _, err := proxy.NewHTTPRouter(grouped); err != nil {
			return ErrProxyPortConflict
		}
	}
	_ = clientID
	return nil
}

// isHTTPProxyName 判断入口登记名是否属于 HTTP 代理而非共享入口。
func (gen *generation) isHTTPProxyName(name string) bool {
	for _, binding := range gen.config.HTTPBindings() {
		if binding.Name == name {
			return true
		}
	}
	if entry := gen.runtimeProxies[name]; entry != nil {
		return entry.proxyType == "http"
	}
	return false
}

// ownsGuestListener 判断监听器是否仍由当前代登记。
func (gen *generation) ownsGuestListener(listener *transport.Listener) bool {
	gen.engine.mu.Lock()
	defer gen.engine.mu.Unlock()
	for _, current := range gen.guestLns {
		if current == listener {
			return true
		}
	}
	return false
}

// hasHTTPEntryUsers 判断共享 HTTP 入口是否仍被配置或运行时代理使用。
func (gen *generation) hasHTTPEntryUsers(entryName string) bool {
	port, ok := httpEntryPort(entryName)
	if !ok {
		return false
	}
	for _, binding := range gen.config.HTTPBindings() {
		if binding.RemotePort == port {
			return true
		}
	}
	for _, runtime := range gen.runtimeProxies {
		if runtime.proxyType == "http" && runtime.entryName == entryName {
			return true
		}
	}
	return false
}

// writeProxyResponse 写出代理操作响应帧。
//
// 写入失败被有意忽略：响应写不出去意味着会话即将被读取路径判定为失败并关闭，
// 这里不再叠加一次会话关闭动作。编码错误同理，只可能是内部缺陷而非对端输入。
func (engine *Engine) writeProxyResponse(session sessionWriter, response proxyOperationResponse) {
	if response.Error != "" {
		// 拒绝必须在服务端留痕：黑盒验收要求"冲突返回明确错误"，而错误只在响应体
		// 里时，服务端日志无从证明它确实按类别拒绝了。类别本身不含敏感材料。
		// 类别并入消息文本：日志查询按消息检索，类别只放结构化字段时外部
		// 观测不到"按什么类别拒绝"。
		engine.log().Info("代理注册被拒："+response.Error, "代理", response.ProxyName)
	}
	body, err := json.Marshal(response)
	if err != nil {
		return
	}
	_ = session.writeMessage(wire.MessageTypeNewProxyResponse, body)
}

// runtimeProxy 是一条运行时注册代理的会话级登记。
//
// ownerConn 是注册它的控制连接：清理按连接归属进行（不是按 clientID）——
// 同一客户端的新会话接管时，旧会话的清理绝不能误伤新会话刚注册的代理。
type runtimeProxy struct {
	name       string
	clientID   string
	ownerConn  *transport.Conn
	proxyType  string
	entryName  string
	listener   *transport.Listener
	udpEntry   *proxy.UDPProxy
	httpRoutes []proxy.HTTPRoute
}

// cleanupRuntimeProxiesForConn 释放某条控制会话注册的全部运行时代理。
//
// 会话结束（正常断开、心跳失活或被新会话替换）时由 handleControl 的 defer
// 调用；只清理本连接注册的代理，其他会话不受影响（FR-03 §3.4/§3.5）。
func (gen *generation) cleanupRuntimeProxiesForConn(conn *transport.Conn) {
	gen.engine.mu.Lock()
	entries := make([]*runtimeProxy, 0)
	for _, entry := range gen.runtimeProxies {
		if entry.ownerConn == conn {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		gen.engine.mu.Unlock()
		return
	}
	listeners := make(map[*transport.Listener]bool)
	udpEntries := make([]*proxy.UDPProxy, 0)
	for _, entry := range entries {
		delete(gen.runtimeProxies, entry.name)
		delete(gen.guestAddr, entry.name)
		if entry.udpEntry != nil {
			udpEntries = append(udpEntries, entry.udpEntry)
			continue
		}
		if entry.proxyType == "http" {
			if !gen.hasHTTPEntryUsers(entry.entryName) {
				delete(gen.guestLns, entry.entryName)
				delete(gen.guestAddr, entry.entryName)
				listeners[entry.listener] = true
			}
			continue
		}
		delete(gen.guestLns, entry.name)
		listeners[entry.listener] = true
	}
	merged := make(proxy.Registry)
	for name, binding := range gen.engine.registry.Current() {
		if _, runtime := gen.runtimeProxies[name]; !runtime {
			merged[name] = binding
		}
	}
	gen.engine.registry.Publish(merged)
	gen.rebuildHTTPRoutesLocked()
	gen.engine.mu.Unlock()

	for _, entry := range udpEntries {
		_ = entry.Close()
	}
	for listener := range listeners {
		if listener != nil {
			_ = listener.Release()
		}
	}
	gen.engine.log().Info("会话结束，运行时代理已清理", "数量", len(entries))
}
