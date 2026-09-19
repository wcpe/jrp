package server

import (
	"context"
	"sync"

	"github.com/wcpe/jrp/core/internal/transport"
)

// maxPendingGuest 是单代理等待配对的访客连接数上限。
//
// 访客只在"工作连接恰好还没到"时排队：一旦工作连接到达即立刻配对，因此队列
// 长度代表瞬时竞态下的排队数，而不是并发用户数。取 16 是给足余量以吸收
// "工作连接批量到达前的瞬时堆积"，同时对连接洪水保持有界。
//
// 不开放为配置项：它是内部竞态缓冲而非宿主需要调优的资源配额，开放只会增加
// 一个可能被误设的旋钮（误设为 0 或 1 会让正常配对也开始失败）。
// 也刻意不复用 IdleWorkConnLimit：两者语义相反——待命工作连接由客户端循环补充，
// 回收最旧的无损失；访客是真实用户连接，拒绝有损。
const maxPendingGuest = 16

// parkOutcome 是访客暂存的结果。
//
// 用三态而不是布尔值：布尔值无法区分"已暂存"与"未接纳"两种情况，而调用方对
// 这两种情况的处置相反——已暂存的连接必须留给配对中心、不得关闭；未接纳的连接
// 必须由调用方关闭。此前靠调用方额外查询引擎停止标记来区分，一旦新增第三种
// 不接纳的原因（如超出暂存上限），那个判断就会漏掉新情况并造成连接泄漏。
type parkOutcome int

const (
	// parkPaired 表示已配对并移交桥接，调用方不得再操作该连接。
	parkPaired parkOutcome = iota
	// parkStaged 表示已暂存，调用方不得关闭该连接。
	parkStaged
	// parkRejected 表示未被接纳（引擎已停止或暂存已达上限），调用方必须关闭该连接。
	parkRejected
)

// workBroker 按代理名管理访客与工作连接的双向暂存配对。
//
// 配对语义：访客与工作连接到达顺序不确定，任一方先到都暂存，另一方到达时
// 立即配对桥接。待命工作连接的暂存由传输层的 StagedWorkConns 承载，空闲上限
// 超出时自动回收最早的一条；访客暂存有数量上限，超出时拒绝新访客并计数。
// 桥接纳入 Engine 的 WaitGroup 管理，Shutdown 时按排水上限等待；关闭后暂存
// 连接全部关闭，不留悬挂 goroutine。
type workBroker struct {
	mu     sync.Mutex
	guests map[string][]*transport.Conn
	works  *transport.StagedWorkConns
	closed bool

	// rejectedGuests 是累计因超出暂存上限被拒绝的访客数。
	rejectedGuests int64
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

// parkGuest 暂存一个访客；若已有待命工作连接，立即配对。
//
// 返回 parkPaired 时访客已移交桥接、调用方不得再操作；返回 parkStaged 时访客
// 已暂存、调用方不得关闭；返回 parkRejected 时连接未被接纳，调用方必须关闭它。
//
// 达到暂存上限时拒绝新访客而不是回收最旧的：访客是真实用户的连接，回收等于
// 静默掐断一个正在等待的用户，且队列正常长度是瞬时竞态排队数而非并发用户数，
// 达到上限本身说明出现了异常。已有访客不受影响（与 UDP 会话上限同一口径）。
// 配对成功后桥接纳入 Engine 的 WaitGroup，由 Shutdown 按排水上限等待。
func (broker *workBroker) parkGuest(
	proxyName string,
	guest *transport.Conn,
	track func(*transport.Conn),
	add func(int),
) parkOutcome {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.closed {
		return parkRejected
	}
	if work := broker.works.Take(proxyName); work != nil {
		broker.startBridge(guest, work, track, add)
		return parkPaired
	}
	if len(broker.guests[proxyName]) >= maxPendingGuest {
		broker.rejectedGuests++
		return parkRejected
	}
	broker.guests[proxyName] = append(broker.guests[proxyName], guest)
	return parkStaged
}

// RejectedGuests 返回累计因超出暂存上限被拒绝的访客数。
//
// 该计数是运维可观测入口：拒绝必须是可见事件，否则"访客连不上"会被误判为
// 客户端问题而无法定位到服务端的容量拒绝。
func (broker *workBroker) RejectedGuests() int64 {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return broker.rejectedGuests
}

// dropGuests 关闭并清空指定代理的全部暂存访客，返回被关闭的连接数。
//
// 用于"该代理的工作连接已被拒绝"的场景：目标地址越权时服务端关闭了工作连接，
// 而等待配对的访客仍在队列里等一个永远不会到来的连接。不清理它们，访客会被
// 永久悬挂（对端一直等待，直到自身超时或 Shutdown），而服务端已经知道这次
// 配对不可能成功。
//
// 只清理指定代理：其他代理的配对不受影响。
func (broker *workBroker) dropGuests(proxyName string) int {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	queue := broker.guests[proxyName]
	if len(queue) == 0 {
		return 0
	}
	delete(broker.guests, proxyName)
	for _, guest := range queue {
		_ = guest.Close()
	}
	return len(queue)
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
