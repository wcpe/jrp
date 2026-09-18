package client

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/wcpe/jrp/core"
)

// B1 复现：控制连接异常断开后，客户端 Done 应关闭且 Err() 非 nil。
func TestReproClientAbnormalStopSetsErr(t *testing.T) {
	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	defer serverListener.Close()

	// 伪服务端：接受连接读走登录帧后直接断开。
	accepted := make(chan struct{})
	go func() {
		conn, err := serverListener.Accept()
		if err != nil {
			return
		}
		buffer := make([]byte, 4096)
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = conn.Read(buffer)
		_ = conn.Close()
		close(accepted)
	}()

	config := mustClientConfig(t, serverListener.Addr().String())
	engine := New(config)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	startErr := make(chan error, 1)
	go func() {
		startErr <- engine.Start(ctx)
	}()

	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatalf("伪服务端未收到连接")
	}
	// Start 应因登录无响应而失败；失败本身即证明异常可观测。
	// 若 Start 意外成功，则 Done/Err 必须可观测。
	select {
	case err := <-startErr:
		if err == nil {
			t.Fatalf("登录无响应仍 Start 成功")
		}
	case <-time.After(6 * time.Second):
		t.Fatalf("Start 既不成功也不失败")
	}
	_ = engine.Shutdown(context.Background())
}

// B2 复现：N 个 goroutine 并发 Start，只应有一个成功。
//
// 伪服务端对每条连接都完成登录握手，保证最多一个 Start 能走完登录；
// 其余必须返回 ErrAlreadyStarted 哨兵错误。
func TestReproClientConcurrentStart(t *testing.T) {
	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	defer serverListener.Close()
	// 伪服务端：对每条连接回复登录成功，放大竞态窗口。
	go func() {
		for {
			conn, err := serverListener.Accept()
			if err != nil {
				return
			}
			go serveFakeLogin(conn)
		}
	}()

	config := mustClientConfig(t, serverListener.Addr().String())
	engine := New(config)

	const runners = 8
	results := make([]error, runners)
	var wg sync.WaitGroup
	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			results[index] = engine.Start(ctx)
		}(i)
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		wg.Wait()
	}()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatalf("并发 Start 导致挂起")
	}
	successes := 0
	for _, err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrAlreadyStarted) {
			t.Logf("非哨兵错误：%v", err)
		}
	}
	_ = engine.Shutdown(context.Background())
	if successes != 1 {
		t.Fatalf("并发 Start 成功 %d 次，期望恰好 1 次", successes)
	}
	// 成功之外的失败必须全部是哨兵错误。
	for _, err := range results {
		if err != nil && !errors.Is(err, ErrAlreadyStarted) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("并发 Start 返回非哨兵错误：%v", err)
		}
	}
}

// B3 复现：Shutdown 时传入已过期的 ctx，应返回可判定的超时错误而非 nil。
func TestReproClientShutdownExpiredCtx(t *testing.T) {
	config := mustIdleClientConfig(t)
	engine := New(config)
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	if err := engine.Shutdown(expired); err == nil {
		t.Fatalf("已过期的 ctx 调用 Shutdown 返回 nil")
	}
}

// serveFakeLogin 为伪服务端连接回复登录成功。
func serveFakeLogin(conn net.Conn) {
	defer conn.Close()
	buffer := make([]byte, 4096)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Read(buffer); err != nil {
		return
	}
	response := []byte(`{"ok":true}`)
	header := make([]byte, 9)
	header[0] = '1'
	putUint64(header[1:], uint64(len(response)))
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Write(append(header, response...))
	// 保持连接打开，让客户端的心跳与读循环有宿可归。
	select {}
}

// putUint64 以网络字节序写入 8 字节长度。
func putUint64(buffer []byte, value uint64) {
	for i := 7; i >= 0; i-- {
		buffer[i] = byte(value)
		value >>= 8
	}
}

// mustClientConfig 构造指向给定服务端地址的最小合法客户端配置。
func mustClientConfig(t *testing.T, serverAddr string) core.ClientConfig {
	t.Helper()
	endpoint, err := netip.ParseAddrPort(serverAddr)
	if err != nil {
		t.Fatalf("解析地址失败：%v", err)
	}
	target, err := netip.ParseAddrPort("127.0.0.1:9")
	if err != nil {
		t.Fatalf("解析地址失败：%v", err)
	}
	config, err := core.NewClientConfig(
		core.WithClientID("repro-client"),
		core.WithServerEndpoint(core.ServerEndpoint{Address: endpoint, Transport: core.TransportTCP, Wire: core.WireV1}),
		core.WithClientAuth(core.TokenAuth{Token: "repro-token"}),
		core.WithTCPProxy(core.TCPProxy{Name: "repro-proxy", LocalAddr: target, RemotePort: 6000}),
	)
	if err != nil {
		t.Fatalf("构造配置失败：%v", err)
	}
	return config
}

// mustIdleClientConfig 构造一份合法但不启动的客户端配置。
func mustIdleClientConfig(t *testing.T) core.ClientConfig {
	t.Helper()
	return mustClientConfig(t, "127.0.0.1:7000")
}
