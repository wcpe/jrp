package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/internal/proxy"
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

	mu           sync.RWMutex
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
	udpEntries   map[string]*proxy.UDPProxy
	registry     *proxy.RegistryView
	httpRoutes   map[int]*proxy.HTTPRouter
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
		udpEntries:   make(map[string]*proxy.UDPProxy),
		registry:     &proxy.RegistryView{},
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

	engine.publishRegistry()
	guests, addresses, udpEntries, err := engine.openGuestEntries()
	if err != nil {
		engine.releaseStartSlot()
		return fmt.Errorf("打开代理入口失败：%w", err)
	}

	engine.commitRunning(guests, addresses, udpEntries)

	engine.wg.Add(1)
	go engine.serveControl(listener)
	for name, guestListener := range guests {
		engine.wg.Add(1)
		// 共享 HTTP 入口按端口路由，独占入口（TCP、HTTPS）直接按代理名配对：
		// 两类入口的接入循环不同，必须按登记名分流。
		if port, ok := httpEntryPort(name); ok {
			go engine.serveHTTPGuest(port, guestListener)
			continue
		}
		go engine.serveGuest(name, guestListener)
	}
	for name, entry := range udpEntries {
		engine.wg.Add(1)
		go engine.serveUDPEntry(name, entry)
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
	udpEntries map[string]*proxy.UDPProxy,
) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.state = stateRunning
	engine.guestLns = guests
	engine.guestAddr = addresses
	engine.udpEntries = udpEntries
}

