package server

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/compat"
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
// generation 是一个 revision 对应的运行资源与连接集合。
//
// 连接与等待组按代隔离，是「切换期间已有流不中断」的实现基础：drain 只等待旧代
// 的桥接结束，新代连接不受牵连。共用一个等待组会让 drain 连带等待新代，切换就
// 退化成"停下来等所有人"。
//
// conns 与 controlConns 沿用 Engine 的锁保护而不另设互斥量：本包存在依赖锁序的
// 约束（见 lockorder_test.go 记录的 ABBA 死锁），新增锁会扩大锁图；wg 自身无锁，
// 放在这里只是为了让等待范围跟着代走。
type generation struct {
	// engine 回指所属引擎：conns 要复用引擎的锁，而本包存在
	// 依赖锁序的约束（见 lockorder_test.go 记录的 ABBA 死锁），代自身不新增互斥量。
	engine   *Engine
	revision uint64
	// config 是本代的不可变配置。
	//
	// 凭据校验、监听地址族与 UDP 参数都从它读取，而不是从 engine.config：
	// 代一经发布就不再修改，指针切换即是同步点；若共享一个可写字段，
	// 握手 goroutine 会与切换路径竞争读。
	config core.ServerConfig
	// guestLns 是本代的访客入口监听器。
	//
	// 控制监听器刻意**不在**代里：它跨代复用（同一入口同时接受旧代与新一代的
	// 控制连接），Accept 循环归 Engine，等待组也挂在 Engine 上。若把它放进代，
	// drain 旧代会停掉控制入口，新一代的客户端就再也连不上来。
	guestLns   map[string]*transport.Listener
	guestAddr  map[string]net.Addr
	udpEntries map[string]*proxy.UDPProxy

	// stopping 标记本代是否已停止接收：drain 旧代时置位的是**旧代的**标记，
	// 新代不受影响。早先它是引擎级字段，Apply 切换后新代的读循环会把读写错误
	// 误判为"预期停止"而不上报。
	stopping bool

	// reused 是本代从上一代接管的入口名（标识与绑定地址都未变）。
	//
	// prepare 失败时不得释放它们：所有权要到 publish 成功才转移，此刻它们仍归
	// 上一代，关闭会把正在服务的入口一并关掉。
	reused map[string]bool
	// donated 是本代已交给后继代的入口名。
	//
	// 交接后本代不得再关闭这些套接字：它们正在为新代接收流量。清空条件是"后继代
	// 又换了别的端口"，那时后继代自己会释放。
	donated map[string]bool

	// runtimeProxies 是本代内经消息族运行时注册的代理（FR-03）。
	//
	// 与快照预建入口分账管理：注册与清理都发生在会话生命周期内，不写 desired。
	// 读写都取 Engine 锁；代清理（cleanupRuntimeProxies）与 close-proxy 共用。
	runtimeProxies map[string]*runtimeProxy

	// acceptWG 跟踪本代的入口接收循环（流入口与 UDP 入口各一个）。
	//
	// 与 wg 分开：交接只等"停止接收"这一段，不能连在途桥接一起等，否则 publish
	// 之后必须等旧代的用户流跑完才能启动新代的接收循环，切换就退化成停下来等。
	acceptWG sync.WaitGroup

	// wg 跟踪本代的访客入口循环、数据桥接与 UDP 循环。
	wg    sync.WaitGroup
	conns map[*transport.Conn]struct{}
	// 控制连接刻意**不在**代里：它们是登录与心跳通道，跨代存活。drain 旧代时
	// 关闭它们会断掉客户端的控制会话，maintain 循环停摆后新工作连接补不上，
	// 新访客永远等不到配对（端到端实测复现）。控制连接的生命周期只到 Shutdown。
}

// newGeneration 构造一代的空资源集合；监听器与入口由调用方在 prepare 阶段填入。
func newGeneration(engine *Engine, revision uint64, config core.ServerConfig) *generation {
	return &generation{
		engine:   engine,
		config:   config,
		revision: revision,
		conns:    make(map[*transport.Conn]struct{}),
		reused:   make(map[string]bool),

		runtimeProxies: make(map[string]*runtimeProxy),
		donated:        make(map[string]bool),
	}
}

type Engine struct {
	config       core.ServerConfig
	logger       *slog.Logger
	dialer       transport.Dialer
	drainTimeout time.Duration

	mu    sync.RWMutex
	state engineState
	// shuttingDown 表示 Engine 进入终停（Shutdown）：与代的 stopping 不同，
	// 它一旦置位就不再有新代，failAbnormal 等据此把读写错误判定为预期。
	shuttingDown bool
	// active 是当前生效的代；首次 publish 之前为 nil。
	active *generation
	// lastGood 是最近一次成功 publish 的 revision。
	lastGood uint64
	// applying 表示有一次配置应用正在进行，用于单飞约束。
	applying bool
	// healthCheckHook 是包内测试注入的失败点；生产路径恒为 nil。
	// prepare 成功即端口可用，真实流程中 health-check 不会失败，该分支
	// 若无钩子就无法被测试触达。
	healthCheckHook func(*generation) error
	done            chan struct{}
	finalErr        error
	stopOnce        sync.Once

	registry   *proxy.RegistryView
	httpRoutes map[int]*proxy.HTTPRouter

	// controlConns 是全部控制连接的登记：跨代存活，只在 Shutdown 清理。
	controlConns map[*transport.Conn]struct{}

	// workConns 是引擎唯一的配对中心。工作连接是客户端拨入的长连接，与某代
	// 监听器无关：新代访客要与已拨入的工作连接配对，broker 若按代切分就会是空的。
	// idle 上限取初始配置，Apply 不重建它（重建会丢掉已拨入的待命连接）。
	//
	// 原子指针而非锁保护字段：读取方（控制连接、访客洪水、宿主观测轮询）遍布
	// 多个已持锁或高频路径，取锁会引入 engine.mu 与 broker.mu 的交错等待
	// （-race 与 TestPendingGuestsAreBounded 均已实锤）；指针交换本身是原子的，
	// 旧 broker 由在途调用安全地用到结束。
	workConns atomic.Pointer[workBroker]

	// controlListener 是控制入口，跨代复用：同一监听器同时服务旧代与新一代
	// 的客户端登录。Accept 循环与它的等待组归 Engine 而不是某一代，否则 drain
	// 旧代会停掉控制入口，新一代客户端就再也连不上来。
	controlListener *transport.Listener
	controlWG       sync.WaitGroup

	heartbeat time.Duration

	// events 是事件中枢：承载订阅登记与事件发布（FR-27）。
	events *core.EventHub
	// controlClients 记录控制连接的客户端标识：登录成功时登记，供状态快照聚合。
	controlClients map[*transport.Conn]string
	// officialSessions 是官方形态控制会话的登记（clientID → 会话写出通道与运行 ID）。
	//
	// 官方客户端的工作连接由服务端指派：服务端在有待处理访客时向控制连接发
	// req-work-conn，客户端随后新建连接并只声明运行 ID 与鉴权材料。要完成这次
	// 指派，服务端必须持有控制连接的写出通道与运行 ID 索引。
	officialSessions map[string]officialControlSession
	// sessionRunIDs 是 runID → clientID 的反向索引，供工作连接声明定位会话。
	sessionRunIDs map[string]string

	// clientControl 是 clientID → 活跃控制连接的反向索引（FR-03 §3.4）。
	//
	// 同一客户端同时只允许一个活跃控制会话：新会话登录时接管索引并关闭旧
	// 会话；工作连接的"活跃会话归属"校验也读它。
	clientControl map[string]*transport.Conn
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
		engine.controlListener = transport.TakeOverListener(listener)
	}
}

// WithLogger 注入宿主的日志器；未注入时丢弃日志。
func WithLogger(logger *slog.Logger) Option {
	return func(engine *Engine) {
		engine.logger = logger
	}
}

// New 构造服务端 Engine，只做纯内存装配，不绑定端口、不启动 goroutine。
//
// config 是首次应用（Start）使用的配置；后续通过 Apply 提交新 revision 替换它。
func New(config core.ServerConfig, options ...Option) *Engine {
	engine := &Engine{
		config:           config,
		done:             make(chan struct{}),
		registry:         &proxy.RegistryView{},
		controlConns:     make(map[*transport.Conn]struct{}),
		clientControl:    make(map[string]*transport.Conn),
		heartbeat:        config.Heartbeat(),
		dialer:           transport.Dialer{Timeout: config.Timeout()},
		drainTimeout:     config.DrainTimeout(),
		events:           core.NewEventHub(),
		controlClients:   make(map[*transport.Conn]string),
		officialSessions: make(map[string]officialControlSession),
		sessionRunIDs:    make(map[string]string),
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

	// 首次应用建立第 0 代。revision 取 SnapshotRevisionUnknown：版本号由宿主
	// 分配并单调递增，宿主从 1 开始即可，0 自然表示"尚未由宿主指定版本"。
	//
	// 代先于资源构造：UDP 工作连接工厂要登记到本代，若等资源建好再建代，
	// 工厂就无处归属。
	// broker 只建一次：跨代共享，不随换代重建。
	engine.workConns.Store(newWorkBroker(engine.config.IdleWorkConnLimit()))
	gen := newGeneration(engine, core.SnapshotRevisionUnknown, engine.config)

	gen.publishRegistry()
	if err := engine.openGuestEntries(gen, engine.config, nil); err != nil {
		engine.releaseStartSlot()
		return fmt.Errorf("打开代理入口失败：%w", err)
	}

	engine.commitRunning(gen)
	engine.startControl(listener)
	engine.startGeneration(gen)
	_ = ctx
	return nil
}

// startControl 启动控制入口的 Accept 循环。
//
// 循环归 Engine：它跨代复用，生命周期到 Shutdown 为止。新连接在 Accept 时
// 固定归属当时生效的代，此后不再跟随 active——凭据校验等读该代的配置。
func (engine *Engine) startControl(listener *transport.Listener) {
	engine.controlWG.Add(1)
	go func() {
		defer engine.controlWG.Done()
		engine.serveControl(listener)
	}()
}

// startGeneration 启动一代的接入循环。
//
// 接入循环与 publish 分开：循环要等 active 已指向该代之后才能开始接受连接，
// 否则新连接可能在 active 尚未切换时就被处理，读到的是上一代的注册表。
// 复用而来的入口必须等上一代的接收循环退出后才能开新循环，因此调用方要先把
// 上一代停到"不再接收"（见 stopAccepting），本函数只负责开新循环。
func (engine *Engine) startGeneration(gen *generation) {
	for name, guestListener := range gen.guestLns {
		gen.acceptWG.Add(1)
		// 共享 HTTP 入口按端口路由，独占入口（TCP、HTTPS）直接按代理名配对：
		// 两类入口的接入循环不同，必须按登记名分流。
		if port, ok := httpEntryPort(name); ok {
			go engine.serveHTTPGuest(gen, port, guestListener)
			continue
		}
		go engine.serveGuest(gen, name, guestListener)
	}
	for name, entry := range gen.udpEntries {
		gen.acceptWG.Add(1)
		go engine.serveUDPEntry(gen, name, entry)
	}
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
	return engine.controlListener, nil
}

// releaseStartSlot 把认领失败的启动位回滚到 idle。
func (engine *Engine) releaseStartSlot() {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.state == stateStarting {
		engine.state = stateIdle
	}
}

// commitRunning 把认领成功的启动位推进到 running，并把 active 指向新一代。
func (engine *Engine) commitRunning(gen *generation) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.state = stateRunning
	engine.active = gen
	engine.lastGood = gen.revision
}

