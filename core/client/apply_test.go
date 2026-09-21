package client

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/internal/wire"
)

const testApplyProxy = "apply-proxy"

// clientFixture 是一对「伪控制服务端 + 已启动客户端」的测试夹具。
//
// 客户端 Apply 需要一条活着的控制会话，但不关心服务端侧的配对逻辑，因此用伪
// 控制服务端替代真实服务端：它只对登录帧回复成功，随后保持连接打开。
type clientFixture struct {
	engine   *Engine
	endpoint core.ServerEndpoint
	clientID string
	token    string
	// target 是本地目标地址，供需要真实桥接的用例自建代理。
	target netip.AddrPort
}

// snapshot 以夹具身份构造客户端的完整快照。
//
// 默认不含代理：多数用例只验证 Apply 的机制（revision、幂等、单飞、失败保留），
// 带代理的快照会立刻建起一条不会自行结束的桥接，让每次切换都等满排水上限。
// 需要真实桥接的用例显式传入代理。
func (f *clientFixture) snapshot(t *testing.T, proxies ...core.TCPProxy) core.ClientConfig {
	t.Helper()
	return testClientConfig(t, f.endpoint, f.clientID, f.token, proxies)
}

// newClientFixture 启动伪控制服务端与真本地目标，并把客户端启动到 running。
func newClientFixture(t *testing.T) *clientFixture {
	t.Helper()
	serverAddr := startFakeControlServer(t)

	fixture := &clientFixture{
		endpoint: core.ServerEndpoint{
			Address:   clientAddrPort(t, serverAddr),
			Transport: core.TransportTCP,
			Wire:      core.WireV1,
		},
		clientID: "apply-client",
		token:    "apply-token",
		target:   startIdleTarget(t),
	}
	fixture.engine = startClientEngine(t, fixture.snapshot(t))
	return fixture
}

// startClientEngine 启动一个客户端引擎并在测试结束时关闭它。
func startClientEngine(t *testing.T, config core.ClientConfig) *Engine {
	t.Helper()
	engine := New(config)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("客户端启动失败：%v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = engine.Shutdown(shutdownCtx)
	})
	return engine
}

// testClientConfig 构造一份最小合法客户端配置。
//
// opts 追加在基础选项之后，用于覆盖单个字段（例如压短排水上限）。
func testClientConfig(
	t *testing.T, endpoint core.ServerEndpoint, clientID, token string,
	proxies []core.TCPProxy, opts ...core.ClientOption,
) core.ClientConfig {
	t.Helper()
	options := []core.ClientOption{
		core.WithClientID(clientID),
		core.WithServerEndpoint(endpoint),
		core.WithClientAuth(core.TokenAuth{Token: token}),
		core.WithTCPProxies(proxies),
		core.WithHeartbeat(200 * time.Millisecond),
		core.WithTimeout(2 * time.Second),
		// 排水上限压短：用例出现回归时按秒失败，而不是等满默认的十秒。
		core.WithClientDrainTimeout(2 * time.Second),
	}
	config, err := core.NewClientConfig(append(options, opts...)...)
	if err != nil {
		t.Fatalf("构造客户端配置失败：%v", err)
	}
	return config
}

// clientAddrPort 解析测试地址。
func clientAddrPort(t *testing.T, value string) netip.AddrPort {
	t.Helper()
	address, err := netip.ParseAddrPort(value)
	if err != nil {
		t.Fatalf("解析测试地址失败：%v", err)
	}
	return address
}

// startFakeControlServer 启动伪控制服务端，返回其监听地址。
func startFakeControlServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听伪控制端口失败：%v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go serveFakeControlConn(conn)
		}
	}()
	return listener.Addr().String()
}

// serveFakeControlConn 对登录帧回复登录成功，随后保持连接打开。
//
// 保持打开是关键：控制连接一旦被对端关闭，客户端的读循环会把连接错误记为
// 异常停止，后续 Apply 会因此落在 health-check 拒绝分支。
func serveFakeControlConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := wire.NewV1Reader(conn, wire.DefaultV1PayloadLimit)
	for {
		frame, err := reader.ReadFrame()
		if err != nil {
			return
		}
		if frame.Type.Name == wire.MessageTypeLogin.Name {
			encoded, encodeErr := wire.EncodeV1Frame(wire.Frame{
				Type:    wire.MessageTypeLoginResponse,
				Payload: []byte(`{"ok":true}`),
			})
			if encodeErr != nil {
				frame.Release()
				return
			}
			if _, writeErr := conn.Write(encoded); writeErr != nil {
				frame.Release()
				return
			}
		}
		frame.Release()
	}
}

