package core_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/client"
	"github.com/wcpe/jrp/core/server"
)

// applySceneServerConfig 构造一个按名指定 TCP 入口端口的服务端配置。
//
// 按名传端口才能表达"某个代理的端口不变、另一个变"这类真实变更；opts 追加在
// 基础选项之后，用于覆盖单字段（例如排水上限）。
func applySceneServerConfig(
	t *testing.T, control netip.AddrPort, target netip.AddrPort,
	tcpPorts map[string]int, opts ...core.ServerOption,
) core.ServerConfig {
	t.Helper()
	options := []core.ServerOption{
		core.WithListen(core.BindEndpoint{Address: control, Transport: core.TransportTCP}),
		core.WithWire(core.WireV1),
		core.WithClientCredential(core.ClientCredential{ClientID: testClientID, Token: testClientToken}),
		core.WithServerHeartbeat(200 * time.Millisecond),
		core.WithServerTimeout(2 * time.Second),
		core.WithServerDrainTimeout(3 * time.Second),
	}
	for name, port := range tcpPorts {
		options = append(options, core.WithTCPProxyBinding(core.TCPProxyBinding{
			Name: name, ClientID: testClientID, RemotePort: port,
			AllowedTargets: []netip.AddrPort{target},
		}))
	}
	config, err := core.NewServerConfig(append(options, opts...)...)
	if err != nil {
		t.Fatalf("构造服务端配置失败：%v", err)
	}
	return config
}

// applySceneClientConfig 构造与 applySceneServerConfig 匹配的客户端配置。
func applySceneClientConfig(
	t *testing.T, control netip.AddrPort, target netip.AddrPort, tcpPorts map[string]int,
) core.ClientConfig {
	t.Helper()
	options := []core.ClientOption{
		core.WithClientID(testClientID),
		core.WithServerEndpoint(core.ServerEndpoint{Address: control, Transport: core.TransportTCP, Wire: core.WireV1}),
		core.WithClientAuth(core.TokenAuth{Token: testClientToken}),
		core.WithHeartbeat(200 * time.Millisecond),
		core.WithTimeout(2 * time.Second),
		core.WithClientDrainTimeout(3 * time.Second),
	}
	for name, port := range tcpPorts {
		options = append(options, core.WithTCPProxy(core.TCPProxy{Name: name, LocalAddr: target, RemotePort: port}))
	}
	config, err := core.NewClientConfig(options...)
	if err != nil {
		t.Fatalf("构造客户端配置失败：%v", err)
	}
	return config
}

// sceneConfigs 按**实际**控制地址构造两端配置。
//
// 控制地址只能先监听 :0 再回填，否则配置里的地址与实际监听地址不一致，客户端
// 会连到无人监听的端口。
type sceneConfigs struct {
	server func(control netip.AddrPort) core.ServerConfig
	client func(control netip.AddrPort) core.ClientConfig
}

