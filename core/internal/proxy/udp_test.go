package proxy_test

import (
	"context"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/wcpe/jrp/core/internal/proxy"
	"github.com/wcpe/jrp/core/internal/transport"
)

// 本文件覆盖 FR-06a §3.4 的 UDP 会话语义：同一对端地址归属同一会话、空闲回收、
// 会话上限拒绝、数据报上限丢弃。先红后绿。

// recorder 记录会话回写到对端的数据报。
type recorder struct {
	mu       sync.Mutex
	received []string
}

func (record *recorder) send(peer netip.AddrPort, data []byte) error {
	record.mu.Lock()
	defer record.mu.Unlock()
	record.received = append(record.received, string(data))
	return nil
}

func (record *recorder) snapshot() []string {
	record.mu.Lock()
	defer record.mu.Unlock()
	return append([]string(nil), record.received...)
}

var testPeer = netip.MustParseAddrPort("127.0.0.1:40000")

// newTestSession 建立一条会话：工作连接用 net.Pipe 的一段，另一端交给测试驱动。
func newTestSession(t *testing.T, idle time.Duration, maxDatagram int) (*proxy.UDPSession, net.Conn, *recorder) {
	t.Helper()
	workSide, peerSide := net.Pipe()
	record := &recorder{}
	session := proxy.NewUDPSession(proxy.UDPSessionConfig{
		Peer:        testPeer,
		Work:        workSide,
		Send:        record.send,
		Idle:        idle,
		MaxDatagram: maxDatagram,
	})
	t.Cleanup(func() {
		_ = session.Close()
		_ = peerSide.Close()
	})
	return session, peerSide, record
}

// TestUDPSessionRoundTrip 覆盖同一对端地址的往返：入口数据报经工作连接送达，
// 客户端回传的数据报写回原对端。
func TestUDPSessionRoundTrip(t *testing.T) {
	session, workPeer, record := newTestSession(t, time.Second, 1500)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go session.Serve(ctx)

	if !session.Deliver([]byte("请求数据报")) {
		t.Fatalf("入队应当成功")
	}
	payload, err := readDatagram(workPeer, 1500)
	if err != nil {
		t.Fatalf("工作连接未收到数据报：%v", err)
	}
	if string(payload) != "请求数据报" {
		t.Fatalf("工作连接收到的数据报不一致：%q", string(payload))
	}
	frame, err := encodeDatagram([]byte("响应数据报"), 1500)
	if err != nil {
		t.Fatalf("编码响应数据报失败：%v", err)
	}
	if err := writeAll(workPeer, frame); err != nil {
		t.Fatalf("回写数据报失败：%v", err)
	}
	waitFor(t, func() bool { return len(record.snapshot()) > 0 }, "响应未回到原对端")
	received := record.snapshot()
	if len(received) != 1 || received[0] != "响应数据报" {
		t.Fatalf("回写到对端的数据报不一致：%+v", received)
	}
}

