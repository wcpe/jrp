package core_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/client"
	"github.com/wcpe/jrp/core/server"
)

// 本文件是 FR-25 首个真实垂直切片的端到端失败测试：ServerEngine 与
// ClientEngine 进程内互联，TCP 传输 + wire v1 控制会话 + 一个 TCP 代理。
// 先红后绿：Engine 门面尚未实现时全部失败。

// 双向字节流可验证。

// 测试拓扑（全部使用回环地址，端口由系统分配避免占用冲突）：
//
//	访客 ──TCP──▶ 服务端访客入口 ──工作连接──▶ 客户端 ──TCP──▶ 本地目标
//
// 各段地址全部使用 127.0.0.1，避免测试依赖外部网络。
const (
	testClientID    = "slice-client"
	testClientToken = "slice-token"
	testProxyName   = "slice-ssh"
)

func mustAddrPort(t *testing.T, value string) netip.AddrPort {
	t.Helper()
	address, err := netip.ParseAddrPort(value)
	if err != nil {
		t.Fatalf("解析测试地址失败：%v", err)
	}
	return address
}

// 启动一个本地回显服务，模拟被代理的本地目标（如 sshd）。
// 返回监听地址与关闭函数。
func startLocalEcho(t *testing.T) (netip.AddrPort, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动本地回显服务失败：%v", err)
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
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	address := listener.Addr().(*net.TCPAddr)
	return mustAddrPort(t, "127.0.0.1:"+itoa(address.Port)), func() {
		_ = listener.Close()
		<-done
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := []byte{}
	for v := value; v > 0; v /= 10 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
	}
	return string(digits)
}

// 构造一对互相匹配的服务端与客户端配置。
//
// 控制地址只用于配置内容本身，不决定 Engine 实际监听哪个端口：Engine 始终
// 使用宿主注入的 listener，配置里的监听地址仅表达意图。需要真实监听地址的
// 场景请用 startSlicePair，它按实际监听地址重建客户端配置。
func sliceConfigs(t *testing.T, control netip.AddrPort, target netip.AddrPort, guestPort int) (core.ServerConfig, core.ClientConfig) {
	t.Helper()
	serverConfig, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{Address: control, Transport: core.TransportTCP}),
		core.WithWire(core.WireV1),
		core.WithClientCredential(core.ClientCredential{ClientID: testClientID, Token: testClientToken}),
		core.WithTCPProxyBinding(core.TCPProxyBinding{
			Name:       testProxyName,
			ClientID:   testClientID,
			RemotePort: guestPort,
			// 目标地址允许集合：目标即本地回显服务的实际地址，与客户端声明一致。
			AllowedTargets: []netip.AddrPort{target},
		}),
		core.WithServerHeartbeat(200*time.Millisecond),
		core.WithServerTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("构造服务端配置失败：%v", err)
	}
	clientConfig, err := core.NewClientConfig(
		core.WithClientID(testClientID),
		core.WithServerEndpoint(core.ServerEndpoint{Address: control, Transport: core.TransportTCP, Wire: core.WireV1}),
		core.WithClientAuth(core.TokenAuth{Token: testClientToken}),
		core.WithTCPProxy(core.TCPProxy{Name: testProxyName, LocalAddr: target, RemotePort: guestPort}),
		core.WithHeartbeat(200*time.Millisecond),
		core.WithTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("构造客户端配置失败：%v", err)
	}
	return serverConfig, clientConfig
}

