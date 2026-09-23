package server

import (
	"net"
	"net/netip"
	"testing"

	"github.com/wcpe/jrp/core"
)

// mustCredentialConfig 构造单凭证配置（测试辅助）。
//
// Token 字段在登录校验链交付后承载摘要（SHA-256 hex）。
func mustCredentialConfig(t *testing.T, clientID, digest string) core.ServerConfig {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请端口失败：%v", err)
	}
	_ = listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	config, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{
			Address:   netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port)),
			Transport: core.TransportTCP,
		}),
		core.WithWire(core.WireV1),
		core.WithClientCredential(core.ClientCredential{ClientID: clientID, Token: digest}),
	)
	if err != nil {
		t.Fatalf("构造配置失败：%v", err)
	}
	return config
}
