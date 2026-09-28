package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// AddrSummary 生成对端地址的脱敏摘要（规格 §3.6：事件载荷只含脱敏后的地址摘要）。
//
// 摘要而非原值：迁移事件用于观测"路径是否变化"，完整地址会把网络拓扑带进事件流，
// 既无必要也会泄露可定位信息。取 SHA-256 前 8 个十六进制位——足以区分不同地址、
// 不可逆推原值，且长度稳定适合日志与告警。
//
// 解析失败时按原始串整体摘要，保证任何地址形态都不会漏报；空值返回空串，
// 便于调用方区分"无地址"与"有地址"。
func AddrSummary(address net.Addr) string {
	if address == nil {
		return ""
	}
	return summarizeAddr(address.String())
}

// summarizeAddr 计算地址串的短摘要。
func summarizeAddr(address string) string {
	trimmed := strings.TrimSpace(address)
	if trimmed == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(digest[:])[:8]
}

// addrKey 从地址串中解析出 IP 与端口，供需要区分"同地址不同端口"的场景使用。
//
// 解析失败返回原始串与端口 0，不报错：地址形态由传输层决定，摘要层不应因此失败。
func addrKey(address string) (string, int) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return address, 0
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return address, 0
	}
	if parsed, parseErr := netip.ParseAddr(host); parseErr == nil {
		return parsed.String(), port
	}
	return host, port
}