// openGuestEntries 为 TCP、HTTP、HTTPS 绑定打开流入口，为 UDP 绑定打开数据报入口。
//
// 监听地址来自代理绑定的 RemotePort 与 Engine 自身的监听端点族：入口端口是
// 配置的一部分，不得在代码里写死回环地址。
// HTTP 入口可被多个代理共享，因此按端口去重；TCP 与 HTTPS 入口独占端口，
// 按代理名登记。
// 标识与绑定地址都未变的入口直接接管上一代的同一个监听套接字（见 takeStream）：
// 关闭再重建会让端口在切换窗口内消失，正是"无中断"要避免的。
// 任一失败时释放本步新建的全部入口并返回错误，不遗留半注册资源（§3.2）；接管的
// 入口不释放，因为所有权要到 publish 成功才转移。
//
// config 单独传入而不是读 engine.config：配置应用的 prepare 阶段要为**新**配置
// 建资源，此时 engine.config 仍是旧值。资源直接填入 gen，供 publish 时整体切换。
// previous 是当前生效代，供复用判定；首次应用传 nil。
func (engine *Engine) openGuestEntries(
	gen *generation, config core.ServerConfig, previous *generation,
) (err error) {
	guests := make(map[string]*transport.Listener)
	addresses := make(map[string]net.Addr)
	udpEntries := make(map[string]*proxy.UDPProxy)
	// reused 记录从上一代接管的入口名，失败路径据此跳过释放。
	reused := make(map[string]bool)
	// 失败即整体释放：任一步出错都不得留下半注册监听器或路由。
	defer func() {
		if err == nil {
			return
		}
		for name, listener := range guests {
			if !reused[name] {
				_ = listener.Release()
			}
		}
		for name, entry := range udpEntries {
			if !reused[name] {
				_ = entry.Close()
			}
		}
	}()

	// takeStream 取一个访客流入口：标识与绑定地址都未变时接管上一代的监听器。
	takeStream := func(name string, port int) (*transport.Listener, error) {
		if listener := reusableStreamEntry(previous, name, entryBindAddr(config, port)); listener != nil {
			reused[name] = true
			return listener, nil
		}
		return engine.listenGuest(config, port)
	}

	for _, binding := range config.Bindings() {
		opened, listenErr := takeStream(binding.Name, binding.RemotePort)
		if listenErr != nil {
			return listenErr
		}
		guests[binding.Name] = opened
		addresses[binding.Name] = opened.Addr()
	}
	for _, binding := range config.HTTPSBindings() {
		opened, listenErr := takeStream(binding.Name, binding.RemotePort)
		if listenErr != nil {
			return listenErr
		}
		guests[binding.Name] = opened
		addresses[binding.Name] = opened.Addr()
	}
	openedPorts := make(map[int]bool)
	for _, binding := range config.HTTPBindings() {
		if openedPorts[binding.RemotePort] {
			continue
		}
		openedPorts[binding.RemotePort] = true
		name := httpEntryName(binding.RemotePort)
		opened, listenErr := takeStream(name, binding.RemotePort)
		if listenErr != nil {
			return listenErr
		}
		guests[name] = opened
		addresses[name] = opened.Addr()
	}
	// 每个 HTTP 代理都能按自身名查到入口地址：共享的是监听器，代理名到地址的
	// 映射必须完整，否则宿主与测试无法按代理名寻址。
	for _, binding := range config.HTTPBindings() {
		addresses[binding.Name] = guests[httpEntryName(binding.RemotePort)].Addr()
	}
	for _, binding := range config.UDPBindings() {
		entry, wasReused, openErr := engine.takeUDPEntry(previous, config, binding)
		if openErr != nil {
			return openErr
		}
		if wasReused {
			reused[binding.Name] = true
		}
		udpEntries[binding.Name] = entry
		addresses[binding.Name] = entry.Addr()
	}

	gen.guestLns = guests
	gen.guestAddr = addresses
	gen.udpEntries = udpEntries
	gen.reused = reused
	return nil
}

// entryBindAddr 返回入口按配置应绑定的地址。
//
// 与 listenGuest／openUDPEntry 使用同一套推导，复用判定才能与新建判定对齐：
// 两处若各算一遍，配置里未指定监听地址族时就会得出不同的地址而不复用。
func entryBindAddr(config core.ServerConfig, port int) netip.AddrPort {
	host := config.Listen().Address.Addr()
	if !host.IsValid() {
		host = netip.IPv4Unspecified()
	}
	return netip.AddrPortFrom(host, uint16(port))
}

// reusableStreamEntry 返回上一代中可被本代接管的流入口；不可复用时返回 nil。
//
// 复用条件是「入口标识与绑定地址都未变」：标识由配置层保证唯一，地址相等说明
// 仍是同一个监听套接字。此外要求该监听器支持免关闭交接——否则只能"关闭再重建"，
// 那会让端口在切换窗口内消失，宁可新建失败也不要静默降级。
//
// 地址比较在**通配语义**下进行，不做逐字节字符串比对：net.Listen("tcp", "0.0.0.0:p")
// 的 Addr() 报告 "[::]:p"（Go 监听通配地址时内核按双栈处理），逐字节比较会把
// 这一对等表达误判为"地址变了"而放弃复用，让 wildcard 配置在换代时全部 EADDRINUSE。
// 判定规则：新旧地址的端口相同，且各自的主机部分至少一个是通配（unspecified），
// 或主机部分逐字节相等。
func reusableStreamEntry(previous *generation, name string, address netip.AddrPort) *transport.Listener {
	if previous == nil {
		return nil
	}
	listener := previous.guestLns[name]
	if listener == nil || listener.Addr() == nil {
		return nil
	}
	old, err := netip.ParseAddrPort(listener.Addr().String())
	if err != nil || old.Port() != address.Port() || !addrOverlap(old.Addr(), address.Addr()) {
		return nil
	}
	return listener
}

// addrOverlap 判断两个监听主机地址在通配语义下是否指向同一组接口。
func addrOverlap(old, next netip.Addr) bool {
	if !old.IsValid() || !next.IsValid() {
		return false
	}
	if old.IsUnspecified() || next.IsUnspecified() {
		return true
	}
	return old == next
}

// takeUDPEntry 取一个 UDP 入口：标识、绑定地址与会话参数都未变时接管上一代的入口。
//
// 三项必须全部相同才能复用：UDPProxy 的会话参数在构造期固定，复用对象就必须沿用
// 它们；而绑定地址相同意味着不可能重新绑定（旧入口仍占着端口）。因此"参数变了但
// 端口没变"没有可行路径，返回可判定的错误，而不是让它表现为端口占用这种与原因
// 无关的失败。
func (engine *Engine) takeUDPEntry(
	previous *generation, config core.ServerConfig, binding core.UDPProxyBinding,
) (*proxy.UDPProxy, bool, error) {
	address := entryBindAddr(config, binding.RemotePort)
	if previous != nil {
		if entry := previous.udpEntries[binding.Name]; entry != nil && entry.Addr() != nil {
			if old, err := netip.ParseAddrPort(entry.Addr().String()); err == nil &&
				old.Port() == address.Port() && addrOverlap(old.Addr(), address.Addr()) {
				if !udpParamsMatch(previous.config, config) {
					return nil, false, fmt.Errorf(
						"UDP 代理 %s 的会话参数已变更但端口 %d 未变：当前版本不支持复用带参数的入口，请一并更换该代理的端口",
						binding.Name, binding.RemotePort)
				}
				return entry, true, nil
			}
		}
	}
	entry, err := engine.openUDPEntry(config, binding.Name, binding.RemotePort)
	if err != nil {
		return nil, false, err
	}
	return entry, false, nil
}

// udpParamsMatch 判断两份快照的 UDP 会话参数是否一致。
//
// UDP 参数是全局的（不按代理区分），因此任一参数变化都会影响全部 UDP 入口。
func udpParamsMatch(previous, next core.ServerConfig) bool {
	return previous.UDPSessionIdle() == next.UDPSessionIdle() &&
		previous.UDPSessionLimit() == next.UDPSessionLimit() &&
		previous.UDPDatagramSize() == next.UDPDatagramSize()
}

