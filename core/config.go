package core

import (
	"net/netip"
	"time"
)

// TLSConfig 是 WSS 与 QUIC 共用的 TLS 配置表达。
//
// PEM 字段由宿主提供，Core 不读取证书文件。客户端默认执行系统信任链校验；
// 指纹固定只作为额外约束，不会自动关闭证书校验。
type TLSConfig struct {
	// ServerName 是客户端校验证书时使用的名称。
	ServerName string
	// RootCAPEM 是额外信任根证书的 PEM 内容。
	RootCAPEM string
	// CertificatePEM 是服务端证书链的 PEM 内容。
	CertificatePEM string
	// PrivateKeyPEM 是服务端私钥的 PEM 内容。
	PrivateKeyPEM string
	// FingerprintSHA256 是可选的证书 SHA-256 指纹十六进制值。
	FingerprintSHA256 string
}

// WebSocketConfig 是 WebSocket 与 WSS 的连接参数。
type WebSocketConfig struct {
	// Path 是 HTTP 升级路径，默认值为 /frp。
	Path string
	// Header 是握手时附加的单个路由提示头，格式为名称和值以冒号分隔；不得放入凭证或正文。
	Header string
	// MaxPayloadBytes 是单个 WebSocket 消息的最大字节数，零值使用 Core 默认值。
	MaxPayloadBytes int
	// TLS 是 WSS 使用的 TLS 配置；WebSocket 明文传输忽略该字段。
	TLS TLSConfig
}

// KCPConfig 是 KCP 传输参数。
type KCPConfig struct {
	// MTU 是 KCP 最大传输单元，零值使用实现默认值。
	MTU int
	// SendWindow 是发送窗口大小，零值使用实现默认值。
	SendWindow int
	// ReceiveWindow 是接收窗口大小，零值使用实现默认值。
	ReceiveWindow int
	// DataShards 是 FEC 数据分片数，零值表示关闭 FEC。
	DataShards int
	// ParityShards 是 FEC 冗余分片数，零值表示关闭 FEC。
	ParityShards int
	// NoDelay 表示是否启用低延迟模式。
	NoDelay bool
	// Interval 是 KCP 刷新间隔，零值使用实现默认值。
	Interval time.Duration
	// Resend 是快速重传阈值，零值使用实现默认值。
	Resend int
	// NoCongestion 表示是否关闭拥塞控制。
	NoCongestion bool
	// Key 是可选的 KCP AES 密钥；为空表示不启用数据包加密。
	Key string
}

// QUICConfig 是 QUIC 流传输参数。
type QUICConfig struct {
	// TLS 是 QUIC 内建 TLS 的配置。
	TLS TLSConfig
	// ALPN 是可选的应用协议标识。
	ALPN string
	// HandshakeTimeout 是握手空闲超时，零值使用实现默认值。
	HandshakeTimeout time.Duration
	// MaxIdleTimeout 是握手完成后的最大空闲超时，零值使用实现默认值。
	MaxIdleTimeout time.Duration
	// KeepAlivePeriod 是保活周期，零值表示关闭保活。
	KeepAlivePeriod time.Duration
	// InitialStreamReceiveWindow 是流初始接收窗口，零值使用实现默认值。
	InitialStreamReceiveWindow uint64
	// MaxStreamReceiveWindow 是流最大接收窗口，零值使用实现默认值。
	MaxStreamReceiveWindow uint64
	// InitialConnectionReceiveWindow 是连接初始接收窗口，零值使用实现默认值。
	InitialConnectionReceiveWindow uint64
	// MaxConnectionReceiveWindow 是连接最大接收窗口，零值使用实现默认值。
	MaxConnectionReceiveWindow uint64
	// MaxIncomingStreams 是允许对端打开的双向流数量，零值使用实现默认值。
	MaxIncomingStreams int64
}

// TransportConfig 是所有连接传输的 Core 自有配置表达。
type TransportConfig struct {
	// WebSocket 是 WebSocket 与 WSS 共用的配置。
	WebSocket WebSocketConfig
	// KCP 是 KCP 传输配置。
	KCP KCPConfig
	// QUIC 是 QUIC 传输配置。
	QUIC QUICConfig
}

