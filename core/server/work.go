package server

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/wcpe/jrp/core/internal/transport"
)

// maxPendingGuest 是单代理等待配对的访客连接数上限。
//
// 取值要覆盖"并发等待用户数"，而不只是瞬时竞态：工作连接池的默认上限是 1
// （DefaultWorkConnPoolSize），每条桥接在整个连接生命周期内独占一个槽位，
// 因此同一代理上第 2 个及以后的在线用户必然排队。此前按"队列长度只是瞬时竞态
// 排队数"取 16，实际会在 17 个并发用户时开始拒绝——那是代理服务的正常负载，
// 不是异常。
//
// 取 64：足以覆盖默认池上限下的常见并发等待，同时对连接洪水保持有界。
// 不开放为配置项——它是内部的连接接纳上限而非宿主调优的配额，开放只会增加
// 一个可能被误设的旋钮（误设为 0 或 1 会让正常配对也开始失败）。
// 也刻意不复用 IdleWorkConnLimit：两者语义相反——待命工作连接由客户端循环补充，
// 回收最旧的无损失；访客是真实用户连接，拒绝有损。
const maxPendingGuest = 64

// maxPairRetries 是单次访客配对允许跳过失效工作连接的最大次数。
//
// 待命池有上限（默认 2，上界 16），且每次重试都会关闭一条失效连接并把它移出
// 池，因此正常情况下的重试次数远小于池上限。取值显著高于池上界仅为防御：
// 将来若引入新的失败模式，这里能保证循环有界。
const maxPairRetries = 32

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
	// parkRejected 表示引擎已停止，不再接纳任何连接，调用方必须关闭它。
	parkRejected
	// parkCapacityFull 表示该代理的暂存队列已达上限，调用方必须关闭该连接。
	//
	// 与 parkRejected 分开是为了让拒绝原因可被记录：容量拒绝是运维需要看见的
	// 事件（它意味着并发等待用户超过了承接能力），而引擎停止是正常终止。
	parkCapacityFull
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
	guests map[string][]stagedGuest
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
		guests: make(map[string][]stagedGuest),
		works:  transport.NewStagedWorkConns(idleLimit),
	}
}

// stagedGuest 是队列中等待配对的一条访客连接。
//
// pending 是该访客已被读走、但尚未送达目标的字节（HTTP 入口解析路由时读走的
// 请求首部）。它必须在配对时补写到工作连接方向——丢掉它会让目标收到一个被
// 截断的请求，而写回访客则会让目标收到空请求。
type stagedGuest struct {
	conn    *transport.Conn
	pending []byte
}

// pairing 是一对等待桥接的连接。
//
// 配对决策由 broker 在自身锁内做出，但登记活动连接要取 Engine 的锁。若在持
// broker 锁时去取 Engine 锁，就与 Shutdown 的锁序（先 Engine 后 broker）构成
// ABBA 死锁。因此 broker 只把配对结果交回调用方，由调用方在锁外完成登记与启动。
type pairing struct {
	guest *transport.Conn
	work  *transport.Conn
	// pending 是配对前需先写入工作连接的字节（见 stagedGuest）。
	pending []byte
}

// start 在调用方上下文中登记并启动一对已配对的连接。
//
// 必须在 broker 锁之外调用：track 与 add 会取 Engine 的锁。
//
// 返回假表示这条工作连接已经不可用：它已被关闭，而**访客保持原样未被服务**，
// 调用方应换用池中下一条工作连接重试。跨网络环境下待命连接会被 NAT 或中间
// 设备静默回收，池中无法预先感知，只有在配对这一刻才会暴露——实测跨 NAT 时
// 成功率仅 7%，而回环下为 93%。此前这里直接关闭访客，把连接失效的代价转嫁
// 给了真实用户。
func (pair pairing) start(track func(*transport.Conn), add func(int)) bool {
	if !workConnAlive(pair.work) {
		_ = pair.work.Close()
		return false
	}
	if len(pair.pending) > 0 {
		if _, err := pair.work.Write(pair.pending); err != nil {
			// 探测之后到写入之间的窗口内失效：同样只丢弃工作连接。
			_ = pair.work.Close()
			return false
		}
	}
	track(pair.guest)
	track(pair.work)
	add(1)
	go func() {
		defer add(-1)
		defer untrackBoth(track, pair.guest, pair.work)
		transport.Bridge(context.Background(), pair.guest, pair.work)
	}()
	return true
}

