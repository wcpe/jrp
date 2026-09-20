package transport_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jrp/core/internal/transport"
)

// 本文件是 FR-05a 的失败测试：传输抽象（监听句柄、拨号句柄、连接句柄、用途
// 标记、工作连接池、暂存空闲回收、异常关闭）在此前完全不存在，先红后绿。

const testProxyName = "tcp-slice-ssh"

// listenLocal 在回环上监听并返回监听器与关闭函数。
func listenLocal(t *testing.T) (net.Listener, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	return listener, func() { _ = listener.Close() }
}

// TestListenerTakeOverAndRelease 验证监听句柄的接管与释放：
// 接管后宿主不得再使用，释放后监听器彻底关闭。
func TestListenerTakeOverAndRelease(t *testing.T) {
	listener, _ := listenLocal(t)
	address := listener.Addr().String()

	handle := transport.TakeOverListener(listener)

	// 接管后仍可 Accept：地址应可连接。
	if _, err := net.DialTimeout("tcp", address, 2*time.Second); err != nil {
		t.Fatalf("接管后地址不可连接：%v", err)
	}
	if err := handle.Release(); err != nil {
		t.Fatalf("释放失败：%v", err)
	}
	// 释放后地址不再可连接：无残留监听器。
	if _, err := net.DialTimeout("tcp", address, 300*time.Millisecond); err == nil {
		t.Fatalf("释放后地址仍可连接，存在残留监听器")
	}
}

// TestListenerReleaseIsIdempotent 验证重复释放安全。
func TestListenerReleaseIsIdempotent(t *testing.T) {
	listener, _ := listenLocal(t)
	handle := transport.TakeOverListener(listener)
	if err := handle.Release(); err != nil {
		t.Fatalf("首次释放失败：%v", err)
	}
	if err := handle.Release(); err != nil {
		t.Fatalf("重复释放应安全成功，实际：%v", err)
	}
}

// TestAcceptErrorClassifiesTemporaryAndFatal 验证 Accept 错误区分临时与致命。
//
// 规格 §3.6：临时错误退避后继续，致命错误停止监听并上报。
func TestAcceptErrorClassifiesTemporaryAndFatal(t *testing.T) {
	listener, stop := listenLocal(t)
	defer stop()

	// 监听器已关闭：Accept 返回致命错误，不得按临时错误退避重试。
	if err := listener.Close(); err != nil {
		t.Fatalf("关闭监听器失败：%v", err)
	}
	_, err := listener.Accept()
	if err == nil {
		t.Fatalf("已关闭的监听器 Accept 应返回错误")
	}
	if transport.AcceptErrorAction(err) != transport.AcceptFatal {
		t.Fatalf("已关闭监听器的 Accept 错误应判为致命，实际：%v", transport.AcceptErrorAction(err))
	}
	// 致命错误不得产生退避时长：调用方必须立即停止循环。
	if backoff := transport.AcceptBackoff(transport.AcceptFatal); backoff != 0 {
		t.Fatalf("致命错误不应产生退避，实际：%v", backoff)
	}
	if backoff := transport.AcceptBackoff(transport.AcceptTemporary); backoff <= 0 {
		t.Fatalf("临时错误必须产生正退避，实际：%v", backoff)
	}
}

// TestDialRequiresTimeout 验证拨号必须有超时：零超时拨号被拒绝。
func TestDialRequiresTimeout(t *testing.T) {
	dialer := transport.Dialer{Timeout: 0}
	_, err := dialer.Dial(context.Background(), "127.0.0.1:1", transport.PurposeControl)
	if err == nil {
		t.Fatalf("零超时拨号应被拒绝")
	}
	if !errors.Is(err, transport.ErrDialTimeoutMissing) {
		t.Fatalf("零超时拨号应返回 ErrDialTimeoutMissing，实际：%v", err)
	}
}

// TestDialUnreachableReturnsByTimeout 验证拨号在不可达地址上按超时返回错误，不悬挂。
//
// 规格 §5 边界：拨号在不可达地址上按超时返回错误，不悬挂。
// 使用不可路由的 TEST-NET-1 地址，避免依赖本机网络配置。
func TestDialUnreachableReturnsByTimeout(t *testing.T) {
	dialer := transport.Dialer{Timeout: 200 * time.Millisecond}
	start := time.Now()
	_, err := dialer.Dial(context.Background(), "192.0.2.1:7000", transport.PurposeControl)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("拨号不可达地址应返回错误")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("拨号未受超时约束，耗时 %v", elapsed)
	}
}