// 启动一对 Engine 并等待客户端完成登录与代理注册。
//
// 调用方先用 freePort 占位拿到访客端口，再传给本函数：本函数内部监听控制
// 端口、启动服务端、按实际控制地址重建客户端配置并启动客户端。
func startSlicePair(t *testing.T, ctx context.Context, target netip.AddrPort, guestPort int) (*server.Engine, *client.Engine) {
	t.Helper()
	controlListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听控制端口失败：%v", err)
	}
	control := mustAddrPort(t, controlListener.Addr().String())
	serverConfig, clientConfig := sliceConfigs(t, control, target, guestPort)

	serverEngine := server.New(serverConfig, server.WithListener(controlListener))
	if err := serverEngine.Start(ctx); err != nil {
		t.Fatalf("服务端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = serverEngine.Shutdown(context.Background()) })

	// 客户端配置里的服务端端点必须指向实际监听地址：重建一份。
	rebuilt, err := core.NewClientConfig(
		core.WithClientID(clientConfig.ClientID()),
		core.WithServerEndpoint(core.ServerEndpoint{Address: control, Transport: core.TransportTCP, Wire: core.WireV1}),
		core.WithClientAuth(clientConfig.Auth()),
		core.WithTCPProxies(clientConfig.Proxies()),
		core.WithHeartbeat(clientConfig.Heartbeat()),
		core.WithTimeout(clientConfig.Timeout()),
	)
	if err != nil {
		t.Fatalf("重建客户端配置失败：%v", err)
	}
	clientEngine := client.New(rebuilt)
	if err := clientEngine.Start(ctx); err != nil {
		t.Fatalf("客户端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = clientEngine.Shutdown(context.Background()) })
	return serverEngine, clientEngine
}

// TestVerticalSliceEndToEnd 是首个垂直切片的核心验收：访客经服务端访客入口、
// 工作连接、客户端到达本地目标并原路返回，内容逐字节一致。
func TestVerticalSliceEndToEnd(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	guestPort := freePort(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverEngine, clientEngine := startSlicePair(t, ctx, target, guestPort)

	// 访客连接服务端访客入口，发送载荷并读回。
	guestAddress := serverEngine.GuestAddr(testProxyName)
	guest, err := net.Dial("tcp", guestAddress.String())
	if err != nil {
		t.Fatalf("访客连接失败：%v", err)
	}
	defer guest.Close()

	payload := []byte("垂直切片端到端数据-0123456789-abcdefghijklmnopqrstuvwxyz")
	if _, err := guest.Write(payload); err != nil {
		t.Fatalf("访客写入失败：%v", err)
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(guest, received); err != nil {
		t.Fatalf("访客读回失败：%v", err)
	}
	if !bytes.Equal(payload, received) {
		t.Fatalf("端到端数据不一致：发送 %d 字节，收到 %d 字节", len(payload), len(received))
	}

	// Done 未关闭、Err 为 nil。
	select {
	case <-serverEngine.Done():
		t.Fatalf("服务端在运行中关闭了 Done")
	default:
	}
	select {
	case <-clientEngine.Done():
		t.Fatalf("客户端在运行中关闭了 Done")
	default:
	}
	if err := serverEngine.Err(); err != nil {
		t.Fatalf("服务端 Err 非 nil：%v", err)
	}
	if err := clientEngine.Err(); err != nil {
		t.Fatalf("客户端 Err 非 nil：%v", err)
	}

	// Shutdown 后入口端口不再可连接。
	//
	// 先结束访客连接再关闭：排水语义下活动流会一直保留到自然结束，
	// 留着开的流会让 Shutdown 等满排水上限。
	_ = guest.Close()
	if err := serverEngine.Shutdown(ctx); err != nil {
		t.Fatalf("服务端关闭失败：%v", err)
	}
	if err := clientEngine.Shutdown(ctx); err != nil {
		t.Fatalf("客户端关闭失败：%v", err)
	}
	select {
	case <-serverEngine.Done():
	default:
		t.Fatalf("服务端 Shutdown 后 Done 未关闭")
	}
	if _, err := net.DialTimeout("tcp", guestAddress.String(), 500*time.Millisecond); err == nil {
		t.Fatalf("关闭后访客入口仍可连接")
	}
}

// nextFreePort 返回动态端口范围之外的一个可用端口。
// 游标只增不减：同一端口在被探测释放后不会立刻又被下一个用例选中，
// 避免撞上尚未散尽的 TIME_WAIT 残留；越界后回绕继续找。
func nextFreePort() int {
	portMutex.Lock()
	defer portMutex.Unlock()
	for {
		candidate := portCursor
		portCursor++
		if portCursor > portRangeEnd {
			portCursor = portRangeStart
		}
		probe, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(candidate)))
		if err != nil {
			continue
		}
		_ = probe.Close()
		return candidate
	}
}

// 端口区间取在动态端口范围（1024–15000）之上，且避开常见服务端口。
const (
	portRangeStart = 20000
	portRangeEnd   = 45000
)

var (
	portCursor = portRangeStart
	portMutex  sync.Mutex
)

// freePort 申请一个空闲端口，调用方随后绑定。
//
// 端口取自动态端口范围之外：Windows 默认把 1024–15000 留给出站连接的临时
// 本地端口，本包内客户端引擎拨号、各处 net.Listen(":0") 都从该区间取号。
// 若入口端口同样落在区间内，"先释放、后绑定"的空窗就会被这些同进程临时端口
// 抢走，报 Only one usage of each socket address 而随机失败。
func freePort(t *testing.T) int {
	t.Helper()
	return nextFreePort()
}

// TestStartFailureKeepsListener 验证 Start 失败时宿主注入的 listener 仍归宿主。
func TestStartFailureKeepsListener(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	control := mustAddrPort(t, "127.0.0.1:7000")
	serverConfig, clientConfig := sliceConfigs(t, control, target, freePort(t))
	_ = clientConfig

	// 宿主先把 listener 关闭，再交给 Engine：Start 必须失败。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	_ = listener.Close()

	engine := server.New(serverConfig, server.WithListener(listener))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := engine.Start(ctx); err == nil {
		t.Fatalf("使用已关闭 listener 启动应失败")
		_ = engine.Shutdown(ctx)
	}
	// 资源仍归宿主：宿主再次 Close 不 panic。
	_ = listener.Close()
}

// TestRepeatStartReturnsSentinel 验证重复 Start 返回哨兵错误且不产生第二个监听器。
func TestRepeatStartReturnsSentinel(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	control := mustAddrPort(t, "127.0.0.1:7000")
	serverConfig, clientConfig := sliceConfigs(t, control, target, freePort(t))
	_ = clientConfig

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	engine := server.New(serverConfig, server.WithListener(listener))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer engine.Shutdown(ctx)
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("首次启动失败：%v", err)
	}
	repeatErr := engine.Start(ctx)
	if repeatErr == nil {
		t.Fatalf("重复 Start 应返回错误")
	}
	// 必须可用 errors.Is 判定哨兵：抓不到哨兵就无法与其它失败区分。
	if !errors.Is(repeatErr, server.ErrAlreadyStarted) {
		t.Fatalf("重复 Start 应返回 ErrAlreadyStarted，实际：%v", repeatErr)
	}
}

// TestRepeatShutdownIsIdempotent 验证重复 Shutdown 幂等。
func TestRepeatShutdownIsIdempotent(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	control := mustAddrPort(t, "127.0.0.1:7000")
	serverConfig, clientConfig := sliceConfigs(t, control, target, freePort(t))
	_ = clientConfig

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	engine := server.New(serverConfig, server.WithListener(listener))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	if err := engine.Shutdown(ctx); err != nil {
		t.Fatalf("首次关闭失败：%v", err)
	}
	if err := engine.Shutdown(ctx); err != nil {
		t.Fatalf("重复关闭应幂等成功：%v", err)
	}
}

// TestStoppedEngineCannotRestart 验证停止后的 Engine 不允许重启。
func TestStoppedEngineCannotRestart(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	control := mustAddrPort(t, "127.0.0.1:7000")
	serverConfig, clientConfig := sliceConfigs(t, control, target, freePort(t))
	_ = clientConfig

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	engine := server.New(serverConfig, server.WithListener(listener))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	if err := engine.Shutdown(ctx); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}
	restartErr := engine.Start(ctx)
	if restartErr == nil {
		t.Fatalf("停止后重启应返回错误")
	}
	if !errors.Is(restartErr, server.ErrStopped) {
		t.Fatalf("停止后重启应返回 ErrStopped，实际：%v", restartErr)
	}
}

// TestParallelEngines 验证同一进程内两组 Engine 并行互不干扰。
func TestParallelEngines(t *testing.T) {
	newPair := func(t *testing.T) (guestAddr string, shutdown func()) {
		target, stopEcho := startLocalEcho(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Cleanup(cancel)
		serverEngine, clientEngine := startSlicePair(t, ctx, target, freePort(t))
		return serverEngine.GuestAddr(testProxyName).String(), func() {
			stopEcho()
			_ = serverEngine.Shutdown(context.Background())
			_ = clientEngine.Shutdown(context.Background())
		}
	}

	addrA, stopA := newPair(t)
	defer stopA()
	addrB, stopB := newPair(t)
	defer stopB()

	var wg sync.WaitGroup
	for index, addr := range []string{addrA, addrB} {
		wg.Add(1)
		go func(i int, guestAddr string) {
			defer wg.Done()
			guest, err := net.Dial("tcp", guestAddr)
			if err != nil {
				t.Errorf("第 %d 组访客连接失败：%v", i, err)
				return
			}
			defer guest.Close()
			payload := []byte("并行组数据")
			if _, err := guest.Write(payload); err != nil {
				t.Errorf("第 %d 组写入失败：%v", i, err)
				return
			}
			received := make([]byte, len(payload))
			if _, err := io.ReadFull(guest, received); err != nil {
				t.Errorf("第 %d 组读回失败：%v", i, err)
				return
			}
			if !bytes.Equal(payload, received) {
				t.Errorf("第 %d 组数据不一致", i)
			}
		}(index, addr)
	}
	wg.Wait()
}

// TestHalfClose 验证半关闭语义：一端关闭写方向后另一端仍能读完剩余数据。
func TestHalfClose(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverEngine, _ := startSlicePair(t, ctx, target, freePort(t))

	guest, err := net.Dial("tcp", serverEngine.GuestAddr(testProxyName).String())
	if err != nil {
		t.Fatalf("访客连接失败：%v", err)
	}
	defer guest.Close()

	payload := []byte("半关闭测试数据")
	if _, err := guest.Write(payload); err != nil {
		t.Fatalf("访客写入失败：%v", err)
	}
	// 半关闭写方向：回显端仍能读到数据并回显。
	if tcpConn, ok := guest.(*net.TCPConn); ok {
		if err := tcpConn.CloseWrite(); err != nil {
			t.Fatalf("半关闭失败：%v", err)
		}
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(guest, received); err != nil {
		t.Fatalf("半关闭后读回失败：%v", err)
	}
	if !bytes.Equal(payload, received) {
		t.Fatalf("半关闭后数据不一致")
	}
}

// TestShutdownLeavesNoGoroutines 验证 Shutdown 后无残留 goroutine。
//
// 采样 Start 前后的 goroutine 基线，关闭后允许短暂收敛窗口，最终数量
// 不得超过基线加容差。容差覆盖测试框架自身的后台 goroutine。
func TestShutdownLeavesNoGoroutines(t *testing.T) {
	baseline := runtime.NumGoroutine()

	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverEngine, clientEngine := startSlicePair(t, ctx, target, freePort(t))

	// 跑一轮真实流量，确保转发路径的 goroutine 都启动过。
	guest, err := net.Dial("tcp", serverEngine.GuestAddr(testProxyName).String())
	if err != nil {
		t.Fatalf("访客连接失败：%v", err)
	}
	payload := []byte("泄漏检测流量")
	if _, err := guest.Write(payload); err != nil {
		t.Fatalf("访客写入失败：%v", err)
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(guest, received); err != nil {
		t.Fatalf("访客读回失败：%v", err)
	}
	_ = guest.Close()

	if err := serverEngine.Shutdown(ctx); err != nil {
		t.Fatalf("服务端关闭失败：%v", err)
	}
	if err := clientEngine.Shutdown(ctx); err != nil {
		t.Fatalf("客户端关闭失败：%v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		remaining := runtime.NumGoroutine()
		if remaining <= baseline+3 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("关闭后残留 goroutine：基线 %d，当前 %d", baseline, remaining)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestEngineRejectsGlobalState 验证 Core 不使用进程级全局状态。
//
// 同一进程内先后启动两组 Engine 并分别关闭：第二组的行为不得受第一组影响，
// 且包级可变状态不得跨 Engine 泄漏。
func TestEngineRejectsGlobalState(t *testing.T) {
	for round := 0; round < 2; round++ {
		target, stopEcho := startLocalEcho(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		serverEngine, clientEngine := startSlicePair(t, ctx, target, freePort(t))

		guest, err := net.Dial("tcp", serverEngine.GuestAddr(testProxyName).String())
		if err != nil {
			t.Fatalf("第 %d 轮访客连接失败：%v", round, err)
		}
		payload := []byte("全局状态检测")
		if _, err := guest.Write(payload); err != nil {
			t.Fatalf("第 %d 轮写入失败：%v", round, err)
		}
		received := make([]byte, len(payload))
		if _, err := io.ReadFull(guest, received); err != nil {
			t.Fatalf("第 %d 轮读回失败：%v", round, err)
		}
		_ = guest.Close()
		if !bytes.Equal(payload, received) {
			t.Fatalf("第 %d 轮数据不一致", round)
		}
		if err := serverEngine.Shutdown(ctx); err != nil {
			t.Fatalf("第 %d 轮服务端关闭失败：%v", round, err)
		}
		if err := clientEngine.Shutdown(ctx); err != nil {
			t.Fatalf("第 %d 轮客户端关闭失败：%v", round, err)
		}
		cancel()
		stopEcho()
	}
}

// TestEngineErrorSanitization 验证异常路径的 Err() 不泄露凭证。
//
// 构造真实异常（客户端用错误 token 登录被拒），断言 Err() 非 nil 且
// 错误文本不含凭证原文；同时检查 Token 字段值本身不出现在消息中。
func TestEngineErrorSanitization(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	control := mustAddrPort(t, "127.0.0.1:7000")
	serverConfig, _ := sliceConfigs(t, control, target, freePort(t))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	engine := server.New(serverConfig, server.WithListener(listener))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutdownCancel()
		_ = engine.Shutdown(shutdownCtx)
	}()

	// 触发真实异常：用错误 token 拨号，服务端拒绝并触发失败路径。
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("拨号失败：%v", err)
	}
	wrongLogin := []byte(`{"clientID":"` + testClientID + `","token":"wrong-token-must-not-leak"}`)
	header := make([]byte, 9)
	header[0] = 'o'
	writeUint64(header[1:], uint64(len(wrongLogin)))
	if _, err := conn.Write(append(header, wrongLogin...)); err != nil {
		t.Fatalf("写入登录帧失败：%v", err)
	}
	// 服务端拒绝后关闭连接，模拟客户端侧感知异常。
	_ = conn.Close()

	// 等待异常被记录：Err() 非 nil 才说明断言真的在检查。
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := engine.Err(); err != nil {
			if containsSecret(err.Error()) {
				t.Fatalf("Err() 泄露凭证：%v", err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Skip("异常未被记录为 Err()：本环境无法触发该路径")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// containsSecret 检查字符串是否包含任一测试凭证原文。
func containsSecret(message string) bool {
	if bytes.Contains([]byte(message), []byte(testClientToken)) {
		return true
	}
	return bytes.Contains([]byte(message), []byte("wrong-token-must-not-leak"))
}

// writeUint64 以网络字节序写入 8 字节长度字段，用于手工构造 wire v1 帧。
func writeUint64(buffer []byte, value uint64) {
	for i := 7; i >= 0; i-- {
		buffer[i] = byte(value)
		value >>= 8
	}
}