// startIdleTarget 启动一个只接收不回应的本地目标。
//
// 客户端桥接会稳定阻塞在它上面，测试因此不会因为工作连接快速重建而产生噪声。
func startIdleTarget(t *testing.T) netip.AddrPort {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动本地目标失败：%v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func(target net.Conn) {
				defer func() { _ = target.Close() }()
				_, _ = io.Copy(io.Discard, target)
			}(conn)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})
	return clientAddrPort(t, listener.Addr().String())
}

// 首次 Apply 建立 active：阶段为 drained，active 与 last-good 均等于本次 revision。
func TestApplyFirstDeploymentEstablishesActive(t *testing.T) {
	fixture := newClientFixture(t)

	result, err := fixture.engine.Apply(context.Background(), Deployment{
		Revision: 1,
		Config:   fixture.snapshot(t),
	})
	if err != nil {
		t.Fatalf("首次应用失败：%v", err)
	}
	if result.Stage != core.StageDrained {
		t.Fatalf("首次应用应为 drained，实际 %s", result.Stage)
	}
	if result.Previous != core.SnapshotRevisionUnknown {
		t.Fatalf("首次应用的 previous 应为 0，实际 %d", result.Previous)
	}
	if result.Changed != 0 {
		t.Fatalf("空代理集合的 changed 应为 0，实际 %d", result.Changed)
	}
	if engine := fixture.engine; engine.ActiveRevision() != 1 || engine.LastGoodRevision() != 1 {
		t.Fatalf("active=%d last-good=%d，均应为 1", engine.ActiveRevision(), engine.LastGoodRevision())
	}
}

// 过期 revision 被拒绝，active 不变。
func TestApplyRejectsStaleRevision(t *testing.T) {
	fixture := newClientFixture(t)
	if _, err := fixture.engine.Apply(context.Background(), Deployment{Revision: 5, Config: fixture.snapshot(t)}); err != nil {
		t.Fatalf("首次应用失败：%v", err)
	}

	_, err := fixture.engine.Apply(context.Background(), Deployment{Revision: 3, Config: fixture.snapshot(t)})
	var stale *core.ApplyError
	if !errors.As(err, &stale) || stale.Stage != core.StageValidate {
		t.Fatalf("过期拒绝应落在 validate 阶段，实际 %v", err)
	}
	if fixture.engine.ActiveRevision() != 5 {
		t.Fatalf("拒绝后 active 不得改变，实际 %d", fixture.engine.ActiveRevision())
	}
}

// 重复提交同一 revision 是幂等成功，不改变 active 也不报错。
func TestApplySameRevisionIsIdempotent(t *testing.T) {
	fixture := newClientFixture(t)
	config := fixture.snapshot(t)
	if _, err := fixture.engine.Apply(context.Background(), Deployment{Revision: 2, Config: config}); err != nil {
		t.Fatalf("首次应用失败：%v", err)
	}

	result, err := fixture.engine.Apply(context.Background(), Deployment{Revision: 2, Config: config})
	if err != nil {
		t.Fatalf("幂等应用不应报错：%v", err)
	}
	if result.Stage != core.StageDrained {
		t.Fatalf("幂等应用应为 drained，实际 %s", result.Stage)
	}
	if fixture.engine.ActiveRevision() != 2 {
		t.Fatalf("幂等应用后 active 应仍为 2，实际 %d", fixture.engine.ActiveRevision())
	}
}

// 幂等不得重建代：用代指针同一性断言，而不是比较代理名。
func TestApplyIdempotentDoesNotRebuildGeneration(t *testing.T) {
	fixture := newClientFixture(t)
	config := fixture.snapshot(t)
	if _, err := fixture.engine.Apply(context.Background(), Deployment{Revision: 1, Config: config}); err != nil {
		t.Fatalf("首次应用失败：%v", err)
	}

	before := fixture.engine.activeGeneration()
	if _, err := fixture.engine.Apply(context.Background(), Deployment{Revision: 1, Config: config}); err != nil {
		t.Fatalf("幂等应用不应报错：%v", err)
	}
	if fixture.engine.activeGeneration() != before {
		t.Fatal("幂等应用重建了代：旧代被停、新代重开，已有桥接会中断")
	}
	if before.isStopping() {
		t.Fatal("幂等应用停掉了旧代，已有桥接会被切断")
	}
}