// workConnAlive 探测一条待命工作连接是否仍然可用。
//
// 用零超时读：连接已被对端关闭或经中间设备回收时立即返回 EOF 或其他错误，
// 连接健康时返回超时错误。这不是多余的谨慎——池里的连接可能已经死了很久，
// 而服务端没有任何其他途径知道。
//
// 配对前的待命工作连接不携带业务数据：客户端只在声明归属时写过一次，服务端
// 读取声明后才把它入池，此后双方都在等配对。因此这次读不会吞掉业务字节。
// 万一真读到字节（协议被扩展出提前发送语义时），按"可用"处理并交由桥接转发，
// 宁可丢这一次探测的字节也不误判为失效而掐断连接。
func workConnAlive(conn *transport.Conn) bool {
	if err := conn.SetReadDeadline(time.Now()); err != nil {
		return false
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	probe := make([]byte, 1)
	read, err := conn.Read(probe)
	if read > 0 || err == nil {
		return true
	}
	return errors.Is(err, os.ErrDeadlineExceeded)
}

// parkGuest 暂存一个访客；若已有待命工作连接，立即配对。
//
// 返回 parkPaired 时访客已移交桥接、调用方不得再操作；返回 parkStaged 时访客
// 已暂存、调用方不得关闭；返回 parkRejected 时连接未被接纳，调用方必须关闭它。
//
// 达到暂存上限时拒绝新访客而不是回收最旧的：访客是真实用户的连接，回收等于
// 静默掐断一个正在等待的用户，且队列正常长度是瞬时竞态排队数而非并发用户数，
// 达到上限本身说明出现了异常。已有访客不受影响（与 UDP 会话上限同一口径）。
//
// 配对成功时不在本函数内启动桥接：那需要在持 broker 锁时取 Engine 锁，会与
// Shutdown 构成死锁。调用方须用返回的 pairing 在锁外调用 start。
func (broker *workBroker) parkGuest(proxyName string, guest *transport.Conn, pending []byte) (parkOutcome, pairing) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.closed {
		return parkRejected, pairing{}
	}
	if work := broker.works.Take(proxyName); work != nil {
		return parkPaired, pairing{guest: guest, work: work, pending: pending}
	}
	if len(broker.guests[proxyName]) >= maxPendingGuest {
		broker.rejectedGuests++
		return parkCapacityFull, pairing{}
	}
	broker.guests[proxyName] = append(broker.guests[proxyName], stagedGuest{conn: guest, pending: pending})
	return parkStaged, pairing{}
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

// dropGuests 关闭并清空指定代理的全部暂存访客，返回被关闭的连接。
//
// 用于"该代理的工作连接已被拒绝"的场景：目标地址越权时服务端关闭了工作连接，
// 而等待配对的访客仍在队列里等一个永远不会到来的连接。不清理它们，访客会被
// 永久悬挂（对端一直等待，直到自身超时或 Shutdown），而服务端已经知道这次
// 配对不可能成功。
//
// 只清理指定代理：其他代理的配对不受影响。
//
// 返回连接切片而不是条数：这些连接在 Engine 的活动集合里各有一条记账，调用方
// 必须在**锁外**逐个 untrack，否则已关闭的连接会永久留在记账表中（引擎长跑时
// 单调增长）。不在此处直接回调 untrack 是因为那会取 Engine 锁，与 Shutdown 的
// 锁序构成 ABBA（见 pairing 的说明）。
func (broker *workBroker) dropGuests(proxyName string) []*transport.Conn {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	queue := broker.guests[proxyName]
	if len(queue) == 0 {
		return nil
	}
	delete(broker.guests, proxyName)
	dropped := make([]*transport.Conn, 0, len(queue))
	for _, staged := range queue {
		_ = staged.conn.Close()
		dropped = append(dropped, staged.conn)
	}
	return dropped
}

// park 暂存一条待命工作连接；若已有等待访客，立即返回待桥接的一对。
//
// 返回值与 parkGuest 同构：paired 为真时调用方须在锁外 start 该配对并在完成后
// 决定连接归属；为假时表示已暂存或已被拒绝（accepted 区分二者）。
func (broker *workBroker) park(proxyName string, work *transport.Conn) (accepted bool, paired *pairing) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.closed {
		return false, nil
	}
	if len(broker.guests[proxyName]) > 0 {
		staged := broker.guests[proxyName][0]
		broker.guests[proxyName] = broker.guests[proxyName][1:]
		if len(broker.guests[proxyName]) == 0 {
			delete(broker.guests, proxyName)
		}
		return true, &pairing{guest: staged.conn, work: work, pending: staged.pending}
	}
	broker.works.Push(proxyName, work)
	return true, nil
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
		for _, staged := range queue {
			_ = staged.conn.Close()
		}
	}
	broker.guests = make(map[string][]stagedGuest)
	_ = broker.works.Close()
}

// workConnRequest 是客户端向服务端声明新建工作连接的载荷。
//
// Target 承载该工作连接最终转发到的本地目标地址，供服务端判定目标是否在该
// 客户端被允许的地址集合内（FR-06a §3.3）。
//
// ClientID 与 Token 是工作连接的鉴权材料：工作连接是与控制连接平行的独立连接，
// 服务端无从由连接本身判断其归属，必须由声明携带凭据，否则任何能连上控制端口的
// 对端都可以声明任意代理名（见 PROTOCOL §7 第 2、3 步：声明鉴权材料，服务端验证
// 工作连接属于当前控制会话）。
type workConnRequest struct {
	ClientID string `json:"client_id"`
	Token    string `json:"token"`
	RunID    string `json:"run_id"`
	Proxy    string `json:"proxy_name"`
	Target   string `json:"target_addr"`
}