// ServerEndpoint 是客户端连接服务端所用的端点。
type ServerEndpoint struct {
	// Address 是服务端控制连接地址，必须是带明确主机与端口的地址。
	Address netip.AddrPort
	// Transport 是控制连接使用的传输方式。
	Transport Transport
	// Wire 是控制连接使用的 wire 版本。
	Wire WireVersion
	// TransportConfig 是该端点的传输参数。
	TransportConfig TransportConfig
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

// UDPProxy 是客户端声明的本地 UDP 代理。
type UDPProxy struct {
	// Name 是代理名，在本客户端内唯一。
	Name string
	// LocalAddr 是客户端侧被代理服务的本地目标地址。
	LocalAddr netip.AddrPort
	// RemotePort 是服务端用于接收数据报的端口。
	RemotePort int
}

// Type 返回代理类型取值。
func (proxy UDPProxy) Type() ProxyType {
	return ProxyTypeUDP
}

// HTTPProxy 是客户端声明的本地 HTTP 代理。
type HTTPProxy struct {
	// Name 是代理名，在本客户端内唯一。
	Name string
	// LocalAddr 是客户端侧被代理服务的本地目标地址。
	LocalAddr netip.AddrPort
	// RemotePort 是服务端用于接收请求的端口；多个 HTTP 代理可共享同一端口。
	RemotePort int
}

// Type 返回代理类型取值。
func (proxy HTTPProxy) Type() ProxyType {
	return ProxyTypeHTTP
}

// HTTPSProxy 是客户端声明的本地 HTTPS 代理。
//
// P1 为 TLS 透传：Core 不持有被代理服务的证书或私钥，因此本类型没有证书字段。
type HTTPSProxy struct {
	// Name 是代理名，在本客户端内唯一。
	Name string
	// LocalAddr 是客户端侧被代理服务的本地目标地址。
	LocalAddr netip.AddrPort
	// RemotePort 是服务端用于接收连接的独占端口。
	RemotePort int
}

// Type 返回代理类型取值。
func (proxy HTTPSProxy) Type() ProxyType {
	return ProxyTypeHTTPS
}

// ClientProxy 是客户端侧四种代理的统一只读视图。
//
// 四种代理的转发语义差异很大，因此不抽取共同的写接口；本接口只暴露注册与
// 校验需要的字段，不承载转发行为。访问器统一带 Proxy 前缀：Go 不允许方法与
// 结构体字段同名，而这些字段名是既有公共 API，不得改名。
type ClientProxy interface {
	// Type 返回代理类型取值。
	Type() ProxyType
	// ProxyName 返回代理名。
	ProxyName() string
	// ProxyLocalAddr 返回客户端侧被代理服务的本地目标地址。
	ProxyLocalAddr() netip.AddrPort
	// ProxyRemotePort 返回服务端用于接收流量的端口。
	ProxyRemotePort() int
}

// ProxyName 返回代理名。
func (proxy TCPProxy) ProxyName() string { return proxy.Name }

// ProxyLocalAddr 返回本地目标地址。
func (proxy TCPProxy) ProxyLocalAddr() netip.AddrPort { return proxy.LocalAddr }

// ProxyRemotePort 返回服务端入口端口。
func (proxy TCPProxy) ProxyRemotePort() int { return proxy.RemotePort }

// ProxyName 返回代理名。
func (proxy UDPProxy) ProxyName() string { return proxy.Name }

// ProxyLocalAddr 返回本地目标地址。
func (proxy UDPProxy) ProxyLocalAddr() netip.AddrPort { return proxy.LocalAddr }

// ProxyRemotePort 返回服务端入口端口。
func (proxy UDPProxy) ProxyRemotePort() int { return proxy.RemotePort }

// ProxyName 返回代理名。
func (proxy HTTPProxy) ProxyName() string { return proxy.Name }

// ProxyLocalAddr 返回本地目标地址。
func (proxy HTTPProxy) ProxyLocalAddr() netip.AddrPort { return proxy.LocalAddr }

// ProxyRemotePort 返回服务端入口端口。
func (proxy HTTPProxy) ProxyRemotePort() int { return proxy.RemotePort }

// ProxyName 返回代理名。
func (proxy HTTPSProxy) ProxyName() string { return proxy.Name }

// ProxyLocalAddr 返回本地目标地址。
func (proxy HTTPSProxy) ProxyLocalAddr() netip.AddrPort { return proxy.LocalAddr }

// ProxyRemotePort 返回服务端入口端口。
func (proxy HTTPSProxy) ProxyRemotePort() int { return proxy.RemotePort }

// ClientCredential 是服务端接受的单个客户端凭证。
type ClientCredential struct {
	// ClientID 是客户端标识。
	ClientID string
	// Token 是该客户端 token 的 SHA-256 摘要（十六进制）。快照持有摘要而不是
	// 明文：错误、日志与状态快照一律脱敏，明文只在宿主存储与客户端配置中存在。
	Token string
	// CompatToken 是该客户端 token 的明文，专供官方 frpc 的鉴权材料校验使用：
	// 官方客户端只发送 `md5(token + 时间戳)` 的摘要前处理材料，服务端必须持有
	// 明文才能复算比对（FR-03 规格登记的兼容例外）。为空表示该客户端只接受
	// 摘要语义登录（纯 jrpc 场景）。
	CompatToken string
}

// BindEndpoint 是服务端的监听端点。
type BindEndpoint struct {
	// Address 是监听地址；监听场景允许未指定地址（0.0.0.0 / ::）。
	Address netip.AddrPort
	// Transport 是监听使用的传输方式。
	Transport Transport
	// TransportConfig 是该监听端点的传输参数。
	TransportConfig TransportConfig
}

// TCPProxyBinding 是服务端为某个客户端声明的 TCP 代理绑定。
type TCPProxyBinding struct {
	// Name 是代理名，在服务端全局唯一。
	Name string
	// ClientID 是承担该代理的客户端标识，必须存在于凭证集合中。
	ClientID string
	// RemotePort 是服务端接收访客连接的端口。
	RemotePort int
	// AllowedTargets 是该客户端被允许转发到的目标地址集合；不得为空。
	//
	// 规格 §3.3：目标地址必须是该客户端已被允许的地址集合内的地址，代理层
	// 不得任意转发到未声明目标。
	AllowedTargets []netip.AddrPort
}

// Type 返回代理类型取值。
func (binding TCPProxyBinding) Type() ProxyType {
	return ProxyTypeTCP
}

// ProxyName 返回代理名。
func (binding TCPProxyBinding) ProxyName() string { return binding.Name }

// OwnerClientID 返回该绑定允许接入的客户端标识。
func (binding TCPProxyBinding) OwnerClientID() string { return binding.ClientID }

// ProxyRemotePort 返回服务端入口端口。
func (binding TCPProxyBinding) ProxyRemotePort() int { return binding.RemotePort }

// ProxyTargets 返回允许的目标地址集合副本。
func (binding TCPProxyBinding) ProxyTargets() []netip.AddrPort {
	return copySlice(binding.AllowedTargets)
}

// UDPProxyBinding 是服务端为某个客户端声明的 UDP 代理绑定。
type UDPProxyBinding struct {
	// Name 是代理名，在服务端全局唯一。
	Name string
	// ClientID 是承担该代理的客户端标识，必须存在于凭证集合中。
	ClientID string
	// RemotePort 是服务端接收数据报的独占端口。
	RemotePort int
	// AllowedTargets 是该客户端被允许转发到的目标地址集合；不得为空。
	AllowedTargets []netip.AddrPort
}

// Type 返回代理类型取值。
func (binding UDPProxyBinding) Type() ProxyType {
	return ProxyTypeUDP
}

// ProxyName 返回代理名。
func (binding UDPProxyBinding) ProxyName() string { return binding.Name }

// OwnerClientID 返回该绑定允许接入的客户端标识。
func (binding UDPProxyBinding) OwnerClientID() string { return binding.ClientID }

// ProxyRemotePort 返回服务端入口端口。
func (binding UDPProxyBinding) ProxyRemotePort() int { return binding.RemotePort }

// ProxyTargets 返回允许的目标地址集合副本。
func (binding UDPProxyBinding) ProxyTargets() []netip.AddrPort {
	return copySlice(binding.AllowedTargets)
}

// HTTPProxyBinding 是服务端为某个客户端声明的 HTTP 代理绑定。
type HTTPProxyBinding struct {
	// Name 是代理名，在服务端全局唯一。
	Name string
	// ClientID 是承担该代理的客户端标识，必须存在于凭证集合中。
	ClientID string
	// RemotePort 是服务端接收请求的端口；多个 HTTP 代理可共享同一端口。
	RemotePort int
	// Hosts 是该代理响应的主机名集合；不得为空，主机名精确匹配。
	Hosts []string
	// Path 是该代理响应的路径前缀；空串与 "/" 等同，两者只允许其一。
	Path string
	// AllowedTargets 是该客户端被允许转发到的目标地址集合；不得为空。
	AllowedTargets []netip.AddrPort
	// CaptureBody 是正文采集挂载点开关；默认关闭。
	//
	// 规格 §3.5：HTTP 是唯一允许开启正文采集的代理类型。Core 只提供开关与
	// 采集点，不实现存储；落盘属于 jrps 适配器。
	CaptureBody bool
}

// Type 返回代理类型取值。
func (binding HTTPProxyBinding) Type() ProxyType {
	return ProxyTypeHTTP
}

// ProxyName 返回代理名。
func (binding HTTPProxyBinding) ProxyName() string { return binding.Name }

// OwnerClientID 返回该绑定允许接入的客户端标识。
func (binding HTTPProxyBinding) OwnerClientID() string { return binding.ClientID }

// ProxyRemotePort 返回服务端入口端口。
func (binding HTTPProxyBinding) ProxyRemotePort() int { return binding.RemotePort }

// ProxyTargets 返回允许的目标地址集合副本。
func (binding HTTPProxyBinding) ProxyTargets() []netip.AddrPort {
	return copySlice(binding.AllowedTargets)
}

// HTTPSProxyBinding 是服务端为某个客户端声明的 HTTPS 代理绑定。
//
// P1 为 TLS 透传：绑定内没有、也不得出现证书或私钥字段，Core 不持有被代理
// 服务的 TLS 材料（规格 §3.6）。
type HTTPSProxyBinding struct {
	// Name 是代理名，在服务端全局唯一。
	Name string
	// ClientID 是承担该代理的客户端标识，必须存在于凭证集合中。
	ClientID string
	// RemotePort 是服务端接收连接的独占端口；透传无法按主机名分发，故不共享。
	RemotePort int
	// AllowedTargets 是该客户端被允许转发到的目标地址集合；不得为空。
	AllowedTargets []netip.AddrPort
}

// Type 返回代理类型取值。
func (binding HTTPSProxyBinding) Type() ProxyType {
	return ProxyTypeHTTPS
}

// ProxyName 返回代理名。
func (binding HTTPSProxyBinding) ProxyName() string { return binding.Name }

// OwnerClientID 返回该绑定允许接入的客户端标识。
func (binding HTTPSProxyBinding) OwnerClientID() string { return binding.ClientID }

// ProxyRemotePort 返回服务端入口端口。
func (binding HTTPSProxyBinding) ProxyRemotePort() int { return binding.RemotePort }

// ProxyTargets 返回允许的目标地址集合副本。
func (binding HTTPSProxyBinding) ProxyTargets() []netip.AddrPort {
	return copySlice(binding.AllowedTargets)
}

// ServerProxyBinding 是服务端四种代理绑定的统一只读视图。
//
// 四种绑定的转发语义差异很大，因此不抽取共同写接口；本接口只暴露注册与校验
// 需要的字段，不承载转发行为。访问器同客户端侧一致，带 Proxy 前缀。
type ServerProxyBinding interface {
	// Type 返回代理类型取值。
	Type() ProxyType
	// ProxyName 返回代理名。
	ProxyName() string
	// OwnerClientID 返回该绑定允许接入的客户端标识。
	OwnerClientID() string
	// ProxyRemotePort 返回服务端入口端口。
	ProxyRemotePort() int
	// ProxyTargets 返回允许的目标地址集合副本。
	ProxyTargets() []netip.AddrPort
}

// ClientConfig 是客户端侧的不可变配置值。
//
// 构建后没有 setter；读取集合返回深复制的新切片。零值不是合法配置，Validate 会返回错误。
type ClientConfig struct {
	clientID     string
	endpoint     ServerEndpoint
	auth         TokenAuth
	tcpProxies   []TCPProxy
	udpProxies   []UDPProxy
	httpProxies  []HTTPProxy
	httpsProxies []HTTPSProxy
	heartbeat    time.Duration
	timeout      time.Duration
	drainTimeout time.Duration
	hasEndpoint  bool
	poolSize     int
	idleLimit    int
}

// ServerConfig 是服务端侧的不可变配置值。
//
// 构建后没有 setter；读取集合返回深复制的新切片。零值不是合法配置，Validate 会返回错误。
type ServerConfig struct {
	listen       BindEndpoint
	wire         WireVersion
	credentials  []ClientCredential
	tcpBindings  []TCPProxyBinding
	udpBindings  []UDPProxyBinding
	httpBindings []HTTPProxyBinding
	// httpsBindings 承载 HTTPS 透传绑定。
	httpsBindings []HTTPSProxyBinding
	heartbeat     time.Duration
	timeout       time.Duration
	drainTimeout  time.Duration
	hasListen     bool
	idleLimit     int
	// udpSessionIdle 是 UDP 会话空闲上限。
	udpSessionIdle time.Duration
	// udpSessionLimit 是单代理 UDP 会话数上限。
	udpSessionLimit int
	// udpDatagramSize 是单个 UDP 数据报字节上限。
	udpDatagramSize int
}

// ClientOption 是客户端配置的选项函数；选项只做赋值，不做校验。
type ClientOption func(*clientDraft)

// ServerOption 是服务端配置的选项函数；选项只做赋值，不做校验。
type ServerOption func(*serverDraft)

// ClientID 返回客户端标识。
func (config ClientConfig) ClientID() string {
	return config.clientID
}

// ServerEndpoint 返回服务端端点副本。
func (config ClientConfig) ServerEndpoint() ServerEndpoint {
	return cloneServerEndpoint(config.endpoint)
}

// Auth 返回鉴权材料副本。
func (config ClientConfig) Auth() TokenAuth {
	return config.auth
}

// Proxies 返回本地 TCP 代理集合的副本，修改返回值不影响配置值。
//
// 四种代理各有自己的集合；本访问器保持 FR-25 既有语义，只返回 TCP 代理。
func (config ClientConfig) Proxies() []TCPProxy {
	return copySlice(config.tcpProxies)
}

// UDPProxies 返回本地 UDP 代理集合的副本。
func (config ClientConfig) UDPProxies() []UDPProxy {
	return copySlice(config.udpProxies)
}

// HTTPProxies 返回本地 HTTP 代理集合的副本。
func (config ClientConfig) HTTPProxies() []HTTPProxy {
	return copySlice(config.httpProxies)
}

// HTTPSProxies 返回本地 HTTPS 代理集合的副本。
func (config ClientConfig) HTTPSProxies() []HTTPSProxy {
	return copySlice(config.httpsProxies)
}

// AllProxies 按 TCP、UDP、HTTP、HTTPS 的顺序返回全部代理的统一视图。
func (config ClientConfig) AllProxies() []ClientProxy {
	all := make([]ClientProxy, 0,
		len(config.tcpProxies)+len(config.udpProxies)+len(config.httpProxies)+len(config.httpsProxies))
	appendTCP := func(proxies []TCPProxy) {
		for _, proxy := range proxies {
			all = append(all, proxy)
		}
	}
	appendUDP := func(proxies []UDPProxy) {
		for _, proxy := range proxies {
			all = append(all, proxy)
		}
	}
	appendHTTP := func(proxies []HTTPProxy) {
		for _, proxy := range proxies {
			all = append(all, proxy)
		}
	}
	appendHTTPS := func(proxies []HTTPSProxy) {
		for _, proxy := range proxies {
			all = append(all, proxy)
		}
	}
	appendTCP(config.tcpProxies)
	appendUDP(config.udpProxies)
	appendHTTP(config.httpProxies)
	appendHTTPS(config.httpsProxies)
	return all
}

// Heartbeat 返回心跳间隔，零值输入已由默认常量取代。
func (config ClientConfig) Heartbeat() time.Duration {
	return config.heartbeat
}

// Timeout 返回连接超时，零值输入已由默认常量取代。
func (config ClientConfig) Timeout() time.Duration {
	return config.timeout
}

// DrainTimeout 返回排水上限：Shutdown 等待活动连接自然结束的最长时间。
func (config ClientConfig) DrainTimeout() time.Duration {
	return config.drainTimeout
}

// WorkConnPoolSize 返回单代理工作连接池上限，零值输入已由默认常量取代。
func (config ClientConfig) WorkConnPoolSize() int {
	return config.poolSize
}

// IdleWorkConnLimit 返回单代理待命工作连接的空闲上限，零值输入已由默认常量取代。
func (config ClientConfig) IdleWorkConnLimit() int {
	return config.idleLimit
}

// Listen 返回监听端点副本。
func (config ServerConfig) Listen() BindEndpoint {
	return cloneBindEndpoint(config.listen)
}

// Wire 返回 wire 版本。
func (config ServerConfig) Wire() WireVersion {
	return config.wire
}

// Credentials 返回客户端凭证集合的副本，修改返回值不影响配置值。
func (config ServerConfig) Credentials() []ClientCredential {
	return copySlice(config.credentials)
}

// Bindings 返回 TCP 代理绑定集合的副本，修改返回值不影响配置值。
//
// 四种代理各有自己的集合；本访问器保持 FR-25 既有语义，只返回 TCP 绑定。
func (config ServerConfig) Bindings() []TCPProxyBinding {
	return copySlice(config.tcpBindings)
}

// UDPBindings 返回 UDP 代理绑定集合的副本。
func (config ServerConfig) UDPBindings() []UDPProxyBinding {
	return copySlice(config.udpBindings)
}

// HTTPBindings 返回 HTTP 代理绑定集合的副本。
func (config ServerConfig) HTTPBindings() []HTTPProxyBinding {
	return copySlice(config.httpBindings)
}

// HTTPSBindings 返回 HTTPS 代理绑定集合的副本。
func (config ServerConfig) HTTPSBindings() []HTTPSProxyBinding {
	return copySlice(config.httpsBindings)
}

// AllBindings 按 TCP、UDP、HTTP、HTTPS 的顺序返回全部绑定的统一视图。
func (config ServerConfig) AllBindings() []ServerProxyBinding {
	all := make([]ServerProxyBinding, 0,
		len(config.tcpBindings)+len(config.udpBindings)+len(config.httpBindings)+len(config.httpsBindings))
	for _, binding := range config.tcpBindings {
		all = append(all, binding)
	}
	for _, binding := range config.udpBindings {
		all = append(all, binding)
	}
	for _, binding := range config.httpBindings {
		all = append(all, binding)
	}
	for _, binding := range config.httpsBindings {
		all = append(all, binding)
	}
	return all
}

// Heartbeat 返回心跳间隔，零值输入已由默认常量取代。
func (config ServerConfig) Heartbeat() time.Duration {
	return config.heartbeat
}

// Timeout 返回连接超时，零值输入已由默认常量取代。
func (config ServerConfig) Timeout() time.Duration {
	return config.timeout
}

// DrainTimeout 返回排水上限：Shutdown 等待活动连接自然结束的最长时间。
func (config ServerConfig) DrainTimeout() time.Duration {
	return config.drainTimeout
}

// IdleWorkConnLimit 返回单代理待命工作连接的空闲上限，零值输入已由默认常量取代。
func (config ServerConfig) IdleWorkConnLimit() int {
	return config.idleLimit
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
		draft.tcpProxies = append(draft.tcpProxies, copySlice(proxies)...)
	}
}

