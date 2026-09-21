package proxy

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wcpe/jrp/core/internal/transport"
	"github.com/wcpe/jrp/core/internal/wire"
)

// udpDatagramFrameLimit 是承载单个数据报的 wire 帧载荷上限。
//
// 数据报原文经 base64 编码后膨胀约 4/3，取值须容纳最大数据报上限（65507 字节）
// 编码后的长度，因此按 90 KiB 留出余量而不是复用默认帧上限。
const udpDatagramFrameLimit = 90 * 1024

// inboundQueueSize 是单个会话待发送数据报的队列深度。
//
// 会话已占用一条工作连接，队列只吸收瞬时突发；超出即丢弃并计数，绝不无界堆积。
const inboundQueueSize = 16

// udpDatagram 是数据报在 wire 帧内的承载形态。
//
// 数据报原文可能不是合法 JSON，因此经 base64 编码后放入对象字段，保证帧载荷
// 仍是 wire v1 要求的 JSON 对象。
type udpDatagram struct {
	Data string `json:"data"`
}

// UDPSessionConfig 是建立一条 UDP 会话所需的全部入参。
type UDPSessionConfig struct {
	// Peer 是会话的对端地址；同一对端地址的数据报归属同一会话。
	Peer netip.AddrPort
	// Work 是送达客户端侧目标的工作连接。
	Work net.Conn
	// Send 把响应数据报写回对端。
	Send func(peer netip.AddrPort, datagram []byte) error
	// Idle 是会话空闲上限，超时后回收。
	Idle time.Duration
	// MaxDatagram 是单个数据报的字节上限。
	MaxDatagram int
}

// UDPSession 是一条 UDP 会话：按对端地址识别，承载数据报的往返。
//
// 无连接语义下的「关闭」等价于「空闲回收」，因此本类型不提供面向流的半关闭：
// 上层不得用 TCP 的关闭语义推断 UDP 状态（规格 §3.4）。
// 会话在空闲超限时结束，结束后不再接受数据报。
type UDPSession struct {
	peer        netip.AddrPort
	work        net.Conn
	send        func(peer netip.AddrPort, datagram []byte) error
	idle        time.Duration
	maxDatagram int

	inbound chan []byte
	done    chan struct{}
	close   sync.Once
	// cancel 在构造时创建：Serve 与 Close 可能并发执行，运行期写入再读取会
	// 产生数据竞争，因此取消函数必须是构造期确定的不可变字段。
	cancel context.CancelFunc

	dropped atomic.Int64
}

// NewUDPSession 建立一条尚未开始服务的会话。
func NewUDPSession(config UDPSessionConfig) *UDPSession {
	// 取消上下文在构造期建立：Serve 持有它直到会话结束，Close 可随时取消，
	// 两者并发安全。
	_, cancel := context.WithCancel(context.Background())
	return &UDPSession{
		peer:        config.Peer,
		work:        config.Work,
		send:        config.Send,
		idle:        config.Idle,
		maxDatagram: config.MaxDatagram,
		inbound:     make(chan []byte, inboundQueueSize),
		done:        make(chan struct{}),
		cancel:      cancel,
	}
}

// Done 在会话结束后关闭。
func (session *UDPSession) Done() <-chan struct{} {
	return session.done
}

// Dropped 返回因超限或队列已满而丢弃的数据报计数。
func (session *UDPSession) Dropped() int64 {
	return session.dropped.Load()
}

// Deliver 入队一个待发往客户端的数据报。
//
// 返回假表示未接受：会话已结束、数据报超限或队列已满；后两者计入丢弃计数。
// 超限判定先于任何分配，绝不按声明长度无界分配（规格 §3.4）。
//
// 入队前必须复制：入口以固定缓冲反复读取，调用方传进来的切片在下一次读取时
// 会被就地覆写。若不复制，队列里尚未处理的数据报内容会被后续数据报（可能是
// 另一个对端的）覆盖——既是数据损坏，也是跨用户的数据串扰。
// 复制只在上限校验通过后进行，超限的数据报直接丢弃、不做无谓分配。
func (session *UDPSession) Deliver(datagram []byte) bool {
	select {
	case <-session.done:
		return false
	default:
	}
	if len(datagram) > session.maxDatagram {
		session.dropped.Add(1)
		return false
	}
	owned := make([]byte, len(datagram))
	copy(owned, datagram)
	select {
	case session.inbound <- owned:
		return true
	default:
		session.dropped.Add(1)
		return false
	}
}

