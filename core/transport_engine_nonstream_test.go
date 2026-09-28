package core_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/client"
	"github.com/wcpe/jrp/core/server"
)

// 本文件验证非 TCP 控制传输真的进了引擎的建链路径：控制入口由 Core 自建监听器
// （宿主不注入 TCP listener），客户端按同一传输拨号并完成登录。只在传输单测里
// 通过是不够的——引擎接线或监听器所有权出错时，单测仍会全绿而互操作必然失败。

// startNonStreamPair 用指定传输启动一对 Engine，返回是否登录成功。
//
// TCP 之外的传输由 Core 自建监听器，因此这里刻意不注入宿主 listener。
func startNonStreamPair(t *testing.T, ctx context.Context, transport core.Transport) error {
	t.Helper()
	control := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(freePort(t)))

	serverConfig, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{Address: control, Transport: transport}),
		core.WithWire(core.WireV1),
		core.WithClientCredential(core.ClientCredential{ClientID: testClientID, Token: server.DigestToken(testClientToken)}),
		core.WithServerHeartbeat(200*time.Millisecond),
		core.WithServerTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("构造服务端配置失败：%v", err)
	}
	// 日志留在缓冲里：失败时连同错误一起打印，否则非 TCP 建链失败只剩一句超时。
	logs := &bytes.Buffer{}
	serverEngine := server.New(serverConfig, server.WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
	if err := serverEngine.Start(ctx); err != nil {
		t.Fatalf("服务端启动失败：%v，日志：%s", err, logs.String())
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("服务端日志：%s", logs.String())
		}
	})
	t.Cleanup(func() { _ = serverEngine.Shutdown(context.Background()) })

	clientConfig, err := core.NewClientConfig(
		core.WithClientID(testClientID),
		core.WithServerEndpoint(core.ServerEndpoint{Address: control, Transport: transport, Wire: core.WireV1}),
		core.WithClientAuth(core.TokenAuth{Token: testClientToken}),
		core.WithHeartbeat(200*time.Millisecond),
		core.WithTimeout(3*time.Second),
	)
	if err != nil {
		t.Fatalf("构造客户端配置失败：%v", err)
	}
	clientEngine := client.New(clientConfig)
	startErr := clientEngine.Start(ctx)
	t.Cleanup(func() { _ = clientEngine.Shutdown(context.Background()) })
	return startErr
}

// testCertificatePEM 生成本机自签名证书，返回证书链与私钥的 PEM。
//
// 证书必须带 IP SAN：仅凭 Common Name 的证书会被现代 TLS 校验拒绝
// （"certificate relies on legacy Common Name field"），无法用于本地回环互操作。
func testCertificatePEM(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败：%v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书失败：%v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("序列化私钥失败：%v", err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	privateKey := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return string(certificate), string(privateKey)
}

// TestQUICProxyRoundTrip 覆盖 QUIC 上的工作连接：控制连接与工作连接是同一条
// QUIC 会话上的两条流，按会话映射的连接抽象会让访客永久等待。
func TestQUICProxyRoundTrip(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	control := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(freePort(t)))
	guestPort := freePort(t)
	certificate, privateKey := testCertificatePEM(t)

	serverConfig, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{
			Address:   control,
			Transport: core.TransportQUIC,
			TransportConfig: core.TransportConfig{
				QUIC: core.QUICConfig{TLS: core.TLSConfig{CertificatePEM: certificate, PrivateKeyPEM: privateKey}},
			},
		}),
		core.WithWire(core.WireV1),
		core.WithClientCredential(core.ClientCredential{ClientID: testClientID, Token: server.DigestToken(testClientToken)}),
		core.WithTCPProxyBinding(core.TCPProxyBinding{
			Name:           testProxyName,
			ClientID:       testClientID,
			RemotePort:     guestPort,
			AllowedTargets: []netip.AddrPort{target},
		}),
		core.WithServerHeartbeat(200*time.Millisecond),
		core.WithServerTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("构造服务端配置失败：%v", err)
	}
	serverEngine := server.New(serverConfig)
	if err := serverEngine.Start(ctx); err != nil {
		t.Fatalf("服务端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = serverEngine.Shutdown(context.Background()) })

	clientConfig, err := core.NewClientConfig(
		core.WithClientID(testClientID),
		core.WithServerEndpoint(core.ServerEndpoint{
			Address:   control,
			Transport: core.TransportQUIC,
			Wire:      core.WireV1,
			TransportConfig: core.TransportConfig{
				QUIC: core.QUICConfig{TLS: core.TLSConfig{RootCAPEM: certificate, ServerName: "127.0.0.1"}},
			},
		}),
		core.WithClientAuth(core.TokenAuth{Token: testClientToken}),
		core.WithTCPProxy(core.TCPProxy{Name: testProxyName, LocalAddr: target, RemotePort: guestPort}),
		core.WithHeartbeat(200*time.Millisecond),
		core.WithTimeout(3*time.Second),
	)
	if err != nil {
		t.Fatalf("构造客户端配置失败：%v", err)
	}
	clientEngine := client.New(clientConfig)
	if err := clientEngine.Start(ctx); err != nil {
		t.Fatalf("客户端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = clientEngine.Shutdown(context.Background()) })

	guest, err := net.Dial("tcp", serverEngine.GuestAddr(testProxyName).String())
	if err != nil {
		t.Fatalf("访客连接入口失败：%v", err)
	}
	defer func() { _ = guest.Close() }()
	if err := echoOnceOn(guest, []byte("QUIC 工作连接回显")); err != nil {
		t.Fatalf("QUIC 代理回显失败：%v", err)
	}
}

// TestKCPControlSessionEstablishes 覆盖 KCP 控制入口：客户端经 KCP 完成登录。
func TestKCPControlSessionEstablishes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := startNonStreamPair(t, ctx, core.TransportKCP); err != nil {
		t.Fatalf("KCP 控制会话建立失败：%v", err)
	}
}

// TestWebSocketControlSessionEstablishes 覆盖 WebSocket 控制入口。
func TestWebSocketControlSessionEstablishes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := startNonStreamPair(t, ctx, core.TransportWebSocket); err != nil {
		t.Fatalf("WebSocket 控制会话建立失败：%v", err)
	}
}