// listenGuest 按监听端点的地址族打开一个访客监听器。
func (engine *Engine) listenGuest(config core.ServerConfig, port int) (*transport.Listener, error) {
	listener, err := net.Listen("tcp", entryBindAddr(config, port).String())
	if err != nil {
		return nil, err
	}
	return transport.TakeOverListener(listener), nil
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

// Shutdown 幂等关闭：先停止接收新连接，再按排水上限等待活动连接自然结束，
// 超限才强制关闭，最后关闭 Done。
func (engine *Engine) Shutdown(ctx context.Context) error {
	engine.mu.Lock()
	active := engine.active
	limit := engine.drainTimeout
	engine.mu.Unlock()

	// 先停控制入口：它跨代复用，不属于任何一代；在 drain 之前关闭，
	// 保证 drain 期间不再有新控制连接进入。
	if engine.controlListener != nil {
		_ = engine.controlListener.Release()
	}
	if active != nil {
		engine.stopOnce.Do(func() {
			active.stop(true)
			// 终停时一次清理暂存：它们不承载用户数据，不进 drain 等待。
			if broker := engine.workConns.Load(); broker != nil {
				broker.closeStaged()
			}
			engine.mu.Lock()
			for conn := range engine.controlConns {
				_ = conn.Close()
			}
			engine.mu.Unlock()
		})
		if err := active.waitDrained(ctx, limit); err != nil {
			return err
		}
	}
	// 等控制 Accept 循环退出：它在监听器 Release 后因 Accept 失败返回，
	// 终停标记已置，其 fatal 路径不会误报。
	engine.controlWG.Wait()
	engine.closeDone()
	return nil
}

// closeDone 标记完全停止并关闭 Done 通道；重复调用安全。
func (engine *Engine) closeDone() {
	engine.events.PublishStop(engine.finalErr)
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

// currentGeneration 返回当前生效代但不加锁。
//
// 专供已持有 engine.mu 的调用方（包括包内测试）使用：Go 的互斥量不可重入，
// 在临界区内调用 activeGeneration 会自死锁。
func (engine *Engine) currentGeneration() *generation {
	return engine.active
}

// activeGeneration 返回当前生效的代；首次 publish 之前为 nil。
//
// 生产路径读 engine.active 时也持同一把锁；这里供包内测试观察代内状态，
// 避免测试直接触碰字段而绕开锁。
func (engine *Engine) activeGeneration() *generation {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.active
}

// GuestAddr 返回指定代理的访客入口地址，用于测试与宿主拨测。
//
// 读当前生效代：切换之后宿主需要的是新一代的入口地址。
func (engine *Engine) GuestAddr(name string) net.Addr {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.active == nil {
		return nil
	}
	return engine.active.guestAddr[name]
}

// RejectedGuests 返回累计因暂存队列达上限而被拒绝的访客连接数。
//
// 该计数是宿主观察服务端承接能力的入口：拒绝意味着并发等待用户超过了上限，
// 是运维需要看到的事件，而不是可以静默吞掉的内部状态。
// RejectedGuests 返回累计因暂存上限被拒绝的访客数。
//
// broker 跨代共享，计数自然跨代累计——这正是运维口径里累计的含义。
func (engine *Engine) RejectedGuests() int64 {
	engine.mu.Lock()
	broker := engine.activeBroker()
	engine.mu.Unlock()
	if broker == nil {
		return 0
	}
	return broker.RejectedGuests()
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
func (gen *generation) track(conn *transport.Conn) {
	gen.engine.mu.Lock()
	defer gen.engine.mu.Unlock()
	gen.conns[conn] = struct{}{}
}

// trackControl 登记一条控制连接，供 Shutdown 统一释放。
//
// 它挂在 Engine 级而不是代里：控制连接是登录与心跳通道，drain 旧代时关闭它
// 会断掉客户端会话，maintain 循环停摆后新访客等不到工作连接。
func (engine *Engine) trackControl(conn *transport.Conn) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.controlConns[conn] = struct{}{}
}

// untrackControl 移除一条控制连接、其客户端标识与会话索引。
//
// 会话索引只在仍指向本连接时删除：被新会话替换的旧连接退出时不得抹掉
// 新会话的索引（FR-03 §3.4 的接管语义）。
func (engine *Engine) untrackControl(conn *transport.Conn) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if clientID, ok := engine.controlClients[conn]; ok {
		if engine.clientControl[clientID] == conn {
			delete(engine.clientControl, clientID)
		}
		// 官方会话登记与运行 ID 索引同属该会话：控制连接结束即失效，
		// 否则后续工作连接可能按已退出的会话被接纳。
		if session, ok := engine.officialSessions[clientID]; ok {
			delete(engine.sessionRunIDs, session.runID)
			delete(engine.officialSessions, clientID)
		}
	}
	delete(engine.controlConns, conn)
	delete(engine.controlClients, conn)
}

// bindControlClient 登记控制连接的客户端标识并返回被替换的旧会话连接。
//
// 同一客户端同时只允许一个活跃控制会话（FR-03 §3.4）：新会话提交登录时
// 接管索引，旧会话连接由调用方关闭。返回 nil 表示没有旧会话。
func (engine *Engine) bindControlClient(conn *transport.Conn, clientID string) *transport.Conn {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.controlClients[conn] = clientID
	old := engine.clientControl[clientID]
	engine.clientControl[clientID] = conn
	return old
}

// hasActiveControlSession 报告给定客户端是否有活跃控制会话。
//
// 工作连接必须属于活跃会话（FR-03 §3.5/§7.3「过期会话必须拒绝」）：
// 控制会话关闭后，其残留在途的工作连接声明不得再被接纳。
func (engine *Engine) hasActiveControlSession(clientID string) bool {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.clientControl[clientID] != nil
}

// untrack 移除一条活动连接。
func (gen *generation) untrack(conn *transport.Conn) {
	gen.engine.mu.Lock()
	defer gen.engine.mu.Unlock()
	delete(gen.conns, conn)
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

// stop 停止本代接收新连接并释放暂存，但不触碰数据桥接。
//
// final 只在 Engine 终停时为真：drain 旧代（Apply 路径）绝不能置引擎终停标记，
// 否则第一次换代后 failAbnormal 会永远走"预期停止"分支，真正的致命错误再也上报名。
// 排水语义分两类：控制连接不承载用户数据，立即释放；数据桥接承载活动流，
// 交由 waitDrained 按排水上限等待自然结束。
// 先停接收再动监听器：各读循环检查到停止标记后不再把唤醒用的超时记为异常。
func (gen *generation) stop(final bool) {
	if final {
		gen.engine.mu.Lock()
		gen.engine.shuttingDown = true
		gen.engine.mu.Unlock()
	}
	gen.stopAccepting()
}

// stopAccepting 停止本代的入口接收循环，并等待它们真正退出。
//
// 被后继代接管的入口（donated）**不关闭**套接字：只唤醒阻塞中的 Accept 并清掉
// 为唤醒设下的截止时间，套接字所有权随后归后继代。被移除或替换的入口在此关闭，
// 这正是「drain 期间旧资源不再接收新流量」的含义。
// 只等 acceptWG 而不是 wg：publish 之后要立刻启动新代的接收循环，不能连在途
// 用户流一起等，否则新代在旧代排空完成前都无法接受连接。
func (gen *generation) stopAccepting() {
	gen.engine.mu.Lock()
	gen.stopping = true
	donated := make(map[string]bool, len(gen.donated))
	for name := range gen.donated {
		donated[name] = true
	}
	// 入口表是可变的：会话结束时运行时代理的清理持同一把锁增删这些条目。
	// 必须先在锁内拷快照，否则下面的遍历会与并发的删除竞争。
	guestListeners := make(map[string]*transport.Listener, len(gen.guestLns))
	for name, guestListener := range gen.guestLns {
		guestListeners[name] = guestListener
	}
	udpEntries := make(map[string]*proxy.UDPProxy, len(gen.udpEntries))
	for name, entry := range gen.udpEntries {
		udpEntries[name] = entry
	}
	gen.engine.mu.Unlock()

	for name, guestListener := range guestListeners {
		if donated[name] {
			_ = guestListener.SetAcceptDeadline(time.Now())
			continue
		}
		_ = guestListener.Release()
	}
	for name, entry := range udpEntries {
		if donated[name] {
			// UDP 入口没有 Accept 循环，用 Suspend 停掉本代的接收而不回收会话。
			entry.Suspend()
			continue
		}
		_ = entry.Close()
	}

	gen.acceptWG.Wait()

	// 交接完成：清掉为唤醒 Accept 设下的截止时间，否则后继代的 Accept 会永远
	// 立即超时，入口看起来"在监听却收不到连接"。
	for name := range donated {
		if guestListener := guestListeners[name]; guestListener != nil {
			_ = guestListener.SetAcceptDeadline(time.Time{})
		}
	}
	// 控制连接不在这里关闭：它们跨代存活，由 Shutdown 统一清理。
	// 暂存的工作连接也不由 drain 释放：broker 跨代共享，关掉它们等于拆掉新代可用的
	// 配对资源；终停清理在 Shutdown 里做一次。
}

// donate 记录本代已把哪些入口交给后继代；此后本代不得再关闭它们。
func (gen *generation) donate(names map[string]bool) {
	gen.engine.mu.Lock()
	defer gen.engine.mu.Unlock()
	for name := range names {
		gen.donated[name] = true
	}
}

// isStopping 报告本代是否已被要求停止接收。
func (gen *generation) isStopping() bool {
	gen.engine.mu.Lock()
	defer gen.engine.mu.Unlock()
	return gen.stopping
}

// waitDrained 等待本代的活动桥接按排水上限结束；超限后强制关闭并继续等待。
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

// serveControl 接受控制连接并逐条处理登录与心跳。
//
// Accept 错误按传输层分类处理：临时错误退避后继续，致命错误停止循环并上报，
// 绝不静默退出（规格 §3.6）。
func (engine *Engine) serveControl(listener *transport.Listener) {
	for {
		conn, action, err := listener.Accept(transport.PurposeControl)
		if err != nil {
			if action == transport.AcceptFatal {
				engine.reportAcceptFatal(nil, listener, err)
				return
			}
			// 临时错误：退避后继续，避免把 Accept 循环变成忙循环。
			time.Sleep(transport.AcceptBackoff(action))
			continue
		}
		gen := engine.activeGeneration()
		if gen == nil {
			_ = conn.Close()
			continue
		}
		engine.controlWG.Add(1)
		go engine.handleControl(gen, conn)
	}
}

// reportAcceptFatal 上报致命的 Accept 错误：停止监听后记录为异常终止。
//
// 引擎已进入停止流程时为空操作：Shutdown 关闭监听器引发的 Accept 错误是预期
// 结果，不属于异常终止。
//
// gen 为 nil 时表示这是控制入口的循环（归 Engine，跨代复用），此时只看引擎级
// 终停标记；传代时看该代的 stopping——drain 旧代关入口引发的错误是预期，不该
// 被当成引擎异常终止。
func (engine *Engine) reportAcceptFatal(gen *generation, listener *transport.Listener, err error) {
	engine.mu.Lock()
	stopping := engine.shuttingDown || engine.state == stateStopped ||
		(gen != nil && gen.stopping)
	engine.mu.Unlock()
	if stopping {
		return
	}
	engine.log().Error("监听循环因致命错误停止", "listener", listener.Addr().String(), "error", err)
	engine.failAbnormal(gen, fmt.Errorf("监听 %s 的 Accept 失败：%w", listener.Addr().String(), err))
}

func (engine *Engine) handleControl(gen *generation, raw *transport.Conn) {
	defer engine.controlWG.Done()
	gen.track(raw)
	defer gen.untrack(raw)
	engine.trackControl(raw)
	defer engine.untrackControl(raw)

	// 连接到达即记录：后续任何一步（版本判定、协商、首帧读取）卡住或失败时，
	// 这条日志是区分"连接没到"与"到了但没走通"的唯一依据。
	engine.log().Info("控制入口收到连接", "来源", raw.RemoteAddr().String())
	guard := wire.NewConnectionGuard(raw, nil, wireOptionsFromCompat())
	version, err := guard.DetectVersion(raw)
	if err != nil {
		engine.closeSession(raw, "版本检测失败", err)
		return
	}
	engine.log().Info("版本判定完成", "来源", raw.RemoteAddr().String(), "版本", string(version))
	// v2 连接先完成协商（hello 往返）再进入消息阶段（FR-03 §3.4/§4.2）：
	// 协商失败必须失败，不静默降级到 v1。
	if version == wire.VersionV2 {
		// v2 下两种连接起始形状不同：控制连接以 client hello 开头并进入协商与
		// 加密；客户端建立的工作连接只发魔数后直接发消息帧（不协商、不加密）。
		firstFrameType, peekErr := guard.PeekV2FirstFrameType()
		if peekErr != nil {
			engine.closeSession(raw, "v2 首帧类型探测失败", peekErr)
			return
		}
		engine.log().Info("v2 首帧类型", "来源", raw.RemoteAddr().String(), "类型", firstFrameType)
		if firstFrameType == wire.V2FrameTypeMessage {
			engine.serveV2WorkConn(gen, raw, guard)
			return
		}
		if negErr := engine.completeV2Negotiation(raw, guard); negErr != nil {
			engine.closeSession(raw, "v2 协商失败", negErr)
			return
		}
	}
	// 版本判定已把读取器绑定到回放后的流：用守卫统一入口读取，
	// 不得重建 V1Reader，否则会重复消费版本判定阶段的预读字节。
	first, err := guard.ReadFrame()
	if err != nil {
		engine.closeSession(raw, "首帧读取失败", err)
		return
	}
	engine.log().Info("控制连接首帧", "来源", raw.RemoteAddr().String(), "类型", first.Type.Name)
	switch first.Type.Name {
	case "login":
		session := sessionWriter{conn: raw, version: version, mu: &sync.Mutex{}}
		official, runID, err := gen.handleLogin(session, first.Payload)
		if err != nil {
			first.Release()
			engine.closeSession(raw, "登录被拒绝", nil)
			return
		}
		var request loginPayload
		_ = json.Unmarshal(first.Payload, &request)
		first.Release()
		// 官方形态客户端在登录响应之后切换加密通道（登录握手本身是明文）：
		// v1 走 AES-128-CFB，v2 走协商出的分帧 AEAD，两者切换时序一致。
		if official {
			if cipherErr := gen.enableControlCipher(guard, &session, request.ClientID, version); cipherErr != nil {
				engine.closeSession(raw, "控制通道加密切换失败", cipherErr)
				return
			}
		}
		if official {
			// 官方形态会话需要登记：其工作连接由服务端指派（req-work-conn），
			// 指派要能拿到控制连接的写出通道与运行 ID。
			engine.registerOfficialSession(request.ClientID, session, runID)
		}
		// 会话登记与旧会话替换（FR-03 §3.4）：同一客户端的新会话接管，
		// 旧会话转入退出——关闭其控制连接，由其 defer 完成会话资源清理。
		// 运行时代理按连接归属清理，替换过程不会误伤新会话的注册。
		if oldSession := engine.bindControlClient(raw, request.ClientID); oldSession != nil {
			engine.log().Info("同一客户端的新会话已接管，旧会话退出", "客户端", request.ClientID)
			_ = oldSession.Close()
		}
		// 会话级清理（FR-03）：该连接上注册的运行时代理随会话结束释放。
		defer gen.cleanupRuntimeProxiesForConn(raw)
		engine.events.Publish(core.ClientConnected{
			ClientID:   request.ClientID,
			RemoteAddr: raw.RemoteAddr().String(),
			EventMeta:  core.NewEventMeta(),
		})
		engine.serveControlLoop(gen, session, guard, request.ClientID)
	case "new-work-conn":
		// 走到这里的都是 v1 工作连接：v2 的连接在入口已按首帧类型分流。
		engine.serveWorkDeclaration(gen, raw, first.Payload, wire.VersionV1)
		first.Release()
	default:
		first.Release()
		engine.closeSession(raw, "控制连接首帧类型非法", nil)
	}
}

// serveControlLoop 在已登录的控制连接上处理心跳与代理管理消息，直到会话结束。
//
// 失活判定（FR-03 §3.5）：每轮读取前把读截止时间设为失活窗口（心跳周期的
// 三倍）。窗口内到达的任何心跳自动续期，单次丢失不影响会话；连续超过窗口
// 未收到心跳即判失活并关闭会话。
//
// 会话级错误（心跳失活、未知帧、读取失败）只关闭本会话：FR-03 的多客户端
// 语义下，单个客户端掉线不能让服务器停机（FR-25 的单会话语义已被取代）。
func (engine *Engine) serveControlLoop(gen *generation, session sessionWriter, guard *wire.ConnectionGuard, clientID string) {
	liveness := engine.controlLivenessWindow()
	for {
		if deadlineErr := session.conn.SetReadDeadline(time.Now().Add(liveness)); deadlineErr != nil {
			engine.closeSession(session.conn, "设置读截止时间失败", deadlineErr)
			return
		}
		frame, err := guard.ReadFrame()
		if err != nil {
			if isReadDeadline(err) {
				engine.closeSession(session.conn, "心跳失活（超过失活窗口未收到心跳）", nil)
			} else {
				engine.closeSession(session.conn, "控制连接读取失败", err)
			}
			return
		}
		switch frame.Type.Name {
		case "ping":
			frame.Release()
			if err := engine.replyPong(session); err != nil {
				engine.closeSession(session.conn, "心跳应答失败", err)
				return
			}
		case "new-proxy":
			engine.handleNewProxy(gen, session, clientID, frame.Payload)
			frame.Release()
		case "close-proxy":
			engine.handleCloseProxy(gen, clientID, frame.Payload)
			frame.Release()
		default:
			frame.Release()
			engine.closeSession(session.conn, "控制连接收到未知帧", nil)
			return
		}
	}
}

// closeSession 关闭一条控制会话并记录会话级诊断。
//
// 与 failAbnormal 的边界：会话级错误（协议违规、鉴权失败、心跳失活、
// 客户端掉线）绝不能让引擎停机——否则一个客户端掉线会杀死全部会话，
// 官方 frpc 的断线重连更会反复触发。引擎停止只由 Accept 致命失败与
// Shutdown 触发（FR-03 §3.4/§3.5 明确"关闭连接"，不是"停止服务"）。
func (engine *Engine) closeSession(conn *transport.Conn, reason string, err error) {
	if err != nil {
		engine.log().Warn("控制会话关闭", "原因", reason, "错误", err)
	} else {
		engine.log().Info("控制会话关闭", "原因", reason)
	}
	_ = conn.Close()
}

// wireOptionsFromCompat 按兼容基线声明推导控制入口的 wire 版本策略。
//
// 兼容声明是真源（core/compat.Current()），不引入独立的服务端配置项：声明
// 同时含 v1/v2 时两个版本都必须在同一入口登录成功（FR-03 §3.9 的「分别强制
// wire v1 与 wire v2 发起连接」）。注意 MaxWireVersion 的既有语义是「入口可
// 接受的最低版本」——取 v1 表示 v1/v2 都接受，只有取 v2 才是「仅接受 v2」；
// v2 是否可用由 V2Enabled 单独控制。
func wireOptionsFromCompat() wire.Options {
	options := wire.Options{MaxWireVersion: wire.VersionV2}
	for _, version := range compat.Current().WireVersions() {
		switch version {
		case string(wire.VersionV1):
			options.MaxWireVersion = wire.VersionV1
		case string(wire.VersionV2):
			options.V2Enabled = true
		}
	}
	return options
}

// sessionWriter 按会话的 wire 版本写出站消息帧。
//
// 入站读取已由连接守卫统一（版本判定后绑定读取器），出站编码没有守卫遮挡：
// v1 是单字节类型前缀 + JSON，v2 是帧头 + 两字节类型 ID + JSON。同一段业务
// 处理必须按会话版本选择编码，否则 v2 客户端解不出服务端响应。
type sessionWriter struct {
	conn    *transport.Conn
	version wire.Version
	// output 是消息写出目标；为空表示直接写连接。官方形态客户端在登录成功后
	// 把出站字节流切换为加密写入器。
	output io.Writer
	// mu 串行化同一控制连接上的并发写入：控制循环的应答（心跳、代理响应）与
	// 访客路径发起的 req-work-conn 来自不同 goroutine，不加锁会撕裂帧边界。
	mu *sync.Mutex
}

// writeMessage 编码并写出单条消息帧。
func (writer sessionWriter) writeMessage(messageType wire.MessageType, body []byte) error {
	encoded, err := encodeSessionMessage(writer.version, messageType, body)
	if err != nil {
		return err
	}
	target := writer.output
	if target == nil {
		target = writer.conn
	}
	if writer.mu != nil {
		writer.mu.Lock()
		defer writer.mu.Unlock()
	}
	_, err = target.Write(encoded)
	return err
}

// encodeSessionMessage 按 wire 版本编码一条消息帧。
func encodeSessionMessage(version wire.Version, messageType wire.MessageType, body []byte) ([]byte, error) {
	if version == wire.VersionV2 {
		return wire.EncodeV2MessageFrame(messageType, body)
	}
	return wire.EncodeV1Frame(wire.Frame{Type: messageType, Payload: body})
}

// completeV2Negotiation 完成 wire v2 协商并回写 server hello。
//
// 协商读取器持有流位置，guard.Negotiate 已把它绑定为后续消息帧的读取器；
// server hello 是协商结果的下行确认，必须在协商成功后立即发送。
func (engine *Engine) completeV2Negotiation(conn *transport.Conn, guard *wire.ConnectionGuard) error {
	result, err := guard.Negotiate()
	if err != nil {
		return err
	}
	payload, err := wire.EncodeServerHello(result)
	if err != nil {
		return err
	}
	// 协商记录要参与控制通道密钥派生：在写出前留下服务端 hello 的原始载荷。
	guard.RecordV2ServerHello(payload)
	frame, err := wire.EncodeV2Frame(wire.V2FrameTypeServerHello, payload)
	if err != nil {
		return err
	}
	_, err = conn.Write(frame)
	return err
}

// controlLivenessWindow 返回控制会话的失活窗口：心跳周期的三倍。
//
// 三倍窗口允许两次连续心跳丢失后仍能恢复；与官方 frpc 的默认参数
// （心跳 30s、超时 90s）一致。下限一秒，防止极小测试配置导致误杀。
func (engine *Engine) controlLivenessWindow() time.Duration {
	window := engine.heartbeat * 3
	if window < time.Second {
		window = time.Second
	}
	return window
}

// isReadDeadline 判定错误是否为读截止时间超时。
func isReadDeadline(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// failAbnormal 记录异常停止的首个错误并关闭 Done。
//
// 用途收窄（FR-03 §3.4/§3.5）：只用于引擎级致命错误——当前唯一调用点是
// 控制入口的 Accept 致命失败。会话级错误（协议违规、鉴权失败、心跳失活、
// 客户端掉线）一律走 closeSession，只影响本会话：一个客户端掉线不能让
// 服务器停机，否则官方 frpc 的断线重连会反复杀死服务端。
//
// 正常 Shutdown 路径不得调用：Shutdown 后 Err() 必须保持 nil。
// 引擎已进入停止流程时调用为空操作——此时连接关闭引发的读错误是 Shutdown
// 的预期结果，不属于异常终止。
// 传入的错误不得包含 token、密码、Authorization 或正文原文。
func (engine *Engine) failAbnormal(gen *generation, err error) {
	engine.mu.Lock()
	if engine.shuttingDown || engine.state == stateStopped || (gen != nil && gen.stopping) {
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

// serveWorkDeclaration 处理一条工作连接的归属声明、鉴权、目标校验并配对访客。
//
// 工作连接是与控制连接平行的独立连接，服务端无从由连接本身判断归属，因此声明
// 必须携带鉴权材料（PROTOCOL §7 第 2、3 步）。缺少这一步时，任何能连上控制端口
// 的对端都能声明任意代理名：既能与真实访客配对并读取其数据，也能借该代理名触发
// 配对中心的副作用（例如释放该代理的暂存访客）。
//
// 四级校验按顺序执行，任一失败都关闭连接且不产生副作用：鉴权材料 → 活跃会话
// 归属 → 代理归属 → 目标地址允许集合。会话校验先于归属校验，使已退出的控制
// 会话无法在残留在途的工作连接上继续声明（FR-03 §7.5「过期会话」）；归属校验
// 先于目标校验，使未认证对端无法借"越权目标"这一分支触碰配对中心。
func (engine *Engine) serveWorkDeclaration(gen *generation, raw *transport.Conn, payload []byte, version wire.Version) {
	declaration, err := parseWorkDeclaration(payload)
	if err != nil {
		// 声明非法必须可见：静默关闭会让对端只看到"连接被断开"而无从定位。
		// 只记录字段是否存在，不记录任何字段值——鉴权材料不得进日志。
		var probe workConnRequest
		_ = json.Unmarshal(payload, &probe)
		engine.log().Warn("工作连接声明解析失败，已拒绝", "错误", err,
			"载荷长度", len(payload), "有客户端字段", probe.ClientID != "",
			"有运行ID字段", probe.RunID != "", "有官方材料字段", probe.PrivilegeKey != "",
			"有明文材料字段", probe.Token != "", "有时间戳", probe.Timestamp != 0)
		_ = raw.Close()
		return
	}
	if declaration.official {
		engine.serveOfficialWorkConn(gen, raw, declaration, version)
		return
	}
	if matched, _ := gen.credentialsMatch(declaration.clientID, declaration.token, declaration.timestamp); !matched {
		engine.log().Warn("工作连接的鉴权材料无效，已拒绝")
		_ = raw.Close()
		return
	}
	if !engine.hasActiveControlSession(declaration.clientID) {
		engine.log().Warn("工作连接声明来自无活跃控制会话的客户端，已拒绝",
			"客户端", declaration.clientID)
		_ = raw.Close()
		return
	}
	if !engine.registry.ProxyBelongsTo(declaration.proxy, declaration.clientID) {
		engine.log().Warn("工作连接声明的代理不属于该客户端，已拒绝",
			"proxy", declaration.proxy)
		_ = raw.Close()
		return
	}
	if !engine.registry.TargetAllowed(declaration.proxy, declaration.target) {
		engine.log().Warn("工作连接的目标地址不在允许集合内，已拒绝",
			"proxy", declaration.proxy, "target", declaration.target.String())
		_ = raw.Close()
		// 该代理的暂存访客同样要释放：它们等的是刚被拒绝的工作连接，
		// 继续留在队列里只会被永久悬挂。此处已通过鉴权与归属校验，
		// 因此释放只可能由该代理的合法客户端触发。
		//
		// 逐个 untrack 必须在锁外进行（dropGuests 返回后）：untrack 取 Engine 锁，
		// 在 broker 锁内回调会与 Shutdown 构成 ABBA。漏掉这一步会让已关闭的连接
		// 永久留在活动集合里，引擎长跑时单调增长。
		if dropped := engine.activeBroker().dropGuests(declaration.proxy); len(dropped) > 0 {
			for _, conn := range dropped {
				gen.untrack(conn)
			}
			engine.log().Warn("目标越权已拒绝工作连接，同时释放等待配对的访客",
				"proxy", declaration.proxy, "访客数", len(dropped))
		}
		return
	}
	// park 只做配对决策，登记与桥接在 broker 锁外完成（见 pairing 的说明）。
	accepted, pair := engine.activeBroker().park(declaration.proxy, raw)
	if !accepted {
		_ = raw.Close()
		return
	}
	if pair != nil {
		pair.start(gen.track, gen.wg.Add)
	}
}

// credentialsMatch 判断客户端鉴权材料是否与服务端快照凭证匹配（FR-03 §3.4）。
//
// 两条链共存（同一条兼容消息路径，两种客户端形态）：
//   - 摘要链：jrpc 送 token 明文，服务端在本地转 SHA-256 摘要后与快照摘要恒定
//     时间比较；
//   - 官方链：官方 frpc 送 `md5(token + 十进制时间戳)` 的摘要前处理材料，服务端
//     必须持有 token 明文才能复算比对（快照的 CompatToken 字段）。
//
// 任一条通过即通过；失败时不区分是哪条链的哪一环，避免泄漏校验进度。
func (gen *generation) credentialsMatch(clientID, material string, timestamp int64) (bool, bool) {
	if clientID == "" || material == "" {
		return false, false
	}
	provided := DigestToken(material)
	for _, credential := range gen.config.Credentials() {
		if credential.ClientID != clientID {
			continue
		}
		if credential.Token != "" && digestEqual(provided, credential.Token) {
			return true, false
		}
		if credential.CompatToken != "" && digestEqual(officialPrivilegeKey(credential.CompatToken, timestamp), material) {
			return true, true
		}
	}
	return false, false
}

// compatToken 返回客户端配置的兼容明文材料。
func (gen *generation) compatToken(clientID string) (string, bool) {
	for _, credential := range gen.config.Credentials() {
		if credential.ClientID == clientID && credential.CompatToken != "" {
			return credential.CompatToken, true
		}
	}
	return "", false
}

// officialControlSession 是官方形态客户端控制会话的登记项。
type officialControlSession struct {
	// writer 是控制连接的写出通道（已含加密切换与并发锁）。
	writer sessionWriter
	// runID 是登录时分配给该会话的运行 ID：官方客户端在 new-work-conn 里回传它。
	runID string
}

// registerOfficialSession 登记官方形态控制会话，并建立运行 ID 反向索引。
//
// 只在本次登录实际走官方链时登记：jrpc 的工作连接走声明式路径（客户端在声明里
// 携带代理名与目标），不需要服务端指派。
func (engine *Engine) registerOfficialSession(clientID string, writer sessionWriter, runID string) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.officialSessions[clientID] = officialControlSession{writer: writer, runID: runID}
	if runID != "" {
		engine.sessionRunIDs[runID] = clientID
	}
}

// sessionByRunID 按运行 ID 查找官方会话。
func (engine *Engine) sessionByRunID(runID string) (string, officialControlSession, bool) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	clientID, ok := engine.sessionRunIDs[runID]
	if !ok {
		return "", officialControlSession{}, false
	}
	session, ok := engine.officialSessions[clientID]
	if !ok {
		return "", officialControlSession{}, false
	}
	return clientID, session, true
}

// proxyOwner 返回代理绑定的属主客户端标识；未注册时返回空字符串。
func (engine *Engine) proxyOwner(name string) string {
	binding := engine.registry.Binding(name)
	if binding == nil {
		return ""
	}
	return binding.OwnerClientID
}

// requestWorkConn 向官方形态客户端的控制连接请求一条工作连接。
//
// 官方语义（FR-03 §3.7）：服务端在有待处理访客时主动请求工作连接，客户端新建
// 连接后只声明运行 ID 与鉴权材料，由服务端在配对那一刻指派它服务哪个代理。
// 非官方会话不做任何事：那条路径由客户端的声明式工作连接承担。
func (engine *Engine) requestWorkConn(clientID string) {
	engine.mu.Lock()
	session, ok := engine.officialSessions[clientID]
	engine.mu.Unlock()
	if !ok {
		// 声明式客户端（jrpc）：工作连接由客户端主动送来，服务端不请求。
		return
	}
	if err := session.writer.writeMessage(wire.MessageTypeReqWorkConn, []byte(`{}`)); err != nil {
		engine.log().Warn("请求工作连接失败", "客户端", clientID, "错误", err)
		return
	}
	engine.log().Info("已请求工作连接", "客户端", clientID)
}

// enableControlCipher 在官方形态客户端登录成功后把控制通道切换为加密读写。
//
// 官方 frpc 的既定行为：登录握手是明文，其后所有消息走 AES-128-CFB 加密通道
// （密钥由 token 明文经 PBKDF2(SHA-1, 盐 "frp", 64 次) 派生）；jrpc 走摘要链、
// 不启用加密。读侧切换由守卫完成（先消费对端 IV），写侧把会话的输出目标替换为
// 加密写入器（首次写入时发送本端 IV）。
func (gen *generation) enableControlCipher(guard *wire.ConnectionGuard, session *sessionWriter, clientID string, version wire.Version) error {
	if version == wire.VersionV2 {
		return gen.enableV2ControlCipher(guard, session, clientID)
	}
	token, ok := gen.compatToken(clientID)
	if !ok {
		return errors.New("官方鉴权链通过但缺少兼容明文材料，无法建立加密通道")
	}
	key, err := wire.V1ControlCipherKey(token)
	if err != nil {
		return err
	}
	encrypted, err := wire.NewV1CipherWriter(session.conn, key)
	if err != nil {
		return err
	}
	if err := guard.EnableV1Cipher(key); err != nil {
		return err
	}
	session.output = encrypted
	gen.engine.log().Info("控制通道已切换加密（官方形态客户端，v1）", "客户端", clientID)
	return nil
}

// enableV2ControlCipher 在 v2 会话上切换分帧 AEAD 通道。
//
// 密钥由协商记录（两段 hello）与 token 明文共同派生，两个方向各取一个：服务端
// 读用 client-to-server、写用 server-to-client。协商记录缺失时拒绝切换——没有它
// 派生出的密钥与对端不一致，切换只会让会话静默失败。
func (gen *generation) enableV2ControlCipher(guard *wire.ConnectionGuard, session *sessionWriter, clientID string) error {
	token, ok := gen.compatToken(clientID)
	if !ok {
		return errors.New("官方鉴权链通过但缺少兼容明文材料，无法建立加密通道")
	}
	transcript, ok := guard.V2Transcript()
	if !ok {
		return errors.New("v2 协商记录不完整，无法派生控制通道密钥")
	}
	clientDigest, serverDigest, _ := guard.V2HelloDigests()
	gen.engine.log().Info("v2 协商载荷", "客户端", clientID,
		"客户端hello", fmt.Sprintf("%x", clientDigest), "服务端hello", fmt.Sprintf("%x", serverDigest))
	readKey, err := wire.DeriveV2ControlKey(token, wire.V2CipherAlgorithmAES256GCM, wire.V2DirectionClientToServer, transcript)
	if err != nil {
		return err
	}
	writeKey, err := wire.DeriveV2ControlKey(token, wire.V2CipherAlgorithmAES256GCM, wire.V2DirectionServerToClient, transcript)
	if err != nil {
		return err
	}
	encrypted, err := wire.NewV2AEADWriter(session.conn, writeKey)
	if err != nil {
		return err
	}
	if err := guard.EnableV2Cipher(readKey); err != nil {
		return err
	}
	session.output = encrypted
	gen.engine.log().Info("控制通道已切换加密（官方形态客户端，v2）", "客户端", clientID)
	return nil
}

// officialPrivilegeKey 复算官方 frpc 的鉴权材料：md5(token ∥ 十进制时间戳) 的十六进制。
//
// 这里使用 MD5 不是为了自身安全性，而是官方协议既定的线上算法：材料只做一次性
// 比对、不落库、不派生任何后续密钥，服务端只按官方语义复算以完成互操作。
func officialPrivilegeKey(token string, timestamp int64) string {
	sum := md5.Sum([]byte(token + strconv.FormatInt(timestamp, 10))) //nolint:gosec // 兼容官方协议的线上算法
	return hex.EncodeToString(sum[:])
}

// workDeclaration 是一条工作连接声明的解出结果。
type workDeclaration struct {
	clientID  string
	token     string
	timestamp int64
	proxy     string
	target    netip.AddrPort
	// official 表示官方形态声明：只带运行 ID 与 md5 鉴权材料，
	// 代理归属与源/目标摘要由服务端在配对那一刻指派（FR-03 §3.7）。
	official bool
	runID    string
}

// parseWorkDeclaration 解析工作连接声明载荷，返回代理归属与本地目标地址。
//
// 目标地址缺失或不可解析时返回错误：不可解析的输入不得被当作放行依据。
func parseWorkDeclaration(payload []byte) (workDeclaration, error) {
	var request workConnRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return workDeclaration{}, err
	}
	// 官方形态：只声明运行 ID（必要时附带 md5 鉴权材料），代理与目标由服务端指派。
	//
	// 材料是否出现取决于客户端的鉴权 scope：实测官方 v0.70.0 的默认配置下
	// new-work-conn 只带 run_id。运行 ID 由服务端分配、经加密通道下发，本身
	// 就是会话凭据；材料存在时仍照常校验。
	if request.ClientID == "" && request.RunID != "" {
		return workDeclaration{
			token:     request.PrivilegeKey,
			timestamp: request.Timestamp,
			official:  true,
			runID:     request.RunID,
		}, nil
	}
	// 鉴权材料与代理名都是必填：缺失即拒绝，不进入后续任何校验或配对流程。
	if request.ClientID == "" || request.Token == "" {
		return workDeclaration{}, errors.New("工作连接缺少鉴权材料")
	}
	if request.Proxy == "" {
		return workDeclaration{}, errors.New("工作连接未声明代理归属")
	}
	target, err := netip.ParseAddrPort(request.Target)
	if err != nil {
		return workDeclaration{}, errors.New("工作连接声明的目标地址不可解析")
	}
	return workDeclaration{
		clientID:  request.ClientID,
		token:     request.Token,
		timestamp: request.Timestamp,
		proxy:     request.Proxy,
		target:    target,
	}, nil
}

// loginPayload 是登录载荷。
//
// 字段名与官方兼容消息族一致（FR-03 §3.3：官方 frpc 与 jrpc 共用同一条协议
// 路径）：官方形状为 {version, hostname, os, arch, user, privilege_key,
// timestamp, run_id, client_id, metas, client_spec, pool_count}，服务端只取
// 鉴权与身份所需的字段；未知字段一律忽略。
type loginPayload struct {
	// ClientID 是客户端标识，官方字段名 client_id。
	ClientID string `json:"client_id"`
	// Token 是独立 token 明文，官方字段名 privilege_key。
	Token string `json:"privilege_key"`
	// RunID 是本轮连接的运行 ID；官方 frpc 首次登录为空，由服务端分配。
	RunID string `json:"run_id"`
	// Timestamp 是鉴权时间材料（Unix 秒，官方语义）。
	Timestamp int64 `json:"timestamp"`
	// Version 是客户端版本号，仅用于诊断与日志。
	Version string `json:"version"`
}

// loginResponsePayload 是登录响应载荷。
//
// 官方形状为 {version, run_id, error}：对端以「error 为空」判定成功，成功后
// 采用响应里的 run_id 作为本会话运行 ID。未知字段被对端忽略，但这里不做多余承诺。
type loginResponsePayload struct {
	Version string `json:"version,omitempty"`
	RunID   string `json:"run_id,omitempty"`
	Error   string `json:"error,omitempty"`
}

// handleLogin 校验客户端凭证并回复登录结果。
//
// 校验链当前阶段：凭证摘要比较（FR-03 §3.4）。快照凭证持有摘要，请求明文
// 在本地转摘要后恒定时间比较；客户端状态、时间窗口与重放边界由登录链
// （loginChain）逐步接入。成功后由服务端分配运行 ID 并回写。
func (gen *generation) handleLogin(session sessionWriter, payload []byte) (bool, string, error) {
	var request loginPayload
	if err := json.Unmarshal(payload, &request); err != nil {
		_ = gen.writeLoginResponse(session, "", "登录载荷非法")
		return false, "", err
	}
	matched, official := gen.credentialsMatch(request.ClientID, request.Token, request.Timestamp)
	if !matched {
		_ = gen.writeLoginResponse(session, "", "鉴权未通过")
		return false, "", errors.New("服务端拒绝客户端登录")
	}
	runID := request.RunID
	if runID == "" {
		// 客户端首次登录不带运行 ID：由服务端分配，后续工作连接声明要回传它。
		runID = newSessionRunID()
	}
	if err := gen.writeLoginResponse(session, runID, ""); err != nil {
		return false, "", err
	}
	return official, runID, nil
}

// newSessionRunID 生成一次控制会话的运行 ID。
func newSessionRunID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		// 随机源不可用时退化为时间派生值：运行 ID 用于会话内配对与重放边界，
		// 不是密钥材料，退化不影响鉴权强度。
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buffer)
}

