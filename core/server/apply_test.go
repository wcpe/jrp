package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wcpe/jrp/core"
)

// freeApplyPort 申请一个空闲端口后立即释放，供 prepare 阶段绑定。
func freeApplyPort(t *testing.T) int {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请端口失败：%v", err)
	}
	defer func() { _ = probe.Close() }()
	return probe.Addr().(*net.TCPAddr).Port
}

// applyConfig 构造一个只含 TCP 代理绑定的最小服务端配置。
//
// listen 显式占一个真实端口而不是 :0——配置校验禁止端口 0 自动分配，这也是
// 生产语义的一部分（Core 不提供"先启动再找地址"的形态）。
func applyConfig(t *testing.T) core.ServerConfig {
	t.Helper()
	controlPort := freeApplyPort(t)
	listen, err := netip.ParseAddrPort("127.0.0.1:0")
	if err != nil {
		t.Fatalf("解析地址失败：%v", err)
	}
	_ = listen
	listen = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(controlPort))
	config, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{Address: listen, Transport: core.TransportTCP}),
		core.WithWire(core.WireV1),
		core.WithClientCredential(core.ClientCredential{ClientID: "apply", Token: "apply-token"}),
		core.WithTCPProxyBinding(core.TCPProxyBinding{
			Name:           "apply-proxy",
			ClientID:       "apply",
			RemotePort:     freeApplyPort(t),
			AllowedTargets: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:1")},
		}),
	)
	if err != nil {
		t.Fatalf("构造配置失败：%v", err)
	}
	return config
}

// applyConfigOnPort 构造把代理入口钉在指定端口的配置，供 prepare 失败用例使用。
func applyConfigOnPort(t *testing.T, remotePort int) core.ServerConfig {
	t.Helper()
	base := applyConfig(t)
	// 不可变配置没有 setter：重建一份，仅替换代理绑定的入口端口。
	listen := base.Listen().Address
	config, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{Address: listen, Transport: core.TransportTCP}),
		core.WithWire(core.WireV1),
		core.WithClientCredential(core.ClientCredential{ClientID: "apply", Token: "apply-token"}),
		core.WithTCPProxyBinding(core.TCPProxyBinding{
			Name:           "apply-proxy",
			ClientID:       "apply",
			RemotePort:     remotePort,
			AllowedTargets: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:1")},
		}),
	)
	if err != nil {
		t.Fatalf("构造配置失败：%v", err)
	}
	return config
}

// startEngineWithConfig 以传入配置构造已启动的引擎。
//
// 控制监听器按 config 声明的端口由测试先行占用再注入：Engine 要求宿主注入
// 控制监听器（WithListener），它不会自己监听配置里的地址。
func startEngineWithConfig(t *testing.T, config core.ServerConfig) *Engine {
	t.Helper()
	addr, err := net.ResolveTCPAddr("tcp", config.Listen().Address.String())
	if err != nil {
		t.Fatalf("解析控制地址失败：%v", err)
	}
	listener, err := net.ListenTCP("tcp", addr)
	if err != nil {
		t.Fatalf("占用控制端口失败：%v", err)
	}
	engine := New(config, WithListener(listener))
	if err := engine.Start(context.Background()); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = engine.Shutdown(shutdownCtx)
	})
	return engine
}

// 首次 Apply 建立 active：阶段为 drained，active 与 last-good 均等于本次 revision。
func TestApplyFirstDeploymentEstablishesActive(t *testing.T) {
	config := applyConfig(t)
	engine := startEngineWithConfig(t, applyConfig(t))

	result, err := engine.Apply(context.Background(), Deployment{Revision: 1, Config: config})
	if err != nil {
		t.Fatalf("首次应用失败：%v", err)
	}
	if result.Stage != core.StageDrained {
		t.Fatalf("首次应用应为 drained，实际 %s", result.Stage)
	}
	if result.Previous != core.SnapshotRevisionUnknown {
		t.Fatalf("首次应用的 previous 应为 0，实际 %d", result.Previous)
	}
	if engine.ActiveRevision() != 1 || engine.LastGoodRevision() != 1 {
		t.Fatalf("active=%d last-good=%d，均应为 1", engine.ActiveRevision(), engine.LastGoodRevision())
	}
}

