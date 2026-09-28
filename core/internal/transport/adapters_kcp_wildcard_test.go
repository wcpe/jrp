package transport_test

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/wcpe/jrp/core/internal/transport"
)

// TestKCPWildcardListenAcceptsIPv4Dial 覆盖通配监听地址 + IPv4 拨号。
//
// 黑盒互操作里服务端按配置监听 0.0.0.0，客户端却拨 127.0.0.1：若通配监听在
// 某些平台上落成只接受 IPv6 的套接字，回环 IPv4 报文会被静默丢弃，表现为
// "客户端连不上而服务端没有任何日志"。该组合必须单独验证。
func TestKCPWildcardListenAcceptsIPv4Dial(t *testing.T) {
	port := reserveFreeUDPPort(t)
	options := transport.KCPOptions{
		MTU:           1350,
		SendWindow:    1024,
		ReceiveWindow: 1024,
		DataShards:    10,
		ParityShards:  3,
		NoDelay:       true,
		Interval:      20 * time.Millisecond,
		Resend:        2,
		NoCongestion:  true,
	}
	listener, err := transport.ListenKCP(net.JoinHostPort("0.0.0.0", strconv.Itoa(port)), options)
	if err != nil {
		t.Fatalf("KCP 监听失败：%v", err)
	}
	defer func() { _ = listener.Release() }()
	t.Logf("KCP 监听地址：%s", listener.Addr().String())

	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	accepted := make(chan *transport.Conn, 1)
	go func() {
		conn, _, acceptErr := listener.Accept(transport.PurposeControl)
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := transport.DialKCP(ctx, target, options, transport.PurposeControl, "")
	if err != nil {
		t.Fatalf("KCP 拨号失败：%v", err)
	}
	defer func() { _ = client.Close() }()
	// KCP 在首次写入前不发送任何报文，服务端因此不会建立会话：必须先写再等接受。
	if _, err := client.Write([]byte("kcp-wildcard")); err != nil {
		t.Fatalf("客户端写入失败：%v", err)
	}

	select {
	case server := <-accepted:
		defer func() { _ = server.Close() }()
		buffer := make([]byte, len("kcp-wildcard"))
		_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := server.Read(buffer); err != nil {
			t.Fatalf("服务端读取失败：%v", err)
		}
		if string(buffer) != "kcp-wildcard" {
			t.Fatalf("载荷不一致：%q", buffer)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("通配监听未在期限内接受 IPv4 回环连接")
	}
}

// reserveFreeUDPPort 申请一个空闲的 UDP 端口号。
func reserveFreeUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("申请 UDP 端口失败：%v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	_ = conn.Close()
	return port
}
