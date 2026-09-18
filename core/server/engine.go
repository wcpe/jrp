package server

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
	"github.com/wcpe/jrp/core/internal/transport"
	"github.com/wcpe/jrp/core/internal/wire"
)

// ErrAlreadyStarted 表示 Engine 已在运行，重复 Start 非法。
var ErrAlreadyStarted = errors.New("服务端引擎已启动，不允许重复启动")

// ErrNotStarted 表示 Engine 尚未 Start。
var ErrNotStarted = errors.New("服务端引擎尚未启动")

// ErrStopped 表示 Engine 已停止，不允许重启。
var ErrStopped = errors.New("服务端引擎已停止，不允许重启")

// Engine 是 Core 暴露给宿主的服务端运行门面。
//
// 生命周期为 New → Start → Shutdown → Done。构造函数只做纯内存装配；Start 接管
// 宿主注入的监听器；Shutdown 释放全部资源。同一进程可并行运行多个 Engine。
type Engine struct {
	config       core.ServerConfig
	logger       *slog.Logger
	dialer       transport.Dialer
	drainTimeout time.Duration
	listener     *transport.Listener

	mu           sync.Mutex
	state        engineState
	stopping     bool
	done         chan struct{}
	finalErr     error
	stopOnce     sync.Once
	wg           sync.WaitGroup
	conns        map[*transport.Conn]struct{}
	controlConns map[*transport.Conn]struct{}
	guestLns     map[string]*transport.Listener
	guestAddr    map[string]net.Addr
	workConns    *workBroker
	clients      map[string]*clientSession

	heartbeat time.Duration
}

// clientSession 是已登录客户端的会话状态：控制连接与待命工作连接配对。
type clientSession struct {
	clientID string
	control  *transport.Conn
}

// engineState 是 Engine 的内部状态，切换只在持锁下进行。
type engineState int

const (
	stateIdle engineState = iota
	stateStarting
	stateRunning
	stateStopped
)

// Option 是服务端 Engine 的装配选项，只做赋值不做校验。
type Option func(*Engine)

// WithListener 注入宿主管理的监听器。
func WithListener(listener net.Listener) Option {
	return func(engine *Engine) {
		engine.listener = transport.TakeOverListener(listener)
	}
}

// WithLogger 注入宿主的日志器；未注入时丢弃日志。
func WithLogger(logger *slog.Logger) Option {
	return func(engine *Engine) {
		engine.logger = logger
	}
}

// New 构造服务端 Engine，只做纯内存装配，不绑定端口、不启动 goroutine。
func New(config core.ServerConfig, options ...Option) *Engine {
	engine := &Engine{
		config:       config,
		done:         make(chan struct{}),
		conns:        make(map[*transport.Conn]struct{}),
		controlConns: make(map[*transport.Conn]struct{}),
		guestLns:     make(map[string]*transport.Listener),
		guestAddr:    make(map[string]net.Addr),
		workConns:    newWorkBroker(config.IdleWorkConnLimit()),
		clients:      make(map[string]*clientSession),
		heartbeat:    config.Heartbeat(),
		dialer:       transport.Dialer{Timeout: config.Timeout()},
		drainTimeout: config.DrainTimeout(),
	}
	for _, option := range options {
		option(engine)
	}
	return engine
}

// Start 校验配置并接管宿主注入的监听器，进入运行。
//
// 只能成功一次；重复调用返回 ErrAlreadyStarted。失败时不关闭宿主的 listener，
// Engine 自行创建的资源由本函数释放，不遗留半启动状态。
func (engine *Engine) Start(ctx context.Context) error {
	listener, err := engine.claimStartSlot()
	if err != nil {
		return err
	}
	if listener == nil {
		engine.releaseStartSlot()
		return fmt.Errorf("服务端引擎缺少宿主注入的监听器：%w", ErrNotStarted)
	}
	if !listenerUsable(listener.Listener()) {
		engine.releaseStartSlot()
		return fmt.Errorf("宿主注入的监听器已关闭或不可用：%w", ErrNotStarted)
	}

	guests, addresses, err := engine.openGuestListeners()
	if err != nil {
		engine.releaseStartSlot()
		return fmt.Errorf("打开访客监听器失败：%w", err)
	}

	engine.commitRunning(guests, addresses)

	engine.wg.Add(1)
	go engine.serveControl(listener)
	for name, guestListener := range guests {
		engine.wg.Add(1)
		go engine.serveGuest(name, guestListener)
	}
	_ = ctx
	return nil
}

