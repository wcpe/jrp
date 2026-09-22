package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/internal/proxy"
	"github.com/wcpe/jrp/core/internal/transport"
	"github.com/wcpe/jrp/core/internal/wire"
)

// ErrAlreadyStarted 表示 Engine 已在运行，重复 Start 非法。
var ErrAlreadyStarted = errors.New("客户端引擎已启动，不允许重复启动")

// ErrNotStarted 表示 Engine 尚未 Start。
var ErrNotStarted = errors.New("客户端引擎尚未启动")

// ErrStopped 表示 Engine 已停止，不允许重启。
var ErrStopped = errors.New("客户端引擎已停止，不允许重启")

const (
	// retryBackoff 是工作连接建链失败后的退避时长。
	retryBackoff = time.Second
	// poolRetryBackoff 是工作连接池达到上限后的退避时长。
	poolRetryBackoff = 100 * time.Millisecond
)

// generation 是一个 revision 对应的运行资源与连接集合。
//
// 连接、等待组与工作连接池按代隔离，是「切换期间已有桥接不中断」的实现基础：
// drain 只等待旧代的活动桥接结束，新代连接不受牵连。共用一个等待组会让 drain
// 连带等待新代，切换就退化成"停下来等所有人"。
//
// 控制连接刻意**不在**代里：它是登录与心跳通道，跨代存活。drain 旧代时若关闭
// 它，旧代的维持循环停摆尚可接受，但新代也会失去控制会话——代理集合的变化并不
// 要求重拨控制连接。
type generation struct {
	// engine 回指所属引擎：conns 复用引擎的锁，本类型不另设互斥量。
	engine   *Engine
	revision uint64
	// config 是本代的不可变配置。凭据、代理集合与池上限都从它读取，而不是从
	// engine.config：代一经发布就不再修改，指针切换即是同步点；若共享一个可写
	// 字段，维持循环会与切换路径竞争读。
	config core.ClientConfig
	// pool 是本代的工作连接池。池上限随快照变化，因此必须按代隔离：共用一个池
	// 会让旧代的在途槽位继续占用新代的容量。
	pool *transport.WorkConnPool
	// dialer 是本代的拨号器，超时来自本代快照。
	dialer transport.Dialer
	// stopCh 停止本代的维持循环。
	stopCh chan struct{}
	// forwardCtx 是本代的转发上下文，随本代停止取消，用于中断阻塞的双向转发。
	forwardCtx context.Context
	cancel     context.CancelFunc
	// stopping 标记本代是否已停止接收流量：drain 旧代时置位的是**旧代的**标记，
	// 新代不受影响。
	stopping bool
	stopOnce sync.Once
	// wg 跟踪本代的维持循环、桥接与 UDP 会话。
	wg sync.WaitGroup
	// conns 是本代登记的活动连接（工作连接）。
	conns map[*transport.Conn]struct{}
}

// newGeneration 构造一代的池与停止信号；维持循环由 startGeneration 在 publish 后启动。
func newGeneration(engine *Engine, revision uint64, config core.ClientConfig) *generation {
	forwardCtx, cancel := context.WithCancel(context.Background())
	return &generation{
		engine:     engine,
		revision:   revision,
		config:     config,
		pool:       transport.NewWorkConnPool(config.WorkConnPoolSize()),
		dialer:     transport.Dialer{Timeout: config.Timeout()},
		stopCh:     make(chan struct{}),
		forwardCtx: forwardCtx,
		cancel:     cancel,
		conns:      make(map[*transport.Conn]struct{}),
	}
}