// writeLoginResponse 写出登录响应帧；failure 为空表示登录成功。
func (gen *generation) writeLoginResponse(session sessionWriter, runID, failure string) error {
	response := loginResponsePayload{RunID: runID, Error: failure}
	body, err := json.Marshal(response)
	if err != nil {
		return err
	}
	return session.writeMessage(wire.MessageTypeLoginResponse, body)
}

// replyPong 回复心跳。
func (engine *Engine) replyPong(session sessionWriter) error {
	return session.writeMessage(wire.MessageTypePong, []byte(`{}`))
}

// serveGuest 接受指定代理的访客连接。
//
// Accept 错误同样按临时/致命分类处理，致命错误停止监听并上报而不是静默退出。
// 临时错误后要复查本代是否已被要求停止接收：入口交接用"把 Accept 提前唤醒"来
// 停掉旧代的循环而不关闭套接字，那条路径只表现为一次超时，看不出停止意图。
func (engine *Engine) serveGuest(gen *generation, name string, listener *transport.Listener) {
	defer gen.acceptWG.Done()
	for {
		conn, action, err := listener.Accept(transport.PurposeWork, name)
		if err != nil {
			if action == transport.AcceptFatal {
				engine.reportAcceptFatal(gen, listener, err)
				return
			}
			if gen.isStopping() {
				return
			}
			time.Sleep(transport.AcceptBackoff(action))
			continue
		}
		gen.wg.Add(1)
		go engine.handleGuest(gen, name, conn)
	}
}

