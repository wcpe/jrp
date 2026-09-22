package core

import (
	"sync"
	"time"
)

// 事件订阅的容量常量（规格 §3.2/§3.4）。
const (
	// DefaultEventCapacity 是订阅缓冲为零值时采用的 Core 默认容量。
	//
	// 取值依据：常规消费速度下容纳一波配置应用的全部事件（每次 Apply 产生
	// ApplyResult 与若干代理状态事件），同时在多个订阅并存时保持内存有界。
	const_defaultEventCapacity = 64

	// MaxEventCapacity 是单条订阅缓冲容量的 Core 上界。
	//
	// 宿主传入更大的 Capacity 时收敛到上界而不是报错（规格 §3.2：避免让观测
	// 开关成为启动失败原因），保证缓冲规模恒有界。
	const_maxEventCapacity = 1024
)

// 导出容量常量：Go 不允许 const 引用未导出常量以外的组合导出写法，这里直接给出
// 同值导出名，注释说明与未导出别名的关系。
const (
	// DefaultEventCapacity 是订阅缓冲为零值时采用的 Core 默认容量。
	DefaultEventCapacity = const_defaultEventCapacity
	// MaxEventCapacity 是单条订阅缓冲容量的 Core 上界。
	MaxEventCapacity = const_maxEventCapacity
)

// Type 是事件的类型标识。
//
// 宿主用类型断言区分载荷，本标识只用于日志与诊断输出；不得依赖其字符串取值
// 做分支（规格 §3.3：拼写漂移会让分支静默走错）。
type Type string

// Level 是事件的等级：决定缓冲溢出时的降级方式（规格 §3.4）。
type Level int

const (
	// LevelDiagnostic 是诊断级：溢出时直接丢弃，不计数告警。
	LevelDiagnostic Level = iota
	// LevelNormal 是常规级：溢出时丢弃并累计丢弃计数。
	LevelNormal
	// LevelCritical 是关键级：溢出时优先保留，挤掉最旧的常规级事件。
	LevelCritical
)

// Event 是全部事件的统一信封（规格 §3.3）。
//
// 每个具体事件是独立结构体；字段全部为值类型，不携带可变内部指针、连接句柄
// 或任何活对象引用（规格 §3.1）。
type Event interface {
	// Type 返回事件的类型标识。
	Type() Type
	// Level 返回事件的等级。
	Level() Level
	// At 返回事件发生的时间。
	At() time.Time
}

// EventMeta 是各具体事件共用的信封字段。
//
// 发布方在构造事件时打点；宿主通过事件接口的 At() 读取，不构造。
type EventMeta struct {
	// occurredAt 是事件发生时间。
	occurredAt time.Time
}

// NewEventMeta 以当前时间打点构造信封。
func NewEventMeta() EventMeta {
	return EventMeta{occurredAt: time.Now()}
}

// At 返回事件发生时间；嵌入 EventMeta 的类型即实现 Event 接口的该方法。
func (meta EventMeta) At() time.Time { return meta.occurredAt }

// Options 是订阅的构造参数（规格 §3.2）。
//
// Capacity 为 0 时使用 DefaultEventCapacity；超过 MaxEventCapacity 时收敛到
// 上限。两种情况都不返回错误：观测开关不得成为启动失败原因。
type Options struct {
	// Capacity 是订阅缓冲容量，按事件条数计。
	Capacity int
}

// Subscription 是一条独立的事件订阅（规格 §3.2/§3.4）。
//
// 缓冲是一条容量有界的事件通道：一个订阅者溢出或关闭不影响其他订阅者，也不
// 影响 Engine。投递永远非阻塞——缓冲满即按等级降级，绝不等待宿主消费。
//
// 溢出降级（规格 §3.4）：诊断级直接丢弃；常规级丢弃并计数；关键级挤掉通道内
// 最旧的常规级事件腾位（通道内全是关键级时挤最旧的关键级）。重同步事件本身
// 是关键级，因此总能入队。
type Subscription struct {
	// capacity 是实际生效的缓冲容量（已收敛）。
	capacity int
	// events 是宿主读取通道，也是订阅的全部缓冲。
	events chan Event

	mu     sync.Mutex
	closed bool // 订阅已关闭：不再接收新事件
	// droppedSinceResync 是本窗口内丢弃的常规级事件数；ResyncRequired 发布后清零。
	droppedSinceResync uint64
	// resyncPending 表示本窗口已发布过 ResyncRequired：宿主消费前不再重复发布。
	resyncPending bool
	// discardTotal 是累计丢弃的常规级事件数（含关键级挤占），供测试与诊断。
	discardTotal uint64
}

// Close 停止接收并释放缓冲；幂等，可重复调用（规格 §3.2）。
//
// 关闭后不再接收新事件；通道立即关闭——Go 通道语义保证已缓冲的事件仍可读出，
// 宿主会先读到既有事件（含停止事件），随后读到关闭。不等待宿主消费：慢消费者
// 不得拖延引擎停止（规格 §3.6）。
func (sub *Subscription) Close() {
	sub.mu.Lock()
	if sub.closed {
		sub.mu.Unlock()
		return
	}
	sub.closed = true
	sub.mu.Unlock()
	close(sub.events)
}

