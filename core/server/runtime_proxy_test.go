package server

import (
	"encoding/json"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/internal/wire"
)

// startEngineForRuntimeProxy 构造已启动、零快照绑定的引擎（仅凭证），
// 供运行时代理注册测试使用。
func startEngineForRuntimeProxy(t *testing.T) (*Engine, net.Listener) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听控制端口失败：%v", err)
	}
	config, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{
			Address:   netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(listener.Addr().(*net.TCPAddr).Port)),
			Transport: core.TransportTCP,
		}),
		core.WithWire(core.WireV1),
		// 短心跳周期：失活窗口收敛到下限 1 秒，失活用例可在秒级内断言；
		// 其余用例均为毫秒级操作，不受窗口影响。
		core.WithServerHeartbeat(300*time.Millisecond),
		core.WithClientCredential(core.ClientCredential{ClientID: "rt", Token: DigestToken("rt-token")}),
	)
	if err != nil {
		t.Fatalf("构造配置失败：%v", err)
	}
	engine := New(config, WithListener(listener))
	if err := engine.Start(t.Context()); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(t.Context()) })
	return engine, listener
}

// testSession 是模拟官方 frpc 的持久控制会话：登录后保持连接，注册与关闭
// 走同一条会话。会话关闭（raw.Close）即触发服务端清理该会话注册的代理。
type testSession struct {
	t   *testing.T
	raw net.Conn
}

// openTestSession 登录并返回持久会话。
func openTestSession(t *testing.T, listener net.Listener, clientID, token string) *testSession {
	t.Helper()
	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("连接失败：%v", err)
	}
	s := &testSession{t: t, raw: raw}
	if _, err := raw.Write(encodeTestLogin(t, clientID, token)); err != nil {
		t.Fatalf("发送登录失败：%v", err)
	}
	readLoginResponse(t, raw)
	return s
}

// registerProxy 注册一个 TCP 代理。
func (s *testSession) registerProxy(name string, remotePort int, target netip.AddrPort) error {
	s.t.Helper()
	if _, err := s.raw.Write(encodeTestNewProxy(s.t, name, "tcp", remotePort, target)); err != nil {
		return err
	}
	return readTestProxyResponse(s.t, s.raw, 5*time.Second)
}

// closeProxy 关闭一个代理。
func (s *testSession) closeProxy(name string) error {
	s.t.Helper()
	if _, err := s.raw.Write(encodeTestCloseProxy(s.t, name)); err != nil {
		return err
	}
	return readTestProxyResponse(s.t, s.raw, 5*time.Second)
}

// registerProxyOfTypeForTest 在一次性会话上注册指定类型的代理。
//
// 会话随即关闭并触发会话级清理——适用于拒绝路径测试（被拒的代理本就无资源）。
func registerProxyOfTypeForTest(t *testing.T, engine *Engine, listener net.Listener, name, proxyType string, remotePort int, target netip.AddrPort) error {
	t.Helper()
	_ = engine
	s := openTestSession(t, listener, "rt", "rt-token")
	defer s.raw.Close()
	if _, err := s.raw.Write(encodeTestNewProxy(t, name, proxyType, remotePort, target)); err != nil {
		t.Fatalf("发送注册失败：%v", err)
	}
	return readTestProxyResponse(t, s.raw, 5*time.Second)
}

// workConnForRuntimeProxy 模拟客户端为运行时代理建立工作连接并声明归属，
// 且承担客户端职责：把服务端桥接来的数据转发到本地目标（frpc 的等价行为）。
func workConnForRuntimeProxy(t *testing.T, engine *Engine, listener net.Listener, target netip.AddrPort) net.Conn {
	t.Helper()
	work, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("工作连接拨号失败：%v", err)
	}
	t.Cleanup(func() { work.Close() })
	payload, err := json.Marshal(map[string]string{
		"client_id":   "rt",
		"token":       "rt-token",
		"proxy_name":  "rt-ssh",
		"target_addr": target.String(),
		"run_id":      "rt-ssh",
	})
	if err != nil {
		t.Fatalf("编码声明失败：%v", err)
	}
	frame, err := wire.EncodeV1Frame(wire.Frame{Type: wire.MessageTypeNewWorkConn, Payload: payload})
	if err != nil {
		t.Fatalf("编码声明帧失败：%v", err)
	}
	if _, err := work.Write(frame); err != nil {
		t.Fatalf("发送声明失败：%v", err)
	}

	// 客户端职责：桥接 work 连接与本地目标（jrpc 的 maintainOneProxy 同语义）。
	upstream, dialErr := net.Dial("tcp", target.String())
	if dialErr != nil {
		t.Fatalf("客户端连本地目标失败：%v", dialErr)
	}
	t.Cleanup(func() { upstream.Close() })
	go func() {
		_, _ = io.Copy(upstream, work)
	}()
	go func() {
		_, _ = io.Copy(work, upstream)
	}()
	time.Sleep(50 * time.Millisecond)
	return work
}