// Serve 服务会话直到空闲超限、连接结束或上下文取消。
//
// 两个方向并行：对端数据报编码后写入工作连接；工作连接回传的数据报写回原
// 对端。返回前必定关闭工作连接并关闭 Done，不留悬挂 goroutine。
func (session *UDPSession) Serve(ctx context.Context) {
	sessionCtx, stopServe := context.WithCancel(ctx)
	defer stopServe()
	defer func() {
		session.close.Do(func() { close(session.done) })
		_ = session.work.Close()
	}()

	responses := make(chan error, 1)
	go session.readResponses(responses)

	timer := time.NewTimer(session.idle)
	defer timer.Stop()
	for {
		select {
		case datagram := <-session.inbound:
			if err := session.sendDatagram(datagram); err != nil {
				return
			}
			resetTimer(timer, session.idle)
		case err := <-responses:
			if err != nil {
				return
			}
			resetTimer(timer, session.idle)
		case <-timer.C:
			return
		case <-sessionCtx.Done():
			return
		}
	}
}

// readResponses 读取工作连接回传的数据报并写回对端，结束或出错时报告一次。
func (session *UDPSession) readResponses(report chan<- error) {
	reader := wire.NewV1Reader(session.work, udpDatagramFrameLimit)
	for {
		frame, err := reader.ReadFrame()
		if err != nil {
			report <- err
			return
		}
		datagram, decoded := decodeDatagram(frame)
		frame.Release()
		if !decoded {
			report <- errDatagramInvalid
			return
		}
		// 回传方向同样受数据报上限约束：规格要求数据报大小受上限约束、超限丢弃
		// 并计数，而该约束若只作用于入口方向，超过配置上限的数据报仍会被写入
		// UDP 对端（可能触发 EMSGSIZE，或超出对端预期的缓冲区大小），且不被计数。
		if len(datagram) > session.maxDatagram {
			session.dropped.Add(1)
			continue
		}
		if err := session.send(session.peer, datagram); err != nil {
			report <- err
			return
		}
	}
}

// sendDatagram 把一个数据报编码为 wire 帧写入工作连接。
func (session *UDPSession) sendDatagram(datagram []byte) error {
	frame, err := encodeDatagram(datagram)
	if err != nil {
		return err
	}
	return writeDatagramFrame(session.work, frame)
}

// Close 结束会话并释放工作连接；重复调用安全。
func (session *UDPSession) Close() error {
	session.cancel()
	select {
	case <-session.done:
	default:
		session.close.Do(func() { close(session.done) })
	}
	_ = session.work.Close()
	return nil
}

// UDPProxyConfig 是建立一个 UDP 代理入口所需的全部入参。
type UDPProxyConfig struct {
	// Name 是代理名，用于日志与事件。
	Name string
	// Port 是服务端接收数据报的入口。
	Port *transport.PacketListener
	// Idle 是单会话空闲上限。
	Idle time.Duration
	// MaxSessions 是会话数上限。
	MaxSessions int
	// MaxDatagram 是单个数据报的字节上限。
	MaxDatagram int
	// Work 为新建会话提供一条工作连接；返回假表示无法建立。
	Work func() (net.Conn, bool)
}

// UDPProxy 是一个 UDP 代理入口：按对端地址把数据报分发到会话。
//
// 入口独占端口；会话按对端地址识别，达到上限时新对端被拒绝并计数，已有会话
// 不受影响（规格 §3.4）。Close 后入口停止接收，活动会话全部回收。
type UDPProxy struct {
	name        string
	port        *transport.PacketListener
	idle        time.Duration
	maxSessions int
	maxDatagram int
	work        func() (net.Conn, bool)

	mu        sync.Mutex
	sessions  map[netip.AddrPort]*UDPSession
	rejected  int64
	oversized int64
	delivered int64
	closed    bool

	// suspend 表示入口被要求停止接收（见 Suspend）。
	//
	// 与 closed 分开：交接要求停掉本代的接收循环，但活动会话必须继续服务到自然
	// 结束，而 Close 会把会话一并回收。
	suspended atomic.Bool

	// cancel 与会话侧同理：构造期确定，避免 Serve 与 Close 并发时的数据竞争。
	cancel context.CancelFunc
	serve  sync.WaitGroup
}

// NewUDPProxy 建立一个尚未开始服务的 UDP 代理入口。
func NewUDPProxy(config UDPProxyConfig) *UDPProxy {
	_, cancel := context.WithCancel(context.Background())
	return &UDPProxy{
		name:        config.Name,
		port:        config.Port,
		idle:        config.Idle,
		maxSessions: config.MaxSessions,
		maxDatagram: config.MaxDatagram,
		work:        config.Work,
		sessions:    make(map[netip.AddrPort]*UDPSession),
		cancel:      cancel,
	}
}

