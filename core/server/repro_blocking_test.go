package server

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/wcpe/jrp/core"
)

// B1（FR-03 修订）：控制连接异常断开不停止引擎。
//
// 原语义（FR-25 垂直切片）：单条控制连接断开 = 引擎异常终止（Done 关闭、
// Err 非 nil）。FR-03 引入多客户端语义后该行为已修订（见 §3.4/§3.5）：
// 会话级错误只关闭本会话，引擎停止只由 Accept 致命失败与 Shutdown 触发——
// 否则一个客户端掉线会杀死全部会话，官方 frpc 的断线重连会反复触发停机。
func TestSessionDropDoesNotStopEngine(t *testing.T) {
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

	// 会话断开后引擎必须保持运行：Done 不关闭、Err 保持 nil。
	time.Sleep(300 * time.Millisecond)
	select {
	case <-engine.Done():
		t.Fatalf("会话断开不应停止引擎，实际 Done 已关闭：%v", engine.Err())
	default:
	}
	if err := engine.Err(); err != nil {
		t.Fatalf("会话断开后 Err() 应为 nil，实际：%v", err)
	}
	// 正常 Shutdown 后 Err() 仍为 nil（正常路径不受此路径污染）。
	if err := engine.Shutdown(context.Background()); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}
	if err := engine.Err(); err != nil {
		t.Fatalf("正常 Shutdown 后 Err() 应为 nil，实际：%v", err)
	}
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
		core.WithClientCredential(core.ClientCredential{ClientID: "repro", Token: DigestToken("repro-token")}),
		core.WithTCPProxyBinding(core.TCPProxyBinding{
			Name:       "repro-proxy",
			ClientID:   "repro",
			RemotePort: guestPort,
			// 该用例不承载数据转发，允许集合取自身环回地址即可满足必填约束。
			AllowedTargets: []netip.AddrPort{listen},
		}),
	)
	if err != nil {
		t.Fatalf("构造配置失败：%v", err)
	}
	return config
}

// freeReproPort 申请一个空闲端口后立即释放，调用方随后绑定。
// freeReproPort 从固定测试端口区间取号。
//
// 此前用 ":0 申请后立即释放"，释放到真正绑定之间存在窗口：并行运行的
// 其他包（含各自引擎用例的出站临时端口分配）可能恰好占住该端口，CI 上
// 偶发 bind: address already in use（Web 构建矩阵的 core/server 已实测）。
// 改为从三平台动态端口范围之外的固定区间游标取号，与 core 根包测试的
// freePort 助手同一策略，不依赖释放-重绑的时序。
// 使用独立测试端口区间：30001–31000 落在三平台动态端口范围之外
// （Windows 1024–15000、Linux 32768–60999、macOS 49152–65535），
// 并与 core 根包的 20000–30000 区间隔离。游标只增不减并加锁，保证并发取号不重复。
var (
	reproPortMutex  sync.Mutex
	reproPortCursor = 30001
)

const reproPortRangeEnd = 31000

func freeReproPort(t *testing.T) int {
	t.Helper()
	reproPortMutex.Lock()
	defer reproPortMutex.Unlock()
	for {
		candidate := reproPortCursor
		reproPortCursor++
		if reproPortCursor > reproPortRangeEnd {
			reproPortCursor = 30001
		}
		probe, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(candidate)))
		if err != nil {
			continue
		}
		_ = probe.Close()
		return candidate
	}
}

// mustServerConfigWithTarget 构造带本地目标的配置（B5 用）。
func mustServerConfigWithTarget(t *testing.T) (core.ServerConfig, string) {
	t.Helper()
	return mustServerConfig(t), "127.0.0.1:9"
}