// claimStartSlot 校验配置并原子认领启动位。
//
// 认领即把状态置为 starting，后续失败由调用方回滚到 idle。重复启动返回哨兵
// 错误而不触碰任何资源。返回宿主注入的监听器句柄。
func (engine *Engine) claimStartSlot() (*transport.Listener, error) {
	if err := engine.config.Validate(); err != nil {
		return nil, err
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.state != stateIdle {
		if engine.state == stateStarting || engine.state == stateRunning {
			return nil, ErrAlreadyStarted
		}
		return nil, ErrStopped
	}
	engine.state = stateStarting
	return engine.listener, nil
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
func (engine *Engine) commitRunning(
	guests map[string]*transport.Listener,
	addresses map[string]net.Addr,
) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.state = stateRunning
	engine.guestLns = guests
	engine.guestAddr = addresses
}

// openGuestListeners 为每个代理绑定打开访客监听器。
//
// 监听地址来自代理绑定的 RemotePort 与 Engine 自身的监听端点族：Review 端口是
// 配置的一部分，不得在代码里写死回环地址。
// 任一失败时释放已打开的监听器并返回错误，不遗留半启动状态。
func (engine *Engine) openGuestListeners() (map[string]*transport.Listener, map[string]net.Addr, error) {
	bindings := engine.config.Bindings()
	guests := make(map[string]*transport.Listener, len(bindings))
	addresses := make(map[string]net.Addr, len(bindings))
	for _, binding := range bindings {
		guestListener, err := engine.listenGuest(binding.RemotePort)
		if err != nil {
			releaseGuestListeners(guests)
			return nil, nil, err
		}
		guests[binding.Name] = guestListener
		addresses[binding.Name] = guestListener.Addr()
	}
	return guests, addresses, nil
}

// listenGuest 按监听端点的地址族打开一个访客监听器。
func (engine *Engine) listenGuest(port int) (*transport.Listener, error) {
	host := engine.config.Listen().Address.Addr()
	if !host.IsValid() {
		host = netip.IPv4Unspecified()
	}
	listener, err := net.Listen("tcp", netip.AddrPortFrom(host, uint16(port)).String())
	if err != nil {
		return nil, err
	}
	return transport.TakeOverListener(listener), nil
}

// releaseGuestListeners 释放一批已创建的访客监听器。
func releaseGuestListeners(guests map[string]*transport.Listener) {
	for _, listener := range guests {
		_ = listener.Release()
	}
}

// Shutdown 幂等关闭：先停止接收新连接，再按排水上限等待活动连接自然结束，
// 超限才强制关闭，最后关闭 Done。
func (engine *Engine) Shutdown(ctx context.Context) error {
	engine.stopOnce.Do(func() {
		engine.stopListenersLocked()
	})
	if err := engine.waitDrained(ctx); err != nil {
		return err
	}
	engine.closeDone()
	return nil
}

// stopListenersLocked 停止接收新连接并释放暂存，但不触碰数据桥接。
//
// 排水语义分两类：控制连接不承载用户数据，立即释放；数据桥接承载活动流，
// 交由 waitDrained 按排水上限等待自然结束。
// 状态先置 stopping 再关监听器：各读循环检查到停止标记后不再把读错误记为异常。
func (engine *Engine) stopListenersLocked() {
	engine.mu.Lock()
	engine.stopping = true
	if engine.listener != nil {
		_ = engine.listener.Release()
	}
	for _, guestListener := range engine.guestLns {
		_ = guestListener.Release()
	}
	controls := make([]*transport.Conn, 0, len(engine.controlConns))
	for conn := range engine.controlConns {
		controls = append(controls, conn)
	}
	// 暂存但未配对的工作连接由 broker 释放；它们尚不承载用户数据。
	engine.workConns.closeStaged()
	engine.mu.Unlock()

	for _, conn := range controls {
		_ = conn.Close()
	}
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

// closeDone 标记完全停止并关闭 Done 通道；重复调用安全。
func (engine *Engine) closeDone() {
	engine.mu.Lock()
	engine.state = stateStopped
	engine.finalErr = nil
	if engine.done != nil {
		select {
		case <-engine.done:
		default:
			close(engine.done)
		}
	}
	engine.mu.Unlock()
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

// GuestAddr 返回指定代理的访客入口地址，用于测试与宿主拨测。
func (engine *Engine) GuestAddr(name string) net.Addr {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.guestAddr[name]
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

// listenerUsable 探测宿主注入的监听器是否可用。
//
// 已关闭的监听器必须在 Start 阶段显式失败，不能等到 Accept 循环才发现：
// 后者会让 Start 先成功返回再悄悄退出，与「Start 成功即接管」的契约矛盾。
// 探测用带超时的临时 Accept，不阻塞、不消费真实连接。
func listenerUsable(listener net.Listener) bool {
	type tempDeadliner interface {
		SetDeadline(time.Time) error
	}
	deadliner, ok := listener.(tempDeadliner)
	if !ok {
		return true
	}
	if err := deadliner.SetDeadline(time.Now().Add(10 * time.Millisecond)); err != nil {
		return false
	}
	accepted, err := listener.Accept()
	_ = deadliner.SetDeadline(time.Time{})
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return true
		}
		return false
	}
	_ = accepted.Close()
	return true
}

// track 登记一条活动连接。
func (engine *Engine) track(conn *transport.Conn) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.conns[conn] = struct{}{}
}

// trackControl 登记一条控制连接，供 Shutdown 立即释放。
//
// 控制连接不承载用户数据，排水时可直接关闭；数据桥接另有 conns 管理。
func (engine *Engine) trackControl(conn *transport.Conn) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.controlConns[conn] = struct{}{}
}