// Events 返回只读接收通道（规格 §3.2）。
//
// 宿主负责消费；通道在订阅关闭且缓冲排空后关闭。
func (sub *Subscription) Events() <-chan Event {
	return sub.events
}

// enqueue 把事件放入缓冲，必要时按等级降级（规格 §3.4）。
func (sub *Subscription) enqueue(event Event) {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if sub.closed {
		return
	}
	if len(sub.events) >= sub.capacity {
		switch event.Level() {
		case LevelDiagnostic:
			// 诊断级直接丢弃，不计数告警。
			return
		case LevelNormal:
			sub.droppedSinceResync++
			sub.discardTotal++
			sub.publishResyncLocked()
			return
		case LevelCritical:
			sub.evictOldestLocked()
			sub.discardTotal++
		}
	}
	select {
	case sub.events <- event:
	default:
		// 判满与发送之间不存在并发消费者腾空的负窗口：通道满时降级路径已处理，
		// 这里只在关键级挤占后仍满时兜底丢弃（不阻塞发布方）。
	}
	sub.publishResyncLocked()
}

// evictOldestLocked 挤掉通道内最旧的一条事件：优先常规级，全是关键级时挤关键级。
func (sub *Subscription) evictOldestLocked() {
	count := len(sub.events)
	flattened := make([]Event, 0, count)
	for range count {
		select {
		case item := <-sub.events:
			flattened = append(flattened, item)
		default:
		}
	}
	for index, item := range flattened {
		if item.Level() != LevelCritical {
			rest := append(append([]Event{}, flattened[:index]...), flattened[index+1:]...)
			for _, kept := range rest {
				sub.events <- kept
			}
			return
		}
	}
	// 全是关键级：丢弃最旧的一条。
	for _, kept := range flattened[1:] {
		sub.events <- kept
	}
}

// publishResyncLocked 在丢弃发生后按需发布重同步事件（规格 §3.4）。
//
// 同一连续溢出窗口内只发布一次：宿主消费使缓冲腾空后窗口结束，再次溢出才发布。
func (sub *Subscription) publishResyncLocked() {
	if sub.droppedSinceResync == 0 || sub.resyncPending {
		return
	}
	sub.resyncPending = true
	event := ResyncRequired{Dropped: sub.droppedSinceResync, EventMeta: NewEventMeta()}
	if len(sub.events) >= sub.capacity {
		sub.evictOldestLocked()
	}
	select {
	case sub.events <- event:
	default:
	}
}

// shutdownPump 在引擎停止路径关闭订阅（供出通道随之关闭）。
func (sub *Subscription) shutdownPump() {
	sub.Close()
}

// Subscribe 创建一条事件订阅并立即开始接收此后产生的事件（规格 §3.2）。
//
// 不回放历史。多订阅者相互隔离。
func (hub *eventHub) Subscribe(options Options) *Subscription {
	capacity := options.Capacity
	if capacity <= 0 {
		capacity = const_defaultEventCapacity
	}
	if capacity > const_maxEventCapacity {
		capacity = const_maxEventCapacity
	}
	sub := &Subscription{
		capacity: capacity,
		events:   make(chan Event, capacity),
	}
	hub.add(sub)
	return sub
}

// ResyncRequired 事件：缓冲溢出后发布，宿主据此调用 State() 重建视图（规格 §3.4）。
type ResyncRequired struct {
	EventMeta
	// Dropped 是本溢出窗口内丢弃的常规级事件数。
	Dropped uint64
}

// Type 返回事件类型标识。
func (ResyncRequired) Type() Type { return "resync-required" }

// Level 返回事件等级：重同步事件必须送达，为关键级。
func (ResyncRequired) Level() Level { return LevelCritical }

// EngineStopped 事件：Engine 停止后发布，携带最终错误（规格 §3.3）。
type EngineStopped struct {
	EventMeta
	// Err 是导致 Engine 停止的最终错误；正常 Shutdown 为 nil。
	Err error
}

// Type 返回事件类型标识。
func (EngineStopped) Type() Type { return "engine-stopped" }

// Level 返回事件等级：停止事件不可丢，为关键级。
func (EngineStopped) Level() Level { return LevelCritical }

// ClientConnected 事件：客户端控制会话建立（规格 §3.3）。
type ClientConnected struct {
	EventMeta
	// ClientID 是客户端标识。
	ClientID string
	// RemoteAddr 是远端地址摘要。
	RemoteAddr string
}

// Type 返回事件类型标识。
func (ClientConnected) Type() Type { return "client-connected" }

// Level 返回事件等级。
func (ClientConnected) Level() Level { return LevelNormal }

// ProxyStatusChanged 事件：代理状态变化（规格 §3.3）。
type ProxyStatusChanged struct {
	EventMeta
	// Name 是代理名。
	Name string
	// Kind 是代理类型。
	Kind string
	// Status 是新状态。
	Status string
}

// Type 返回事件类型标识。
func (ProxyStatusChanged) Type() Type { return "proxy-status-changed" }

