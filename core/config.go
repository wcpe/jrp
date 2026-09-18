package core

import (
	"net/netip"
	"time"
)

// ServerEndpoint 是客户端连接服务端所用的端点。
type ServerEndpoint struct {
	// Address 是服务端控制连接地址，必须是带明确主机与端口的地址。
	Address netip.AddrPort
	// Transport 是控制连接使用的传输方式。
	Transport Transport
	// Wire 是控制连接使用的 wire 版本。
	Wire WireVersion
}

// TokenAuth 是客户端的鉴权材料。
type TokenAuth struct {
	// Token 是每客户端 token 明文；错误、日志与状态快照必须脱敏。
	Token string
}

// TCPProxy 是客户端声明的本地 TCP 代理。
type TCPProxy struct {
	// Name 是代理名，在本客户端内唯一。
	Name string
	// LocalAddr 是客户端侧被代理服务的本地目标地址。
	LocalAddr netip.AddrPort
	// RemotePort 是服务端用于接收访客连接的端口。
	RemotePort int
}

// Type 返回代理类型取值。
func (proxy TCPProxy) Type() ProxyType {
	return ProxyTypeTCP
}

// ClientCredential 是服务端接受的单个客户端凭证。
type ClientCredential struct {
	// ClientID 是客户端标识。
	ClientID string
	// Token 是该客户端的 token 明文；错误、日志与状态快照必须脱敏。
	Token string
}

// BindEndpoint 是服务端的监听端点。
type BindEndpoint struct {
	// Address 是监听地址；监听场景允许未指定地址（0.0.0.0 / ::）。
	Address netip.AddrPort
	// Transport 是监听使用的传输方式。
	Transport Transport
}

// TCPProxyBinding 是服务端为某个客户端声明的 TCP 代理绑定。
type TCPProxyBinding struct {
	// Name 是代理名，在服务端全局唯一。
	Name string
	// ClientID 是承担该代理的客户端标识，必须存在于凭证集合中。
	ClientID string
	// RemotePort 是服务端接收访客连接的端口。
	RemotePort int
}

// Type 返回代理类型取值。
func (binding TCPProxyBinding) Type() ProxyType {
	return ProxyTypeTCP
}

// ClientConfig 是客户端侧的不可变配置值。
//
// 构建后没有 setter；读取集合返回深复制的新切片。零值不是合法配置，Validate 会返回错误。
type ClientConfig struct {
	clientID    string
	endpoint    ServerEndpoint
	auth        TokenAuth
	proxies     []TCPProxy
	heartbeat   time.Duration
	timeout     time.Duration
	hasEndpoint bool
}

// ClientOption 是客户端配置的选项函数；选项只做赋值，不做校验。
type ClientOption func(*clientDraft)

// ServerConfig 是服务端侧的不可变配置值。
//
// 构建后没有 setter；读取集合返回深复制的新切片。零值不是合法配置，Validate 会返回错误。
type ServerConfig struct {
	listen      BindEndpoint
	wire        WireVersion
	credentials []ClientCredential
	bindings    []TCPProxyBinding
	heartbeat   time.Duration
	timeout     time.Duration
	hasListen   bool
}

// ServerOption 是服务端配置的选项函数；选项只做赋值，不做校验。
type ServerOption func(*serverDraft)

// ClientID 返回客户端标识。
func (config ClientConfig) ClientID() string {
	return config.clientID
}

// ServerEndpoint 返回服务端端点副本。
func (config ClientConfig) ServerEndpoint() ServerEndpoint {
	return config.endpoint
}

// Auth 返回鉴权材料副本。
func (config ClientConfig) Auth() TokenAuth {
	return config.auth
}

// Proxies 返回本地代理集合的副本，修改返回值不影响配置值。
func (config ClientConfig) Proxies() []TCPProxy {
	return copySlice(config.proxies)
}

// Heartbeat 返回心跳间隔，零值输入已由默认常量取代。
func (config ClientConfig) Heartbeat() time.Duration {
	return config.heartbeat
}

// Timeout 返回连接超时，零值输入已由默认常量取代。
func (config ClientConfig) Timeout() time.Duration {
	return config.timeout
}

// Listen 返回监听端点副本。
func (config ServerConfig) Listen() BindEndpoint {
	return config.listen
}

// Wire 返回 wire 版本。
func (config ServerConfig) Wire() WireVersion {
	return config.wire
}

// Credentials 返回客户端凭证集合的副本，修改返回值不影响配置值。
func (config ServerConfig) Credentials() []ClientCredential {
	return copySlice(config.credentials)
}

// Bindings 返回代理绑定集合的副本，修改返回值不影响配置值。
func (config ServerConfig) Bindings() []TCPProxyBinding {
	return copySlice(config.bindings)
}

// Heartbeat 返回心跳间隔，零值输入已由默认常量取代。
func (config ServerConfig) Heartbeat() time.Duration {
	return config.heartbeat
}

// Timeout 返回连接超时，零值输入已由默认常量取代。
func (config ServerConfig) Timeout() time.Duration {
	return config.timeout
}