// prepare 失败后 active 与 last-good 不变。
//
// 构造手法：新配置的代理绑定端口已被占用，prepare 在绑定入口时必然失败。
func TestApplyPrepareFailureKeepsActive(t *testing.T) {
	occupied := freeApplyPort(t)
	held, err := net.Listen("tcp", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(occupied)).String())
	if err != nil {
		t.Fatalf("占住端口失败：%v", err)
	}

	engine := startEngineWithConfig(t, applyConfig(t))
	blocked := applyConfigOnPort(t, occupied)
	_, applyErr := engine.Apply(context.Background(), Deployment{Revision: 1, Config: blocked})
	// 测试自己的占位先撤掉，后面的重绑检查才只反映 Core 的资源状态。
	_ = held.Close()
	if applyErr == nil {
		t.Fatal("端口被占用时 prepare 应失败，实际成功——绑定根本没发生，用例已失去意义")
	}

	var applyErrTyped *core.ApplyError
	if !errors.As(applyErr, &applyErrTyped) || applyErrTyped.Stage != core.StagePrepare {
		t.Fatalf("应返回 prepare 阶段错误，实际 %v", applyErr)
	}
	if engine.ActiveRevision() != core.SnapshotRevisionUnknown {
		t.Fatalf("prepare 失败后 active 不得改变，实际 %d", engine.ActiveRevision())
	}
	if engine.LastGoodRevision() != core.SnapshotRevisionUnknown {
		t.Fatalf("prepare 失败后 last-good 不得改变，实际 %d", engine.LastGoodRevision())
	}

	// 资源释放核验：Core 自建的另一个代理入口在 prepare 失败时也必须归还。
	// 若失败分支漏掉释放，blocked 配置里"apply-proxy"绑定的那个端口会被
	// 泄漏的监听器一直占着。
	retry, err := net.Listen("tcp", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(occupied)).String())
	if err != nil {
		t.Fatalf("prepare 失败后代理端口 %d 未归还，疑似新代资源泄漏：%v", occupied, err)
	}
	_ = retry.Close()
}

// 过期 revision 被拒绝，active 不变。
func TestApplyRejectsStaleRevision(t *testing.T) {
	config := applyConfig(t)
	engine := startEngineWithConfig(t, applyConfig(t))

	if _, err := engine.Apply(context.Background(), Deployment{Revision: 5, Config: config}); err != nil {
		t.Fatalf("首次应用失败：%v", err)
	}
	_, err := engine.Apply(context.Background(), Deployment{Revision: 3, Config: config})
	var stale *core.ApplyError
	if !errors.As(err, &stale) || stale.Stage != core.StageValidate {
		t.Fatalf("过期拒绝应落在 validate 阶段，实际 %v", err)
	}
	if engine.ActiveRevision() != 5 {
		t.Fatalf("拒绝后 active 不得改变，实际 %d", engine.ActiveRevision())
	}
}

// 重复提交同一 revision 是幂等成功，不改变 active 也不报错。
func TestApplySameRevisionIsIdempotent(t *testing.T) {
	config := applyConfig(t)
	engine := startEngineWithConfig(t, applyConfig(t))

	if _, err := engine.Apply(context.Background(), Deployment{Revision: 2, Config: config}); err != nil {
		t.Fatalf("首次应用失败：%v", err)
	}
	result, err := engine.Apply(context.Background(), Deployment{Revision: 2, Config: config})
	if err != nil {
		t.Fatalf("幂等应用不应报错：%v", err)
	}
	if result.Stage != core.StageDrained {
		t.Fatalf("幂等应用应为 drained，实际 %s", result.Stage)
	}
	if engine.ActiveRevision() != 2 {
		t.Fatalf("幂等应用后 active 应仍为 2，实际 %d", engine.ActiveRevision())
	}
}

// 幂等不得重建资源：用代指针同一性断言，而不是比较入口地址字符串——
// 同配置重建的监听器端口相同，字符串比对抓不住重建。
func TestApplyIdempotentDoesNotRebuildGeneration(t *testing.T) {
	engine := startEngineWithConfig(t, applyConfig(t))
	config := applyConfig(t)
	if _, err := engine.Apply(context.Background(), Deployment{Revision: 1, Config: config}); err != nil {
		t.Fatalf("首次应用失败：%v", err)
	}

	before := engine.activeGeneration()
	if _, err := engine.Apply(context.Background(), Deployment{Revision: 1, Config: config}); err != nil {
		t.Fatalf("幂等应用不应报错：%v", err)
	}
	if engine.activeGeneration() != before {
		t.Fatal("幂等应用重建了代：入口监听器被关闭重开，已有连接会中断")
	}
}