// drain 上限到达：旧代还有在途桥接时，Apply 等满排水上限并如实标记排空未完成。
//
// 覆盖规格 §5 的"排空上限到达"边界：publish 已成功、active 已是新版本，但旧代的
// 桥接没有在时限内结束，宿主必须能从结果里区分这种情况；同时核验旧代真的被停掉，
// 否则旧代的维持循环会继续拨号占用资源。
//
// 用真实代理构造在途桥接：维持循环会拨一条到本地目标的连接并桥接，而本地目标
// 只接收不回应，桥接不会自行结束，因此 drain 必然等满时限。
func TestApplyDrainLimitReportsIncomplete(t *testing.T) {
	fixture := newClientFixture(t)
	proxy := core.TCPProxy{Name: testApplyProxy, LocalAddr: fixture.target, RemotePort: 6100}
	if _, err := fixture.engine.Apply(context.Background(), Deployment{Revision: 1, Config: fixture.snapshot(t, proxy)}); err != nil {
		t.Fatalf("首次应用失败：%v", err)
	}
	old := fixture.engine.activeGeneration()
	waitForTrackedConn(t, old)

	// 新快照压短排水上限：本用例要的是"到达上限"这条路径，不该等默认的十秒。
	shortDrain := testClientConfig(
		t, fixture.endpoint, fixture.clientID, fixture.token,
		[]core.TCPProxy{proxy}, core.WithClientDrainTimeout(150*time.Millisecond),
	)
	result, err := fixture.engine.Apply(context.Background(), Deployment{Revision: 2, Config: shortDrain})
	if err != nil {
		t.Fatalf("二次应用失败：%v", err)
	}
	if !result.Published() {
		t.Fatalf("排空超上限不影响发布事实，实际阶段 %s", result.Stage)
	}
	if !result.DrainIncomplete {
		t.Fatal("旧代桥接仍在途，结果应标记排空未完成")
	}
	if result.Stage != core.StageApplied {
		t.Fatalf("排空未完成时阶段应为 applied，实际 %s", result.Stage)
	}
	if result.Changed != 1 || result.Drained != 1 {
		t.Fatalf("changed=%d drained=%d，均应等于 1", result.Changed, result.Drained)
	}
	if fixture.engine.ActiveRevision() != 2 {
		t.Fatalf("排空异常不得回切 active，实际 %d", fixture.engine.ActiveRevision())
	}
	if !old.isStopping() {
		t.Fatal("换代后旧代未被停：旧代的维持循环会继续占用资源")
	}
}

// waitForTrackedConn 等待该代真正登记了活动连接，再继续后续断言。
//
// 不等待的话，切换用例可能跑在"维持循环还没建起桥接"的窗口里，drain 会立刻
// 收敛成 drained，用例就退化成了另一条普通切换断言。
func waitForTrackedConn(t *testing.T, gen *generation) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for gen.connCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("等待维持循环建立桥接超时")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 并发 Apply 只有一个成功，另一个返回 ErrApplyInProgress。
func TestApplyConcurrentReturnsInProgress(t *testing.T) {
	fixture := newClientFixture(t)

	// 用 healthCheckHook 把第一次 Apply 卡在 publish 之前：单飞标记此时已被置位，
	// 第二次并发调用必须拿到 ErrApplyInProgress。这验证的是真正的并发互斥，
	// 而不是"同步调用结束后标记被释放"（那不需要并发也能测）。
	entered := make(chan struct{})
	release := make(chan struct{})
	// 钩子只放行第一次调用：close(entered) 二次执行会 panic。
	var fired atomic.Bool
	fixture.engine.healthCheckHook = func(*generation) error {
		if fired.Swap(true) {
			return nil
		}
		close(entered)
		<-release
		return nil
	}
	defer func() { fixture.engine.healthCheckHook = nil }()

	firstDone := make(chan error, 1)
	go func() {
		_, err := fixture.engine.Apply(context.Background(), Deployment{Revision: 1, Config: fixture.snapshot(t)})
		firstDone <- err
	}()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("第一次 Apply 未进入 health-check，单飞窗口未能建立")
	}

	_, err := fixture.engine.Apply(context.Background(), Deployment{Revision: 2, Config: fixture.snapshot(t)})
	if !errors.Is(err, core.ErrApplyInProgress) {
		close(release)
		t.Fatalf("并发的第二次 Apply 应返回 ErrApplyInProgress，实际 %v", err)
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("第一次 Apply 应在放行后成功，实际 %v", err)
	}

	// 单飞标记在返回后被释放：之后的新调用必须能正常进行。
	if _, err := fixture.engine.Apply(context.Background(), Deployment{Revision: 2, Config: fixture.snapshot(t)}); err != nil {
		t.Fatalf("单飞标记应在返回后释放，实际 %v", err)
	}
}