// WithClientID 设置客户端标识。
func WithClientID(clientID string) ClientOption {
	return func(draft *clientDraft) {
		draft.clientID = clientID
	}
}

// WithServerEndpoint 设置服务端端点。
func WithServerEndpoint(endpoint ServerEndpoint) ClientOption {
	return func(draft *clientDraft) {
		draft.endpoint = endpoint
		draft.hasEndpoint = true
	}
}

// WithClientAuth 设置鉴权材料。
func WithClientAuth(auth TokenAuth) ClientOption {
	return func(draft *clientDraft) {
		draft.auth = auth
	}
}

// WithTCPProxy 追加一个本地 TCP 代理；可多次调用，重名由校验发现。
func WithTCPProxy(proxy TCPProxy) ClientOption {
	return WithTCPProxies([]TCPProxy{proxy})
}

// WithTCPProxies 批量追加本地 TCP 代理，入参在选项执行时即被复制。
func WithTCPProxies(proxies []TCPProxy) ClientOption {
	return func(draft *clientDraft) {
		draft.proxies = append(draft.proxies, copySlice(proxies)...)
	}
}

// WithHeartbeat 设置客户端心跳间隔；零值表示使用 DefaultHeartbeat。
func WithHeartbeat(interval time.Duration) ClientOption {
	return func(draft *clientDraft) {
		draft.heartbeat = interval
	}
}

// WithTimeout 设置客户端连接超时；零值表示使用 DefaultTimeout。
func WithTimeout(timeout time.Duration) ClientOption {
	return func(draft *clientDraft) {
		draft.timeout = timeout
	}
}

// WithServerHeartbeat 设置服务端心跳间隔；零值表示使用 DefaultHeartbeat。
//
// 与客户端的 WithHeartbeat 同名会冲突，因此服务端侧带 Server 前缀。
func WithServerHeartbeat(interval time.Duration) ServerOption {
	return func(draft *serverDraft) {
		draft.heartbeat = interval
	}
}

// WithServerTimeout 设置服务端超时；零值表示使用 DefaultTimeout。
func WithServerTimeout(timeout time.Duration) ServerOption {
	return func(draft *serverDraft) {
		draft.timeout = timeout
	}
}

// WithListen 设置监听端点。
func WithListen(endpoint BindEndpoint) ServerOption {
	return func(draft *serverDraft) {
		draft.listen = endpoint
		draft.hasListen = true
	}
}

// WithWire 设置 wire 版本。
func WithWire(wire WireVersion) ServerOption {
	return func(draft *serverDraft) {
		draft.wire = wire
	}
}

// WithClientCredential 追加一条客户端凭证；可多次调用，重复标识由校验发现。
func WithClientCredential(credential ClientCredential) ServerOption {
	return WithClientCredentials([]ClientCredential{credential})
}

// WithClientCredentials 批量追加客户端凭证，入参在选项执行时即被复制。
func WithClientCredentials(credentials []ClientCredential) ServerOption {
	return func(draft *serverDraft) {
		draft.credentials = append(draft.credentials, copySlice(credentials)...)
	}
}

// WithTCPProxyBinding 追加一条 TCP 代理绑定；可多次调用，重名由校验发现。
func WithTCPProxyBinding(binding TCPProxyBinding) ServerOption {
	return WithTCPProxyBindings([]TCPProxyBinding{binding})
}

// WithTCPProxyBindings 批量追加 TCP 代理绑定，入参在选项执行时即被复制。
func WithTCPProxyBindings(bindings []TCPProxyBinding) ServerOption {
	return func(draft *serverDraft) {
		draft.bindings = append(draft.bindings, copySlice(bindings)...)
	}
}

// clientDraft 是客户端配置的构建中间态，仅在构建函数内存在。
type clientDraft struct {
	clientID    string
	endpoint    ServerEndpoint
	auth        TokenAuth
	proxies     []TCPProxy
	heartbeat   time.Duration
	timeout     time.Duration
	hasEndpoint bool
}

// serverDraft 是服务端配置的构建中间态，仅在构建函数内存在。
type serverDraft struct {
	listen      BindEndpoint
	wire        WireVersion
	credentials []ClientCredential
	bindings    []TCPProxyBinding
	heartbeat   time.Duration
	timeout     time.Duration
	hasListen   bool
}

// withDefaults 把零值心跳与超时替换为默认常量。
func (draft *clientDraft) withDefaults() {
	draft.heartbeat = defaultDuration(draft.heartbeat, DefaultHeartbeat)
	draft.timeout = defaultDuration(draft.timeout, DefaultTimeout)
}

// withDefaults 把零值心跳与超时替换为默认常量。
func (draft *serverDraft) withDefaults() {
	draft.heartbeat = defaultDuration(draft.heartbeat, DefaultHeartbeat)
	draft.timeout = defaultDuration(draft.timeout, DefaultTimeout)
}

// defaultDuration 在取值为零时返回回退值；负值保持原样交由校验报错。
func defaultDuration(value, fallback time.Duration) time.Duration {
	if value == 0 {
		return fallback
	}
	return value
}

// copySlice 返回入参切片的独立副本。
func copySlice[T any](values []T) []T {
	return append([]T(nil), values...)
}
