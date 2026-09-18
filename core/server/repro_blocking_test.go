package server

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/wcpe/jrp/core"
)

// B1 复现：控制连接异常断开后，Err() 应返回非 nil 且 Done 应关闭。
// 当前行为：Err() 恒 nil，Done 在 Shutdown 前不关闭。
func TestReproServerAbnormalStopSetsErr(t *testing.T) {
	config := mustServerConfig(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	engine := New(config, WithListener(listener))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	// 模拟控制连接异常：直接拨号后立即断开。
	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("拨号失败：%v", err)
	}
	_ = raw.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case <-engine.Done():
			goto CHECKED
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("复现确认：异常连接断开后 Done 未关闭（当前行为）")
		}
		time.Sleep(20 * time.Millisecond)
	}
CHECKED:
	if err := engine.Err(); err == nil {
		t.Fatalf("复现确认 B1：Done 已关闭但 Err() 为 nil（当前行为）")
	}
	_ = engine.Shutdown(context.Background())
}

// B2 复现：N 个 goroutine 并发 Start，只应有一个成功。
func TestReproServerConcurrentStart(t *testing.T) {
	config := mustServerConfig(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	defer listener.Close()
	engine := New(config, WithListener(listener))

	const runners = 8
	results := make([]error, runners)
	var wg sync.WaitGroup
	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			results[index] = engine.Start(ctx)
		}(i)
	}
	wg.Wait()

	successes := 0
	for _, err := range results {
		if err == nil {
			successes++
		}
	}
	_ = engine.Shutdown(context.Background())
	if successes != 1 {
		t.Fatalf("复现确认 B2：并发 Start 成功 %d 次，期望恰好 1 次（当前行为）", successes)
	}
}

// B3 复现：Shutdown 时传入已过期的 ctx，应返回可判定的超时错误而非 nil。
func TestReproServerShutdownExpiredCtx(t *testing.T) {
	config := mustServerConfig(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	engine := New(config, WithListener(listener))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	defer engine.Shutdown(context.Background())

	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	if err := engine.Shutdown(expired); err == nil {
		t.Fatalf("复现确认 B3：已过期的 ctx 调用 Shutdown 返回 nil（当前行为）")
	}
}

// B5 复现：配对桥接后 Shutdown，goroutine 应回到基线。
func TestReproServerBridgeShutdownNoLeak(t *testing.T) {
	config, target := mustServerConfigWithTarget(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	engine := New(config, WithListener(listener))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	_ = target
	if err := engine.Shutdown(ctx); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}
}

// mustServerConfig 构造最小合法服务端配置，访客端口由系统分配避免占用冲突。
func mustServerConfig(t *testing.T) core.ServerConfig {
	t.Helper()
	listen, err := netip.ParseAddrPort("127.0.0.1:7000")
	if err != nil {
		t.Fatalf("解析地址失败：%v", err)
	}
	guestPort := freeReproPort(t)
	config, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{Address: listen, Transport: core.TransportTCP}),
		core.WithWire(core.WireV1),
		core.WithClientCredential(core.ClientCredential{ClientID: "repro", Token: "repro-token"}),
		core.WithTCPProxyBinding(core.TCPProxyBinding{Name: "repro-proxy", ClientID: "repro", RemotePort: guestPort}),
	)
	if err != nil {
		t.Fatalf("构造配置失败：%v", err)
	}
	return config
}

// freeReproPort 申请一个空闲端口后立即释放，调用方随后绑定。
func freeReproPort(t *testing.T) int {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请端口失败：%v", err)
	}
	defer probe.Close()
	return probe.Addr().(*net.TCPAddr).Port
}

// mustServerConfigWithTarget 构造带本地目标的配置（B5 用）。
func mustServerConfigWithTarget(t *testing.T) (core.ServerConfig, string) {
	t.Helper()
	return mustServerConfig(t), "127.0.0.1:9"
}
