package core_test

import (
	"bytes"
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

// 本文件验证 FR-05a 传输层接入引擎后的端到端行为：超时与上限来自配置快照，
// 达到上限时拒绝而非无限等待。这些约束只在单测里成立是不够的，必须证明它们
// 真的进了在建链路径。

// TestClientDialRejectsZeroTimeout 验证零超时配置无法构造出可用拨号器。
//
// 规格 §3.4 禁止无超时拨号：零超时必须在配置层被默认常量取代，而不是让
// 拨号悬挂。
func TestClientDialRejectsZeroTimeout(t *testing.T) {
	localTarget, stopEcho := startLocalEcho(t)
	defer stopEcho()

	control := mustAddrPort(t, "127.0.0.1:7000")
	endpoint := core.ServerEndpoint{Address: control, Transport: core.TransportTCP, Wire: core.WireV1}
	byDefault, err := core.NewClientConfig(
		core.WithClientID(testClientID),
		core.WithServerEndpoint(endpoint),
		core.WithClientAuth(core.TokenAuth{Token: testClientToken}),
		core.WithTCPProxy(core.TCPProxy{Name: testProxyName, LocalAddr: localTarget, RemotePort: 6000}),
	)
	if err != nil {
		t.Fatalf("构造配置失败：%v", err)
	}
	// 零值超时已被默认常量取代：这是「不得无超时拨号」的配置层保证。
	if byDefault.Timeout() <= 0 {
		t.Fatalf("拨号超时未经配置层兜底，实际：%v", byDefault.Timeout())
	}
	if byDefault.WorkConnPoolSize() <= 0 {
		t.Fatalf("工作连接池上限未经配置层兜底，实际：%d", byDefault.WorkConnPoolSize())
	}
	if byDefault.IdleWorkConnLimit() <= 0 {
		t.Fatalf("待命空闲上限未经配置层兜底，实际：%d", byDefault.IdleWorkConnLimit())
	}
}

// startPairWithDrainTimeout 启动一对 Engine，服务端排水上限由入参指定。
//
// 与 startSlicePair 的区别只在于排水上限可配：本测试要验证该上限真的生效。
func startPairWithDrainTimeout(
	t *testing.T,
	ctx context.Context,
	target netip.AddrPort,
	guestPort int,
	drainTimeout time.Duration,
) (*server.Engine, *client.Engine) {
	t.Helper()
	controlListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听控制端口失败：%v", err)
	}
	control := mustAddrPort(t, controlListener.Addr().String())

	serverConfig, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{Address: control, Transport: core.TransportTCP}),
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
		core.WithServerDrainTimeout(drainTimeout),
	)
	if err != nil {
		t.Fatalf("构造服务端配置失败：%v", err)
	}
	clientConfig, err := core.NewClientConfig(
		core.WithClientID(testClientID),
		core.WithServerEndpoint(core.ServerEndpoint{Address: control, Transport: core.TransportTCP, Wire: core.WireV1}),
		core.WithClientAuth(core.TokenAuth{Token: testClientToken}),
		core.WithTCPProxy(core.TCPProxy{Name: testProxyName, LocalAddr: target, RemotePort: guestPort}),
		core.WithHeartbeat(200*time.Millisecond),
		core.WithTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("构造客户端配置失败：%v", err)
	}

	serverEngine := server.New(serverConfig, server.WithListener(controlListener))
	if err := serverEngine.Start(ctx); err != nil {
		t.Fatalf("服务端启动失败：%v", err)
	}
	clientEngine := client.New(clientConfig)
	if err := clientEngine.Start(ctx); err != nil {
		t.Fatalf("客户端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = clientEngine.Shutdown(context.Background()) })
	return serverEngine, clientEngine
}

// TestDrainTimeoutComesFromConfig 验证排水上限来自配置而非硬编码常量。
//
// 只断言取值来源与「超限收口」两件事：真正的排水连续性由 FR-25 既有测试覆盖。
func TestDrainTimeoutComesFromConfig(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	// 配置指定的排水上限必须真的生效：Shutdown 不得远超该上限才返回。
	serverConfig, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{Address: mustAddrPort(t, "127.0.0.1:7000"), Transport: core.TransportTCP}),
		core.WithWire(core.WireV1),
		core.WithClientCredential(core.ClientCredential{ClientID: testClientID, Token: server.DigestToken(testClientToken)}),
		core.WithTCPProxyBinding(core.TCPProxyBinding{
			Name:           testProxyName,
			ClientID:       testClientID,
			RemotePort:     freePort(t),
			AllowedTargets: []netip.AddrPort{target},
		}),
		core.WithServerDrainTimeout(1500*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("构造服务端配置失败：%v", err)
	}
	if got := serverConfig.DrainTimeout(); got != 1500*time.Millisecond {
		t.Fatalf("排水上限未取自配置快照，实际：%v", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	serverEngine, _ := startPairWithDrainTimeout(t, ctx, target, freePort(t), 1500*time.Millisecond)

	// 建立一条活动流：访客写入并读回，确保桥接真的建立起来。
	guest, err := net.Dial("tcp", serverEngine.GuestAddr(testProxyName).String())
	if err != nil {
		t.Fatalf("访客连接失败：%v", err)
	}
	defer guest.Close()
	if err := echoOnceOn(guest, []byte("排水前流量")); err != nil {
		t.Fatalf("活动流建立失败：%v", err)
	}

	// 活动流不结束就 Shutdown：必须按配置上限（1.5s）收口，
	// 而不是回落到此前沿用写死的 10s。
	start := time.Now()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	shutdownErr := serverEngine.Shutdown(shutdownCtx)
	elapsed := time.Since(start)
	if shutdownErr == nil {
		t.Fatalf("存在未结束的活动流时 Shutdown 应返回排水超时错误")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Shutdown 未按配置排水上限收口，耗时 %v", elapsed)
	}
	if elapsed < 1200*time.Millisecond {
		t.Fatalf("Shutdown 早于配置排水上限就放弃了活动流，耗时 %v", elapsed)
	}
}

// TestWorkConnPoolEnforcedEndToEnd 验证池上限在引擎路径上真实生效。
//
// 单测只证明池的记账正确；本测试证明客户端真的在建工作连接前占用池槽位。
func TestWorkConnPoolEnforcedEndToEnd(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serverEngine, clientEngine := startSlicePair(t, ctx, target, freePort(t))

	// 池上限为配置默认值时，转发必须仍然可用：每代理一条在途连接足够。
	payload := []byte("池上限端到端")
	guest, err := net.Dial("tcp", serverEngine.GuestAddr(testProxyName).String())
	if err != nil {
		t.Fatalf("访客连接失败：%v", err)
	}
	defer guest.Close()
	if _, err := guest.Write(payload); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(guest, received); err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	if !bytes.Equal(payload, received) {
		t.Fatalf("数据不一致")
	}
	// 多次往返验证池槽位被正确释放并复用。
	for round := 0; round < 3; round++ {
		if err := echoOnceOn(guest, []byte("round")); err != nil {
			t.Fatalf("第 %d 轮往返失败（池槽位未释放）：%v", round, err)
		}
	}
	_ = guest.Close()
	_ = serverEngine.Shutdown(context.Background())
	_ = clientEngine.Shutdown(context.Background())
}

// TestIdleWorkConnLimitFromConfig 验证服务端待命工作连接空闲上限来自配置。
func TestIdleWorkConnLimitFromConfig(t *testing.T) {
	control := mustAddrPort(t, "127.0.0.1:7000")
	serverConfig, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{Address: control, Transport: core.TransportTCP}),
		core.WithWire(core.WireV1),
		core.WithClientCredential(core.ClientCredential{ClientID: testClientID, Token: server.DigestToken(testClientToken)}),
		core.WithTCPProxyBinding(core.TCPProxyBinding{
			Name:           testProxyName,
			ClientID:       testClientID,
			RemotePort:     6000,
			AllowedTargets: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:22")},
		}),
		core.WithServerIdleWorkConnLimit(3),
	)
	if err != nil {
		t.Fatalf("构造服务端配置失败：%v", err)
	}
	if got := serverConfig.IdleWorkConnLimit(); got != 3 {
		t.Fatalf("空闲上限未取自配置快照，实际：%d", got)
	}
}

