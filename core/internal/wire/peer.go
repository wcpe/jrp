package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
)

// PeerDigest 返回对端地址的脱敏摘要。
//
// 事件与日志只保留摘要：既能让运维按连接归因，又不落库完整地址与身份。
// 地址缺失时返回固定的占位值，不返回空串以免事件字段含义歧义。
func PeerDigest(conn net.Conn) string {
	if conn == nil {
		return "unknown"
	}
	addr := conn.RemoteAddr()
	if addr == nil {
		return "unknown"
	}
	sum := sha256.Sum256([]byte(addr.String()))
	return hex.EncodeToString(sum[:6])
}
