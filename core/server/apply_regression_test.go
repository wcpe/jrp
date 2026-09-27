package server_test

import (
	"context"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/client"
	"github.com/wcpe/jrp/core/server"
)

// TestApplyKeepsStagedWorkConnUsable 固定「等价配置二次应用后新访客立即可用」。
//
// 曾经失败：换代重建配对中心时只丢弃旧中心的待命工作连接而不关闭，客户端的
// 维持循环误以为仍有待命连接而不补建，新访客于是一直暂存到失活超时——在 -race
// 下必现（12–13/15），main 上只是被时序掩盖。修复是换代时关闭旧中心的待命
// 连接（访客不在此列），让客户端立刻补建。
func TestApplyKeepsStagedWorkConnUsable(t *testing.T) {
	const clientID = "regression-client"
	const token = "regression-token"
	const proxyName = "svc-echo"

	// 回显目标：访客流量最终被桥接到这里。
	target, stopEcho := startEchoTarget(t)
	defer stopEcho()

	controlListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听控制端口失败：%v", err)
	}
	t.Cleanup(func() { _ = controlListener.Close() })
	controlPort := controlListener.Addr().(*net.TCPAddr).Port

	guestPort := reservePort(t)
	serverConfig := func(revision uint64) core.ServerConfig {
		config, err := core.NewServerConfig(
			core.WithListen(core.BindEndpoint{
				Address:   netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(controlPort)),
				Transport: core.TransportTCP,
			}),
			core.WithWire(core.WireV1),
			core.WithClientCredential(core.ClientCredential{ClientID: clientID, Token: server.DigestToken(token)}),
			core.WithTCPProxyBinding(core.TCPProxyBinding{
				Name:           proxyName,
				ClientID:       clientID,
				RemotePort:     guestPort,
				AllowedTargets: []netip.AddrPort{target},
			}),
		)
		if err != nil {
			t.Fatalf("构造服务端配置失败：%v", err)
		}
		return config
	}

	engine := server.New(serverConfig(0), server.WithListener(controlListener))
	if err := engine.Start(context.Background()); err != nil {
		t.Fatalf("服务端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })

	clientConfig, err := core.NewClientConfig(
		core.WithClientID(clientID),
		core.WithServerEndpoint(core.ServerEndpoint{
			Address:   netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(controlPort)),
			Transport: core.TransportTCP,
			Wire:      core.WireV1,
		}),
		core.WithClientAuth(core.TokenAuth{Token: token}),
		core.WithTCPProxy(core.TCPProxy{Name: proxyName, LocalAddr: target, RemotePort: guestPort}),
		core.WithHeartbeat(200*time.Millisecond),
		core.WithTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("构造客户端配置失败：%v", err)
	}
	clientEngine := client.New(clientConfig)
	if err := clientEngine.Start(context.Background()); err != nil {
		t.Fatalf("客户端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = clientEngine.Shutdown(context.Background()) })

	// 首次应用建立第 1 代，客户端随后建好待命工作连接。
	if _, err := engine.Apply(context.Background(), server.Deployment{Revision: 1, Config: serverConfig(1)}); err != nil {
		t.Fatalf("首次应用失败：%v", err)
	}
	waitForStagedWorkConn(t, engine, proxyName)

	// 等价配置二次应用：内容不变、仅推进版本。
	if _, err := engine.Apply(context.Background(), server.Deployment{Revision: 2, Config: serverConfig(2)}); err != nil {
		t.Fatalf("二次应用失败：%v", err)
	}

	// 新访客必须立刻配对到待命连接，而不是暂存到失活超时。
	deadline := time.Now().Add(5 * time.Second)
	for {
		guest, dialErr := net.Dial("tcp", engine.GuestAddr(proxyName).String())
		if dialErr == nil {
			if echoErr := echoOnce(guest, []byte("换代后新流")); echoErr == nil {
				return
			}
			_ = guest.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("换代后新访客回显失败：%v", dialErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForStagedWorkConn 等待配对中心出现该代理的待命工作连接。
func waitForStagedWorkConn(t *testing.T, engine *server.Engine, proxyName string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if engine.StagedWorkConns(proxyName) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("等待待命工作连接超时：客户端没有为换代备好连接")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startEchoTarget 启动本地回显服务，返回其地址与停止函数。
func startEchoTarget(t *testing.T) (netip.AddrPort, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听回显目标失败：%v", err)
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).AddrPort(), func() {
		_ = listener.Close()
		close(done)
	}
}

// reservePort 申请一个当前空闲的 TCP 端口号。
func reservePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请端口失败：%v", err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port
}

// echoOnce 发送载荷并读回，断言逐字节一致。
func echoOnce(connection net.Conn, payload []byte) error {
	if _, err := connection.Write(payload); err != nil {
		return err
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer func() { _ = connection.SetReadDeadline(time.Time{}) }()
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(connection, received); err != nil {
		return err
	}
	for index := range payload {
		if received[index] != payload[index] {
			return io.ErrShortBuffer
		}
	}
	return nil
}