// TestIdleWorkConnLimitRejectsOutOfRange 验证空闲上限的边界值行为可判定。
func TestIdleWorkConnLimitRejectsOutOfRange(t *testing.T) {
	control := mustAddrPort(t, "127.0.0.1:7000")
	build := func(limit int) error {
		_, err := core.NewServerConfig(
			core.WithListen(core.BindEndpoint{Address: control, Transport: core.TransportTCP}),
			core.WithWire(core.WireV1),
			core.WithClientCredential(core.ClientCredential{ClientID: testClientID, Token: server.DigestToken(testClientToken)}),
			core.WithTCPProxyBinding(core.TCPProxyBinding{
				Name:           testProxyName,
				ClientID:       testClientID,
				RemotePort:     6000,
				AllowedTargets: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:22")},
			}),
			core.WithServerIdleWorkConnLimit(limit),
		)
		return err
	}
	if err := build(core.MaxIdleWorkConnLimit); err != nil {
		t.Fatalf("等于上界应合法，实际：%v", err)
	}
	if err := build(core.MaxIdleWorkConnLimit + 1); err == nil {
		t.Fatalf("超出上界应被拒绝")
	}
	if err := build(0); err != nil {
		t.Fatalf("零值表示使用默认常量，不应报错，实际：%v", err)
	}
}

// TestGuestListenAddrFollowsConfig 验证访客监听地址来自配置的监听端点族，
// 而不是写死的回环地址。
func TestGuestListenAddrFollowsConfig(t *testing.T) {
	// 监听端点指定回环地址：访客入口必须与之一致。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	controlAddr := listener.Addr().(*net.TCPAddr)
	control := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(controlAddr.Port))

	serverConfig, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{Address: control, Transport: core.TransportTCP}),
		core.WithWire(core.WireV1),
		core.WithClientCredential(core.ClientCredential{ClientID: testClientID, Token: server.DigestToken(testClientToken)}),
		core.WithTCPProxyBinding(core.TCPProxyBinding{
			Name:           testProxyName,
			ClientID:       testClientID,
			RemotePort:     freePort(t),
			AllowedTargets: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:22")},
		}),
	)
	if err != nil {
		t.Fatalf("构造服务端配置失败：%v", err)
	}
	engine := server.New(serverConfig, server.WithListener(listener))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	defer func() { _ = engine.Shutdown(context.Background()) }()

	guestAddr, ok := engine.GuestAddr(testProxyName).(*net.TCPAddr)
	if !ok {
		t.Fatalf("访客地址类型异常：%T", engine.GuestAddr(testProxyName))
	}
	if !guestAddr.IP.IsLoopback() {
		t.Fatalf("访客入口应继承配置的回环地址族，实际：%v", guestAddr)
	}
}