// startSceneEngines 启动一对互相匹配的引擎，返回它们与实际控制地址。
func startSceneEngines(
	t *testing.T, configs sceneConfigs,
) (*server.Engine, *client.Engine, netip.AddrPort) {
	t.Helper()
	controlListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听控制端口失败：%v", err)
	}
	control := mustAddrPort(t, controlListener.Addr().String())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	serverEngine := server.New(configs.server(control), server.WithListener(controlListener))
	if err := serverEngine.Start(ctx); err != nil {
		t.Fatalf("服务端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = serverEngine.Shutdown(context.Background()) })

	clientEngine := client.New(configs.client(control))
	if err := clientEngine.Start(ctx); err != nil {
		t.Fatalf("客户端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = clientEngine.Shutdown(context.Background()) })
	return serverEngine, clientEngine, control
}

// TestApplyUnchangedProxyKeepsEstablishedStream 验证规格 §5 的"未变化资源复用"。
//
// 两次 Apply 的代理集合完全相同（入口端口、目标地址、代理名都不变），只有与代理
// 无关的排水上限不同。已有访客流必须在切换期间与切换之后都保持可用：如果实现把
// "新 revision"一律当作"重建全部资源"，入口会被关掉重开，端到端表现为流被切断。
//
// 与换代用例的区别：那条换端口，验证入口切换与 drain 收敛；本用例不换任何代理
// 资源，验证"配置变了但资源没变"时活动流不受牵连。
func TestApplyUnchangedProxyKeepsEstablishedStream(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	tcpPorts := map[string]int{testProxyName: freePort(t)}
	serverEngine, clientEngine, control := startSceneEngines(t, sceneConfigs{
		server: func(control netip.AddrPort) core.ServerConfig {
			return applySceneServerConfig(t, control, target, tcpPorts)
		},
		client: func(control netip.AddrPort) core.ClientConfig {
			return applySceneClientConfig(t, control, target, tcpPorts)
		},
	})

	guestAddr := serverEngine.GuestAddr(testProxyName).String()
	guest, err := net.Dial("tcp", guestAddr)
	if err != nil {
		t.Fatalf("访客连接失败：%v", err)
	}
	defer func() { _ = guest.Close() }()
	if err := echoOnceOn(guest, []byte("切换前")); err != nil {
		t.Fatalf("切换前基线回显失败：%v", err)
	}

	// 入口端口不变、只改排水上限：这就是"资源未变化"的条件。
	changedServer := applySceneServerConfig(t, control, target, tcpPorts,
		core.WithServerDrainTimeout(4*time.Second))
	changedClient := applySceneClientConfig(t, control, target, tcpPorts)

	// 两端都在后台切换：旧桥还活着，Apply 会停在 drain。
	serverApply := applyAsync(serverEngine, 1, changedServer)
	clientApply := applyAsyncClient(clientEngine, 1, changedClient)
	waitForBothActive(t, serverEngine, clientEngine, serverApply)

	// 关键断言：入口未变化，旧流在这期间必须一直可用。
	if err := echoOnceOn(guest, []byte("切换中")); err != nil {
		t.Fatalf("代理未变化时旧流被切断：%v", err)
	}
	// 入口端口也没变：新访客应连同一个地址并被服务。
	newGuest, err := net.Dial("tcp", guestAddr)
	if err != nil {
		t.Fatalf("新访客连接失败：%v", err)
	}
	defer func() { _ = newGuest.Close() }()
	if err := echoOnceOn(newGuest, []byte("切换后新流")); err != nil {
		t.Fatalf("未变化端口上的新访客回显失败：%v", err)
	}
	// 回到旧流再验一次：两条流并存，而非旧流先被牺牲。
	if err := echoOnceOn(guest, []byte("切换后旧流")); err != nil {
		t.Fatalf("新流建立后旧流被切断：%v", err)
	}

	// 关闭两条流放行 drain，两端 Apply 收敛。
	_ = guest.Close()
	_ = newGuest.Close()
	assertCleanDrain(t, waitApplies(t, serverApply, 1))
	waitApplies(t, clientApply, 1)
	if serverEngine.ActiveRevision() != 1 || clientEngine.ActiveRevision() != 1 {
		t.Fatalf("两端 active 均应为 1，实际服务端 %d、客户端 %d",
			serverEngine.ActiveRevision(), clientEngine.ActiveRevision())
	}
}

// TestApplyAddingProxyKeepsExistingEntry 覆盖最常见的变更：新增一个代理，已有代理
// 原样保留。旧实现会因已有入口端口仍被旧代占用而在 prepare 直接失败。
func TestApplyAddingProxyKeepsExistingEntry(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	existingPort := freePort(t)
	addedPort := freePort(t)
	before := map[string]int{testProxyName: existingPort}
	after := map[string]int{testProxyName: existingPort, "added-proxy": addedPort}

	serverEngine, clientEngine, control := startSceneEngines(t, sceneConfigs{
		server: func(control netip.AddrPort) core.ServerConfig {
			return applySceneServerConfig(t, control, target, before)
		},
		client: func(control netip.AddrPort) core.ClientConfig {
			return applySceneClientConfig(t, control, target, before)
		},
	})

	existingAddr := serverEngine.GuestAddr(testProxyName).String()
	guest, err := net.Dial("tcp", existingAddr)
	if err != nil {
		t.Fatalf("已有代理访客连接失败：%v", err)
	}
	defer func() { _ = guest.Close() }()
	if err := echoOnceOn(guest, []byte("变更前")); err != nil {
		t.Fatalf("变更前基线回显失败：%v", err)
	}

	changedServer := applySceneServerConfig(t, control, target, after)
	changedClient := applySceneClientConfig(t, control, target, after)

	serverApply := applyAsync(serverEngine, 1, changedServer)
	clientApply := applyAsyncClient(clientEngine, 1, changedClient)
	waitForBothActive(t, serverEngine, clientEngine, serverApply)

	// 已有代理未变化：它的活动流必须连续，入口地址也不变。
	if err := echoOnceOn(guest, []byte("变更中")); err != nil {
		t.Fatalf("新增代理时已有流被切断：%v", err)
	}
	if got := serverEngine.GuestAddr(testProxyName).String(); got != existingAddr {
		t.Fatalf("已有代理入口地址不应变化：%s → %s", existingAddr, got)
	}
	// 已有入口仍在接受新连接：交接只解除旧代的 Accept，没有关闭套接字。
	// 漏掉所有权转移时这里会表现为连接被拒，而"已有流还能用"察觉不到——
	// 已建立的桥接不依赖监听器存活。
	waitForEcho(t, existingAddr, []byte("变更后新连接"))

	// 新增的代理必须真的可用：等待客户端为新代理补上工作连接后回显成功。
	addedAddr := serverEngine.GuestAddr("added-proxy")
	if addedAddr == nil {
		t.Fatal("新增代理缺少入口地址")
	}
	waitForEcho(t, addedAddr.String(), []byte("新增代理"))

	// 已有流仍可用：新增入口没有把旧入口换掉。
	if err := echoOnceOn(guest, []byte("变更后旧流")); err != nil {
		t.Fatalf("新增代理后已有流被切断：%v", err)
	}

	_ = guest.Close()
	assertCleanDrain(t, waitApplies(t, serverApply, 1))
	waitApplies(t, clientApply, 1)
	if serverEngine.ActiveRevision() != 1 {
		t.Fatalf("服务端 active 应为 1，实际 %d", serverEngine.ActiveRevision())
	}
}

// TestApplyChangedUDPParamsOnSamePortRejected 验证"参数变了但端口没变"是可判定的
// 失败，而不是裸的端口占用错误。
//
// UDPProxy 的会话参数在构造期固定，复用对象就必须沿用旧参数；而地址不变意味着
// 不可能重新绑定。两条路都走不通时应当明确拒绝并说明原因，否则宿主只会看到一个
// 与原因无关的 bind 失败。
func TestApplyChangedUDPParamsOnSamePortRejected(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	udpPort := freePort(t)
	buildServer := func(idle time.Duration) func(netip.AddrPort) core.ServerConfig {
		return func(control netip.AddrPort) core.ServerConfig {
			config, err := core.NewServerConfig(
				core.WithListen(core.BindEndpoint{Address: control, Transport: core.TransportTCP}),
				core.WithWire(core.WireV1),
				core.WithClientCredential(core.ClientCredential{ClientID: testClientID, Token: testClientToken}),
				core.WithUDPProxyBinding(core.UDPProxyBinding{
					Name: "udp-scene", ClientID: testClientID, RemotePort: udpPort,
					AllowedTargets: []netip.AddrPort{target},
				}),
				core.WithUDPSessionIdle(idle),
				core.WithServerHeartbeat(200*time.Millisecond),
				core.WithServerTimeout(2*time.Second),
			)
			if err != nil {
				t.Fatalf("构造 UDP 服务端配置失败：%v", err)
			}
			return config
		}
	}
	buildClient := func(control netip.AddrPort) core.ClientConfig {
		config, err := core.NewClientConfig(
			core.WithClientID(testClientID),
			core.WithServerEndpoint(core.ServerEndpoint{Address: control, Transport: core.TransportTCP, Wire: core.WireV1}),
			core.WithClientAuth(core.TokenAuth{Token: testClientToken}),
			core.WithUDPProxy(core.UDPProxy{Name: "udp-scene", LocalAddr: target, RemotePort: udpPort}),
			core.WithHeartbeat(200*time.Millisecond),
			core.WithTimeout(2*time.Second),
		)
		if err != nil {
			t.Fatalf("构造 UDP 客户端配置失败：%v", err)
		}
		return config
	}

	serverEngine, _, control := startSceneEngines(t, sceneConfigs{
		server: buildServer(60 * time.Second),
		client: buildClient,
	})

	_, applyErr := serverEngine.Apply(context.Background(),
		server.Deployment{Revision: 1, Config: buildServer(30 * time.Second)(control)})
	var typed *core.ApplyError
	if !errors.As(applyErr, &typed) || typed.Stage != core.StagePrepare {
		t.Fatalf("同端口改 UDP 参数应在 prepare 阶段被拒绝，实际 %v", applyErr)
	}
	if serverEngine.ActiveRevision() != core.SnapshotRevisionUnknown {
		t.Fatalf("拒绝后 active 不得改变，实际 %d", serverEngine.ActiveRevision())
	}
	// 拒绝原因必须指向参数而非端口占用：否则宿主无从判断该改什么。
	if !strings.Contains(applyErr.Error(), "会话参数") {
		t.Fatalf("拒绝原因未说明参数变更：%v", applyErr)
	}
}

// TestApplyChangedUDPPortKeepsOldEntryServing 验证换 UDP 端口走的是正常换代路径：
// 新端口接管成功、旧端口随旧代关闭，说明 UDP 入口的释放没有漏。
func TestApplyChangedUDPPortKeepsOldEntryServing(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	oldPort := freePort(t)
	newPort := freePort(t)
	buildServer := func(port int) func(netip.AddrPort) core.ServerConfig {
		return func(control netip.AddrPort) core.ServerConfig {
			config, err := core.NewServerConfig(
				core.WithListen(core.BindEndpoint{Address: control, Transport: core.TransportTCP}),
				core.WithWire(core.WireV1),
				core.WithClientCredential(core.ClientCredential{ClientID: testClientID, Token: testClientToken}),
				core.WithUDPProxyBinding(core.UDPProxyBinding{
					Name: "udp-scene", ClientID: testClientID, RemotePort: port,
					AllowedTargets: []netip.AddrPort{target},
				}),
				core.WithServerHeartbeat(200*time.Millisecond),
				core.WithServerTimeout(2*time.Second),
			)
			if err != nil {
				t.Fatalf("构造 UDP 服务端配置失败：%v", err)
			}
			return config
		}
	}
	buildClient := func(control netip.AddrPort) core.ClientConfig {
		config, err := core.NewClientConfig(
			core.WithClientID(testClientID),
			core.WithServerEndpoint(core.ServerEndpoint{Address: control, Transport: core.TransportTCP, Wire: core.WireV1}),
			core.WithClientAuth(core.TokenAuth{Token: testClientToken}),
			core.WithUDPProxy(core.UDPProxy{Name: "udp-scene", LocalAddr: target, RemotePort: oldPort}),
			core.WithHeartbeat(200*time.Millisecond),
			core.WithTimeout(2*time.Second),
		)
		if err != nil {
			t.Fatalf("构造 UDP 客户端配置失败：%v", err)
		}
		return config
	}

	serverEngine, _, control := startSceneEngines(t, sceneConfigs{
		server: buildServer(oldPort),
		client: buildClient,
	})
	oldAddr := serverEngine.GuestAddr("udp-scene").String()

	result, applyErr := serverEngine.Apply(context.Background(),
		server.Deployment{Revision: 1, Config: buildServer(newPort)(control)})
	if applyErr != nil {
		t.Fatalf("换 UDP 端口应成功，实际 %v", applyErr)
	}
	assertCleanDrain(t, []core.ApplyResult{result})
	if got := serverEngine.GuestAddr("udp-scene").String(); got == oldAddr {
		t.Fatalf("UDP 入口地址未切换：%s", got)
	}
	if serverEngine.ActiveRevision() != 1 {
		t.Fatalf("active 应为 1，实际 %d", serverEngine.ActiveRevision())
	}
	// 旧端口已随旧代关闭：再次绑定必须成功，证明套接字确实被释放。
	probe, err := net.ListenPacket("udp", oldAddr)
	if err != nil {
		t.Fatalf("旧 UDP 端口未释放：%v", err)
	}
	_ = probe.Close()
}

// applyOutcome 是一次后台 Apply 的结果与错误。
type applyOutcome struct {
	result core.ApplyResult
	err    error
}

// applyAsync 在后台发起一次服务端 Apply，结果与错误一并回传。
func applyAsync(engine *server.Engine, revision uint64, config core.ServerConfig) <-chan applyOutcome {
	done := make(chan applyOutcome, 1)
	go func() {
		result, err := engine.Apply(context.Background(), server.Deployment{Revision: revision, Config: config})
		done <- applyOutcome{result: result, err: err}
	}()
	return done
}

// applyAsyncClient 在后台发起一次客户端 Apply，结果与错误一并回传。
func applyAsyncClient(engine *client.Engine, revision uint64, config core.ClientConfig) <-chan applyOutcome {
	done := make(chan applyOutcome, 1)
	go func() {
		result, err := engine.Apply(context.Background(), client.Deployment{Revision: revision, Config: config})
		done <- applyOutcome{result: result, err: err}
	}()
	return done
}

// waitForBothActive 等两端 apply 完成 publish（active 推进到 1）。
//
// 不等 Apply 返回：两端都会停在 drain 直到旧流结束，而本用例要在切换窗口内验证
// 旧流连续。也不看入口地址：入口端口未变的用例地址本就不变。
// 结果通道上的错误会在等待期间被捕获并如实报出，否则失败只会表现为"active 没动"。
func waitForBothActive(
	t *testing.T, serverEngine *server.Engine, clientEngine *client.Engine, applyDone <-chan applyOutcome,
) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case outcome := <-applyDone:
			t.Fatalf("切换失败：%v", outcome.err)
		default:
		}
		if serverEngine.ActiveRevision() == 1 && clientEngine.ActiveRevision() == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("publish 未完成：服务端 active %d，客户端 active %d",
				serverEngine.ActiveRevision(), clientEngine.ActiveRevision())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitApplies 等待期望次数的 Apply 收敛，核验都未失败，并返回服务端的结果。
//
// 服务端结果用于核验排空结论：无在途流时排空应当立即完成，而不是撑满排水上限
// 后被标记为"排空异常"。
func waitApplies(t *testing.T, applyDone <-chan applyOutcome, count int) []core.ApplyResult {
	t.Helper()
	results := make([]core.ApplyResult, 0, count)
	for range count {
		select {
		case outcome := <-applyDone:
			if outcome.err != nil {
				t.Fatalf("Apply 失败：%v", outcome.err)
			}
			results = append(results, outcome.result)
		case <-time.After(15 * time.Second):
			t.Fatal("Apply 未收敛（drain 挂死）")
		}
	}
	return results
}

// assertCleanDrain 核验一次排空是干净收敛的。
//
// 这条断言守的是"控制连接被算进代的等待组"这类缺陷：客户端的控制连接在整个运行
// 期都开着，一旦它计入代的等待组，排空就永远等满上限并白白标记为异常。
func assertCleanDrain(t *testing.T, results []core.ApplyResult) {
	t.Helper()
	for _, result := range results {
		if result.Stage != core.StageDrained || result.DrainIncomplete {
			t.Fatalf("无在途流时排空应立即收敛，实际 stage=%s incomplete=%v",
				result.Stage, result.DrainIncomplete)
		}
	}
}

// waitForEcho 反复用新连接尝试回显，直到成功或超时。
//
// 新增代理的第一次回显需要等客户端拨出并声明该代理的工作连接，属正常等待，
// 因此在同一地址上重试而不是只试一次。每次重试都用全新连接：配对失败的那条
// 访客连接已被服务端暂存，复用它会读到上一次的残留状态。
func waitForEcho(t *testing.T, address string, payload []byte) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		guest, err := net.Dial("tcp", address)
		if err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		lastErr = echoOnceOn(guest, payload)
		_ = guest.Close()
		if lastErr == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("地址 %s 在超时前未能回显：%v", address, lastErr)
}