// serveHTTPGuest 接受共享 HTTP 入口上的连接并按主机与路径路由。
func (engine *Engine) serveHTTPGuest(gen *generation, port int, listener *transport.Listener) {
	defer gen.acceptWG.Done()
	for {
		conn, action, err := listener.Accept(transport.PurposeWork, httpEntryName(port))
		if err != nil {
			if action == transport.AcceptFatal {
				engine.reportAcceptFatal(gen, listener, err)
				return
			}
			if gen.isStopping() {
				return
			}
			time.Sleep(transport.AcceptBackoff(action))
			continue
		}
		gen.wg.Add(1)
		go engine.handleHTTPGuest(gen, port, conn)
	}
}

// pendingProxyFor 返回该客户端名下「有暂存访客」的代理名；没有时返回空字符串。
//
// 官方工作连接不声明代理归属，服务端用这个查询把「刚到达的工作连接」与
// 「正在等它的访客」对上：请求方（req-work-conn）与到达方都按同一条件匹配。
func (engine *Engine) pendingProxyFor(gen *generation, clientID string) string {
	broker := engine.activeBroker()
	gen.engine.mu.Lock()
	names := make([]string, 0, len(gen.runtimeProxies))
	for name, entry := range gen.runtimeProxies {
		if entry.clientID == clientID {
			names = append(names, name)
		}
	}
	gen.engine.mu.Unlock()
	for _, name := range names {
		if broker.hasStagedGuest(name) {
			return name
		}
	}
	return ""
}