// health-check 失败必须释放新代资源，且 active 与 last-good 不变。
//
// 这条用例是变异验证的直接产物：真实流程下客户端的 health-check 不会失败，
// 失败分支若无钩子就无法被测试触达——实测删掉失败分支的 release 后，仅断言
// "active 不变"与"重试成功"的版本仍然全绿。
func TestApplyHealthCheckFailureReleasesGenerationAndKeepsActive(t *testing.T) {
	fixture := newClientFixture(t)

	// 钩子拿得到新代指针，失败后据此直接核验释放，而不是靠间接推断。
	var failing *generation
	fixture.engine.healthCheckHook = func(gen *generation) error {
		failing = gen
		return errors.New("注入的 health-check 失败")
	}
	defer func() { fixture.engine.healthCheckHook = nil }()

	_, applyErr := fixture.engine.Apply(context.Background(), Deployment{Revision: 1, Config: fixture.snapshot(t)})
	var typed *core.ApplyError
	if !errors.As(applyErr, &typed) || typed.Stage != core.StageHealthCheck {
		t.Fatalf("应返回 health-check 阶段错误，实际 %v", applyErr)
	}
	if fixture.engine.ActiveRevision() != core.SnapshotRevisionUnknown {
		t.Fatalf("health-check 失败后 active 不得改变，实际 %d", fixture.engine.ActiveRevision())
	}
	if fixture.engine.LastGoodRevision() != core.SnapshotRevisionUnknown {
		t.Fatalf("health-check 失败后 last-good 不得改变，实际 %d", fixture.engine.LastGoodRevision())
	}
	if failing == nil {
		t.Fatal("钩子未收到新代，用例已失去意义")
	}
	if !errors.Is(failing.forwardCtx.Err(), context.Canceled) {
		t.Fatalf("失败的新代转发上下文未取消，资源泄漏：%v", failing.forwardCtx.Err())
	}
	if _, acquireErr := failing.pool.Acquire(context.Background(), testApplyProxy); acquireErr == nil {
		t.Fatal("失败的新代工作连接池未关闭，资源泄漏")
	}
}

// ctx 在 health-check 期间取消：apply 中止并保留旧版本，原因可判定。
func TestApplyContextCancelKeepsActive(t *testing.T) {
	fixture := newClientFixture(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	fixture.engine.healthCheckHook = func(*generation) error {
		close(entered)
		<-release
		return nil
	}
	defer func() { fixture.engine.healthCheckHook = nil }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-entered
		cancel()
		close(release)
	}()

	_, applyErr := fixture.engine.Apply(ctx, Deployment{Revision: 1, Config: fixture.snapshot(t)})
	if !errors.Is(applyErr, context.Canceled) {
		t.Fatalf("取消后应返回可判定为 context.Canceled 的错误，实际 %v", applyErr)
	}
	var typed *core.ApplyError
	if !errors.As(applyErr, &typed) || typed.Stage != core.StageHealthCheck {
		t.Fatalf("取消应落在 health-check 阶段，实际 %v", applyErr)
	}
	if fixture.engine.ActiveRevision() != core.SnapshotRevisionUnknown {
		t.Fatalf("取消后 active 不得改变，实际 %d", fixture.engine.ActiveRevision())
	}
}

// 控制会话身份变更被拒绝：首版不重拨控制连接。
func TestApplyRejectsControlIdentityChange(t *testing.T) {
	fixture := newClientFixture(t)
	if _, err := fixture.engine.Apply(context.Background(), Deployment{Revision: 1, Config: fixture.snapshot(t)}); err != nil {
		t.Fatalf("首次应用失败：%v", err)
	}

	changed := testClientConfig(t, fixture.endpoint, fixture.clientID, "rotated-token", nil)
	_, applyErr := fixture.engine.Apply(context.Background(), Deployment{Revision: 2, Config: changed})
	var typed *core.ApplyError
	if !errors.As(applyErr, &typed) || typed.Stage != core.StageValidate {
		t.Fatalf("身份变更应在 validate 阶段拒绝，实际 %v", applyErr)
	}
	if fixture.engine.ActiveRevision() != 1 {
		t.Fatalf("拒绝后 active 不得改变，实际 %d", fixture.engine.ActiveRevision())
	}
}