// WithUDPProxy 追加一个本地 UDP 代理；可多次调用，重名由校验发现。
func WithUDPProxy(proxy UDPProxy) ClientOption {
	return WithUDPProxies([]UDPProxy{proxy})
}

// WithUDPProxies 批量追加本地 UDP 代理，入参在选项执行时即被复制。
func WithUDPProxies(proxies []UDPProxy) ClientOption {
	return func(draft *clientDraft) {
		draft.udpProxies = append(draft.udpProxies, copySlice(proxies)...)
	}
}

// WithHTTPProxy 追加一个本地 HTTP 代理；可多次调用，重名由校验发现。
func WithHTTPProxy(proxy HTTPProxy) ClientOption {
	return WithHTTPProxies([]HTTPProxy{proxy})
}

// WithHTTPProxies 批量追加本地 HTTP 代理，入参在选项执行时即被复制。
func WithHTTPProxies(proxies []HTTPProxy) ClientOption {
	return func(draft *clientDraft) {
		draft.httpProxies = append(draft.httpProxies, copySlice(proxies)...)
	}
}

// WithHTTPSProxy 追加一个本地 HTTPS 代理；可多次调用，重名由校验发现。
func WithHTTPSProxy(proxy HTTPSProxy) ClientOption {
	return WithHTTPSProxies([]HTTPSProxy{proxy})
}