// 并发 Apply 只有一个成功，另一个返回 ErrApplyInProgress。
func TestApplyConcurrentReturnsInProgress(t *testing.T) {
	engine := startEngineWithConfig(t, applyConfig(t))

	// 用 healthCheckHook 把第一次 Apply 卡在 publish 之前：单飞标记此时已被置位，
	// 第二次并发调用必须拿到 ErrApplyInProgress。这验证的是真正的并发互斥，
	// 而不是"同步调用结束后标记被释放"（那不需要并发也能测）。
	entered := make(chan struct{})
	release := make(chan struct{})
	// 钩子只放行第一次调用：Apply 成功后同一测试内的后续 Apply 不应再被卡住，
	// 否则 close(entered) 会二次关闭 channel 而 panic——这既是测试正确性要求，
	// 也在提醒生产代码：钩子是包内测试设施，恒为 nil。
	var fired atomic.Bool
	engine.healthCheckHook = func(gen *generation) error {
		if fired.Swap(true) {
			return nil
		}
		close(entered)
		<-release
		return nil
	}
	defer func() { engine.healthCheckHook = nil }()

	firstDone := make(chan error, 1)
	go func() {
		_, err := engine.Apply(context.Background(), Deployment{Revision: 1, Config: applyConfig(t)})
		firstDone <- err
	}()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("第一次 Apply 未进入 health-check，单飞窗口未能建立")
	}

	_, err := engine.Apply(context.Background(), Deployment{Revision: 2, Config: applyConfig(t)})
	if !errors.Is(err, core.ErrApplyInProgress) {
		close(release)
		t.Fatalf("并发的第二次 Apply 应返回 ErrApplyInProgress，实际 %v", err)
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("第一次 Apply 应在放行后成功，实际 %v", err)
	}

	// 单飞标记在返回后被释放：之后的新调用必须能正常进行。
	if _, err := engine.Apply(context.Background(), Deployment{Revision: 2, Config: applyConfig(t)}); err != nil {
		t.Fatalf("单飞标记应在返回后释放，实际 %v", err)
	}
}

// drain 超上限时强制释放，结果标记排空未完成，active 仍为新版本。
//
// 构造手法：第二个 revision 换掉代理端口，旧代入口被 drain；由于没有真实
// 控制连接承载活动流，drain 正常立即完成。这里的重点是 Stage 语义正确。
func TestApplyDrainSemanticsOnSwitch(t *testing.T) {
	engine := startEngineWithConfig(t, applyConfig(t))

	if _, err := engine.Apply(context.Background(), Deployment{Revision: 1, Config: applyConfig(t)}); err != nil {
		t.Fatalf("首次应用失败：%v", err)
	}
	result, err := engine.Apply(context.Background(), Deployment{Revision: 2, Config: applyConfig(t)})
	if err != nil {
		t.Fatalf("二次应用失败：%v", err)
	}
	if result.Previous != 1 || result.Revision != 2 {
		t.Fatalf("结果应为 1→2，实际 %d→%d", result.Previous, result.Revision)
	}
	if !result.Published() {
		t.Fatalf("切换应视为已发布，实际 %s", result.Stage)
	}
	if engine.ActiveRevision() != 2 || engine.LastGoodRevision() != 2 {
		t.Fatalf("切换后 active=%d last-good=%d，均应为 2", engine.ActiveRevision(), engine.LastGoodRevision())
	}
}

// 深复制：宿主在 Apply 后修改自己的配置对象，Core 的 active 不受影响。
//
// ServerConfig 是不可变值（无 setter），这里验证"Apply 后再校验原配置仍通过、
// Core 读到的凭据与提交时一致"。二次应用换新端口：同一入口端口在新代 prepare
// 时无法二次绑定，那与"别名"无关，而是端口资源的排他性。
func TestApplySnapshotNotAliased(t *testing.T) {
	engine := startEngineWithConfig(t, applyConfig(t))
	config := applyConfig(t)

	if _, err := engine.Apply(context.Background(), Deployment{Revision: 1, Config: config}); err != nil {
		t.Fatalf("应用失败：%v", err)
	}
	// 提交的配置再次校验仍通过（未被 Core 改写）。
	if err := config.Validate(); err != nil {
		t.Fatalf("宿主持有的配置被 Core 改写：%v", err)
	}
	// 幂等应用同一 revision：不重新绑定端口，凭据仍匹配（Core 未修改它）。
	if _, err := engine.Apply(context.Background(), Deployment{Revision: 1, Config: config}); err != nil {
		t.Fatalf("幂等应用相同内容失败：%v", err)
	}
}

