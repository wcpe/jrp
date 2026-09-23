package server

import (
	"encoding/json"
	"errors"
	"net/netip"

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
	ProxyName  string `json:"proxy_name"`
	ProxyType  string `json:"proxy_type"`
	RemotePort int    `json:"remote_port"`
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

	// ── 第一段：字段完整性 ────────────────────────────────
	if request.ProxyName == "" || request.RemotePort <= 0 || request.RemotePort > 65535 {
		engine.writeProxyResponse(session, proxyOperationResponse{ProxyName: request.ProxyName, Error: ErrProxyFieldInvalid.Error()})
		return
	}

	// ── 第二段：权限（P2 类型在此拒绝）────────────────────
	if !runtimeProxyTypes[request.ProxyType] {
		engine.writeProxyResponse(session, proxyOperationResponse{ProxyName: request.ProxyName, Error: ErrProxyTypeUnsupported.Error()})
		return
	}

	// ── 第三段：冲突（名称与端口）─────────────────────────
	if err := engine.checkRuntimeProxyConflicts(gen, clientID, request); err != nil {
		engine.writeProxyResponse(session, proxyOperationResponse{ProxyName: request.ProxyName, Error: err.Error()})
		return
	}

	// ── 第四段：创建数据面资源 ────────────────────────────
	entry, openErr := engine.listenGuest(gen.config, request.RemotePort)
	if openErr != nil {
		engine.writeProxyResponse(session, proxyOperationResponse{ProxyName: request.ProxyName, Error: ErrProxyPortConflict.Error()})
		return
	}

	engine.mu.Lock()
	// 注册表增量并入：工作连接的归属与目标校验立即生效。
	merged := make(proxy.Registry)
	current := gen.engine.registry.Current()
	for name, binding := range current {
		merged[name] = binding
	}
	merged[request.ProxyName] = &proxy.Binding{
		Name:          request.ProxyName,
		OwnerClientID: clientID,
		// 官方协议不携带本地目标地址，服务端不预知目标：运行时代理的目标
		// 不受服务端约束（快照声明的代理仍按 Targets 校验）。
		UnrestrictedTargets: true,
	}
	gen.runtimeProxies[request.ProxyName] = &runtimeProxy{
		name:      request.ProxyName,
		clientID:  clientID,
		ownerConn: session.conn,
		listener:  entry,
	}
	gen.guestLns[request.ProxyName] = entry
	gen.guestAddr[request.ProxyName] = entry.Addr()
	gen.engine.registry.Publish(merged)
	engine.mu.Unlock()

	// 接入循环独立启动：动态注册的入口与快照入口共用同一条访客服务路径。
	gen.acceptWG.Add(1)
	go engine.serveGuest(gen, request.ProxyName, entry)

	engine.writeProxyResponse(session, proxyOperationResponse{ProxyName: request.ProxyName, RemoteAddr: entry.Addr().String()})
	engine.log().Info("运行时代理已注册", "代理", request.ProxyName, "客户端", clientID, "入口", entry.Addr().String())
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
	listener := proxyEntry.listener
	delete(gen.runtimeProxies, request.ProxyName)
	delete(gen.guestLns, request.ProxyName)
	delete(gen.guestAddr, request.ProxyName)

	merged := make(proxy.Registry)
	for name, binding := range gen.engine.registry.Current() {
		if name != request.ProxyName {
			merged[name] = binding
		}
	}
	gen.engine.registry.Publish(merged)
	engine.mu.Unlock()

	// 停止接收：与 stopAccepting 同一语义，但不走代的整体排水。
	_ = listener.Release()

	engine.log().Info("运行时代理已关闭", "代理", request.ProxyName, "客户端", clientID)
}

// checkRuntimeProxyConflicts 校验名称与端口的运行期冲突。
func (engine *Engine) checkRuntimeProxyConflicts(gen *generation, clientID string, request runtimeProxyRequest) error {
	current := gen.engine.registry.Current()
	if _, exists := current[request.ProxyName]; exists {
		return ErrProxyNameConflict
	}
	// 端口冲突：检查当前代所有入口的绑定端口（快照入口 + 运行时入口）。
	for _, addr := range gen.guestAddr {
		if resolved, err := netip.ParseAddrPort(addr.String()); err == nil && resolved.Port() == uint16(request.RemotePort) {
			return ErrProxyPortConflict
		}
	}
	_ = clientID
	return nil
}

// writeProxyResponse 写出代理操作响应帧。
//
// 写入失败被有意忽略：响应写不出去意味着会话即将被读取路径判定为失败并关闭，
// 这里不再叠加一次会话关闭动作。编码错误同理，只可能是内部缺陷而非对端输入。
func (engine *Engine) writeProxyResponse(session sessionWriter, response proxyOperationResponse) {
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
	name      string
	clientID  string
	ownerConn *transport.Conn
	listener  *transport.Listener
}

// cleanupRuntimeProxiesForConn 释放某条控制会话注册的全部运行时代理。
//
// 会话结束（正常断开、心跳失活或被新会话替换）时由 handleControl 的 defer
// 调用；只清理本连接注册的代理，其他会话不受影响（FR-03 §3.4/§3.5）。
func (gen *generation) cleanupRuntimeProxiesForConn(conn *transport.Conn) {
	gen.engine.mu.Lock()
	names := make([]string, 0, len(gen.runtimeProxies))
	for name, entry := range gen.runtimeProxies {
		if entry.ownerConn != conn {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		gen.engine.mu.Unlock()
		return
	}
	merged := make(proxy.Registry)
	for name, binding := range gen.engine.registry.Current() {
		if _, runtime := gen.runtimeProxies[name]; !runtime {
			merged[name] = binding
		}
	}
	for _, name := range names {
		_ = gen.runtimeProxies[name].listener.Release()
		delete(gen.runtimeProxies, name)
		delete(gen.guestLns, name)
		delete(gen.guestAddr, name)
	}
	gen.engine.registry.Publish(merged)
	gen.engine.mu.Unlock()
	gen.engine.log().Info("会话结束，运行时代理已清理", "数量", len(names))
}
