package core_test

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/client"
	"github.com/wcpe/jrp/core/server"
)

// sliceServerConfigP2 构造与 sliceConfigs 同形、仅入口端口与排水上限不同的配置。
//
// 不直接复用 sliceConfigs：它不接受排水上限参数，而本用例需要一个短上限作为
// 安全兜底（旧流由测试主动关闭，正常路径不会等到上限）。
func sliceServerConfigP2(t *testing.T, control netip.AddrPort, target netip.AddrPort, guestPort int) core.ServerConfig {
	t.Helper()
	config, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{Address: control, Transport: core.TransportTCP}),
		core.WithWire(core.WireV1),
		core.WithClientCredential(core.ClientCredential{ClientID: testClientID, Token: testClientToken}),
		core.WithTCPProxyBinding(core.TCPProxyBinding{
			Name:           testProxyName,
			ClientID:       testClientID,
			RemotePort:     guestPort,
			AllowedTargets: []netip.AddrPort{target},
		}),
		core.WithServerHeartbeat(200*time.Millisecond),
		core.WithServerTimeout(2*time.Second),
		core.WithServerDrainTimeout(3*time.Second),
	)
	if err != nil {
		t.Fatalf("构造 P2 服务端配置失败：%v", err)
	}
	return config
}

// clientConfigForControl 按实际控制地址重建客户端配置。
//
// sliceConfigs 产出的客户端配置里控制地址只表达意图，Engine 实际拨号的是
// ServerEndpoint，因此这里把它指向真实监听地址。
func clientConfigForControl(t *testing.T, control netip.AddrPort, target netip.AddrPort, guestPort int) core.ClientConfig {
	t.Helper()
	config, err := core.NewClientConfig(
		core.WithClientID(testClientID),
		core.WithServerEndpoint(core.ServerEndpoint{Address: control, Transport: core.TransportTCP, Wire: core.WireV1}),
		core.WithClientAuth(core.TokenAuth{Token: testClientToken}),
		core.WithTCPProxy(core.TCPProxy{Name: testProxyName, LocalAddr: target, RemotePort: guestPort}),
		core.WithHeartbeat(200*time.Millisecond),
		core.WithTimeout(2*time.Second),
		core.WithClientDrainTimeout(3*time.Second),
	)
	if err != nil {
		t.Fatalf("构造客户端配置失败：%v", err)
	}
	return config
}

// TestApplySwitchKeepsActiveStreamThenDrainsOldPort 验证换代时的流连续性与入口切换。
//
// 这是本功能的验收核心（规格 §5）：旧代上已建立的桥接在切换期间保持可用且传输
// 完整；新访客只能进入新端口；旧端口在 drain 后拒绝新连接。
//
// 时序上两端 Apply 都会卡在 drain（旧桥还开着），因此在后台进行。客户端一并换代
// 是本用例的关键：TCP 代理的维持循环对单个代理是单连接串行，客户端不换代时唯一
// 的工作连接会被旧桥一直占着，新访客永远等不到配对。
func TestApplySwitchKeepsActiveStreamThenDrainsOldPort(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	controlListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听控制端口失败：%v", err)
	}
	control := mustAddrPort(t, controlListener.Addr().String())

	guestPort1 := freePort(t)
	guestPort2 := freePort(t)
	serverP1 := sliceServerConfigP2(t, control, target, guestPort1)
	serverP2 := sliceServerConfigP2(t, control, target, guestPort2)
	clientP1 := clientConfigForControl(t, control, target, guestPort1)

	serverEngine := server.New(serverP1, server.WithListener(controlListener))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := serverEngine.Start(ctx); err != nil {
		t.Fatalf("服务端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = serverEngine.Shutdown(context.Background()) })

	clientEngine := client.New(clientP1)
	if err := clientEngine.Start(ctx); err != nil {
		t.Fatalf("客户端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = clientEngine.Shutdown(context.Background()) })

	// 旧访客先建桥：切换前的基线，回显必须成功。
	oldAddr := serverEngine.GuestAddr(testProxyName).String()
	oldGuest, err := net.Dial("tcp", oldAddr)
	if err != nil {
		t.Fatalf("旧访客连接失败：%v", err)
	}
	defer func() { _ = oldGuest.Close() }()
	if err := echoOnceOn(oldGuest, []byte("A")); err != nil {
		t.Fatalf("切换前基线回显失败：%v", err)
	}

	// 后台切换：服务端换访客入口端口，客户端换一代。
	applyDone := make(chan error, 2)
	go func() {
		_, applyErr := serverEngine.Apply(ctx, server.Deployment{Revision: 1, Config: serverP2})
		applyDone <- applyErr
	}()
	go func() {
		_, applyErr := clientEngine.Apply(ctx, client.Deployment{Revision: 1, Config: clientP1})
		applyDone <- applyErr
	}()

	waitForBothPublished(t, serverEngine, clientEngine, oldAddr)

	// 切换窗口内旧流必须连续：这是"无中断"的直接证据。
	if err := echoOnceOn(oldGuest, []byte("B")); err != nil {
		t.Fatalf("切换期间旧流回显失败（流被切断）：%v", err)
	}

	// 新旧流并发：旧桥仍活着时，新访客必须能在新端口上被服务。这正是客户端换代
	// 要保证的事——新代有自己的维持循环，不被旧桥占着。
	newGuest, err := net.Dial("tcp", serverEngine.GuestAddr(testProxyName).String())
	if err != nil {
		t.Fatalf("新访客连接新端口失败：%v", err)
	}
	defer func() { _ = newGuest.Close() }()
	if err := echoOnceOn(newGuest, []byte("C")); err != nil {
		t.Fatalf("旧桥仍在时新端口回显失败：%v", err)
	}
	// 再回到旧流：两条流此时确实并存，而不是旧流先被切断。
	if err := echoOnceOn(oldGuest, []byte("D")); err != nil {
		t.Fatalf("新流建立后旧流回显失败：%v", err)
	}

	// 关闭两条流放行两端 drain，两次 Apply 都应收敛。
	_ = oldGuest.Close()
	_ = newGuest.Close()
	for range 2 {
		select {
		case applyErr := <-applyDone:
			if applyErr != nil {
				t.Fatalf("切换失败：%v", applyErr)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("关闭流后 Apply 未收敛（drain 挂死）")
		}
	}
	if clientEngine.ActiveRevision() != 1 || clientEngine.LastGoodRevision() != 1 {
		t.Fatalf("客户端 active=%d last-good=%d，均应为 1",
			clientEngine.ActiveRevision(), clientEngine.LastGoodRevision())
	}

	// 旧端口必须拒绝新连接：它已被 drain 释放。
	dialer := net.Dialer{Timeout: 2 * time.Second}
	if probe, dialErr := dialer.DialContext(ctx, "tcp", oldAddr); dialErr == nil {
		_ = probe.Close()
		t.Fatal("旧端口仍接受新连接：drain 未真正释放入口")
	}
}

// waitForBothPublished 等两端完成 publish：服务端入口换到新地址，客户端 active 推进。
//
// 轮询而不是等 Apply 返回：两端 Apply 都会卡在 drain 直到旧流结束，而本用例要在
// 切换窗口内验证新旧流并存。
func waitForBothPublished(t *testing.T, serverEngine *server.Engine, clientEngine *client.Engine, oldAddr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		guestAddr := serverEngine.GuestAddr(testProxyName).String()
		if guestAddr != oldAddr && clientEngine.ActiveRevision() == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("publish 未完成：服务端入口 %s，客户端 active %d",
				guestAddr, clientEngine.ActiveRevision())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
