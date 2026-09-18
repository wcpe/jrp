package core

// Transport 是连接传输方式的具名枚举。
//
// 只定义已按各自功能规格交付的取值：KCP、QUIC、WebSocket、WSS 等取值待对应功能交付后加入，
// 此处不预留空枚举。宿主不得用裸字符串字面量构造，避免拼写漂移。
type Transport string

// TransportTCP 是 TCP 连接传输，对应 FR-05a。
const TransportTCP Transport = "tcp"

// supportedTransports 列出当前已交付的传输取值。
var supportedTransports = []Transport{TransportTCP}

// WireVersion 是控制协议线上封装版本的具名枚举。
//
// 只定义已交付的取值：wire v2 待 FR-04 交付后加入，此处不预留空枚举。
type WireVersion string

// WireV1 是 wire 版本 1，对应 FR-04 的 v1 部分。
const WireV1 WireVersion = "v1"

// supportedWireVersions 列出当前已交付的 wire 版本取值。
var supportedWireVersions = []WireVersion{WireV1}

// ProxyType 是代理类型的具名枚举。
//
// 只定义已交付的取值：STCP、XTCP 待 FR-06b 交付后加入，此处不预留空枚举。
type ProxyType string

const (
	// ProxyTypeTCP 是 TCP 代理，对应 FR-06a 的 TCP 部分。
	ProxyTypeTCP ProxyType = "tcp"
	// ProxyTypeUDP 是 UDP 代理，按对端地址会话化转发。
	ProxyTypeUDP ProxyType = "udp"
	// ProxyTypeHTTP 是 HTTP 代理，按主机名与路径路由，可共享入口端口。
	ProxyTypeHTTP ProxyType = "http"
	// ProxyTypeHTTPS 是 HTTPS 代理，P1 为不终止 TLS 的透传。
	ProxyTypeHTTPS ProxyType = "https"
)

// supportedProxyTypes 列出当前已交付的代理类型取值。
var supportedProxyTypes = []ProxyType{ProxyTypeTCP, ProxyTypeUDP, ProxyTypeHTTP, ProxyTypeHTTPS}

// String 返回代理类型取值，满足 Stringer。
func (proxyType ProxyType) String() string {
	return string(proxyType)
}