// WithHTTPSProxies 批量追加本地 HTTPS 代理，入参在选项执行时即被复制。
func WithHTTPSProxies(proxies []HTTPSProxy) ClientOption {
	return func(draft *clientDraft) {
		draft.httpsProxies = append(draft.httpsProxies, copySlice(proxies)...)
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

// WithClientDrainTimeout 设置客户端排水上限；零值表示使用 DefaultDrainTimeout。
func WithClientDrainTimeout(timeout time.Duration) ClientOption {
	return func(draft *clientDraft) {
		draft.drainTimeout = timeout
	}
}

// WithWorkConnPoolSize 设置单代理工作连接池上限；零值表示使用 DefaultWorkConnPoolSize。
func WithWorkConnPoolSize(size int) ClientOption {
	return func(draft *clientDraft) {
		draft.poolSize = size
	}
}

// WithIdleWorkConnLimit 设置单代理待命工作连接的空闲上限；零值表示使用 DefaultIdleWorkConnLimit。
func WithIdleWorkConnLimit(limit int) ClientOption {
	return func(draft *clientDraft) {
		draft.idleLimit = limit
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

// WithServerDrainTimeout 设置服务端排水上限；零值表示使用 DefaultDrainTimeout。
func WithServerDrainTimeout(timeout time.Duration) ServerOption {
	return func(draft *serverDraft) {
		draft.drainTimeout = timeout
	}
}

// WithServerIdleWorkConnLimit 设置服务端单代理待命工作连接的空闲上限；
// 零值表示使用 DefaultIdleWorkConnLimit。
func WithServerIdleWorkConnLimit(limit int) ServerOption {
	return func(draft *serverDraft) {
		draft.idleLimit = limit
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
		draft.tcpBindings = append(draft.tcpBindings, copySlice(bindings)...)
	}
}

// WithUDPProxyBinding 追加一条 UDP 代理绑定；可多次调用，重名由校验发现。
func WithUDPProxyBinding(binding UDPProxyBinding) ServerOption {
	return WithUDPProxyBindings([]UDPProxyBinding{binding})
}

// WithUDPProxyBindings 批量追加 UDP 代理绑定，入参在选项执行时即被复制。
func WithUDPProxyBindings(bindings []UDPProxyBinding) ServerOption {
	return func(draft *serverDraft) {
		draft.udpBindings = append(draft.udpBindings, copySlice(bindings)...)
	}
}

// WithHTTPProxyBinding 追加一条 HTTP 代理绑定；可多次调用，重名由校验发现。
func WithHTTPProxyBinding(binding HTTPProxyBinding) ServerOption {
	return WithHTTPProxyBindings([]HTTPProxyBinding{binding})
}

// WithHTTPProxyBindings 批量追加 HTTP 代理绑定，入参在选项执行时即被复制。
func WithHTTPProxyBindings(bindings []HTTPProxyBinding) ServerOption {
	return func(draft *serverDraft) {
		draft.httpBindings = append(draft.httpBindings, copySlice(bindings)...)
	}
}

// WithHTTPSProxyBinding 追加一条 HTTPS 代理绑定；可多次调用，重名由校验发现。
func WithHTTPSProxyBinding(binding HTTPSProxyBinding) ServerOption {
	return WithHTTPSProxyBindings([]HTTPSProxyBinding{binding})
}

// WithHTTPSProxyBindings 批量追加 HTTPS 代理绑定，入参在选项执行时即被复制。
func WithHTTPSProxyBindings(bindings []HTTPSProxyBinding) ServerOption {
	return func(draft *serverDraft) {
		draft.httpsBindings = append(draft.httpsBindings, copySlice(bindings)...)
	}
}

// WithUDPSessionIdle 设置服务端 UDP 会话空闲上限；零值表示使用 DefaultUDPSessionIdle。
func WithUDPSessionIdle(idle time.Duration) ServerOption {
	return func(draft *serverDraft) {
		draft.udpSessionIdle = idle
	}
}

// WithUDPSessionLimit 设置单代理 UDP 会话数上限；零值表示使用 DefaultUDPSessionLimit。
func WithUDPSessionLimit(limit int) ServerOption {
	return func(draft *serverDraft) {
		draft.udpSessionLimit = limit
	}
}

// WithUDPDatagramSize 设置单个 UDP 数据报字节上限；零值表示使用 DefaultUDPDatagramSize。
func WithUDPDatagramSize(size int) ServerOption {
	return func(draft *serverDraft) {
		draft.udpDatagramSize = size
	}
}

// UDPSessionIdle 返回 UDP 会话空闲上限，零值输入已由默认常量取代。
func (config ServerConfig) UDPSessionIdle() time.Duration {
	return config.udpSessionIdle
}

// UDPSessionLimit 返回单代理 UDP 会话数上限，零值输入已由默认常量取代。
func (config ServerConfig) UDPSessionLimit() int {
	return config.udpSessionLimit
}

// UDPDatagramSize 返回单个 UDP 数据报字节上限，零值输入已由默认常量取代。
func (config ServerConfig) UDPDatagramSize() int {
	return config.udpDatagramSize
}

// clientDraft 是客户端配置的构建中间态，仅在构建函数内存在。
type clientDraft struct {
	clientID     string
	endpoint     ServerEndpoint
	auth         TokenAuth
	tcpProxies   []TCPProxy
	udpProxies   []UDPProxy
	httpProxies  []HTTPProxy
	httpsProxies []HTTPSProxy
	heartbeat    time.Duration
	timeout      time.Duration
	drainTimeout time.Duration
	hasEndpoint  bool
	poolSize     int
	idleLimit    int
}

// serverDraft 是服务端配置的构建中间态，仅在构建函数内存在。
type serverDraft struct {
	listen         BindEndpoint
	wire           WireVersion
	credentials    []ClientCredential
	tcpBindings    []TCPProxyBinding
	udpBindings    []UDPProxyBinding
	httpBindings   []HTTPProxyBinding
	httpsBindings  []HTTPSProxyBinding
	heartbeat      time.Duration
	timeout        time.Duration
	drainTimeout   time.Duration
	hasListen      bool
	idleLimit      int
	udpSessionIdle time.Duration
	// udpSessionLimit 是单代理 UDP 会话数上限。
	udpSessionLimit int
	// udpDatagramSize 是单个 UDP 数据报字节上限。
	udpDatagramSize int
}

// withDefaults 把零值心跳与超时替换为默认常量。
func (draft *clientDraft) withDefaults() {
	draft.heartbeat = defaultDuration(draft.heartbeat, DefaultHeartbeat)
	draft.timeout = defaultDuration(draft.timeout, DefaultTimeout)
	draft.drainTimeout = defaultDuration(draft.drainTimeout, DefaultDrainTimeout)
	draft.poolSize = defaultValue(draft.poolSize, DefaultWorkConnPoolSize)
	draft.idleLimit = defaultValue(draft.idleLimit, DefaultIdleWorkConnLimit)
}

// withDefaults 把零值心跳与超时替换为默认常量。
func (draft *serverDraft) withDefaults() {
	draft.heartbeat = defaultDuration(draft.heartbeat, DefaultHeartbeat)
	draft.timeout = defaultDuration(draft.timeout, DefaultTimeout)
	draft.drainTimeout = defaultDuration(draft.drainTimeout, DefaultDrainTimeout)
	draft.idleLimit = defaultValue(draft.idleLimit, DefaultIdleWorkConnLimit)
	draft.udpSessionIdle = defaultDuration(draft.udpSessionIdle, DefaultUDPSessionIdle)
	draft.udpSessionLimit = defaultValue(draft.udpSessionLimit, DefaultUDPSessionLimit)
	draft.udpDatagramSize = defaultValue(draft.udpDatagramSize, DefaultUDPDatagramSize)
}

// defaultDuration 在取值为零时返回回退值；负值保持原样交由校验报错。
func defaultDuration(value, fallback time.Duration) time.Duration {
	if value == 0 {
		return fallback
	}
	return value
}

// defaultValue 在取值为零时返回回退值；负值保持原样交由校验报错。
func defaultValue(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

// copySlice 返回入参切片的独立副本。
func copySlice[T any](values []T) []T {
	return append([]T(nil), values...)
}