// Addr 返回入口地址。
func (entry *UDPProxy) Addr() net.Addr {
	return entry.port.Addr()
}

// Sessions 返回当前活动会话数。
func (entry *UDPProxy) Sessions() int {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return len(entry.sessions)
}

// Rejected 返回因会话数达上限而被拒绝对端的次数。
func (entry *UDPProxy) Rejected() int64 {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.rejected
}

// Oversized 返回被丢弃的超限数据报计数。
func (entry *UDPProxy) Oversized() int64 {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.oversized
}

// Delivered 返回已送达会话的数据报计数。
func (entry *UDPProxy) Delivered() int64 {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.delivered
}

// Serve 接收数据报并分发到会话，直到入口关闭、上下文取消或入口被 Suspend。
//
// 每次进入都从"接收中"开始：交接后由新一代调用本方法恢复接收，因此 Suspend 只是
// 停掉**当次**接收循环的信号，不是入口的持久状态。
func (entry *UDPProxy) Serve(ctx context.Context) {
	entry.suspended.Store(false)
	serveCtx, stopServe := context.WithCancel(ctx)
	defer stopServe()

	// 缓冲比上限多一字节：读满即说明数据报超过上限，须丢弃并计数。
	buffer := make([]byte, entry.maxDatagram+1)
	for {
		if err := entry.port.SetReadDeadline(time.Now().Add(readDeadlineStep)); err != nil {
			return
		}
		read, peer, err := entry.port.Read(buffer)
		if err != nil {
			// 读错误分三类：超时（正常轮转）、数据报超限（须丢弃并继续）、
			// 其余（入口已关闭或真正异常）。
			if entry.isClosed() {
				return
			}
			var netErr net.Error
			if isNetError(err, &netErr) && netErr.Timeout() {
				if serveCtx.Err() != nil || entry.suspended.Load() {
					return
				}
				continue
			}
			if isOversizedDatagramError(err) {
				// Windows 的 recvfrom 对大于缓冲的数据报返回 WSAEMSGSIZE，
				// 而 Linux/BSD 会截断并正常返回。不识别这一形态时，单个超限
				// 数据报就会让整个入口永久停止服务——任何能发包的第三方都能
				// 用一个包触发它。
				entry.countOversized()
				continue
			}
			// 其余错误按致命处理：但先确认入口是否仍应运行，避免把一次
			// 瞬时错误当成终止条件。
			return
		}
		if read > entry.maxDatagram {
			entry.countOversized()
			continue
		}
		entry.dispatch(serveCtx, peer, buffer[:read])
	}
}

// Suspend 停止当前接收循环但不回收活动会话。
//
// 入口交接用：套接字要随入口一起归后继代，因此本代的接收循环必须先退出，否则
// 两个循环会争抢同一套接字。不能用 Close 代替：它会把活动会话一并回收，而交接
// 要求已有会话不受影响（它们继续服务到自然结束）。
// 下一次 Serve 会恢复接收，因此本状态只在两次 Serve 之间有效。
func (entry *UDPProxy) Suspend() {
	entry.suspended.Store(true)
}

// isOversizedDatagramError 判断读错误是否表示"数据报超出接收缓冲"。
//
// 这类错误在语义上等同于"数据报超限"，应当丢弃并继续；把它当致命错误会让
// 单个超限包终止整个入口。按错误文本识别是因为该错误码（WSAEMSGSIZE）没有
// 跨平台的标准库常量，按平台分文件又会把同一逻辑分散到多处。
func isOversizedDatagramError(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	// Windows 的 wsarecvfrom 错误文本；Linux/BSD 在截断语义下不产生错误，
	// 因此该分支只在真正会返回错误的平台上命中。
	return strings.Contains(text, "larger than the internal message buffer") ||
		strings.Contains(text, "message too long") ||
		strings.Contains(text, "Message too long")
}

// isClosed 报告入口是否已被关闭。
//
// 用于区分"入口被关闭"与"读操作出错"：前者是正常的终止条件，后者不应被当作
// 继续循环的理由，也不该在入口已关闭后再纠缠错误归类。
func (entry *UDPProxy) isClosed() bool {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.closed
}