// serveV2WorkConn 处理 v2 的工作连接：明文路径，不经协商也不加密。
//
// 官方客户端建立工作连接时只发送版本魔数，随后直接是消息帧（首个即
// new-work-conn）；它既不参与 hello 协商，也不启用 AEAD。这里读出该帧并按
// 既有的声明校验与指派流程处理，与 v1 的工作连接语义一致。
func (engine *Engine) serveV2WorkConn(gen *generation, raw *transport.Conn, guard *wire.ConnectionGuard) {
	// 工作连接不参与协商，协商路径不会为它建立读取器：这里按明文语义单独绑定。
	if bindErr := guard.BindV2PlainReader(); bindErr != nil {
		engine.closeSession(raw, "v2 工作连接读取器绑定失败", bindErr)
		return
	}
	frame, err := guard.ReadFrame()
	if err != nil {
		engine.closeSession(raw, "v2 工作连接读取失败", err)
		return
	}
	defer frame.Release()
	engine.log().Info("v2 工作连接到达", "来源", raw.RemoteAddr().String(), "类型", frame.Type.Name)
	if frame.Type.Name != "new-work-conn" {
		engine.closeSession(raw, "v2 工作连接首帧类型非法", nil)
		return
	}
	engine.serveWorkDeclaration(gen, raw, frame.Payload, wire.VersionV2)
}