// openGuestEntries 为 TCP、HTTP、HTTPS 绑定打开流入口，为 UDP 绑定打开数据报入口。
//
// 监听地址来自代理绑定的 RemotePort 与 Engine 自身的监听端点族：入口端口是
// 配置的一部分，不得在代码里写死回环地址。
// HTTP 入口可被多个代理共享，因此按端口去重；TCP 与 HTTPS 入口独占端口，
// 按代理名登记。
// 任一失败时释放已打开的全部入口并返回错误，不遗留半注册资源（§3.2）。
func (engine *Engine) openGuestEntries() (
	guests map[string]*transport.Listener,
	addresses map[string]net.Addr,
	udpEntries map[string]*proxy.UDPProxy,
	err error,
) {
	guests = make(map[string]*transport.Listener)
	addresses = make(map[string]net.Addr)
	udpEntries = make(map[string]*proxy.UDPProxy)
	// 失败即整体释放：任一步出错都不得留下半注册监听器或路由。
	defer func() {
		if err != nil {
			releaseGuestListeners(guests)
			releaseUDPEntries(udpEntries)
		}
	}()

	for _, binding := range engine.config.Bindings() {
		if guests[binding.Name], err = engine.listenGuest(binding.RemotePort); err != nil {
			return nil, nil, nil, err
		}
		addresses[binding.Name] = guests[binding.Name].Addr()
	}
	for _, binding := range engine.config.HTTPSBindings() {
		if guests[binding.Name], err = engine.listenGuest(binding.RemotePort); err != nil {
			return nil, nil, nil, err
		}
		addresses[binding.Name] = guests[binding.Name].Addr()
	}
	openedPorts := make(map[int]bool)
	for _, binding := range engine.config.HTTPBindings() {
		if openedPorts[binding.RemotePort] {
			continue
		}
		openedPorts[binding.RemotePort] = true
		name := httpEntryName(binding.RemotePort)
		if guests[name], err = engine.listenGuest(binding.RemotePort); err != nil {
			return nil, nil, nil, err
		}
		addresses[name] = guests[name].Addr()
	}
	// 每个 HTTP 代理都能按自身名查到入口地址：共享的是监听器，代理名到地址的
	// 映射必须完整，否则宿主与测试无法按代理名寻址。
	for _, binding := range engine.config.HTTPBindings() {
		addresses[binding.Name] = guests[httpEntryName(binding.RemotePort)].Addr()
	}
	for _, binding := range engine.config.UDPBindings() {
		if udpEntries[binding.Name], err = engine.openUDPEntry(binding.Name, binding.RemotePort); err != nil {
			return nil, nil, nil, err
		}
		addresses[binding.Name] = udpEntries[binding.Name].Addr()
	}
	return guests, addresses, udpEntries, nil
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

// httpEntryPrefix 是共享 HTTP 入口登记名的前缀。
const httpEntryPrefix = "http:"

// httpEntryName 返回共享 HTTP 入口在入口表中的登记名。
//
// HTTP 入口按端口共享，因此登记名由端口派生而不是代理名；代理选择发生在
// 请求解析之后的路由环节。
func httpEntryName(port int) string {
	return httpEntryPrefix + strconv.Itoa(port)
}

// releaseUDPEntries 释放一批已创建的 UDP 入口。
func releaseUDPEntries(entries map[string]*proxy.UDPProxy) {
	for _, entry := range entries {
		_ = entry.Close()
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
	udpEntries := make([]*proxy.UDPProxy, 0, len(engine.udpEntries))
	for _, entry := range engine.udpEntries {
		udpEntries = append(udpEntries, entry)
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
	// UDP 入口同样停止接收；已有会话按其空闲上限自然回收，不强制切断。
	for _, entry := range udpEntries {
		_ = entry.Close()
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

// serveWorkDeclaration 处理一条工作连接的归属声明、校验目标地址并配对访客。
//
// 目标地址越权在配对之前判定：未通过即关闭工作连接，绝不把越权目标接入数据
// 面（规格 §3.3）。校验通过后才按有等待访客立即配对、否则暂存的既有语义处理。
func (engine *Engine) serveWorkDeclaration(raw *transport.Conn, payload []byte) {
	declaration, err := parseWorkDeclaration(payload)
	if err != nil {
		_ = raw.Close()
		return
	}
	if !engine.registry.TargetAllowed(declaration.proxy, declaration.target) {
		engine.log().Warn("工作连接的目标地址不在允许集合内，已拒绝",
			"proxy", declaration.proxy, "target", declaration.target.String())
		_ = raw.Close()
		return
	}
	if !engine.workConns.park(declaration.proxy, raw, engine.track, engine.wg.Add) {
		_ = raw.Close()
	}
}

// workDeclaration 是一条工作连接声明的解出结果。
type workDeclaration struct {
	proxy  string
	target netip.AddrPort
}

// parseWorkDeclaration 解析工作连接声明载荷，返回代理归属与本地目标地址。
//
// 目标地址缺失或不可解析时返回错误：不可解析的输入不得被当作放行依据。
func parseWorkDeclaration(payload []byte) (workDeclaration, error) {
	var request workConnRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return workDeclaration{}, err
	}
	if request.Proxy == "" {
		return workDeclaration{}, errors.New("工作连接未声明代理归属")
	}
	target, err := netip.ParseAddrPort(request.Target)
	if err != nil {
		return workDeclaration{}, errors.New("工作连接声明的目标地址不可解析")
	}
	return workDeclaration{proxy: request.Proxy, target: target}, nil
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

// serveHTTPGuest 接受共享 HTTP 入口上的连接并按主机与路径路由。
func (engine *Engine) serveHTTPGuest(port int, listener *transport.Listener) {
	defer engine.wg.Done()
	for {
		conn, action, err := listener.Accept(transport.PurposeWork, httpEntryName(port))
		if err != nil {
			if action == transport.AcceptFatal {
				engine.reportAcceptFatal(listener, err)
				return
			}
			time.Sleep(transport.AcceptBackoff(action))
			continue
		}
		engine.wg.Add(1)
		go engine.handleHTTPGuest(port, conn)
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

// publishRegistry 从配置快照构造代理注册表并原子发布。
//
// 注册表只承载「目标地址是否允许」与「入口归属」两类运行期判定依据；字段、
// 权限、冲突与 P1 范围四级校验已在配置层完成，此处不复检（FR-06a §3.2）。
func (engine *Engine) publishRegistry() {
	registry := make(proxy.Registry)
	for _, binding := range engine.config.AllBindings() {
		registry[binding.ProxyName()] = &proxy.Binding{
			Name:    binding.ProxyName(),
			Targets: binding.ProxyTargets(),
		}
	}
	engine.registry.Publish(registry)
	engine.publishHTTPRoutes()
}

// publishHTTPRoutes 从 HTTP 绑定构造共享端口的路由表并发布。
//
// 路由表按「端口 → 表」组织：多个 HTTP 代理共享同一入口端口，请求解析出主机
// 与路径后按最长前缀命中目标代理（规格 §3.5）。构造失败即保留空表并记日志，
// 绝不带着半张表进入运行——配置层已完成冲突校验，此处失败属防御性分支。
func (engine *Engine) publishHTTPRoutes() {
	grouped := make(map[int][]proxy.HTTPRoute)
	for _, binding := range engine.config.HTTPBindings() {
		for _, host := range binding.Hosts {
			grouped[binding.RemotePort] = append(grouped[binding.RemotePort], proxy.HTTPRoute{
				Proxy: binding.Name,
				Host:  host,
				Path:  binding.Path,
			})
		}
	}
	routes := make(map[int]*proxy.HTTPRouter, len(grouped))
	for port, items := range grouped {
		router, err := proxy.NewHTTPRouter(items)
		if err != nil {
			engine.log().Error("HTTP 路由表构造失败，该入口端口将不匹配任何请求",
				"port", port, "error", err)
			continue
		}
		routes[port] = router
	}
	engine.mu.Lock()
	engine.httpRoutes = routes
	engine.mu.Unlock()
}

// openUDPEntry 按监听端点的地址族打开一个 UDP 入口。
func (engine *Engine) openUDPEntry(name string, remotePort int) (*proxy.UDPProxy, error) {
	host := engine.config.Listen().Address.Addr()
	if !host.IsValid() {
		host = netip.IPv4Unspecified()
	}
	port, err := transport.ListenUDP(netip.AddrPortFrom(host, uint16(remotePort)))
	if err != nil {
		return nil, err
	}
	return proxy.NewUDPProxy(proxy.UDPProxyConfig{
		Name:        name,
		Port:        port,
		Idle:        engine.config.UDPSessionIdle(),
		MaxSessions: engine.config.UDPSessionLimit(),
		MaxDatagram: engine.config.UDPDatagramSize(),
		Work:        engine.udpWorkFactory(name),
	}), nil
}

// udpWorkFactory 返回为 UDP 会话提供工作连接的工厂。
//
// 会话需要一条新工作连接时向配对待命池索取；索不到即拒绝并计数，由入口累积
// 可观测事件（规格 §3.5：不得无限等待）。
func (engine *Engine) udpWorkFactory(name string) func() (net.Conn, bool) {
	return func() (net.Conn, bool) {
		work := engine.workConns.takeStaged(name)
		if work == nil {
			return nil, false
		}
		return work, true
	}
}

// serveUDPEntry 服务一个 UDP 入口，直到入口关闭。
func (engine *Engine) serveUDPEntry(name string, entry *proxy.UDPProxy) {
	defer engine.wg.Done()
	entry.Serve(context.Background())
	engine.log().Info("UDP 代理入口已停止", "proxy", name)
}

// handleHTTPGuest 处理共享 HTTP 入口上的一条连接：解析请求首部并路由到目标代理。
//
// 请求行与首部必须先读出来才能路由，但这些字节属于请求本身，因此路由命中后
// 要原样回放到桥接流上：漏掉回放会让目标服务收到一个被截断的请求。
// 未匹配时返回明确的 404 且响应体不回显内部路由表；此后关闭连接，不留悬挂。
func (engine *Engine) handleHTTPGuest(port int, guest *transport.Conn) {
	defer engine.wg.Done()
	engine.track(guest)
	defer engine.untrack(guest)

	host, path, pending, err := readRequestTarget(guest)
	if err != nil {
		_ = guest.Close()
		return
	}
	proxyName, ok := engine.selectHTTPProxy(port, host, path)
	if !ok {
		_ = writeUnmatchedResponse(guest)
		_ = guest.Close()
		return
	}
	engine.bridgeHTTPGuest(proxyName, guest, pending)
}

// bridgeHTTPGuest 把已解析出代理的访客连接交给配对中心，并回放请求首部。
//
// 回放先于桥接：配对中心一旦把连接交给桥接，本函数就不再持有它，因此必须
// 在移交前把已读出的字节写回同一条连接的方向。
func (engine *Engine) bridgeHTTPGuest(proxyName string, guest *transport.Conn, pending []byte) {
	if len(pending) > 0 {
		if _, err := guest.Write(pending); err != nil {
			_ = guest.Close()
			return
		}
	}
	if !engine.workConns.parkGuest(proxyName, guest, engine.track, engine.wg.Add) {
		if engine.isStopped() {
			engine.untrack(guest)
			_ = guest.Close()
		}
	}
}

// selectHTTPProxy 在指定入口端口的路由表上选择目标代理。
func (engine *Engine) selectHTTPProxy(port int, host, path string) (string, bool) {
	engine.mu.RLock()
	router := engine.httpRoutes[port]
	engine.mu.RUnlock()
	if router == nil {
		return "", false
	}
	return router.Select(host, path)
}

// delimCRLF 是 HTTP 首部的行分隔符。
const delimCRLF = '\n'

// headerEndCRLF 与 headerEndLF 是首部块结束的两种形态。
const (
	headerEndCRLF = "\r\n"
	headerEndLF   = "\n"
)

// unmatchedResponse 是路由未命中时返回的响应。
//
// 响应体只说明「无匹配路由」，不列出已配置的主机或路径：内部路由表不得经
// 响应回显（规格 §3.5）。
const unmatchedResponse = "HTTP/1.1 404 Not Found\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Content-Length: 33\r\n" +
	"Connection: close\r\n\r\n" +
	"未找到匹配该主机与路径的路由"

// requestLineLimit 是单个 HTTP 请求行与首部的读取上限。
//
// 上限防止未受信输入把首部缓冲撑爆；超过上限即按非法请求处理，不继续解析。
const requestLineLimit = 8192

// engineRequestReadTimeout 是读取 HTTP 请求行与首部的时间上限。
//
// 入口不得被慢速未受信请求无限占用：超时即按非法请求关闭，不继续解析。
const engineRequestReadTimeout = 10 * time.Second

// readRequestTarget 读取一个 HTTP 请求的主机名、目标路径与待回放字节。
//
// 只读请求行与 Host 首部，不解析正文。请求行与首部属于请求本身，必须原样
// 随后续字节流转发给目标服务，因此本函数把它们原样返回供回放。
// 读取带截止时间，绝不无限等待。
func readRequestTarget(guest *transport.Conn) (host, path string, pending []byte, err error) {
	if deadlineErr := guest.SetReadDeadline(time.Now().Add(engineRequestReadTimeout)); deadlineErr != nil {
		return "", "", nil, deadlineErr
	}
	defer func() { _ = guest.SetReadDeadline(time.Time{}) }()

	reader := bufio.NewReaderSize(guest, requestLineLimit)
	line, err := reader.ReadString(delimCRLF)
	if err != nil {
		return "", "", nil, err
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", "", nil, errors.New("请求行缺少方法或目标")
	}
	path = requestPath(fields[1])
	pending = append(pending, line...)
	// Host 首部：逐行读到空行，只取 Host，其余首部随缓冲一并回放。
	for {
		header, err := reader.ReadString(delimCRLF)
		if err != nil {
			return "", "", nil, err
		}
		pending = append(pending, header...)
		if header == headerEndCRLF || header == headerEndLF {
			break
		}
		if name, value, ok := splitHeader(header); ok && strings.EqualFold(name, "Host") {
			host = value
		}
	}
	if host == "" {
		return "", "", nil, errors.New("请求缺少 Host 首部")
	}
	return host, path, pending, nil
}

// requestPath 从请求目标中取出路径部分，去掉查询串与片段标识。
func requestPath(target string) string {
	if index := strings.IndexAny(target, "?#"); index >= 0 {
		target = target[:index]
	}
	if target == "" {
		return "/"
	}
	return target
}

// splitHeader 拆分一行首部为名称与取值。
func splitHeader(line string) (name, value string, ok bool) {
	separator := strings.Index(line, ":")
	if separator < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:separator]), strings.TrimSpace(line[separator+1:]), true
}

// writeUnmatchedResponse 写出路由未命中的响应。
func writeUnmatchedResponse(guest *transport.Conn) error {
	_ = guest.SetWriteDeadline(time.Now().Add(engineRequestReadTimeout))
	defer func() { _ = guest.SetWriteDeadline(time.Time{}) }()
	_, err := guest.Write([]byte(unmatchedResponse))
	return err
}

// httpEntryPort 还原共享 HTTP 入口登记名中的端口；非共享入口返回假。
func httpEntryPort(name string) (int, bool) {
	if !strings.HasPrefix(name, httpEntryPrefix) {
		return 0, false
	}
	port, err := strconv.Atoi(strings.TrimPrefix(name, httpEntryPrefix))
	if err != nil {
		return 0, false
	}
	return port, true
}