// 未启动的引擎拒绝 Apply，原因可判定。
func TestApplyBeforeStartRejected(t *testing.T) {
	fixture := newClientFixture(t)
	engine := New(fixture.snapshot(t))

	_, applyErr := engine.Apply(context.Background(), Deployment{Revision: 1, Config: fixture.snapshot(t)})
	var typed *core.ApplyError
	if !errors.As(applyErr, &typed) || typed.Stage != core.StageValidate {
		t.Fatalf("未启动应在 validate 阶段拒绝，实际 %v", applyErr)
	}
	if !errors.Is(applyErr, ErrNotStarted) {
		t.Fatalf("未启动应可判定为 ErrNotStarted，实际 %v", applyErr)
	}
}

// Shutdown 后 Apply 在 validate 阶段按停止拒绝，且可判定为 ErrStopped。
func TestApplyAfterShutdownRejected(t *testing.T) {
	fixture := newClientFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := fixture.engine.Shutdown(ctx); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}

	_, applyErr := fixture.engine.Apply(ctx, Deployment{Revision: 1, Config: fixture.snapshot(t)})
	var typed *core.ApplyError
	if !errors.As(applyErr, &typed) || typed.Stage != core.StageValidate {
		t.Fatalf("停止后的 Apply 应在 validate 阶段拒绝，实际 %v", applyErr)
	}
	if !errors.Is(applyErr, ErrStopped) {
		t.Fatalf("停止后的 Apply 应可判定为 ErrStopped，实际 %v", applyErr)
	}
}

// Shutdown 后重复 Shutdown 幂等。
func TestApplyEngineDoubleShutdownIdempotent(t *testing.T) {
	fixture := newClientFixture(t)
	if _, err := fixture.engine.Apply(context.Background(), Deployment{Revision: 1, Config: fixture.snapshot(t)}); err != nil {
		t.Fatalf("应用失败：%v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := fixture.engine.Shutdown(ctx); err != nil {
		t.Fatalf("首次 Shutdown 失败：%v", err)
	}
	if err := fixture.engine.Shutdown(ctx); err != nil {
		t.Fatalf("重复 Shutdown 应幂等，实际 %v", err)
	}
	select {
	case <-fixture.engine.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown 后 Done 未关闭")
	}
}

// Bindings 非空时被明确拒绝，而不是静默忽略。
func TestApplyRejectsHostBindings(t *testing.T) {
	fixture := newClientFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	defer func() { _ = listener.Close() }()

	_, applyErr := fixture.engine.Apply(context.Background(), Deployment{
		Revision: 1,
		Config:   fixture.snapshot(t),
		Bindings: []core.Binding{{ID: "host", Resource: listener}},
	})
	var typed *core.ApplyError
	if !errors.As(applyErr, &typed) || typed.Stage != core.StageValidate {
		t.Fatalf("绑定资源应在 validate 阶段拒绝，实际 %v", applyErr)
	}
}

// 错误与结果不得泄露令牌。
func TestApplyErrorsDoNotLeakToken(t *testing.T) {
	const secret = "super-secret-token-value"
	serverAddr := startFakeControlServer(t)
	target := startIdleTarget(t)
	endpoint := core.ServerEndpoint{
		Address:   clientAddrPort(t, serverAddr),
		Transport: core.TransportTCP,
		Wire:      core.WireV1,
	}
	proxies := []core.TCPProxy{{Name: testApplyProxy, LocalAddr: target, RemotePort: 6100}}
	config := testClientConfig(t, endpoint, "apply-client", secret, proxies)
	engine := startClientEngine(t, config)

	if _, err := engine.Apply(context.Background(), Deployment{Revision: 2, Config: config}); err != nil {
		t.Fatalf("首次应用失败：%v", err)
	}

	// 构造一次可判定的失败：revision 过期。
	_, applyErr := engine.Apply(context.Background(), Deployment{Revision: 1, Config: config})
	if applyErr == nil {
		t.Fatal("过期 revision 应失败")
	}
	if strings.Contains(applyErr.Error(), secret) {
		t.Fatalf("错误文本泄露令牌：%s", applyErr.Error())
	}
}

// revision 缺失在 validate 阶段拒绝。
func TestApplyRejectsMissingRevision(t *testing.T) {
	fixture := newClientFixture(t)
	_, applyErr := fixture.engine.Apply(context.Background(), Deployment{Config: fixture.snapshot(t)})
	var typed *core.ApplyError
	if !errors.As(applyErr, &typed) || typed.Stage != core.StageValidate {
		t.Fatalf("缺失 revision 应在 validate 阶段拒绝，实际 %v", applyErr)
	}
}