// Engine 是 Core 暴露给宿主的客户端运行门面。
//
// 生命周期为 New → Start → Shutdown → Done。构造函数只做纯内存装配；Start 按
// 配置拨号并建立控制会话与工作连接维持循环；Shutdown 释放全部资源。
// 同一进程可并行运行多个 Engine。
//
// 配置热更：Start 建立第 0 代（revision 0），此后用 Apply 提交新 revision；
// 运行资源按「代」隔离（见 generation）。
type Engine struct {
	config       core.ClientConfig
	logger       *slog.Logger
	dialer       transport.Dialer
	drainTimeout time.Duration

	// shutdownCh 在 Shutdown 时关闭，只约束跨代存活的控制会话读写与心跳循环；
	// 各代的维持循环与转发由该代自己的停止信号收口。
	shutdownCh chan struct{}

	mu       sync.Mutex
	state    engineState
	done     chan struct{}
	finalErr error
	stopOnce sync.Once
	// control 是控制会话，跨代存活：代理集合的变化不重拨控制连接，因此已有桥接
	// 不受换代影响。控制会话的身份（服务端端点、客户端标识、令牌）由 Start 固定，
	// 热更身份被 Apply 拒绝（见 controlIdentityMatches）。
	control *controlSession
	// controlWG 跟踪控制会话的读循环与心跳循环，Shutdown 时按排水上限等待。
	controlWG sync.WaitGroup
	// active 是当前生效的代；首次应用之前为 nil。
	active *generation
	// doneClosed 防止停止事件重复发布：closeDone 与 failAbnormal 都可能到达。
	doneClosed bool
	// lastGood 是最近一次成功 publish 的 revision。
	lastGood uint64
	// applying 表示有一次配置应用正在进行，用于单飞约束。
	applying bool
	// healthCheckHook 是包内测试注入的失败点；生产路径恒为 nil。
	healthCheckHook func(*generation) error
	// targetLn 是历史遗留字段：当前实现不使用本地目标监听器（转发直接拨号目标），
	// 保留以免改动既有装配面。
	targetLn map[string]net.Listener
	// events 是事件中枢：承载订阅登记与事件发布（FR-27）。
	events *core.EventHub
}

// engineState 是 Engine 的内部状态，切换只在持锁下进行。
type engineState int

const (
	stateIdle engineState = iota
	stateStarting
	stateRunning
	stateStopped
)

// Option 是客户端 Engine 的装配选项，只做赋值不做校验。
type Option func(*Engine)

// WithLogger 注入宿主的日志器；未注入时丢弃日志。
//
// 待办：客户端侧目前尚未产生日志事件（服务端侧已有），注入的值只被保存而不被消费。
// 规格 docs/specs/core-engine-facade.md 已约定该注入点，故保留公共面不变；
// 待客户端诊断事件落地后接入，届时同步补规格与用例。
func WithLogger(logger *slog.Logger) Option {
	return func(engine *Engine) {
		engine.logger = logger
	}
}

// New 构造客户端 Engine，只做纯内存装配，不拨号、不启动 goroutine。
//
// config 是首次应用（Start）使用的配置；后续通过 Apply 提交新 revision 替换它。
func New(config core.ClientConfig, options ...Option) *Engine {
	engine := &Engine{
		config:       config,
		done:         make(chan struct{}),
		shutdownCh:   make(chan struct{}),
		targetLn:     make(map[string]net.Listener),
		dialer:       transport.Dialer{Timeout: config.Timeout()},
		drainTimeout: config.DrainTimeout(),
		events:       core.NewEventHub(),
	}
	for _, option := range options {
		option(engine)
	}
	return engine
}

// Start 校验配置、建立控制会话并启动第 0 代的工作连接维持循环。
//
// 只能成功一次；重复调用返回 ErrAlreadyStarted。失败时不遗留半启动的连接或
// 监听器，宿主可修正后重试。
func (engine *Engine) Start(ctx context.Context) error {
	endpoint, err := engine.claimStartSlot()
	if err != nil {
		return err
	}

	control, err := engine.dialControl(ctx, endpoint)
	if err != nil {
		engine.releaseStartSlot()
		return err
	}

	if err := loginControl(ctx, control, engine.config); err != nil {
		_ = control.conn.Close()
		engine.releaseStartSlot()
		return err
	}

	// 首次应用建立第 0 代。revision 取 SnapshotRevisionUnknown：版本号由宿主
	// 分配并单调递增，宿主从 1 开始即可，0 自然表示"尚未由宿主指定版本"。
	gen := newGeneration(engine, core.SnapshotRevisionUnknown, engine.config)
	engine.commitRunning(control, gen)

	engine.startControl(control)
	engine.startGeneration(gen)
	return nil
}