// untrackControl 移除一条控制连接。
func (engine *Engine) untrackControl(conn *transport.Conn) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	delete(engine.controlConns, conn)
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

// serveControl 接受控制连接并逐条处理登录与心跳。
//
// Accept 错误按传输层分类处理：临时错误退避后继续，致命错误停止循环并上报，
// 绝不静默退出（规格 §3.6）。
func (engine *Engine) serveControl(listener *transport.Listener) {
	defer engine.wg.Done()
	for {
		conn, action, err := listener.Accept(transport.PurposeControl)
		if err != nil {
			if action == transport.AcceptFatal {
				engine.reportAcceptFatal(listener, err)
				return
			}
			// 临时错误：退避后继续，避免把 Accept 循环变成忙循环。
			time.Sleep(transport.AcceptBackoff(action))
			continue
		}
		engine.wg.Add(1)
		go engine.handleControl(conn)
	}
}

// reportAcceptFatal 上报致命的 Accept 错误：停止监听后记录为异常终止。
//
// 引擎已进入停止流程时为空操作：Shutdown 关闭监听器引发的 Accept 错误是预期
// 结果，不属于异常终止。
func (engine *Engine) reportAcceptFatal(listener *transport.Listener, err error) {
	engine.mu.Lock()
	stopping := engine.stopping || engine.state == stateStopped
	engine.mu.Unlock()
	if stopping {
		return
	}
	engine.log().Error("监听循环因致命错误停止", "listener", listener.Addr().String(), "error", err)
	engine.failAbnormal(fmt.Errorf("监听 %s 的 Accept 失败：%w", listener.Addr().String(), err))
}