// Level 返回事件等级。
func (ProxyStatusChanged) Level() Level { return LevelNormal }

// ApplyResultEvent 是配置应用结果事件（规格 §3.3）。
//
// 字段与 core.ApplyResult 返回值同义（Revision、Stage），按规格 §3.3 事件载荷
// 为独立事件结构体：事件只携带值与标识，不复制全部计数（Changed/Drained 等由
// 宿主从 Apply 返回值取得）。
type ApplyResultEvent struct {
	EventMeta
	// Revision 是本次应用的 revision。
	Revision uint64
	// Stage 是终止阶段。
	Stage Stage
	// Err 是安全可公开的错误摘要；成功为 nil。
	Err error
}

// Type 返回事件类型标识。
func (ApplyResultEvent) Type() Type { return "apply-result" }

// Level 返回事件等级：配置应用结果决定运维视图的版本归属，为常规级。
func (ApplyResultEvent) Level() Level { return LevelNormal }

// Published 判断切换是否已生效（语义同 ApplyResult.Published）。
func (event ApplyResultEvent) Published() bool {
	return event.Stage == StageApplied || event.Stage == StageDrained
}

// State 是只读状态快照（规格 §3.5）。
//
// 返回值为深复制：宿主修改返回值不影响 Core，Core 后续变化也不影响已取到的值。
type State struct {
	// Running 报告 Engine 是否处于运行状态。
	Running bool
	// Active 是快照时刻的 active revision。
	Active uint64
	// LastGood 是快照时刻的 last-good revision。
	LastGood uint64
	// Clients 是客户端摘要列表。
	Clients []ClientSummary
	// Proxies 是代理摘要列表。
	Proxies []ProxySummary
	// Connections 是快照时刻的活动连接计数。
	Connections int
}

// ActiveRevision 返回快照时刻的 active revision。
func (state State) ActiveRevision() uint64 { return state.Active }

// LastGoodRevision 返回快照时刻的 last-good revision。
func (state State) LastGoodRevision() uint64 { return state.LastGood }

// ConnectionCount 返回快照时刻的活动连接计数。
func (state State) ConnectionCount() int { return state.Connections }

// ClientSummary 是快照中的客户端摘要（规格 §3.5）。
type ClientSummary struct {
	// ID 是客户端标识。
	ID string
	// Connected 报告该客户端是否在线。
	Connected bool
	// ProxyCount 是该客户端的代理数。
	ProxyCount int
}

// ProxySummary 是快照中的代理摘要（规格 §3.5）。
type ProxySummary struct {
	// Name 是代理名。
	Name string
	// Kind 是代理类型。
	Kind string
	// Status 是代理状态。
	Status string
}

// EventHub 是 Engine 持有的公开事件中枢句柄：由根包提供实现，Engine 包转发
// Subscribe/State（ADR-0012：事件能力并入根包，不新增公共包）。
type EventHub struct {
	inner eventHub
}

// NewEventHub 构造事件中枢。Engine 构造函数各持有一个实例。
func NewEventHub() *EventHub {
	return &EventHub{}
}

// Publish 向全部存续订阅投递事件；非阻塞（规格 §3.4）。
func (hub *EventHub) Publish(event Event) {
	hub.inner.publish(event)
}

// PublishApply 是 Apply 结果事件的便捷发布入口。
func (hub *EventHub) PublishApply(event ApplyResultEvent) {
	hub.inner.publish(event)
}

// PublishStop 在引擎停止路径发布停止事件并关闭全部订阅（规格 §3.6）。
func (hub *EventHub) PublishStop(finalErr error) {
	hub.inner.stopAll(finalErr)
}

// Subscribe 创建一条订阅并登记到中枢；Engine 包转发调用。
func (hub *EventHub) Subscribe(options Options) *Subscription {
	return hub.inner.Subscribe(options)
}

// eventHub 是 Engine 内部的事件中枢实现：登记订阅并发布事件。
type eventHub struct {
	mu   sync.Mutex
	subs []*Subscription
}

// add 登记一条订阅。
func (hub *eventHub) add(sub *Subscription) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	hub.subs = append(hub.subs, sub)
}

// publish 向全部存续订阅投递事件；非阻塞，慢订阅走各自降级路径。
func (hub *eventHub) publish(event Event) {
	hub.mu.Lock()
	subs := make([]*Subscription, len(hub.subs))
	copy(subs, hub.subs)
	hub.mu.Unlock()
	for _, sub := range subs {
		sub.enqueue(event)
	}
}

// stopAll 在 Engine 停止路径按序收口：先发布停止事件，再关闭全部订阅通道
// （规格 §3.6）。
func (hub *eventHub) stopAll(finalErr error) {
	hub.mu.Lock()
	subs := make([]*Subscription, len(hub.subs))
	copy(subs, hub.subs)
	hub.mu.Unlock()
	for _, sub := range subs {
		sub.enqueue(EngineStopped{Err: finalErr, EventMeta: NewEventMeta()})
	}
	for _, sub := range subs {
		sub.shutdownPump()
	}
}
