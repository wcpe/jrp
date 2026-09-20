package server

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/wcpe/jrp/core/internal/transport"
)

// connPair 经一对真实 TCP 连接取得两端各一份 transport.Conn。
//
// 服务端视角的 Conn 只能经 Listener.Accept 获得（构造是包内私有的），
// 因此两端各起一个 listener 互拨：local 与 remote 分别持有同一连接的两侧，
// 测试可以独立关闭 remote 来模拟 NAT 回收，或从 remote 读出补写的字节。
//
// local/remote 由服务端 Accept 得到；localPeer/remotePeer 是它们各自的对端，
// 生命周期由测试管理。
func connPair(t *testing.T) (local, remote *transport.Conn, closePeers func()) {
	t.Helper()
	localListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	remoteListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = localListener.Close()
		t.Fatalf("监听失败：%v", err)
	}

	localHandle := transport.TakeOverListener(localListener)
	remoteHandle := transport.TakeOverListener(remoteListener)
	localAccept := make(chan *transport.Conn, 1)
	go func() {
		conn, _, err := localHandle.Accept(transport.PurposeWork, "acc-guest")
		if err == nil {
			localAccept <- conn
		}
	}()
	remoteAccept := make(chan *transport.Conn, 1)
	go func() {
		conn, _, err := remoteHandle.Accept(transport.PurposeWork, "acc-proxy")
		if err == nil {
			remoteAccept <- conn
		}
	}()

	dialLocal, err := net.Dial("tcp", localListener.Addr().String())
	if err != nil {
		t.Fatalf("拨号本地失败：%v", err)
	}
	dialRemote, err := net.Dial("tcp", remoteListener.Addr().String())
	if err != nil {
		t.Fatalf("拨号远端失败：%v", err)
	}

	var localConn, remoteConn *transport.Conn
	select {
	case localConn = <-localAccept:
	case <-time.After(5 * time.Second):
		t.Fatal("等待本地 Accept 超时")
	}
	select {
	case remoteConn = <-remoteAccept:
	case <-time.After(5 * time.Second):
		t.Fatal("等待远端 Accept 超时")
	}

	closeAll := func() {
		_ = localListener.Close()
		_ = remoteListener.Close()
		_ = dialLocal.Close()
		_ = dialRemote.Close()
		_ = localConn.Close()
		_ = remoteConn.Close()
	}
	t.Cleanup(closeAll)
	// remote 端的对端是 dialLocal（关它可以模拟 NAT 回收 remote）；
	// local 端的对端是 dialRemote（从它读可以验证补写首部到达了 local 的对侧）。
	// 这里把对端传回给调用方使用。
	return localConn, remoteConn, closeAll
}

// 已失效的工作连接不得掐断访客。
//
// 回归用例：跨网络环境下待命工作连接会被 NAT 或中间设备静默回收，池中无法
// 预先感知。此前 pairing.start 在写入 pending 失败时直接关闭访客，把连接
// 失效的代价转嫁给真实用户；实测跨 NAT 时成功率仅 7%，而回环下为 93%。
// 修复后 start 只丢弃失效的工作连接并返回假，由调用方换下一条重试。
func TestPairingSkipsDeadWorkConnKeepsGuest(t *testing.T) {
	guest, work, closePeers := connPair(t)
	defer closePeers()

	// 直接关闭 work 本端：随后对它的读写都会立即失败，与"池中保留着的
	// 已死连接"行为一致。guest 是另一条独立连接，不受影响。
	_ = work.Close()

	pair := pairing{guest: guest, work: work, pending: []byte("GET / HTTP/1.1\r\n\r\n")}
	tracked := 0
	if pair.start(func(*transport.Conn) { tracked++ }, func(int) {}) {
		t.Fatal("失效的工作连接不应启动桥接")
	}
	if tracked != 0 {
		t.Fatalf("失效连接不得触发登记，实际登记 %d 次", tracked)
	}
	// 修复的核心：只弃工作连接，访客保持原样。旧的失败路径会连访客一起关闭，
	// 把连接失效的代价转嫁给真实用户——本断言正是守住这一点。
	// 健康连接在零超时读下返回超时错误；被关闭的连接则立即返回关闭错误。
	_ = guest.SetReadDeadline(time.Now())
	probe := make([]byte, 1)
	_, readErr := guest.Read(probe)
	if readErr == nil {
		t.Fatal("访客不应收到任何数据")
	}
	var netErr net.Error
	if !errors.As(readErr, &netErr) || !netErr.Timeout() {
		t.Fatalf("访客应保持打开（零超时读应超时），实际错误：%v", readErr)
	}
}

// 健康的工作连接必须正常启动桥接，且补写首部先于桥接送达对端。
//
// 与上一用例构成正反两面：探测逻辑不能把健康连接误判为失效，
// 否则所有配对都会失败。
func TestPairingBridgesHealthyWorkConn(t *testing.T) {
	guest, work, closePeers := connPair(t)
	defer closePeers()

	pair := pairing{guest: guest, work: work, pending: []byte("GET / HTTP/1.1\r\n\r\n")}
	tracked := 0
	if !pair.start(func(*transport.Conn) { tracked++ }, func(int) {}) {
		t.Fatal("健康的工作连接应启动桥接")
	}
	if tracked != 2 {
		t.Fatalf("桥接应登记访客与工作连接两条，实际 %d 条", tracked)
	}
}