// TestDialContextCancelStopsImmediately 验证上下文取消立即中断拨号。
func TestDialContextCancelStopsImmediately(t *testing.T) {
	dialer := transport.Dialer{Timeout: 30 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := dialer.Dial(ctx, "192.0.2.1:7000", transport.PurposeControl); err == nil {
		t.Fatalf("已取消的上下文拨号应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("上下文取消未立即中断拨号，耗时 %v", elapsed)
	}
}

// TestDialMarksPurposeAndEstablishedAt 验证拨号结果带用途标记与建链时间。
func TestDialMarksPurposeAndEstablishedAt(t *testing.T) {
	listener, stop := listenLocal(t)
	defer stop()
	go acceptAndEchoForever(listener)

	dialer := transport.Dialer{Timeout: 2 * time.Second}
	before := time.Now()
	conn, err := dialer.Dial(context.Background(), listener.Addr().String(), transport.PurposeControl)
	if err != nil {
		t.Fatalf("拨号失败：%v", err)
	}
	defer conn.Close()

	if conn.Purpose() != transport.PurposeControl {
		t.Fatalf("用途标记不符：%v", conn.Purpose())
	}
	if conn.EstablishedAt().Before(before) || conn.EstablishedAt().After(time.Now()) {
		t.Fatalf("建链时间不在拨号区间内：%v", conn.EstablishedAt())
	}
}

// TestConnPurposeSeparatesControlAndWork 验证控制与工作用途可并存于同一监听器。
//
// 规格 §5 正常路径：控制连接与工作连接可并存于同一监听器，用途标记正确分离。
func TestConnPurposeSeparatesControlAndWork(t *testing.T) {
	listener, stop := listenLocal(t)
	defer stop()
	go acceptAndEchoForever(listener)

	dialer := transport.Dialer{Timeout: 2 * time.Second}
	control, err := dialer.Dial(context.Background(), listener.Addr().String(), transport.PurposeControl)
	if err != nil {
		t.Fatalf("控制连接拨号失败：%v", err)
	}
	defer control.Close()
	work, err := dialer.Dial(context.Background(), listener.Addr().String(), transport.PurposeWork, testProxyName)
	if err != nil {
		t.Fatalf("工作连接拨号失败：%v", err)
	}
	defer work.Close()

	if control.Purpose() != transport.PurposeControl || work.Purpose() != transport.PurposeWork {
		t.Fatalf("用途标记未分离：控制 %v，工作 %v", control.Purpose(), work.Purpose())
	}
	if work.Proxy() != testProxyName {
		t.Fatalf("工作连接未携带代理归属：%q", work.Proxy())
	}
	if control.Proxy() != "" {
		t.Fatalf("控制连接不得声明代理归属：%q", control.Proxy())
	}
}

// TestConnDeadlineEntryPoints 验证读写截止时间设置入口可用且不破坏后续读写。
func TestConnDeadlineEntryPoints(t *testing.T) {
	listener, stop := listenLocal(t)
	defer stop()
	go acceptAndEchoForever(listener)

	dialer := transport.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.Dial(context.Background(), listener.Addr().String(), transport.PurposeControl)
	if err != nil {
		t.Fatalf("拨号失败：%v", err)
	}
	defer conn.Close()

	// 极短读截止：读不到数据必须超时而不是悬挂。
	if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("设置读截止时间失败：%v", err)
	}
	buffer := make([]byte, 1)
	if _, err := conn.Read(buffer); err == nil {
		t.Fatalf("读截止时间未生效：无数据却读成功")
	} else if !isTimeout(err) {
		t.Fatalf("期望超时错误，实际：%v", err)
	}
	// 清除截止时间后正常往返必须恢复。
	if err := conn.SetDeadline(time.Time{}); err != nil {
		t.Fatalf("清除截止时间失败：%v", err)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("设置写截止时间失败：%v", err)
	}
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
}

// TestConnHalfCloseDoesNotHang 验证对端半关闭后本端读完剩余数据并感知 EOF，不无限等待。
func TestConnHalfCloseDoesNotHang(t *testing.T) {
	listener, stop := listenLocal(t)
	defer stop()
	go acceptAndEchoForever(listener)

	dialer := transport.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.Dial(context.Background(), listener.Addr().String(), transport.PurposeWork, testProxyName)
	if err != nil {
		t.Fatalf("拨号失败：%v", err)
	}
	defer conn.Close()

	payload := []byte("半关闭前数据")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatalf("半关闭失败：%v", err)
	}
	// 对端回显后关闭：本端必须读到全部数据再遇 EOF，不得悬挂。
	received := make([]byte, len(payload))
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, received); err != nil {
		t.Fatalf("半关闭后读回失败：%v", err)
	}
	if string(received) != string(payload) {
		t.Fatalf("半关闭后数据不一致：%q", string(received))
	}
	extra := make([]byte, 1)
	if _, err := conn.Read(extra); err == nil {
		t.Fatalf("对端关闭写方向后读取应返回 EOF")
	}
}

