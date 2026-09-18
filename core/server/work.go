package server

import (
	"context"
	"sync"

	"github.com/wcpe/jrp/core/internal/transport"
)

// workBroker 按代理名管理访客与工作连接的双向暂存配对。
//
// 配对语义：访客与工作连接到达顺序不确定，任一方先到都暂存，另一方到达时
// 立即配对桥接。待命工作连接的暂存由传输层的 StagedWorkConns 承载，空闲上限
// 超出时自动回收最早的一条。
// 桥接纳入 Engine 的 WaitGroup 管理，Shutdown 时按排水上限等待；关闭后暂存
// 连接全部关闭，不留悬挂 goroutine。
type workBroker struct {
	mu     sync.Mutex
	guests map[string][]*transport.Conn
	works  *transport.StagedWorkConns
	closed bool
}

// newWorkBroker 建立空的配对中心。
//
// idleLimit 是单个代理允许暂存的待命工作连接上限，来自配置快照。
func newWorkBroker(idleLimit int) *workBroker {
	return &workBroker{
		guests: make(map[string][]*transport.Conn),
		works:  transport.NewStagedWorkConns(idleLimit),
	}
}

// parkGuest 暂存一个访客；若已有待命工作连接，立即配对并返回配对结果。
//
// 返回的布尔值表示是否完成配对。未配对时访客已暂存，调用方不得关闭它；
// 已配对时访客已移交给桥接，调用方同样不得再操作。
// 配对成功后桥接纳入 Engine 的 WaitGroup，由 Shutdown 按排水上限等待。
func (broker *workBroker) parkGuest(
	proxyName string,
	guest *transport.Conn,
	track func(*transport.Conn),
	add func(int),
) bool {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.closed {
		return false
	}
	if work := broker.works.Take(proxyName); work != nil {
		broker.startBridge(guest, work, track, add)
		return true
	}
	broker.guests[proxyName] = append(broker.guests[proxyName], guest)
	return false
}

// park 暂存一条待命工作连接；若已有等待访客，立即配对并返回真。
func (broker *workBroker) park(
	proxyName string,
	work *transport.Conn,
	track func(*transport.Conn),
	add func(int),
) bool {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.closed {
		return false
	}
	if len(broker.guests[proxyName]) > 0 {
		guest := broker.guests[proxyName][0]
		broker.guests[proxyName] = broker.guests[proxyName][1:]
		if len(broker.guests[proxyName]) == 0 {
			delete(broker.guests, proxyName)
		}
		broker.startBridge(guest, work, track, add)
		return true
	}
	broker.works.Push(proxyName, work)
	return true
}

// startBridge 启动一对已配对连接的双向转发。
//
// 转发结束只取决于任一端自然关闭：这里不使用可取消上下文，避免与 Shutdown
// 的排水上限互相打断——排水由 Engine 统一按上限等待，超限才强制关闭。
func (broker *workBroker) startBridge(
	guest, work *transport.Conn,
	track func(*transport.Conn),
	add func(int),
) {
	track(guest)
	track(work)
	add(1)
	go func() {
		defer add(-1)
		defer untrackBoth(track, guest, work)
		transport.Bridge(context.Background(), guest, work)
	}()
}

// untrackBoth 从活动集合移除一对已桥接的连接。
func untrackBoth(untrack func(*transport.Conn), guest, work *transport.Conn) {
	untrack(guest)
	untrack(work)
}

// takeStaged 取出一条指定代理的待命工作连接；队列为空时返回 nil。
//
// UDP 会话需要一条独占的工作连接，因此直接索取而不经过访客配对：取不到即
// 拒绝新会话并计数，绝不无限等待（规格 §3.5）。
func (broker *workBroker) takeStaged(proxyName string) *transport.Conn {
	return broker.works.Take(proxyName)
}

// closeStaged 关闭尚未配对的暂存连接并拒绝后续配对。
//
// 已配对并进入桥接的连接不在此处理：它们承载活动流，由 Engine 按排水上限
// 等待自然结束。
func (broker *workBroker) closeStaged() {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.closed {
		return
	}
	broker.closed = true
	for _, queue := range broker.guests {
		for _, guest := range queue {
			_ = guest.Close()
		}
	}
	broker.guests = make(map[string][]*transport.Conn)
	_ = broker.works.Close()
}

// workConnRequest 是客户端向服务端声明新建工作连接的载荷。
//
// Target 承载该工作连接最终转发到的本地目标地址，供服务端判定目标是否在该
// 客户端被允许的地址集合内（FR-06a §3.3）。
type workConnRequest struct {
	RunID  string `json:"run_id"`
	Proxy  string `json:"proxy_name"`
	Target string `json:"target_addr"`
}
