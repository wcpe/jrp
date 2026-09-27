package transport_test

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/internal/transport"
)

func TestCoreTransportOptionsRemainThirdPartyFree(t *testing.T) {
	ws, err := transport.WebSocketOptionsFromConfig(core.WebSocketConfig{Path: "/data", Header: "X-JRP: test"}, false)
	if err != nil {
		t.Fatalf("转换 WebSocket 配置失败：%v", err)
	}
	if ws.Path != "/data" || ws.Headers.Get("X-JRP") != "test" {
		t.Fatalf("WebSocket 配置转换结果不正确：%+v", ws)
	}

	kcpOptions, err := transport.KCPOptionsFromConfig(core.KCPConfig{MTU: 1200, Interval: 50 * time.Millisecond})
	if err != nil || kcpOptions.MTU != 1200 {
		t.Fatalf("KCP 配置转换失败：%v %+v", err, kcpOptions)
	}

	quicOptions, err := transport.QUICOptionsFromConfig(core.QUICConfig{ALPN: "jrp"}, false)
	if err != nil || quicOptions.TLS == nil {
		t.Fatalf("QUIC 配置转换失败：%v", err)
	}
}

func TestWebSocketAdapterRoundTrip(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("创建监听器失败：%v", err)
	}
	adapter := transport.NewWebSocketListener(listener, transport.WebSocketOptions{Path: "/data"})
	defer adapter.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := adapter.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	conn, err := transport.DialWebSocket(context.Background(), listener.Addr().String(), transport.WebSocketOptions{Path: "/data"}, transport.PurposeControl, "")
	if err != nil {
		t.Fatalf("WebSocket 拨号失败：%v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("WebSocket 写入失败：%v", err)
	}

	select {
	case serverConn := <-accepted:
		defer serverConn.Close()
		buffer := make([]byte, 5)
		if _, err := serverConn.Read(buffer); err != nil {
			t.Fatalf("WebSocket 读取失败：%v", err)
		}
		if string(buffer) != "hello" {
			t.Fatalf("WebSocket 载荷不一致：%q", buffer)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待 WebSocket 连接超时")
	}
}

func TestCoreTransportValuesAreAccepted(t *testing.T) {
	endpoint := core.ServerEndpoint{Address: netip.MustParseAddrPort("127.0.0.1:7000"), Transport: core.TransportQUIC, Wire: core.WireV1}
	if _, err := core.NewClientConfig(core.WithClientID("client"), core.WithServerEndpoint(endpoint), core.WithClientAuth(core.TokenAuth{Token: "token"})); err != nil {
		t.Fatalf("QUIC 端点配置不应失败：%v", err)
	}
}