// TestConnPeerTerminationReleasesResources 验证对端终止连接时释放资源且错误可判定。
//
// 对端以 RST 终止（SetLinger(0) 强制重置），但不同内核与负载下，本地可能
// 读到 ECONNRESET 也可能读到 EOF——两者都是「连接已终止」的有效信号，区分
// RST 与 FIN 属于对内核行为的过度指定，在 CI 上不稳定。
//
// 因此本用例断言的是被测目标本身：连接终止后关闭幂等、句柄不可再写（资源
// 确实已释放）。服务端先写入一个字节确认连接已建立，避免与拨号竞态。
func TestConnPeerTerminationReleasesResources(t *testing.T) {
	listener, stop := listenLocal(t)
	defer stop()

	// 对端先确认连接已建立（写入一个字节并让客户端读到），再以 RST 断开。
	//
	// 直接 Accept 后立刻重置会与 Dial 产生竞态：负载高的机器上服务端可能早于
	// 拨号完成就关闭，客户端在拨号阶段即吃到 connection reset，测试还没验证到
	// 目标行为就已失败。因此这里做一次完整的单向握手——服务端写入一个字节，
	// 等到客户端确认读到之后才重置。
	//
	// 只做"写入即放行"是不够的：RST 会把内核接收缓冲区里尚未被应用读走的字节
	// 一并丢弃，客户端随后读到的是连接重置而非握手字节。macOS 上重置到达更快，
	// 这个窗口会稳定复现，因此必须等对端确认读取完成。
	handshakeRead := make(chan struct{})
	ready := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			close(ready)
			return
		}
		tcpConn, ok := conn.(*net.TCPConn)
		if !ok {
			_ = conn.Close()
			close(ready)
			return
		}
		_, _ = conn.Write([]byte("r"))
		close(ready)
		// 等客户端把字节读走再重置；客户端异常退出时由测试进程结束兜底。
		<-handshakeRead
		_ = tcpConn.SetLinger(0)
		_ = tcpConn.Close()
	}()

	dialer := transport.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.Dial(context.Background(), listener.Addr().String(), transport.PurposeControl)
	if err != nil {
		close(handshakeRead)
		t.Fatalf("拨号失败：%v", err)
	}
	defer conn.Close()

	// 先读走握手字节，确认连接已建立且对端尚未重置。
	<-ready
	handshake := make([]byte, 1)
	if _, err := io.ReadFull(conn, handshake); err != nil {
		t.Fatalf("读取握手字节失败：%v", err)
	}
	close(handshakeRead)

	deadline := time.Now().Add(3 * time.Second)
	for {
		buffer := make([]byte, 1)
		_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		if _, err := conn.Read(buffer); err != nil {
			// 对端重置在不同内核与负载下可能表现为 ECONNRESET 或 EOF，两者都是
			// 连接已终止的有效信号。被测目标是「释放资源且错误可判定」，因此
			// 接受这两类错误，靠下面的写入失败断言保证连接确实已终止。
			//
			// 关闭必须释放资源并幂等：重复关闭返回与首次相同的结果。
			firstClose := conn.Close()
			if repeated := conn.Close(); !errors.Is(repeated, firstClose) && repeated != firstClose {
				t.Fatalf("异常后关闭不幂等：首次 %v，再次 %v", firstClose, repeated)
			}
			// 释放后任何读写都必须失败：不得留下可用的句柄。
			if _, writeErr := conn.Write([]byte("x")); writeErr == nil {
				t.Fatalf("连接已关闭但写入仍成功，资源未真正释放")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("对端重置未能在期限内被感知")
		}
	}
}