// dialControl 按配置超时拨号控制连接。
//
// 超时来自配置快照：拨号必须带超时，禁止无超时拨号（规格 §3.4）。
func (engine *Engine) dialControl(ctx context.Context, endpoint core.ServerEndpoint) (*controlSession, error) {
	conn, err := engine.dial(ctx, endpoint.Address.String(), transport.PurposeControl)
	if err != nil {
		return nil, fmt.Errorf("客户端拨号失败：%w", err)
	}
	return &controlSession{conn: conn, done: make(chan struct{})}, nil
}

// dial 拨号一条带用途标记的工作连接。
//
// 超时来自配置快照，由传输层强制门禁：禁止无超时拨号（规格 §3.4）。
func (engine *Engine) dial(
	ctx context.Context,
	address string,
	purpose transport.Purpose,
	proxy ...string,
) (*transport.Conn, error) {
	return engine.dialer.Dial(ctx, address, purpose, proxy...)
}

// claimStartSlot 校验配置并原子认领启动位，返回服务端端点。
//
// 认领即把状态置为 starting，后续失败由调用方回滚到 idle。
func (engine *Engine) claimStartSlot() (core.ServerEndpoint, error) {
	if err := engine.config.Validate(); err != nil {
		return core.ServerEndpoint{}, err
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.state != stateIdle {
		if engine.state == stateStarting || engine.state == stateRunning {
			return core.ServerEndpoint{}, ErrAlreadyStarted
		}
		return core.ServerEndpoint{}, ErrStopped
	}
	engine.state = stateStarting
	return engine.config.ServerEndpoint(), nil
}

// releaseStartSlot 把认领失败的启动位回滚到 idle。
func (engine *Engine) releaseStartSlot() {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.state == stateStarting {
		engine.state = stateIdle
	}
}

// commitRunning 把认领成功的启动位推进到 running，并登记控制会话与第 0 代。
func (engine *Engine) commitRunning(control *controlSession, gen *generation) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.state = stateRunning
	engine.control = control
	engine.active = gen
	engine.lastGood = gen.revision
}

// startControl 启动控制会话的读循环与心跳循环。
//
// 它们归 Engine 而不是某一代：控制会话跨代存活，换代不重拨控制连接。
func (engine *Engine) startControl(control *controlSession) {
	engine.controlWG.Add(2)
	go engine.serveControl(control)
	go engine.heartbeatLoop(control)
}

// startGeneration 启动一代的工作连接维持循环。
//
// 与 publish 分开：维持循环要等 active 已指向该代之后才能开始拨号，否则新拨出的
// 待命连接可能在 active 尚未切换时被服务端按旧代凭据校验。
func (engine *Engine) startGeneration(gen *generation) {
	gen.wg.Add(1)
	go engine.maintainWorkConns(gen)
}

// Shutdown 幂等关闭：停止心跳与控制会话，按排水上限等待活动连接，随后释放全部资源。
//
// 停止标记先于等待：读循环检查到停止状态后不再把连接关闭引发的读错误记为异常。
func (engine *Engine) Shutdown(ctx context.Context) error {
	engine.stopOnce.Do(func() {
		engine.markStopped()
	})
	if err := engine.waitDrained(ctx); err != nil {
		return err
	}
	engine.closeDone()
	return nil
}

// markStopped 标记停止、清理异常记录、停止当前代并关闭控制会话。
//
// 取消当前代的转发上下文会中断阻塞的双向转发：工作连接随 Engine 一起收尾，不留
// 悬挂 goroutine。活动连接仍交由 waitDrained 按排水上限等待，不在此处强制关闭。
func (engine *Engine) markStopped() {
	engine.mu.Lock()
	engine.state = stateStopped
	engine.finalErr = nil
	close(engine.shutdownCh)
	active := engine.active
	control := engine.control
	for _, listener := range engine.targetLn {
		_ = listener.Close()
	}
	engine.mu.Unlock()

	if active != nil {
		active.stop(true)
	}
	if control != nil {
		_ = control.conn.Close()
	}
}