// serveOfficialWorkConn 处理官方形态的工作连接声明并指派一个代理。
//
// 顺序与声明式路径同构：会话归属（运行 ID）→ 鉴权材料 → 代理归属（由服务端
// 在配对这一刻决定）→ 回写 start-work-conn → 交给配对中心。官方对端在收到
// start-work-conn 后才开始转发，因此指派必须发生在配对之前。
func (engine *Engine) serveOfficialWorkConn(gen *generation, raw *transport.Conn, declaration workDeclaration, version wire.Version) {
	clientID, _, ok := engine.sessionByRunID(declaration.runID)
	if !ok {
		engine.log().Warn("工作连接声明的运行 ID 不属于活跃会话，已拒绝", "运行ID", declaration.runID)
		_ = raw.Close()
		return
	}
	engine.log().Info("官方工作连接到达", "客户端", clientID, "运行ID", declaration.runID)
	if declaration.token != "" {
		if matched, _ := gen.credentialsMatch(clientID, declaration.token, declaration.timestamp); !matched {
			engine.log().Warn("工作连接的鉴权材料无效，已拒绝")
			_ = raw.Close()
			return
		}
	}
	name := engine.pendingProxyFor(gen, clientID)
	if name == "" {
		engine.log().Warn("工作连接到达时该客户端没有待处理访客，已拒绝", "客户端", clientID)
		_ = raw.Close()
		return
	}
	summary, _ := engine.activeBroker().stagingSummaryFor(name)
	payload, err := json.Marshal(startWorkConnPayload{
		ProxyName: name,
		SrcAddr:   summary.srcAddr,
		SrcPort:   summary.srcPort,
		DstAddr:   summary.dstAddr,
		DstPort:   summary.dstPort,
	})
	if err != nil {
		_ = raw.Close()
		return
	}
	// 按会话的 wire 版本编码：工作连接的帧格式必须与控制连接一致，v2 下是
	// 帧头 + 消息类型 ID 的消息帧，v1 下是单字节类型前缀。
	encoded, err := encodeSessionMessage(version, wire.MessageTypeStartWorkConn, payload)
	if err != nil {
		_ = raw.Close()
		return
	}
	if _, err := raw.Write(encoded); err != nil {
		_ = raw.Close()
		return
	}
	gen.track(raw)
	accepted, pair := engine.activeBroker().park(name, raw)
	if !accepted {
		_ = raw.Close()
		return
	}
	if pair != nil {
		pair.start(gen.track, gen.wg.Add)
	}
}

// pairGuest 为访客配对工作连接，跳过已失效的连接。
//
// 跨网络环境下待命工作连接会被 NAT 或中间设备静默回收，池中无法预先感知，
// 只在配对这一刻才暴露。不重试会让这类失效直接变成访客失败——实测跨 NAT 时
// 成功率仅 7%，而回环下为 93%。每条失效连接都会在重试中被关闭并移出池，
// 因此重试次数受池大小约束，maxPairRetries 只是防御上限。
//
// 记账约定：返回 parkPaired 表示已移交桥接，访客与工作连接由 start 接管，
// 调用方不得再操作；其余结果由调用方按各自既有方式处理（两处调用点对暂存的
// 记账处理不同，本函数不擅自统一）。
func (engine *Engine) pairGuest(gen *generation, name string, guest *transport.Conn, pending []byte) parkOutcome {
	for attempt := 0; attempt < maxPairRetries; attempt++ {
		outcome, pair := engine.activeBroker().parkGuest(name, guest, pending)
		if outcome != parkPaired {
			return outcome
		}
		// 访客与工作连接都由 start 重新登记，先撤回调用方为访客做的登记。
		gen.untrack(guest)
		if pair.start(gen.track, gen.wg.Add) {
			return parkPaired
		}
		// 该工作连接已失效并被关闭；访客尚未被服务，恢复登记后继续尝试下一条。
		gen.track(guest)
	}
	// 正常不会到达：池有界，重试会把失效连接逐步清空。
	engine.log().Error("配对重试已达上限，按不可接纳处理",
		"proxy", name, "retries", maxPairRetries)
	return parkRejected
}

// handleGuest 把访客连接交给配对中心：有待命工作连接立即桥接，否则暂存。
func (engine *Engine) handleGuest(gen *generation, name string, guest *transport.Conn) {
	defer gen.wg.Done()
	gen.track(guest)
	// 配对与桥接在 broker 锁之外完成：parkGuest 只在自身锁内做决策，登记活动
	// 连接要取 Engine 的锁，持 broker 锁去取会与 Shutdown 的锁序构成死锁。
	switch outcome := engine.pairGuest(gen, name, guest, nil); outcome {
	case parkPaired:
		// 已移交桥接，记账由 pairing.start 接管。
	case parkStaged:
		// 连接留在配对中心，Shutdown 与 close 负责最终释放。
		// untrack 不在此处调用，避免重复记账：配对中心的关闭路径统一处理。
		//
		// 官方形态客户端不会主动送来工作连接：服务端必须在这里请求一条
		// （req-work-conn），否则访客会一直停到失活超时。
		if owner := engine.proxyOwner(name); owner != "" {
			engine.requestWorkConn(owner)
		}
	case parkRejected:
		gen.untrack(guest)
		_ = guest.Close()
	case parkCapacityFull:
		// 容量拒绝必须可见：否则"访客连不上"会被当成客户端问题，而真实原因是
		// 服务端到达了承接上限。
		gen.untrack(guest)
		_ = guest.Close()
		engine.log().Warn("该代理的等待访客已达上限，拒绝新访客",
			"proxy", name, "累计拒绝", engine.activeBroker().RejectedGuests())
	}
}

// proxySummaries 返回本代代理摘要列表（深复制，规格 §3.5）。
//
// 来源是本代配置快照的绑定集合：代理名、类型与运行状态。HTTP 多代理共享同一
// 入口端口，各自仍按代理名出一行摘要。
func (gen *generation) proxySummaries() []core.ProxySummary {
	bindings := gen.config.AllBindings()
	summaries := make([]core.ProxySummary, 0, len(bindings))
	for _, binding := range bindings {
		summaries = append(summaries, core.ProxySummary{
			Name:   binding.ProxyName(),
			Kind:   string(binding.Type()),
			Status: "running",
		})
	}
	return summaries
}

// activeClientSummaries 返回已登记控制连接的客户端摘要（深复制，规格 §3.5）。
//
// 同一客户端标识的多条控制连接只记一次；代理数取该客户端在当前代的绑定数。
// 引擎锁由调用方持有。
func (engine *Engine) activeClientSummaries() []core.ClientSummary {
	identifiers := make(map[string]bool, len(engine.controlClients))
	order := make([]string, 0, len(engine.controlClients))
	for _, id := range engine.controlClients {
		if id == "" || identifiers[id] {
			continue
		}
		identifiers[id] = true
		order = append(order, id)
	}
	summaries := make([]core.ClientSummary, 0, len(order))
	for _, id := range order {
		proxyCount := 0
		if engine.active != nil {
			for _, binding := range engine.active.config.AllBindings() {
				if binding.OwnerClientID() == id {
					proxyCount++
				}
			}
		}
		summaries = append(summaries, core.ClientSummary{
			ID:         id,
			Connected:  true,
			ProxyCount: proxyCount,
		})
	}
	return summaries
}

// publishRegistry 从配置快照构造代理注册表并原子发布。//
// 注册表只承载「目标地址是否允许」与「入口归属」两类运行期判定依据；字段、
// 权限、冲突与 P1 范围四级校验已在配置层完成，此处不复检（FR-06a §3.2）。
func (gen *generation) publishRegistry() {
	registry := make(proxy.Registry)
	for _, binding := range gen.config.AllBindings() {
		registry[binding.ProxyName()] = &proxy.Binding{
			Name:          binding.ProxyName(),
			OwnerClientID: binding.OwnerClientID(),
			Targets:       binding.ProxyTargets(),
		}
	}
	gen.engine.registry.Publish(registry)
	gen.publishHTTPRoutes()
}

// publishHTTPRoutes 从 HTTP 绑定构造共享端口的路由表并发布。
//
// 路由表按「端口 → 表」组织：多个 HTTP 代理共享同一入口端口，请求解析出主机
// 与路径后按最长前缀命中目标代理（规格 §3.5）。构造失败即保留空表并记日志，
// 绝不带着半张表进入运行——配置层已完成冲突校验，此处失败属防御性分支。
func (gen *generation) publishHTTPRoutes() {
	grouped := make(map[int][]proxy.HTTPRoute)
	for _, binding := range gen.config.HTTPBindings() {
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
			gen.engine.log().Error("HTTP 路由表构造失败，该入口端口将不匹配任何请求",
				"port", port, "error", err)
			continue
		}
		routes[port] = router
	}
	gen.engine.mu.Lock()
	gen.engine.httpRoutes = routes
	gen.engine.mu.Unlock()
}

