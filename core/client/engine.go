package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/wcpe/jrp/core"
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

// Engine 是 Core 暴露给宿主的客户端运行门面。
//
// 生命周期为 New → Start → Shutdown → Done。构造函数只做纯内存装配；Start 按
// 配置拨号并建立控制会话与本地目标监听；Shutdown 释放全部资源。
// 同一进程可并行运行多个 Engine。
type Engine struct {
	config       core.ClientConfig
	logger       *slog.Logger
	dialer       transport.Dialer
	drainTimeout time.Duration
	pool         *transport.WorkConnPool

	mu       sync.Mutex
	state    engineState
	done     chan struct{}
	finalErr error
	stopOnce sync.Once
	wg       sync.WaitGroup
	conns    map[*transport.Conn]struct{}
	targetLn map[string]net.Listener
	stopCh   chan struct{}
	stopCtx  context.Context
	// cancelWork 取消转发上下文，在 Shutdown 时调用一次。
	cancelWork context.CancelFunc
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
func WithLogger(logger *slog.Logger) Option {
	return func(engine *Engine) {
		engine.logger = logger
	}
}

// New 构造客户端 Engine，只做纯内存装配，不拨号、不启动 goroutine。
func New(config core.ClientConfig, options ...Option) *Engine {
	engine := &Engine{
		config:       config,
		done:         make(chan struct{}),
		conns:        make(map[*transport.Conn]struct{}),
		targetLn:     make(map[string]net.Listener),
		stopCh:       make(chan struct{}),
		dialer:       transport.Dialer{Timeout: config.Timeout()},
		drainTimeout: config.DrainTimeout(),
		pool:         transport.NewWorkConnPool(config.WorkConnPoolSize()),
	}
	// 转发上下文：Engine 生命周期内唯一，随 Shutdown 取消，用于中断阻塞的转发。
	// 每条工作连接各建一个会随连接轮换累积 goroutine，此处必须按 Engine 持有。
	engine.stopCtx, engine.cancelWork = context.WithCancel(context.Background())
	for _, option := range options {
		option(engine)
	}
	return engine
}

// Start 校验配置、建立控制会话并启动本地目标监听。
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

	engine.commitRunning()

	engine.track(control.conn)
	engine.wg.Add(3)
	go engine.serveControl(control)
	go engine.heartbeatLoop(control)
	go engine.maintainWorkConns(endpoint)
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

// commitRunning 把认领成功的启动位推进到 running。
func (engine *Engine) commitRunning() {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.state = stateRunning
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

// markStopped 标记停止、清理异常记录、取消转发上下文并关闭本地监听器。
//
// 取消转发上下文会中断阻塞的双向转发：工作连接随 Engine 一起收尾，不留悬挂
// goroutine。活动连接仍交由 waitDrained 按排水上限等待，不在此处强制关闭。
func (engine *Engine) markStopped() {
	engine.mu.Lock()
	engine.state = stateStopped
	engine.finalErr = nil
	close(engine.stopCh)
	engine.cancelWork()
	for _, listener := range engine.targetLn {
		_ = listener.Close()
	}
	engine.mu.Unlock()
}

// waitDrained 等待活动连接按排水上限结束；超限后强制关闭并继续等待。
func (engine *Engine) waitDrained(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		engine.closeConns()
		return err
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		engine.wg.Wait()
	}()

	deadline := engine.drainTimeout
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

// closeDone 在完全停止后关闭 Done 通道并释放转发上下文；重复调用安全。
//
// 取消函数在此统一收口：未 Start 就 Shutdown 的路径也经过这里，因此不会
// 残留未取消的 context（go vet 的 lostcancel 检查点）。
func (engine *Engine) closeDone() {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.cancelWork()
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
func (engine *Engine) log() *slog.Logger {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.logger != nil {
		return engine.logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// track 登记一条活动连接。
func (engine *Engine) track(conn *transport.Conn) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.conns[conn] = struct{}{}
}

// untrack 移除一条活动连接。
func (engine *Engine) untrack(conn *transport.Conn) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	delete(engine.conns, conn)
}

// closeConns 强制关闭全部活动连接。
func (engine *Engine) closeConns() {
	engine.mu.Lock()
	conns := make([]*transport.Conn, 0, len(engine.conns))
	for conn := range engine.conns {
		conns = append(conns, conn)
	}
	engine.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
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
func (engine *Engine) serveControl(control *controlSession) {
	defer engine.wg.Done()
	defer engine.untrack(control.conn)
	reader := wire.NewV1Reader(control.conn, wire.DefaultV1PayloadLimit)
	for {
		select {
		case <-engine.stopCh:
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

// heartbeatLoop 按配置心跳间隔发送 ping；写入失败即记录异常并退出。
func (engine *Engine) heartbeatLoop(control *controlSession) {
	defer engine.wg.Done()
	interval := engine.config.Heartbeat()
	if interval <= 0 {
		interval = core.DefaultHeartbeat
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-engine.stopCh:
			return
		case <-ticker.C:
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
	select {
	case <-engine.done:
	default:
		close(engine.done)
	}
	engine.mu.Unlock()
}

// maintainWorkConns 为每个代理维持待命工作连接。
//
// 每个代理独立循环：先占用池槽位（受池上限约束），再建链、声明归属并服务
// 转发；连接结束后释放槽位并重建下一条。停止时全部待命连接随 Engine 一起关闭。
func (engine *Engine) maintainWorkConns(endpoint core.ServerEndpoint) {
	defer engine.wg.Done()
	for _, proxy := range engine.config.Proxies() {
		engine.wg.Add(1)
		go engine.maintainOneProxy(endpoint, proxy)
	}
}

// maintainOneProxy 为单个代理维持一条待命工作连接并在配对后服务转发。
//
// 池槽位在整条工作连接的生命周期内持有：达到池上限时本循环退避后重试，
// 绝不无限等待也不静默放弃（规格 §3.5）。
func (engine *Engine) maintainOneProxy(endpoint core.ServerEndpoint, proxy core.TCPProxy) {
	defer engine.wg.Done()
	for {
		select {
		case <-engine.stopCh:
			return
		default:
		}
		slot, err := engine.pool.Acquire(context.Background(), proxy.Name)
		if err != nil {
			// 池已满：退避后重试，避免忙循环。
			select {
			case <-engine.stopCh:
				return
			case <-time.After(poolRetryBackoff):
				continue
			}
		}
		engine.serveOneWorkConn(endpoint, proxy)
		slot.Release()
	}
}

// serveOneWorkConn 建链、声明归属并服务一条工作连接，直到连接结束。
func (engine *Engine) serveOneWorkConn(endpoint core.ServerEndpoint, proxy core.TCPProxy) {
	work, err := engine.dial(context.Background(), endpoint.Address.String(), transport.PurposeWork, proxy.Name)
	if err != nil {
		select {
		case <-engine.stopCh:
		case <-time.After(retryBackoff):
		}
		return
	}
	if err := declareWorkConn(work, proxy.Name); err != nil {
		_ = work.Close()
		return
	}
	// 声明已发送：服务端暂存该连接等待访客，本端拨号本地目标后进入双向转发。
	engine.track(work)
	engine.serveWorkConn(work, proxy.LocalAddr.String())
	engine.untrack(work)
	_ = work.Close()
}

// serveWorkConn 把一条已声明归属的工作连接桥接到本地目标。
//
// 本地目标的拨号同样带配置超时，禁止无超时拨号。任一方向结束即关闭两端并
// 返回，调用方负责释放池槽位并重建下一条待命连接。
func (engine *Engine) serveWorkConn(work *transport.Conn, target string) {
	targetConn, err := engine.dialer.Dial(context.Background(), target, transport.PurposeWork)
	if err != nil {
		return
	}
	defer func() { _ = targetConn.Close() }()
	transport.Bridge(engine.stopCtx, work, targetConn)
}

// declareWorkConn 在新建的工作连接首帧声明代理归属，供服务端配对访客。
func declareWorkConn(work *transport.Conn, proxyName string) error {
	body, err := json.Marshal(map[string]string{"run_id": proxyName, "proxy_name": proxyName})
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