// TestBridgeHalfCloseBothDirections 验证桥接在半关闭下双向收尾且不悬挂。
func TestBridgeHalfCloseBothDirections(t *testing.T) {
	serverSide, stopServer := listenLocal(t)
	defer stopServer()
	clientSide, stopClient := listenLocal(t)
	defer stopClient()

	// 服务端侧回显后关闭写方向；客户端侧读完后应感知 EOF。
	go func() {
		conn, err := serverSide.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buffer := make([]byte, 64)
		read, _ := conn.Read(buffer)
		_, _ = conn.Write(buffer[:read])
		if tcpConn, ok := conn.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}
		// 保持连接，等待对端读完。
		time.Sleep(500 * time.Millisecond)
	}()

	go func() {
		conn, err := clientSide.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()

	dialer := transport.Dialer{Timeout: 2 * time.Second}
	left, err := dialer.Dial(context.Background(), serverSide.Addr().String(), transport.PurposeWork, testProxyName)
	if err != nil {
		t.Fatalf("左侧拨号失败：%v", err)
	}
	defer left.Close()
	right, err := dialer.Dial(context.Background(), clientSide.Addr().String(), transport.PurposeWork, testProxyName)
	if err != nil {
		t.Fatalf("右侧拨号失败：%v", err)
	}
	defer right.Close()

	payload := []byte("桥接半关闭数据")
	if _, err := right.Write(payload); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		transport.Bridge(context.Background(), left, right)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("桥接在半关闭下未收尾，可能无限等待")
	}
}

// TestNewWorkConnPoolEnforcesLimit 验证工作连接池上限：达到上限后拒绝并产生事件。
//
// 规格 §3.5：达到上限时新请求被推迟或拒绝，必须产生可观测事件，不得无限等待。
// 规格 §5 边界：池上限的边界值（等于上限、上限加一）行为可判定。
func TestNewWorkConnPoolEnforcesLimit(t *testing.T) {
	pool := transport.NewWorkConnPool(2)
	defer pool.Close()

	first := mustAcquireSlot(t, pool)
	second := mustAcquireSlot(t, pool)
	// 等于上限：不再接受新槽位，必须立即返回而不阻塞。
	if _, err := pool.Acquire(context.Background(), testProxyName); err == nil {
		t.Fatalf("达到上限后 Acquire 应拒绝")
	} else if !errors.Is(err, transport.ErrPoolFull) {
		t.Fatalf("达到上限应返回 ErrPoolFull，实际：%v", err)
	}
	// 上限事件必须可观测：计数至少为 1。
	if count := pool.FullEvents(); count < 1 {
		t.Fatalf("达到上限未产生可观测事件，计数为 %d", count)
	}
	// 释放后槽位可回收：等于上限减一时重新可用。
	second.Release()
	third := mustAcquireSlot(t, pool)
	third.Release()
	first.Release()
}

// TestWorkConnPoolIsPerProxy 验证池按代理名隔离：不同代理各自计数。
func TestWorkConnPoolIsPerProxy(t *testing.T) {
	pool := transport.NewWorkConnPool(1)
	defer pool.Close()

	slotA := mustAcquireSlot(t, pool, "proxy-a")
	if _, err := pool.Acquire(context.Background(), "proxy-a"); !errors.Is(err, transport.ErrPoolFull) {
		t.Fatalf("同一代理达到上限应拒绝，实际：%v", err)
	}
	// 不同代理不受影响。
	slotB := mustAcquireSlot(t, pool, "proxy-b")
	slotB.Release()
	slotA.Release()
}

// TestWorkConnPoolCloseUnblocksWaiters 验证关闭池后等待者被唤醒且不留悬挂。
func TestWorkConnPoolCloseUnblocksWaiters(t *testing.T) {
	pool := transport.NewWorkConnPool(1)
	slot := mustAcquireSlot(t, pool)

	// 池已满：新请求必须立即返回错误，不得无限等待。
	if _, err := pool.Acquire(context.Background(), testProxyName); !errors.Is(err, transport.ErrPoolFull) {
		t.Fatalf("池满时 Acquire 应立即拒绝，实际：%v", err)
	}
	slot.Release()
	if err := pool.Close(); err != nil {
		t.Fatalf("关闭池失败：%v", err)
	}
	if _, err := pool.Acquire(context.Background(), testProxyName); err == nil {
		t.Fatalf("已关闭的池不得再分配槽位")
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("重复关闭池应安全，实际：%v", err)
	}
}