// handleControl 处理一条控制连接：版本判定 → 登录 → 心跳/工作连接服务。
//
// 控制连接只承载登录与心跳；工作连接是独立的 TCP 连接，由客户端主动拨号到
// 同一监听器建立。两类连接用首帧类型区分：登录帧走控制路径，工作声明帧走
// 配对路径。
func (engine *Engine) handleControl(raw *transport.Conn) {
	defer engine.wg.Done()
	engine.track(raw)
	defer engine.untrack(raw)
	engine.trackControl(raw)
	defer engine.untrackControl(raw)

	guard := wire.NewConnectionGuard(raw, nil, wire.Options{
		MaxWireVersion: wire.VersionV1,
		V2Enabled:      false,
	})
	version, err := guard.DetectVersion(raw)
	if err != nil {
		engine.failAbnormal(err)
		return
	}
	if version != wire.VersionV1 {
		engine.failAbnormal(errors.New("服务端仅接受 wire v1"))
		return
	}
	// 版本判定已把读取器绑定到回放后的流：用守卫统一入口读取，
	// 不得重建 V1Reader，否则会重复消费版本判定阶段的预读字节。
	first, err := guard.ReadFrame()
	if err != nil {
		engine.failAbnormal(err)
		return
	}
	switch first.Type.Name {
	case "login":
		clientID, err := engine.handleLogin(raw, first.Payload)
		first.Release()
		if err != nil {
			engine.failAbnormal(err)
			return
		}
		engine.registerClient(clientID, raw)
		engine.serveControlLoop(raw, guard, clientID)
		engine.unregisterClient(clientID)
	case "new-work-conn":
		engine.serveWorkDeclaration(raw, first.Payload)
		first.Release()
	default:
		first.Release()
		engine.failAbnormal(errors.New("控制连接首帧类型非法"))
	}
}

// serveControlLoop 在已登录的控制连接上处理心跳，直到连接结束。
//
// 心跳中断或未知帧都视为异常终止：记录首个异常错误并关闭 Done，
// 使宿主可通过 Err() 判定。正常 Shutdown 不经过本路径。
func (engine *Engine) serveControlLoop(conn *transport.Conn, guard *wire.ConnectionGuard, clientID string) {
	_ = clientID
	for {
		frame, err := guard.ReadFrame()
		if err != nil {
			engine.failAbnormal(err)
			return
		}
		switch frame.Type.Name {
		case "ping":
			frame.Release()
			if err := engine.replyPong(conn); err != nil {
				engine.failAbnormal(err)
				return
			}
		default:
			frame.Release()
			engine.failAbnormal(errors.New("控制连接收到未知帧"))
			return
		}
	}
}

