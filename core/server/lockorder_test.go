package server

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// Shutdown 与访客配对并发执行不得死锁。
//
// 回归用例：`stopListenersLocked` 在持有 Engine 锁时调用 broker 的 closeStaged
// （取 broker 锁），而 broker 的配对路径曾在持有自身锁时调用 track（取 Engine
// 锁）——两条相反的锁序构成 ABBA 死锁。致命之处在于 waitDrained 的兜底
// closeConns 同样要取 Engine 锁，因此连"排水超限强制关闭"这条兜底路径也会锁死。
//
// 触发需要同时满足两点：Shutdown 走到持 Engine 锁调 closeStaged 的窗口，且有
// 另一条路径正持 broker 锁调用 track。本用例用独立 goroutine 同时压这两条边，
// 并以超时保护把"死锁"判定为失败而不是挂死整个测试进程。
func TestShutdownDoesNotDeadlockWithGuestPairing(t *testing.T) {
	config := mustServerConfig(t)
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
	guestAddr := engine.GuestAddr("repro-proxy").String()

	// 一边持续制造配对压力：先暂存访客，再放入待命工作连接促使配对发生。
	// 配对路径是本用例要压的那条锁边。
	stopPressure := make(chan struct{})
	pressureDone := make(chan struct{})
	go func() {
		defer close(pressureDone)
		for {
			select {
			case <-stopPressure:
				return
			default:
			}
			conn, dialErr := net.Dial("tcp", guestAddr)
			if dialErr != nil {
				return
			}
			// 给服务端时间把访客暂存，随后关闭制造队列变动。
			time.Sleep(200 * time.Microsecond)
			_ = conn.Close()
		}
	}()

	time.Sleep(20 * time.Millisecond)

	// 另一边执行 Shutdown：它会走 stopListenersLocked → closeStaged 这条锁边。
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		shutdownDone <- engine.Shutdown(shutdownCtx)
	}()

	select {
	case <-shutdownDone:
		// 返回即说明两条锁边未互锁。
	case <-time.After(8 * time.Second):
		close(stopPressure)
		<-pressureDone
		t.Fatal("Shutdown 与访客配对并发时死锁：Shutdown 未在 8 秒内返回")
	}
	close(stopPressure)
	<-pressureDone
}

// broker 的锁内代码不得调用 Engine 的登记/注销方法。
//
// 这是对锁序的静态守护，与上面的并发压测互补：并发用例依赖时序窗口，可能在某次
// 运行中错过；而锁序一旦反向就是确定的死锁隐患，必须每次构建都拦住。
//
// 实现方式：读取 work.go 源码，取出 broker 自身的加锁临界区（从 mu.Lock() 到
// 匹配的 mu.Unlock()），断言其中不出现 track/untrack/add 调用。这些回调都会取
// Engine 锁，出现在 broker 锁内即构成与 stopListenersLocked 的 ABBA。
func TestBrokerCriticalSectionDoesNotCallEngine(t *testing.T) {
	source, err := os.ReadFile("work.go")
	if err != nil {
		t.Fatalf("读取 work.go 失败：%v", err)
	}
	calls := findBrokerLockedCalls(t, string(source))
	for _, call := range calls {
		t.Errorf("broker 锁内调用了 %s：该方法会取 Engine 锁，与 "+
			"stopListenersLocked（持 Engine 锁调 closeStaged）构成 ABBA 死锁。"+
			"应改为把配对结果返回给调用方，由调用方在锁外完成登记。", call)
	}
}

// findBrokerLockedCalls 返回 broker 方法内出现的 Engine 回调调用。
//
// 判定单位是**函数**而不是"Lock 到第一个 Unlock 之间的文本"：work.go 的加锁
// 统一写成 `Lock()` 紧跟 `defer Unlock()`，按文本截取会把临界区错误地截成两行，
// 从而漏掉函数体后半段的调用（本检测的第一版就因此漏检过一次真实的 ABBA 变异）。
// 以函数为界既不会误判 defer 形式，也不会因为解锁写法变化而失效。
func findBrokerLockedCalls(t *testing.T, source string) []string {
	t.Helper()
	const lockToken = "broker.mu.Lock()"
	// 会取 Engine 锁的回调名。
	engineCallbacks := []string{"track(", "untrack(", "add("}

	// 按函数切分：以 "func " 开头的行作为每个函数体的起点。
	blocks := splitTopLevelFunctions(source)
	var found []string
	for _, block := range blocks {
		if !strings.Contains(block, lockToken) {
			continue
		}
		header := firstLine(block)
		for _, callback := range engineCallbacks {
			if strings.Contains(block, callback) {
				found = append(found, callback+" 于 "+header)
			}
		}
	}
	return found
}

// splitTopLevelFunctions 把源码按顶层函数声明切分成若干块。
func splitTopLevelFunctions(source string) []string {
	lines := strings.Split(source, "\n")
	var blocks []string
	var current []string
	flushing := false
	for _, line := range lines {
		if strings.HasPrefix(line, "func ") {
			if flushing {
				blocks = append(blocks, strings.Join(current, "\n"))
			}
			current = nil
			flushing = true
		}
		if flushing {
			current = append(current, line)
		}
	}
	if flushing {
		blocks = append(blocks, strings.Join(current, "\n"))
	}
	return blocks
}

// firstLine 返回块的首行，用于在错误信息里指明是哪个函数。
func firstLine(block string) string {
	if index := strings.Index(block, "\n"); index >= 0 {
		return block[:index]
	}
	return block
}