// waitDrained 等待控制会话与当前代的活动连接按排水上限结束；超限后强制关闭并继续等待。
func (engine *Engine) waitDrained(ctx context.Context) error {
	engine.mu.Lock()
	active := engine.active
	limit := engine.drainTimeout
	engine.mu.Unlock()

	if err := ctx.Err(); err != nil {
		engine.closeConns()
		return err
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		engine.controlWG.Wait()
		if active != nil {
			active.wg.Wait()
		}
	}()

	deadline := limit
	if ctxDeadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(ctxDeadline); remaining < deadline {
			deadline = remaining
		}
	}
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case <-finished:
	case <-timer.C:
		engine.closeConns()
		<-finished
		return context.DeadlineExceeded
	case <-ctx.Done():
		engine.closeConns()
		<-finished
		return ctx.Err()
	}
	return nil
}

// closeDone 在完全停止后关闭 Done 通道；重复调用安全。
func (engine *Engine) closeDone() {
	engine.mu.Lock()
	already := engine.doneClosed
	engine.doneClosed = true
	final := engine.finalErr
	engine.mu.Unlock()
	if !already {
		// 停止事件先于通道关闭发布（规格 §3.6：先通知后关通道）。
		engine.events.PublishStop(final)
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	select {
	case <-engine.done:
	default:
		close(engine.done)
	}
}

// Done 在完全停止后关闭；Stopped 之前永不关闭，也不返回 nil。
func (engine *Engine) Done() <-chan struct{} {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.done == nil {
		engine.done = make(chan struct{})
	}
	return engine.done
}

// Err 返回导致 Engine 停止的最终错误；正常 Shutdown 后返回 nil。
func (engine *Engine) Err() error {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.finalErr
}

// log 返回可用的日志器；未注入时返回丢弃日志器。
//
// 待办：尚无调用方，保留以配合 WithLogger 的规格约定，接入时同时补用例。
func (engine *Engine) log() *slog.Logger {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.logger != nil {
		return engine.logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// track 登记本代的一条活动连接。
func (gen *generation) track(conn *transport.Conn) {
	gen.engine.mu.Lock()
	defer gen.engine.mu.Unlock()
	gen.conns[conn] = struct{}{}
}

// untrack 移除本代的一条活动连接。
func (gen *generation) untrack(conn *transport.Conn) {
	gen.engine.mu.Lock()
	defer gen.engine.mu.Unlock()
	delete(gen.conns, conn)
}

// closeConns 强制关闭控制连接与当前代的全部活动连接。
//
// 先取快照再关闭：关闭连接会阻塞在系统调用上，持锁关闭会把锁暴露给网络 IO。
func (engine *Engine) closeConns() {
	engine.mu.Lock()
	control := engine.control
	active := engine.active
	engine.mu.Unlock()
	if control != nil {
		_ = control.conn.Close()
	}
	if active != nil {
		active.closeConns()
	}
}

// closeIdleWorkConns 关闭本代所有尚未收到下行字节的工作连接。
//
// 下行字节判据（协议层成立）：工作连接的下行即访客数据；客户端声明连接后，
// 服务端在配对访客前不会写任何字节。因此 Received()==0 的连接必然未承载访客
// 数据，drain 时可立即关闭——新代会重新补充待命连接，而承载访客数据的桥接
// （Received()>0）继续到自然结束，两个承诺同时成立。
func (gen *generation) closeIdleWorkConns() {
	gen.engine.mu.Lock()
	candidates := make([]*transport.Conn, 0, len(gen.conns))
	for conn := range gen.conns {
		candidates = append(candidates, conn)
	}
	gen.engine.mu.Unlock()
	for _, conn := range candidates {
		if conn.Received() == 0 {
			_ = conn.Close()
		}
	}
}

// closeConns 强制关闭本代的全部活动连接。
func (gen *generation) closeConns() {
	gen.engine.mu.Lock()
	conns := make([]*transport.Conn, 0, len(gen.conns))
	for conn := range gen.conns {
		conns = append(conns, conn)
	}
	gen.engine.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

// stop 停止本代补充维持连接，并按需取消本代的转发上下文。
//
// final 只在 Engine 终停时为真：drain 旧代（Apply 路径）只停维持循环，**不**取消
// 在途桥接的转发上下文——已有流要继续到自然结束或排水上限，取消它会当场切断
// 用户连接，正是本功能要避免的。
// 幂等：drain 与 Shutdown 各可能调用一次，重复调用安全（cancel 本身可重复调用）。
func (gen *generation) stop(final bool) {
	gen.stopOnce.Do(func() {
		gen.engine.mu.Lock()
		gen.stopping = true
		gen.engine.mu.Unlock()
		close(gen.stopCh)
	})
	// 未承载访客数据的待命连接立即关闭：drain 与终停都适用，新代会重新补充。
	// 承载访客数据的桥接收 Received()>0 保护，不在此列。
	gen.closeIdleWorkConns()
	if final {
		gen.cancel()
	}
}

// release 释放 prepare 阶段新建、但 publish 之前失败的资源。
//
// 本代尚未发布：没有维持循环与桥接，直接取消上下文并关闭池即可。
func (gen *generation) release() {
	gen.cancel()
	if gen.pool != nil {
		_ = gen.pool.Close()
	}
}

// waitDrained 等待本代的维持循环与桥接按排水上限结束；超限后强制关闭并继续等待。
//
// 只等待本代的等待组：新代连接不受牵连。这正是"切换期间已有流不中断"的实现要点，
// 共用一个等待组会让切换退化成停下来等所有人。
func (gen *generation) waitDrained(ctx context.Context, limit time.Duration) error {
	if err := ctx.Err(); err != nil {
		gen.closeConns()
		return err
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		gen.wg.Wait()
	}()

	deadline := limit
	if ctxDeadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(ctxDeadline); remaining < deadline {
			deadline = remaining
		}
	}
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case <-finished:
	case <-timer.C:
		gen.closeConns()
		<-finished
		return context.DeadlineExceeded
	case <-ctx.Done():
		gen.closeConns()
		<-finished
		return ctx.Err()
	}
	return nil
}

// countProxies 统计一代维持的代理数量，供 Apply 结果报告变更规模。
func countProxies(gen *generation) int {
	return len(gen.config.AllProxies())
}

// connTotal 返回当前活动连接总数：控制会话与当前代的工作连接。
func (engine *Engine) connTotal() int {
	total := 0
	if engine.control != nil {
		total++
	}
	if engine.active != nil {
		total += len(engine.active.conns)
	}
	return total
}

// activeGeneration 返回当前生效的代；首次应用之前为 nil。
//
// 生产路径读 engine.active 时也持同一把锁；这里供包内测试观察代内状态，
// 避免测试直接触碰字段而绕开锁。
func (engine *Engine) activeGeneration() *generation {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.active
}

// isStopping 返回本代是否已停止接收流量。
//
// 供包内测试核验 drain 与 Shutdown 是否真的停掉了对应的代。
func (gen *generation) isStopping() bool {
	gen.engine.mu.Lock()
	defer gen.engine.mu.Unlock()
	return gen.stopping
}

// proxySummaries 返回本代代理摘要列表（规格 §3.5）。
//
// 来源是本代配置快照的代理声明：代理名、类型与运行状态。客户端侧的代理
// 在声明后即由维持循环服务，状态恒为 running。
func (gen *generation) proxySummaries() []core.ProxySummary {
	proxies := gen.config.AllProxies()
	summaries := make([]core.ProxySummary, 0, len(proxies))
	for _, item := range proxies {
		summaries = append(summaries, core.ProxySummary{
			Name:   item.ProxyName(),
			Kind:   string(item.Type()),
			Status: "running",
		})
	}
	return summaries
}

// trackedConns 返回本代已登记活动连接的快照。
//
// 供包内测试等待维持循环真正建起承载访客数据的桥接（Received()>0），避免切换
// 用例跑在"只有待命连接"的窗口里。
func (gen *generation) trackedConns() []*transport.Conn {
	gen.engine.mu.Lock()
	defer gen.engine.mu.Unlock()
	conns := make([]*transport.Conn, 0, len(gen.conns))
	for conn := range gen.conns {
		conns = append(conns, conn)
	}
	return conns
}

// clientLogin 是 wire v1 登录载荷的最小形态。
type clientLogin struct {
	ClientID string `json:"clientID"`
	Token    string `json:"token"`
}

// loginResponse 是登录响应载荷的最小形态。
type loginResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// controlSession 封装一条控制连接的读写状态。
//
// 控制连接的读写必须串行：心跳写入与响应读取不可并发，否则帧边界错乱。
// 本结构用一把互斥锁串行全部控制帧写出，读循环独占读取。
type controlSession struct {
	conn *transport.Conn
	done chan struct{}

	mu sync.Mutex
}

// loginControl 在控制连接上完成登录握手。
func loginControl(ctx context.Context, control *controlSession, config core.ClientConfig) error {
	if err := control.writeLogin(config); err != nil {
		return err
	}
	return control.readLoginResponse(config.Timeout())
}

// writeLogin 编码并发送登录帧。
func (control *controlSession) writeLogin(config core.ClientConfig) error {
	body, err := json.Marshal(clientLogin{ClientID: config.ClientID(), Token: config.Auth().Token})
	if err != nil {
		return err
	}
	encoded, err := wire.EncodeV1Frame(wire.Frame{Type: wire.MessageTypeLogin, Payload: body})
	if err != nil {
		return err
	}
	writeDeadline := time.Now().Add(config.Timeout())
	if deadlineErr := control.conn.SetWriteDeadline(writeDeadline); deadlineErr != nil {
		return deadlineErr
	}
	control.mu.Lock()
	_, err = control.conn.Write(encoded)
	control.mu.Unlock()
	if err != nil {
		return fmt.Errorf("发送登录帧失败：%w", err)
	}
	_ = control.conn.SetWriteDeadline(time.Time{})
	return nil
}

// readLoginResponse 读取并校验登录响应帧。
func (control *controlSession) readLoginResponse(timeout time.Duration) error {
	readDeadline := time.Now().Add(timeout)
	if deadlineErr := control.conn.SetReadDeadline(readDeadline); deadlineErr != nil {
		return deadlineErr
	}
	defer func() { _ = control.conn.SetReadDeadline(time.Time{}) }()

	reader := wire.NewV1Reader(control.conn, wire.DefaultV1PayloadLimit)
	frame, err := reader.ReadFrame()
	if err != nil {
		return fmt.Errorf("读取登录响应失败：%w", err)
	}
	defer frame.Release()
	if frame.Type.Name != "login-response" {
		return errors.New("登录响应类型不符")
	}
	var response loginResponse
	if err := json.Unmarshal(frame.Payload, &response); err != nil {
		return fmt.Errorf("登录响应载荷非法：%w", err)
	}
	if !response.OK {
		return errors.New("服务端拒绝客户端登录")
	}
	return nil
}

// serveControl 维持控制连接的读循环：服务端主动关闭或出错即记录异常并退出。
//
// 控制会话跨代存活，因此停止信号取 Engine 级的 shutdownCh 而不是某一代的标记。
func (engine *Engine) serveControl(control *controlSession) {
	defer engine.controlWG.Done()
	reader := wire.NewV1Reader(control.conn, wire.DefaultV1PayloadLimit)
	for {
		select {
		case <-engine.shutdownCh:
			return
		default:
		}
		frame, err := reader.ReadFrame()
		if err != nil {
			engine.failAbnormal(err)
			return
		}
		frame.Release()
	}
}

// heartbeatLoop 按当前生效代的心跳间隔发送 ping；写入失败即记录异常并退出。
//
// 每个周期重新读取间隔：心跳属于配置快照，换代后新的间隔应立即生效，而不是
// 绑定在控制会话建立时的取值上。
func (engine *Engine) heartbeatLoop(control *controlSession) {
	defer engine.controlWG.Done()
	for {
		ticker := time.NewTicker(engine.heartbeatInterval())
		select {
		case <-engine.shutdownCh:
			ticker.Stop()
			return
		case <-ticker.C:
		}
		ticker.Stop()

		encoded, err := wire.EncodeV1Frame(wire.Frame{Type: wire.MessageTypePing, Payload: []byte(`{}`)})
		if err != nil {
			engine.failAbnormal(err)
			return
		}
		control.mu.Lock()
		_, err = control.conn.Write(encoded)
		control.mu.Unlock()
		if err != nil {
			engine.failAbnormal(err)
			return
		}
	}
}

// heartbeatInterval 返回当前生效代的心跳间隔；无生效代时取 Core 默认值。
func (engine *Engine) heartbeatInterval() time.Duration {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.active != nil {
		if interval := engine.active.config.Heartbeat(); interval > 0 {
			return interval
		}
	}
	return core.DefaultHeartbeat
}

// failAbnormal 记录异常停止的首个错误并关闭 Done。
//
// 正常 Shutdown 路径不得调用：Shutdown 后 Err() 必须保持 nil。
// 引擎已进入停止流程时调用为空操作——此时连接关闭引发的读错误是 Shutdown
// 的预期结果，不属于异常终止。
func (engine *Engine) failAbnormal(err error) {
	engine.mu.Lock()
	if engine.state == stateStopped {
		engine.mu.Unlock()
		return
	}
	if engine.finalErr == nil {
		engine.finalErr = err
	}
	shouldPublish := !engine.doneClosed
	engine.doneClosed = true
	closeDone := engine.done == nil
	if closeDone {
		engine.done = make(chan struct{})
	}
	select {
	case <-engine.done:
	default:
		close(engine.done)
	}
	engine.mu.Unlock()
	if shouldPublish {
		// 异常停止同样先发事件再关订阅通道（规格 §5 错误路径第五条）。
		engine.events.PublishStop(err)
	}
}

// maintainWorkConns 为本代的每个代理维持待命工作连接。
//
// 每个代理独立循环：先占用池槽位（受池上限约束），再建链、声明归属并服务
// 转发；连接结束后释放槽位并重建下一条。停止时本代的待命连接随该代一起关闭。
// 四种代理共用本循环：差异只在本地目标的网络类型与转发形态。
func (engine *Engine) maintainWorkConns(gen *generation) {
	defer gen.wg.Done()
	for _, proxy := range gen.config.AllProxies() {
		gen.wg.Add(1)
		go engine.maintainOneProxy(gen, proxy)
	}
}

// maintainOneProxy 为单个代理维持一条待命工作连接并在配对后服务转发。
//
// 池槽位在整条工作连接的生命周期内持有：达到池上限时本循环退避后重试，
// 绝不无限等待也不静默放弃（规格 §3.5）。
// UDP 会话长期占用一条工作连接，若等它结束再建下一条，第二个对端将永远拿
// 不到连接。因此 UDP 代理的转发交给独立 goroutine，本循环只负责持续补充待命
// 连接，二者的生命周期解耦。
func (engine *Engine) maintainOneProxy(gen *generation, proxy core.ClientProxy) {
	defer gen.wg.Done()
	for {
		select {
		case <-gen.stopCh:
			return
		default:
		}
		slot, err := gen.pool.Acquire(context.Background(), proxy.ProxyName())
		if err != nil {
			// 池已满：退避后重试，避免忙循环。
			select {
			case <-gen.stopCh:
				return
			case <-time.After(poolRetryBackoff):
				continue
			}
		}
		if proxy.Type() == core.ProxyTypeUDP {
			gen.wg.Add(1)
			go func() {
				defer gen.wg.Done()
				defer slot.Release()
				engine.serveOneWorkConn(gen, proxy)
			}()
			continue
		}
		engine.serveOneWorkConn(gen, proxy)
		slot.Release()
	}
}

// serveOneWorkConn 建链、声明归属并服务一条工作连接，直到连接结束。
//
// 声明载荷携带本地目标地址：服务端据此判定目标是否在该客户端被允许的地址
// 集合内，越权即拒绝（FR-06a §3.3）。
func (engine *Engine) serveOneWorkConn(gen *generation, proxy core.ClientProxy) {
	work, err := gen.dialer.Dial(
		context.Background(), gen.config.ServerEndpoint().Address.String(), transport.PurposeWork, proxy.ProxyName())
	if err != nil {
		select {
		case <-gen.stopCh:
		case <-time.After(retryBackoff):
		}
		return
	}
	// 停止复查：拨号期间本代可能已被停（drain/Shutdown）。此时连接尚未登记，
	// closeStandby 的清理覆盖不到它，直接关闭并返回，否则它会作为"漏网待命"
	// 桥接挂进旧代等待组，让 drain 永远等不满。
	select {
	case <-gen.stopCh:
		_ = work.Close()
		return
	default:
	}
	auth := gen.config.Auth()
	if err := declareWorkConn(work, gen.config.ClientID(), auth.Token, proxy.ProxyName(), proxy.ProxyLocalAddr()); err != nil {
		_ = work.Close()
		return
	}
	// 声明已发送：服务端暂存该连接等待访客，本端拨号本地目标后进入转发。
	gen.track(work)
	engine.serveWorkConn(gen, work, proxy)
	gen.untrack(work)
	_ = work.Close()
}

// serveWorkConn 把一条已声明归属的工作连接桥接或会话化到本地目标。
//
// UDP 代理的本地目标是数据报语义，因此按会话形态转发而非字节流桥接
// （规格 §3.4：不得用 TCP 的关闭语义推断 UDP 状态）。
// 本地目标的拨号同样带配置超时，禁止无超时拨号。
//
// 桥接期间下行字节计数持续累加：drain 旧代时以 Received()==0 识别未承载
// 访客数据的待命连接（见 closeIdleWorkConns），活动桥接受该判据保护。
func (engine *Engine) serveWorkConn(gen *generation, work *transport.Conn, proxy core.ClientProxy) {
	if proxy.Type() == core.ProxyTypeUDP {
		engine.serveUDPWorkConn(gen, work, proxy.ProxyLocalAddr())
		return
	}
	targetConn, err := gen.dialer.Dial(context.Background(), proxy.ProxyLocalAddr().String(), transport.PurposeWork)
	if err != nil {
		return
	}
	defer func() { _ = targetConn.Close() }()
	transport.Bridge(gen.forwardCtx, work, targetConn)
}

// serveUDPWorkConn 把一条工作连接上的数据报往返转发到本地 UDP 目标。
//
// UDP 会话是长驻的：它会一直服务到会话空闲回收，不像 TCP 转发那样随任一端
// 关闭而自然结束。因此这里必须显式响应停止信号：由本代的停止信号与转发上下文
// 共同收口，避免 Shutdown 被一条长驻会话悬挂。
func (engine *Engine) serveUDPWorkConn(gen *generation, work *transport.Conn, target netip.AddrPort) {
	socket, err := gen.dialer.DialUDP(target)
	if err != nil {
		return
	}
	defer func() { _ = socket.Close() }()

	sessionCtx, stop := context.WithCancel(gen.forwardCtx)
	defer stop()
	go func() {
		// 本代停止即关闭会话两侧的承载连接：会话无处可退，必须显式收口。
		select {
		case <-gen.stopCh:
			_ = socket.Close()
			stop()
		case <-sessionCtx.Done():
		}
	}()
	proxy.ServeUDPClient(work, socket, sessionCtx)
}

// declareWorkConn 在新建的工作连接首帧声明代理归属与本地目标地址，
// 供服务端配对访客并校验目标地址是否在允许集合内（FR-06a §3.3）。
func declareWorkConn(
	work *transport.Conn, clientID, token, proxyName string, target netip.AddrPort,
) error {
	body, err := json.Marshal(workConnRequest{
		ClientID: clientID,
		Token:    token,
		RunID:    proxyName,
		Proxy:    proxyName,
		Target:   target.String(),
	})
	if err != nil {
		return err
	}
	encoded, err := wire.EncodeV1Frame(wire.Frame{Type: wire.MessageTypeNewWorkConn, Payload: body})
	if err != nil {
		return err
	}
	_, err = work.Write(encoded)
	return err
}

// workConnRequest 是客户端向服务端声明新建工作连接的载荷。
//
// target 承载本条工作连接最终转发到的本地目标地址，服务端据此判定目标是否在
// 该客户端被允许的地址集合内（FR-06a §3.3）。
type workConnRequest struct {
	ClientID string `json:"client_id"`
	Token    string `json:"token"`
	RunID    string `json:"run_id"`
	Proxy    string `json:"proxy_name"`
	Target   string `json:"target_addr"`
}