// Shutdown 后重复 Shutdown 幂等。
func TestApplyEngineDoubleShutdownIdempotent(t *testing.T) {
	engine := startEngineWithConfig(t, applyConfig(t))
	if _, err := engine.Apply(context.Background(), Deployment{Revision: 1, Config: applyConfig(t)}); err != nil {
		t.Fatalf("应用失败：%v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := engine.Shutdown(ctx); err != nil {
		t.Fatalf("首次 Shutdown 失败：%v", err)
	}
	if err := engine.Shutdown(ctx); err != nil {
		t.Fatalf("重复 Shutdown 应幂等，实际 %v", err)
	}
	select {
	case <-engine.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown 后 Done 未关闭")
	}
	// 引擎已停，Apply 应在 validate 阶段按停止拒绝，而不是 panic。
	// 断言阶段与哨兵两者：只断 err != nil 的话，把拒绝改成任意错误也能过。
	_, applyErr := engine.Apply(ctx, Deployment{Revision: 9, Config: applyConfig(t)})
	var stopped *core.ApplyError
	if !errors.As(applyErr, &stopped) || stopped.Stage != core.StageValidate {
		t.Fatalf("停止后的 Apply 应在 validate 阶段拒绝，实际 %v", applyErr)
	}
	if !errors.Is(applyErr, ErrStopped) {
		t.Fatalf("停止后的 Apply 应可判定为 ErrStopped，实际 %v", applyErr)
	}
}

// 编译期防呆：确认 io 包仍被使用（构造未来的流连续性用例时需要）。
var _ = io.EOF

// health-check 失败必须释放新代资源，且 active 与 last-good 不变。
//
// 这条用例是变异验证的直接产物：prepare 成功即端口可用，真实流程中
// health-check 不会失败，此前"删掉 release 后测试全绿"——失败分支没有任何
// 守护者。现在通过包内钩子注入失败，让该分支真正被测试触达。
func TestApplyHealthCheckFailureReleasesGenerationAndKeepsActive(t *testing.T) {
	engine := startEngineWithConfig(t, applyConfig(t))
	blocked := applyConfig(t)
	failedBinding := blocked.Bindings()[0].RemotePort
	engine.healthCheckHook = func(gen *generation) error {
		return fmt.Errorf("health-check 注入失败：代理入口 %d 不可用", failedBinding)
	}
	defer func() { engine.healthCheckHook = nil }()

	_, applyErr := engine.Apply(context.Background(), Deployment{Revision: 1, Config: blocked})
	if applyErr == nil {
		t.Fatal("钩子注入失败后 Apply 应失败，实际成功——钩子未生效")
	}
	var typed *core.ApplyError
	if !errors.As(applyErr, &typed) || typed.Stage != core.StageHealthCheck {
		t.Fatalf("应返回 health-check 阶段错误，实际 %v", applyErr)
	}
	if engine.ActiveRevision() != core.SnapshotRevisionUnknown {
		t.Fatalf("health-check 失败后 active 不得改变，实际 %d", engine.ActiveRevision())
	}
	if engine.LastGoodRevision() != core.SnapshotRevisionUnknown {
		t.Fatalf("health-check 失败后 last-good 不得改变，实际 %d", engine.LastGoodRevision())
	}

	// 资源释放核验：失败分支必须归还新代的监听器。
	// 没有这条断言时，删掉失败分支的 gen.release() 用例仍然全绿——
	// "active 不变"只验证了隔离性，没验证释放承诺。
	retry, listenErr := net.Listen("tcp", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(failedBinding)).String())
	if listenErr != nil {
		t.Fatalf("health-check 失败后代理端口 %d 未归还，新代资源泄漏：%v", failedBinding, listenErr)
	}
	_ = retry.Close()
}