// failAbnormal 记录异常停止的首个错误并关闭 Done。
//
// 正常 Shutdown 路径不得调用：Shutdown 后 Err() 必须保持 nil。
// 引擎已进入停止流程时调用为空操作——此时连接关闭引发的读错误是 Shutdown
// 的预期结果，不属于异常终止。
// 传入的错误不得包含 token、密码、Authorization 或正文原文。
func (engine *Engine) failAbnormal(err error) {
	engine.mu.Lock()
	if engine.stopping || engine.state == stateStopped {
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

// serveWorkDeclaration 处理一条工作连接的归属声明并与访客配对。
func (engine *Engine) serveWorkDeclaration(raw *transport.Conn, payload []byte) {
	proxyName, err := parseWorkDeclaration(payload)
	if err != nil {
		_ = raw.Close()
		return
	}
	// 有等待访客则立即配对，否则暂存等待访客到达。
	// 配对成功后桥接纳入 WaitGroup，由 Shutdown 按排水上限等待。
	if !engine.workConns.park(proxyName, raw, engine.track, engine.wg.Add) {
		_ = raw.Close()
	}
}

// parseWorkDeclaration 解析工作连接声明载荷，返回代理归属名。
func parseWorkDeclaration(payload []byte) (string, error) {
	var request workConnRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return "", err
	}
	if request.Proxy == "" {
		return "", errors.New("工作连接未声明代理归属")
	}
	return request.Proxy, nil
}

// loginPayload 是 wire v1 登录载荷的最小形态。
type loginPayload struct {
	ClientID string `json:"clientID"`
	Token    string `json:"token"`
}

// loginResponsePayload 是登录响应载荷的最小形态。
type loginResponsePayload struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// handleLogin 校验客户端凭证并回复登录结果。
func (engine *Engine) handleLogin(conn *transport.Conn, payload []byte) (string, error) {
	var request loginPayload
	if err := json.Unmarshal(payload, &request); err != nil {
		_ = engine.writeLoginResponse(conn, false, "登录载荷非法")
		return "", err
	}
	matched := false
	for _, credential := range engine.config.Credentials() {
		if credential.ClientID == request.ClientID && credential.Token == request.Token {
			matched = true
			break
		}
	}
	if !matched {
		_ = engine.writeLoginResponse(conn, false, "鉴权未通过")
		return "", errors.New("服务端拒绝客户端登录")
	}
	if err := engine.writeLoginResponse(conn, true, ""); err != nil {
		return "", err
	}
	return request.ClientID, nil
}

// writeLoginResponse 写出登录响应帧。
func (engine *Engine) writeLoginResponse(conn *transport.Conn, ok bool, message string) error {
	response := loginResponsePayload{OK: ok, Error: message}
	body, err := json.Marshal(response)
	if err != nil {
		return err
	}
	encoded, err := wire.EncodeV1Frame(wire.Frame{Type: wire.MessageTypeLoginResponse, Payload: body})
	if err != nil {
		return err
	}
	_, err = conn.Write(encoded)
	return err
}

// replyPong 回复心跳。
func (engine *Engine) replyPong(conn *transport.Conn) error {
	body := []byte(`{}`)
	encoded, err := wire.EncodeV1Frame(wire.Frame{Type: wire.MessageTypePong, Payload: body})
	if err != nil {
		return err
	}
	_, err = conn.Write(encoded)
	return err
}

// serveGuest 接受指定代理的访客连接。
//
// Accept 错误同样按临时/致命分类处理，致命错误停止监听并上报而不是静默退出。
func (engine *Engine) serveGuest(name string, listener *transport.Listener) {
	defer engine.wg.Done()
	for {
		conn, action, err := listener.Accept(transport.PurposeWork, name)
		if err != nil {
			if action == transport.AcceptFatal {
				engine.reportAcceptFatal(listener, err)
				return
			}
			time.Sleep(transport.AcceptBackoff(action))
			continue
		}
		engine.wg.Add(1)
		go engine.handleGuest(name, conn)
	}
}

// registerClient 登记已登录客户端的控制连接，供工作连接配对使用。
func (engine *Engine) registerClient(clientID string, control *transport.Conn) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.clients[clientID] = &clientSession{clientID: clientID, control: control}
}

// unregisterClient 移除客户端会话；控制连接断开时调用。
func (engine *Engine) unregisterClient(clientID string) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	delete(engine.clients, clientID)
}

// isStopped 返回引擎是否已进入停止状态。
func (engine *Engine) isStopped() bool {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.state == stateStopped
}

// handleGuest 把访客连接交给配对中心：有待命工作连接立即桥接，否则暂存。
func (engine *Engine) handleGuest(name string, guest *transport.Conn) {
	defer engine.wg.Done()
	engine.track(guest)
	// parkGuest 接管访客的所有权：配对成功后桥接负责关闭，未配对时暂存；
	// 暂存失败（引擎停止）时此处关闭。
	paired := engine.workConns.parkGuest(name, guest, engine.track, engine.wg.Add)
	if paired {
		engine.untrack(guest)
		return
	}
	if engine.isStopped() {
		engine.untrack(guest)
		_ = guest.Close()
		return
	}
	// 暂存成功：连接留在配对中心，Shutdown 与 close 负责最终释放。
	// untrack 不在此处调用，避免重复记账：配对中心的关闭路径统一处理。
}