// openUDPEntry 按监听端点的地址族打开一个 UDP 入口。
func (engine *Engine) openUDPEntry(config core.ServerConfig, name string, remotePort int) (*proxy.UDPProxy, error) {
	port, err := transport.ListenUDP(entryBindAddr(config, remotePort))
	if err != nil {
		return nil, err
	}
	return proxy.NewUDPProxy(proxy.UDPProxyConfig{
		Name:        name,
		Port:        port,
		Idle:        config.UDPSessionIdle(),
		MaxSessions: config.UDPSessionLimit(),
		MaxDatagram: config.UDPDatagramSize(),
		Work:        engine.udpWorkFactory(name),
	}), nil
}

// udpWorkFactory 返回为 UDP 会话提供工作连接的工厂。
//
// 会话需要一条新工作连接时向配对待命池索取；索不到即拒绝并计数，由入口累积
// 可观测事件（规格 §3.5：不得无限等待）。
// broker 跨代共享，因此工厂与具体某一代无关：交接后的入口继续用同一工厂取连接。
func (engine *Engine) udpWorkFactory(name string) func() (net.Conn, bool) {
	return func() (net.Conn, bool) {
		work := engine.activeBroker().takeStaged(name)
		if work == nil {
			return nil, false
		}
		return work, true
	}
}

// serveUDPEntry 服务一个 UDP 入口，直到入口关闭或被本代停止。
//
// 复用的入口由上一代 Suspend 停掉，Serve 进入时自行恢复接收，因此交接后不需要
// 额外的恢复动作。
func (engine *Engine) serveUDPEntry(gen *generation, name string, entry *proxy.UDPProxy) {
	defer gen.acceptWG.Done()
	entry.Serve(context.Background())
	engine.log().Info("UDP 代理入口已停止", "proxy", name)
}

// handleHTTPGuest 处理共享 HTTP 入口上的一条连接：解析请求首部并路由到目标代理。
//
// 请求行与首部必须先读出来才能路由，但这些字节属于请求本身，因此路由命中后
// 要原样回放到桥接流上：漏掉回放会让目标服务收到一个被截断的请求。
// 未匹配时返回明确的 404 且响应体不回显内部路由表；此后关闭连接，不留悬挂。
//
// 访客的记账在移交配对中心时一并移交（见 bridgeHTTPGuest），因此本函数不设
// defer untrack：那会在桥接接管记账之后把登记撤掉，使活动连接脱离 Shutdown 的
// 排水等待。
func (engine *Engine) handleHTTPGuest(gen *generation, port int, guest *transport.Conn) {
	defer gen.wg.Done()
	gen.track(guest)

	host, path, pending, err := readRequestTarget(guest)
	if err != nil {
		gen.untrack(guest)
		_ = guest.Close()
		return
	}
	proxyName, ok := engine.selectHTTPProxy(port, host, path)
	if !ok {
		_ = writeUnmatchedResponse(guest)
		gen.untrack(guest)
		_ = guest.Close()
		return
	}
	engine.bridgeHTTPGuest(gen, proxyName, guest, pending)
}

// bridgeHTTPGuest 把已判定路由的 HTTP 访客交给配对中心。
//
// 请求首部在解析路由时已从访客连接读走，必须随访客一起交给配对中心保管：配对
// 时它要补写到**通往目标的连接**（写回访客会让目标收到空请求、访客收到自己请求
// 的回声），未配对时它随访客一起暂存等后续工作连接。
//
// 访客的登记已由 handleHTTPGuest 完成（它 defer 了 untrack），本函数不再重复
// track：桥接启动时 pairing.start 会为这一对连接重新登记，而调用方的 defer
// untrack 在桥接登记之后执行会把它撤掉——因此这里先把访客从本函数的记账中
// 移出，交由 pairing 接管。
func (engine *Engine) bridgeHTTPGuest(gen *generation, proxyName string, guest *transport.Conn, pending []byte) {
	// 配对与桥接在 broker 锁外完成（见 pairing 的说明）。pending 首部随访客
	// 一并交给配对中心，配对时补写到工作连接方向。
	switch outcome := engine.pairGuest(gen, proxyName, guest, pending); outcome {
	case parkPaired:
		// 已移交桥接，记账由 pairing.start 接管。
	case parkStaged:
		// 访客与首部都留在配对中心，等后续工作连接到达时配对。
		// 配对中心会在配对或关闭时接管访客的记账，因此这里同样撤掉调用方的登记。
		gen.untrack(guest)
	case parkRejected:
		gen.untrack(guest)
		_ = guest.Close()
	case parkCapacityFull:
		// 容量拒绝必须可见：否则"访客连不上"会被当成客户端问题，而真实原因是
		// 服务端到达了承接上限。
		gen.untrack(guest)
		_ = guest.Close()
		engine.log().Warn("该代理的等待访客已达上限，拒绝新访客",
			"proxy", proxyName, "累计拒绝", engine.activeBroker().RejectedGuests())
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

// headerEndCRLF 与 headerEndLF 是首部块结束的两种形态。
const (
	headerEndCRLF = "\r\n"
	headerEndLF   = "\n"
)

// unmatchedBody 是路由未命中时返回的响应体。
//
// 只说明「无匹配路由」，不列出已配置的主机或路径：内部路由表不得经响应回显
// （规格 §3.5）。
const unmatchedBody = "未找到匹配该主机与路径的路由"

// unmatchedResponse 是路由未命中时返回的完整响应。
//
// Content-Length 由 unmatchedBody 的**字节数**在包初始化时算出，不写字面量：
// 手写的长度会随正文改动而漂移，而声明值与实际不符会让合规客户端截断正文
// （甚至切在多字节字符中间），产出非法 HTTP 消息。len 对 string 即为字节数，
// 正是 Content-Length 要求的语义。
var unmatchedResponse = "HTTP/1.1 404 Not Found\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Content-Length: " + strconv.Itoa(len(unmatchedBody)) + "\r\n" +
	"Connection: close\r\n\r\n" +
	unmatchedBody

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
// readRequestTarget 读取请求行与首部，返回路由所需的主机与路径以及**已消费的
// 原始字节**（供后续补写给目标），不读取也不丢弃首部之后的任何字节。
//
// 逐字节读取而非用 bufio：bufio 会预读整块，而预读到的字节既不属于 pending、
// 也无法再被后续的桥接读回——带正文的请求会因此丢掉正文（目标收到
// Content-Length 却拿不到数据）。逐字节读取保证"读到的就是消费掉的"。
// 首部通常仅数百字节且每连接只读一次，系统调用开销可接受。
//
// 上限是真实生效的：累计字节数超过 requestLineLimit 即按非法请求拒绝。
// 用 bufio.ReadString 时该常量只是缓冲大小，超长首部会被无界累积。
func readRequestTarget(guest *transport.Conn) (host, path string, pending []byte, err error) {
	if deadlineErr := guest.SetReadDeadline(time.Now().Add(engineRequestReadTimeout)); deadlineErr != nil {
		return "", "", nil, deadlineErr
	}
	defer func() { _ = guest.SetReadDeadline(time.Time{}) }()

	head, err := readHeaderBlock(guest)
	if err != nil {
		return "", "", nil, err
	}
	lines := strings.Split(head, headerEndLF)
	if len(lines) == 0 {
		return "", "", nil, errors.New("请求为空")
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 2 {
		return "", "", nil, errors.New("请求行缺少方法或目标")
	}
	path = requestPath(fields[1])
	for _, line := range lines[1:] {
		name, value, ok := splitHeader(line)
		if ok && strings.EqualFold(name, "Host") {
			host = normalizeHost(value)
		}
	}
	if host == "" {
		return "", "", nil, errors.New("请求缺少 Host 首部")
	}
	return host, path, []byte(head), nil
}

// readHeaderBlock 逐字节读入首部块，直到空行；返回的字节原样包含行尾。
//
// 不预读是刻意的：首部之后可能紧跟请求正文，任何预读都会让那部分字节既不在
// 返回值里、也无法再被后续读取。上限判定基于实际累计字节数。
func readHeaderBlock(guest *transport.Conn) (string, error) {
	var block []byte
	buffer := make([]byte, 1)
	for len(block) <= requestLineLimit {
		if _, err := io.ReadFull(guest, buffer); err != nil {
			return "", err
		}
		block = append(block, buffer[0])
		if headerBlockComplete(block) {
			return string(block), nil
		}
	}
	return "", fmt.Errorf("请求首部超过 %d 字节上限", requestLineLimit)
}

// headerBlockComplete 判断已读字节是否构成完整的首部块。
//
// 判据是**空行**而不只是行尾：单看 `\n` 会让第一行就被误判成结束。三种行尾
// 组合都接受（CRLFCRLF、LFLF、以及混用），与既有解析保持同一宽容度。
func headerBlockComplete(block []byte) bool {
	length := len(block)
	if length >= 4 &&
		block[length-4] == '\r' && block[length-3] == '\n' &&
		block[length-2] == '\r' && block[length-1] == '\n' {
		return true
	}
	if length >= 2 && block[length-2] == '\n' && block[length-1] == '\n' {
		return true
	}
	if length >= 3 && block[length-3] == '\n' && block[length-2] == '\r' && block[length-1] == '\n' {
		return true
	}
	if length >= 3 && block[length-3] == '\r' && block[length-2] == '\n' && block[length-1] == '\n' {
		return true
	}
	return false
}

// normalizeHost 从 Host 首部取出主机名部分。
//
// HTTP/1.1 客户端在非默认端口上会把 Host 写成 `host:port`——这是标准行为而非
// 异常输入。路由表里配置的是裸主机名，若不做这一步，所有带端口的真实请求都会
// 被判为未匹配。IPv6 字面量形如 `[::1]:8080`，需要连同方括号一起保留。
func normalizeHost(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	// IPv6 字面量：方括号内的内容才是主机，端口在括号之后。
	if strings.HasPrefix(trimmed, "[") {
		if end := strings.Index(trimmed, "]"); end >= 0 {
			return trimmed[:end+1]
		}
		return trimmed
	}
	if colon := strings.LastIndex(trimmed, ":"); colon >= 0 {
		return trimmed[:colon]
	}
	return trimmed
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