// TestUDPSessionSamePeerKeepsOneSession 覆盖同一对端地址的后续数据报走同一会话。
func TestUDPSessionSamePeerKeepsOneSession(t *testing.T) {
	port, err := transport.ListenUDP(netip.MustParseAddrPort("127.0.0.1:0"))
	if err != nil {
		t.Fatalf("打开 UDP 入口失败：%v", err)
	}
	entry := proxy.NewUDPProxy(proxy.UDPProxyConfig{
		Name:        "dns",
		Port:        port,
		Idle:        500 * time.Millisecond,
		MaxSessions: 4,
		MaxDatagram: 1500,
		Work:        pipeWork(t),
	})
	t.Cleanup(func() { _ = entry.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go entry.Serve(ctx)

	client, err := net.Dial("udp", port.Addr().String())
	if err != nil {
		t.Fatalf("拨号 UDP 入口失败：%v", err)
	}
	defer client.Close()
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := client.Write([]byte("同一对端")); err != nil {
			t.Fatalf("写入数据报失败：%v", err)
		}
	}
	waitFor(t, func() bool { return entry.Delivered() >= 1 }, "数据报未送达会话")
	if got := entry.Sessions(); got != 1 {
		t.Fatalf("同一对端应复用同一会话，实际会话数 %d", got)
	}
}

// TestUDPSessionIdleRecycled 覆盖会话空闲回收：空闲超过上限后会话结束，
// 回收后不再接受数据报。
func TestUDPSessionIdleRecycled(t *testing.T) {
	session, workPeer, _ := newTestSession(t, 60*time.Millisecond, 1500)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go session.Serve(ctx)

	if !session.Deliver([]byte("首个数据报")) {
		t.Fatalf("入队应当成功")
	}
	if _, err := readDatagram(workPeer, 1500); err != nil {
		t.Fatalf("工作连接未收到数据报：%v", err)
	}
	select {
	case <-session.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("空闲超过上限后会话未回收")
	}
	if session.Deliver([]byte("回收后的数据报")) {
		t.Fatalf("会话回收后不应再接受数据报")
	}
}

// TestUDPSessionDatagramLimit 覆盖数据报上限边界：等于上限接受、上限加一丢弃、
// 上限减一接受，且丢弃被计数。
func TestUDPSessionDatagramLimit(t *testing.T) {
	const maxDatagram = 1500
	session, _, _ := newTestSession(t, time.Second, maxDatagram)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go session.Serve(ctx)

	if !session.Deliver(make([]byte, maxDatagram)) {
		t.Fatalf("等于上限的数据报应当被接受")
	}
	if !session.Deliver(make([]byte, maxDatagram-1)) {
		t.Fatalf("上限减一的数据报应当被接受")
	}
	if session.Deliver(make([]byte, maxDatagram+1)) {
		t.Fatalf("上限加一的数据报应当被丢弃")
	}
	if got := session.Dropped(); got != 1 {
		t.Fatalf("丢弃计数不匹配：期望 1，实际 %d", got)
	}
}

// TestUDPProxySessionLimit 覆盖会话上限：达到上限后新对端被拒绝并计数，
// 已有会话不受影响。
func TestUDPProxySessionLimit(t *testing.T) {
	port, err := transport.ListenUDP(netip.MustParseAddrPort("127.0.0.1:0"))
	if err != nil {
		t.Fatalf("打开 UDP 入口失败：%v", err)
	}
	entry := proxy.NewUDPProxy(proxy.UDPProxyConfig{
		Name:        "dns",
		Port:        port,
		Idle:        500 * time.Millisecond,
		MaxSessions: 1,
		MaxDatagram: 1500,
		Work:        pipeWork(t),
	})
	t.Cleanup(func() { _ = entry.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go entry.Serve(ctx)

	client, err := net.Dial("udp", port.Addr().String())
	if err != nil {
		t.Fatalf("拨号 UDP 入口失败：%v", err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("首个对端")); err != nil {
		t.Fatalf("写入数据报失败：%v", err)
	}
	waitFor(t, func() bool { return entry.Sessions() == 1 }, "首个对端应当建立会话")

	// 第二个对端：会话数已达上限，应被拒绝并计数。
	other, err := net.Dial("udp", port.Addr().String())
	if err != nil {
		t.Fatalf("拨号 UDP 入口失败：%v", err)
	}
	defer other.Close()
	if _, err := other.Write([]byte("第二个对端")); err != nil {
		t.Fatalf("写入数据报失败：%v", err)
	}
	waitFor(t, func() bool { return entry.Rejected() >= 1 }, "新对端达到上限时应当被拒绝并计数")
	if got := entry.Sessions(); got != 1 {
		t.Fatalf("拒绝新对端后会话数不应增长：%d", got)
	}
}

// TestUDPProxyOversizedDatagram 覆盖超上限数据报：被丢弃并计数，不建立会话。
func TestUDPProxyOversizedDatagram(t *testing.T) {
	port, err := transport.ListenUDP(netip.MustParseAddrPort("127.0.0.1:0"))
	if err != nil {
		t.Fatalf("打开 UDP 入口失败：%v", err)
	}
	entry := proxy.NewUDPProxy(proxy.UDPProxyConfig{
		Name:        "syslog",
		Port:        port,
		Idle:        time.Second,
		MaxSessions: 4,
		MaxDatagram: 100,
		Work:        pipeWork(t),
	})
	t.Cleanup(func() { _ = entry.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go entry.Serve(ctx)

	client, err := net.Dial("udp", port.Addr().String())
	if err != nil {
		t.Fatalf("拨号 UDP 入口失败：%v", err)
	}
	defer client.Close()
	if _, err := client.Write(make([]byte, 101)); err != nil {
		t.Fatalf("写入数据报失败：%v", err)
	}
	waitFor(t, func() bool { return entry.Oversized() >= 1 }, "超限数据报应当被丢弃并计数")
	if got := entry.Sessions(); got != 0 {
		t.Fatalf("超限数据报不应建立会话：%d", got)
	}
}

// pipeWork 返回一条工作连接：一端交给会话，另一端由读取 goroutine 取走数据，
// 避免 net.Pipe 的同步语义让会话卡在写入上。
func pipeWork(t *testing.T) func() (net.Conn, bool) {
	t.Helper()
	return func() (net.Conn, bool) {
		left, right := net.Pipe()
		t.Cleanup(func() { _ = right.Close() })
		go func() {
			defer func() { _ = right.Close() }()
			_, _ = io.Copy(io.Discard, right)
		}()
		return left, true
	}
}
func waitFor(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s", message)
}

// writeAll 写满给定字节并施加写截止时间，避免 net.Pipe 无缓冲时永久阻塞。
func writeAll(target net.Conn, data []byte) error {
	_ = target.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err := target.Write(data)
	return err
}

// 入口复用读取缓冲时，已入队的数据报内容不得被后续读取覆写。
//
// 回归用例：入口用固定缓冲反复读取，若入队时不复制，队列里尚未处理的数据报会
// 在下一次读取时被就地改写。跨对端时这会让 A 会话交付 A 的内容变成 B 的载荷，
// 既是数据损坏也是跨用户数据串扰。
func TestDeliverCopiesDatagramBuffer(t *testing.T) {
	session, peerSide, _ := newTestSession(t, time.Second, 4096)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go session.Serve(ctx)

	// net.Pipe 无缓冲：测试端不读时，会话的转发会阻塞，后续数据报积压在队列。
	first := []byte("AAAAAAAAAAAAAAAA")
	if !session.Deliver(first) {
		t.Fatal("首个数据报应被接受")
	}
	second := []byte("BBBBBBBBBBBBBBBB")
	if !session.Deliver(second) {
		t.Fatal("第二个数据报应被接受")
	}

	// 模拟入口循环复用同一缓冲：就地覆写，这不应影响已入队的内容。
	first = append(first[:0], []byte("CCCCCCCCCCCCCCCC")...)
	second = append(second[:0], []byte("DDDDDDDDDDDDDDDD")...)

	// 逐个读出：内容必须是最初投递的 A 与 B，而不是覆写后的 C 与 D。
	// 工作连接上走的是线协议帧，用 readDatagram 解出载荷。
	firstPayload, err := readDatagram(peerSide, 4096)
	if err != nil {
		t.Fatalf("读取首个数据报失败：%v", err)
	}
	if got := string(firstPayload); got != "AAAAAAAAAAAAAAAA" {
		t.Fatalf("首个数据报内容被覆写：期望 AAAAAAAAAAAAAAAA，实际 %q", got)
	}

	secondPayload, err := readDatagram(peerSide, 4096)
	if err != nil {
		t.Fatalf("读取第二个数据报失败：%v", err)
	}
	if got := string(secondPayload); got != "BBBBBBBBBBBBBBBB" {
		t.Fatalf("第二个数据报内容被覆写：期望 BBBBBBBBBBBBBBBB，实际 %q", got)
	}
}

// 超限数据报之后入口必须继续服务。
//
// 回归用例：Windows 的 recvfrom 对大于接收缓冲的数据报返回 WSAEMSGSIZE（非超时
// 错误），而 Serve 把任何非超时读错误都当作致命错误直接返回——单个超限数据报即
// 让整个入口永久停止，任何能向入口发包的第三方都能用一个包触发它。
//
// 既有用例只断言 Oversized 计数，未验证入口存活：在 Windows 上该计数永远为 0
// （超限分支不可达），却因"数据报不被接受"而恰好满足"不应建立会话"的断言。
func TestUDPProxyOversizedDatagramDoesNotStopEntry(t *testing.T) {
	port, err := transport.ListenUDP(netip.MustParseAddrPort("127.0.0.1:0"))
	if err != nil {
		t.Fatalf("打开 UDP 入口失败：%v", err)
	}
	entry := proxy.NewUDPProxy(proxy.UDPProxyConfig{
		Name:        "syslog",
		Port:        port,
		Idle:        time.Second,
		MaxSessions: 4,
		MaxDatagram: 100,
		Work:        pipeWork(t),
	})
	t.Cleanup(func() { _ = entry.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go entry.Serve(ctx)

	client, err := net.Dial("udp", port.Addr().String())
	if err != nil {
		t.Fatalf("拨号 UDP 入口失败：%v", err)
	}
	defer client.Close()

	// 先发一个超限数据报。
	if _, err := client.Write(make([]byte, 2000)); err != nil {
		t.Fatalf("写入超限数据报失败：%v", err)
	}
	waitFor(t, func() bool { return entry.Oversized() >= 1 }, "超限数据报应当被丢弃并计数")

	// 入口必须仍然服务：再发正常数据报应能建立会话。
	second, err := net.Dial("udp", port.Addr().String())
	if err != nil {
		t.Fatalf("第二次拨号失败：%v", err)
	}
	defer second.Close()
	if _, err := second.Write(make([]byte, 50)); err != nil {
		t.Fatalf("写入正常数据报失败：%v", err)
	}
	waitFor(t, func() bool { return entry.Sessions() >= 1 }, "超限数据报之后入口应当继续服务")
}