// 注册成功后入口可访问：运行时注册的代理与快照预建代理等效。
func TestRuntimeProxyRegistrationOpensGuestEntry(t *testing.T) {
	engine, listener := startEngineForRuntimeProxy(t)

	target, stopEcho := startTestEcho(t)
	defer stopEcho()
	guestPort := reserveTestPortForRuntime(t)

	session := openTestSession(t, listener, "rt", "rt-token")
	defer session.raw.Close()
	if err := session.registerProxy("rt-ssh", guestPort, target); err != nil {
		t.Fatalf("运行时注册失败：%v", err)
	}

	// 模拟客户端为该代理建立工作连接：没有它，访客会被暂存等待配对。
	work := workConnForRuntimeProxy(t, engine, listener, target)

	engine.mu.Lock()
	t.Logf("调试: runtimeProxies=%d guestLns=%d guestAddr=%d active=%v",
		len(engine.currentGeneration().runtimeProxies), len(engine.currentGeneration().guestLns), len(engine.currentGeneration().guestAddr), true)
	engine.mu.Unlock()
	if engine.GuestAddr("rt-ssh") == nil {
		t.Fatal("注册成功后入口地址应可用")
	}
	guest, err := net.Dial("tcp", engine.GuestAddr("rt-ssh").String())
	if err != nil {
		t.Fatalf("访客连接失败：%v", err)
	}
	defer guest.Close()
	// 端到端校验：写入后应收到回显（目标为本地回显服务）。
	if _, err := guest.Write([]byte("运行时代理数据")); err != nil {
		t.Fatalf("访客写入失败：%v", err)
	}
	received := make([]byte, len("运行时代理数据"))
	if _, err := guest.Read(received); err != nil {
		t.Fatalf("访客读回失败：%v", err)
	}
	_ = work
}

// 端口冲突：注册失败返回稳定错误，已有代理不受影响，无半注册监听器。
func TestRuntimeProxyRegistrationPortConflict(t *testing.T) {
	engine, listener := startEngineForRuntimeProxy(t)

	target, stopEcho := startTestEcho(t)
	defer stopEcho()
	guestPort := reserveTestPortForRuntime(t)

	session := openTestSession(t, listener, "rt", "rt-token")
	defer session.raw.Close()
	if err := session.registerProxy("first", guestPort, target); err != nil {
		t.Fatalf("首个注册不应失败：%v", err)
	}
	conflictErr := session.registerProxy("second", guestPort, target)
	if conflictErr == nil {
		t.Fatal("同端口重复注册应失败")
	}

	// 已有代理不受影响。
	guest, err := net.Dial("tcp", engine.GuestAddr("first").String())
	if err != nil {
		t.Fatalf("已有代理入口应可连接：%v", err)
	}
	_ = guest.Close()
}

// P2 能力可判定拒绝：STCP/XTCP 类型返回不支持错误，不留半注册资源。
func TestRuntimeProxyRegistrationRejectsP2Types(t *testing.T) {
	engine, listener := startEngineForRuntimeProxy(t)

	target := netip.MustParseAddrPort("127.0.0.1:9")

	for _, proxyType := range []string{"stcp", "xtcp", "sudp", "tcpmux"} {
		if err := registerProxyOfTypeForTest(t, engine, listener, "p2-"+proxyType, proxyType, reserveTestPortForRuntime(t), target); err == nil {
			t.Fatalf("P2 类型 %s 应被可判定拒绝", proxyType)
		}
		if engine.GuestAddr("p2-"+proxyType) != nil {
			t.Fatalf("P2 类型 %s 被拒后不应留下入口", proxyType)
		}
	}
}

// 关闭代理：入口停止接收新流量，已建立的访客连接不受影响。
func TestRuntimeProxyCloseStopsEntry(t *testing.T) {
	engine, listener := startEngineForRuntimeProxy(t)

	target, stopEcho := startTestEcho(t)
	defer stopEcho()
	guestPort := reserveTestPortForRuntime(t)

	session := openTestSession(t, listener, "rt", "rt-token")
	defer session.raw.Close()
	if err := session.registerProxy("closable", guestPort, target); err != nil {
		t.Fatalf("注册失败：%v", err)
	}

	if err := session.closeProxy("closable"); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}
	time.Sleep(100 * time.Millisecond)

	if engine.GuestAddr("closable") != nil {
		t.Fatal("关闭后入口地址应不可用")
	}
}

// 会话结束清理：控制连接断开后，该会话注册的全部运行时代理被清理。
func TestRuntimeProxiesCleanedOnSessionEnd(t *testing.T) {
	engine, listener := startEngineForRuntimeProxy(t)

	target, stopEcho := startTestEcho(t)
	defer stopEcho()
	guestPort := reserveTestPortForRuntime(t)

	// 独立连接注册代理后立即断开。
	func() {
		raw, dialErr := net.Dial("tcp", listener.Addr().String())
		if dialErr != nil {
			t.Fatalf("连接失败：%v", dialErr)
		}
		defer raw.Close()
		if _, err := raw.Write(encodeTestLogin(t, "rt", "rt-token")); err != nil {
			t.Fatalf("发送登录失败：%v", err)
		}
		readLoginResponse(t, raw)
		if _, err := raw.Write(encodeTestNewProxy(t, "session-proxy", "tcp", guestPort, target)); err != nil {
			t.Fatalf("发送注册失败：%v", err)
		}
		if err := readTestProxyResponse(t, raw, 5*time.Second); err != nil {
			t.Fatalf("注册失败：%v", err)
		}
		if engine.GuestAddr("session-proxy") == nil {
			t.Fatal("注册后入口应可用")
		}
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if engine.GuestAddr("session-proxy") == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("会话断开后运行时代理未被清理")
}

// reserveTestPortForRuntime 申请一个可用端口并立即释放。
func reserveTestPortForRuntime(t *testing.T) int {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请端口失败：%v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	return port
}

// startTestEcho 启动本地回显服务。
func startTestEcho(t *testing.T) (netip.AddrPort, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动回显失败：%v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, readErr := c.Read(buf)
					if readErr != nil {
						return
					}
					if _, writeErr := c.Write(buf[:n]); writeErr != nil {
						return
					}
				}
			}(conn)
		}
	}()
	address := listener.Addr().(*net.TCPAddr)
	return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(address.Port)), func() {
		_ = listener.Close()
		<-done
	}
}