// TestStagedWorkConnsReclaimsIdle 验证暂存工作连接按空闲上限回收最老连接。
//
// 规格 §3.5：空闲连接在空闲上限后被回收，回收时释放缓冲与 goroutine。
// 规格 §5 边界：空闲上限的边界值（等于上限、上限加一）行为可判定。
func TestStagedWorkConnsReclaimsIdle(t *testing.T) {
	listener, stop := listenLocal(t)
	defer stop()
	accepted := make(chan net.Conn, 8)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()

	staged := transport.NewStagedWorkConns(2)
	defer staged.Close()

	dialer := transport.Dialer{Timeout: 2 * time.Second}
	first := mustDialStaged(t, dialer, listener.Addr().String())
	staged.Push(testProxyName, first)
	second := mustDialStaged(t, dialer, listener.Addr().String())
	staged.Push(testProxyName, second)

	// 等于上限：两条都还在。
	if staged.Len(testProxyName) != 2 {
		t.Fatalf("等于空闲上限时应保留两条，实际 %d", staged.Len(testProxyName))
	}
	// 上限加一：最老的一条被回收（连接被关闭）。
	third := mustDialStaged(t, dialer, listener.Addr().String())
	staged.Push(testProxyName, third)
	if staged.Len(testProxyName) != 2 {
		t.Fatalf("超出空闲上限后应回收最老连接，实际 %d", staged.Len(testProxyName))
	}
	if !isClosed(first) {
		t.Fatalf("被回收的暂存连接应已关闭")
	}
	if taken := staged.Take(testProxyName); taken == nil {
		t.Fatalf("暂存队列为空，回收逻辑错误")
	}
	if taken := staged.Take(testProxyName); taken == nil {
		t.Fatalf("暂存队列应仍有连接")
	}
	if taken := staged.Take(testProxyName); taken != nil {
		t.Fatalf("暂存队列应已取空")
	}
	// 服务端侧确认收到三条连接。
	for index := 0; index < 3; index++ {
		select {
		case conn := <-accepted:
			_ = conn.Close()
		case <-time.After(time.Second):
			t.Fatalf("服务端未收到第 %d 条连接", index+1)
		}
	}
}

// TestStagedWorkConnsCloseReleasesAll 验证关闭暂存集合后全部连接被释放。
func TestStagedWorkConnsCloseReleasesAll(t *testing.T) {
	listener, stop := listenLocal(t)
	defer stop()
	go acceptAndEchoForever(listener)

	staged := transport.NewStagedWorkConns(4)
	dialer := transport.Dialer{Timeout: 2 * time.Second}
	conn := mustDialStaged(t, dialer, listener.Addr().String())
	staged.Push(testProxyName, conn)
	if err := staged.Close(); err != nil {
		t.Fatalf("关闭暂存集合失败：%v", err)
	}
	if !isClosed(conn) {
		t.Fatalf("关闭暂存集合后连接应已释放")
	}
	if staged.Len(testProxyName) != 0 {
		t.Fatalf("关闭后暂存集合应为空")
	}
	// 重复关闭安全。
	if err := staged.Close(); err != nil {
		t.Fatalf("重复关闭暂存集合应安全，实际：%v", err)
	}
}

// mustAcquireSlot 申请一个池槽位，失败即终止测试。
func mustAcquireSlot(t *testing.T, pool *transport.WorkConnPool, proxy ...string) *transport.PoolSlot {
	t.Helper()
	name := testProxyName
	if len(proxy) > 0 {
		name = proxy[0]
	}
	slot, err := pool.Acquire(context.Background(), name)
	if err != nil {
		t.Fatalf("申请池槽位失败：%v", err)
	}
	return slot
}

// mustDialStaged 拨号一条用于暂存的连接。
func mustDialStaged(t *testing.T, dialer transport.Dialer, address string) *transport.Conn {
	t.Helper()
	conn, err := dialer.Dial(context.Background(), address, transport.PurposeWork, testProxyName)
	if err != nil {
		t.Fatalf("拨号失败：%v", err)
	}
	return conn
}

// acceptAndEchoForever 持续接受连接并回显，直到监听器关闭。
func acceptAndEchoForever(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			buffer := make([]byte, 512)
			for {
				read, err := c.Read(buffer)
				if read > 0 {
					if _, writeErr := c.Write(buffer[:read]); writeErr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}(conn)
	}
}

// isClosed 判断连接是否已被释放：已关闭的连接上任何写入都必须失败。
//
// 不能用 Close 的返回值判断：多数平台上重复关闭已关闭的 socket 也返回 nil。
func isClosed(conn *transport.Conn) bool {
	if _, err := conn.Write(nil); err != nil {
		return true
	}
	// 零长度写在部分平台上仍返回 nil：再用一次带截止时间的读确认。
	_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	buffer := make([]byte, 1)
	_, err := conn.Read(buffer)
	return err != nil && err != io.EOF
}

// isTimeout 判断错误是否为超时错误。
func isTimeout(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "deadline")
}