// dispatch 把一个数据报交给对应会话，必要时建立新会话。
func (entry *UDPProxy) dispatch(ctx context.Context, peer netip.AddrPort, datagram []byte) {
	entry.mu.Lock()
	if entry.closed {
		entry.mu.Unlock()
		return
	}
	session, ok := entry.sessions[peer]
	if !ok {
		if len(entry.sessions) >= entry.maxSessions {
			entry.rejected++
			entry.mu.Unlock()
			return
		}
		work, ready := entry.work()
		if !ready {
			entry.rejected++
			entry.mu.Unlock()
			return
		}
		session = NewUDPSession(UDPSessionConfig{
			Peer:        peer,
			Work:        work,
			Send:        entry.writeBack,
			Idle:        entry.idle,
			MaxDatagram: entry.maxDatagram,
		})
		entry.sessions[peer] = session
		entry.serve.Add(1)
		go func() {
			defer entry.serve.Done()
			session.Serve(ctx)
			entry.forget(peer, session)
		}()
	}
	entry.mu.Unlock()

	if session.Deliver(datagram) {
		entry.countDelivered()
	}
}

// forget 在会话结束后把它从活动集合中移除。
func (entry *UDPProxy) forget(peer netip.AddrPort, session *UDPSession) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.sessions[peer] == session {
		delete(entry.sessions, peer)
	}
}

// writeBack 把响应数据报写回原对端。
func (entry *UDPProxy) writeBack(peer netip.AddrPort, datagram []byte) error {
	_, err := entry.port.Write(datagram, peer)
	return err
}

func (entry *UDPProxy) countOversized() {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	entry.oversized++
}

func (entry *UDPProxy) countDelivered() {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	entry.delivered++
}

// Close 关闭入口并回收全部会话；重复调用安全。
func (entry *UDPProxy) Close() error {
	entry.mu.Lock()
	if entry.closed {
		entry.mu.Unlock()
		return nil
	}
	entry.closed = true
	entry.cancel()
	sessions := make([]*UDPSession, 0, len(entry.sessions))
	for peer, session := range entry.sessions {
		sessions = append(sessions, session)
		delete(entry.sessions, peer)
	}
	entry.mu.Unlock()

	for _, session := range sessions {
		_ = session.Close()
	}
	entry.serve.Wait()
	if entry.port != nil {
		return entry.port.Release()
	}
	return nil
}

// isNetError 判定错误是否为网络错误，供超时分类使用。
func isNetError(err error, target *net.Error) bool {
	if netErr, ok := err.(net.Error); ok {
		*target = netErr
		return true
	}
	return false
}

// resetTimer 重置定时器到给定时长。
func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

// ServeUDPClient 在客户端侧服务一条 UDP 工作连接。
//
// 数据报从工作连接解出后发往本地 UDP 目标，目标回传的数据报编码后写回工作
// 连接。这是服务端 UDPSession 的对侧：帧格式与上限由 datagram.go 统一定义，
// 两侧不各写一套。
// 两个方向都靠关闭解除阻塞：接入方向结束即会话结束，回传方向随连接关闭返回。
func ServeUDPClient(work net.Conn, target *net.UDPConn, ctx context.Context) {
	var once sync.Once
	stopped := func() {
		once.Do(func() {
			_ = target.Close()
			_ = work.Close()
		})
	}
	defer stopped()

	// 目标回传方向独立成 goroutine。它的结束不构成会话的结束条件：UDP 目标在
	// 收到请求前没有数据可读，若把该方向的结束当作整条会话的结束，会话会在
	// 建立后立刻退出。两个方向都靠 Close 解除阻塞，因此无需读取截止时间。
	go func() {
		_ = readUDPResponses(target, func(frame []byte) error {
			return writeDatagramFrame(work, frame)
		})
	}()

	// 取消即关闭两侧：两个方向都阻塞在读上，只有关闭才能让它们返回。
	// 用读截止时间轮询则不可行——wire 读取器会把超时包装成协议错误，无法判定
	// 为超时，轮询会被误当作连接结束而提前关闭会话。
	go func() {
		<-ctx.Done()
		stopped()
	}()

	reader := wire.NewV1Reader(work, udpDatagramFrameLimit)
	for {
		frame, err := reader.ReadFrame()
		if err != nil {
			return
		}
		datagram, decoded := decodeDatagram(frame)
		frame.Release()
		if !decoded {
			return
		}
		if _, err := target.Write(datagram); err != nil {
			return
		}
	}
}

// readUDPResponses 读取本地目标的回传数据报并编码写回工作连接。
//
// 阻塞读取靠关闭解除，不设读截止时间：UDP 目标在收到请求前本就无数据可读，
// 用超时轮询会把「暂时无数据」误判为连接结束。
func readUDPResponses(target *net.UDPConn, write func([]byte) error) error {
	buffer := make([]byte, udpDatagramFrameLimit)
	for {
		read, _, err := target.ReadFrom(buffer)
		if err != nil {
			return err
		}
		frame, encodeErr := encodeDatagram(buffer[:read])
		if encodeErr != nil {
			return encodeErr
		}
		if writeErr := write(frame); writeErr != nil {
			return writeErr
		}
	}
}
