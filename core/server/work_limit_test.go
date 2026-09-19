package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wcpe/jrp/core/internal/wire"
)

// 访客暂存必须有界：超出上限的新访客被拒绝，已有访客不受影响。
//
// 回归用例：暂存队列此前无上限，连接洪水可耗尽文件描述符，进而让 Accept
// 返回 EMFILE 并被判为致命错误、异常终止整个引擎。
func TestPendingGuestsAreBounded(t *testing.T) {
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
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })

	guestAddr := engine.GuestAddr("repro-proxy").String()
	// 不启动客户端，因此没有待命工作连接：所有访客都会进入暂存队列。
	guests := make([]net.Conn, 0, maxPendingGuest+1)
	for index := 0; index <= maxPendingGuest; index += 1 {
		conn, dialErr := net.Dial("tcp", guestAddr)
		if dialErr != nil {
			t.Fatalf("第 %d 个访客拨号失败：%v", index+1, dialErr)
		}
		guests = append(guests, conn)
	}
	t.Cleanup(func() {
		for _, conn := range guests {
			_ = conn.Close()
		}
	})

	// 拒绝发生在服务端，拨号本身仍会成功（TCP 三次握手先于应用层判定），
	// 因此以计数而非拨号失败作为判定依据。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if engine.workConns.RejectedGuests() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rejected := engine.workConns.RejectedGuests(); rejected < 1 {
		t.Fatalf("超出暂存上限 %d 后应有访客被拒绝，实际拒绝 %d 个", maxPendingGuest, rejected)
	}

	// 暂存队列不得超过上限。
	engine.workConns.mu.Lock()
	queued := len(engine.workConns.guests["repro-proxy"])
	engine.workConns.mu.Unlock()
	if queued > maxPendingGuest {
		t.Fatalf("暂存队列不得超过上限 %d，实际 %d", maxPendingGuest, queued)
	}
}

// 被拒绝的访客连接必须由服务端关闭：既不暂存也不泄漏。
//
// 回归用例：parkGuest 此前用布尔值区分结果，调用方靠额外的 isStopped 判断决定
// 是否关闭连接。新增"超出上限"这一拒绝原因后，若调用方仍用旧判断，被拒连接
// 会既不被关闭也不被记账，形成真正的连接泄漏。
func TestRejectedGuestIsClosed(t *testing.T) {
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
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })

	guestAddr := engine.GuestAddr("repro-proxy").String()
	// 先填满暂存队列。
	filling := make([]net.Conn, 0, maxPendingGuest)
	for index := 0; index < maxPendingGuest; index += 1 {
		conn, dialErr := net.Dial("tcp", guestAddr)
		if dialErr != nil {
			t.Fatalf("填充第 %d 个访客失败：%v", index+1, dialErr)
		}
		filling = append(filling, conn)
	}
	defer func() {
		for _, conn := range filling {
			_ = conn.Close()
		}
	}()

	// 等待队列真正填满再拨下一个，避免与服务端处理竞态。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		engine.workConns.mu.Lock()
		queued := len(engine.workConns.guests["repro-proxy"])
		engine.workConns.mu.Unlock()
		if queued >= maxPendingGuest {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	rejected, err := net.Dial("tcp", guestAddr)
	if err != nil {
		t.Fatalf("被拒访客的拨号仍应成功：%v", err)
	}
	defer rejected.Close()

	// 服务端应主动关闭它：读操作返回 EOF 或错误，而不是一直等待。
	if err := rejected.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("设置读超时失败：%v", err)
	}
	buffer := make([]byte, 1)
	if _, err := rejected.Read(buffer); err == nil {
		t.Fatal("被拒绝的访客连接应由服务端关闭")
	} else if netError, ok := err.(net.Error); ok && netError.Timeout() {
		t.Fatal("被拒绝的访客连接不得悬挂等待")
	}
}

// 越权拒绝释放暂存访客时，必须同时撤销它们的活动记账。
//
// 回归用例：`dropGuests` 关闭访客但不从 `engine.conns` 移除，每释放一条就留下
// 一条已关闭连接的记账。触发条件是"客户端持续声明越权目标"——正是该释放逻辑
// 要防御的场景，于是上限封住了暂存队列，记账表却单调增长。
func TestDroppedGuestsAreUntracked(t *testing.T) {
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
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })

	guestAddr := engine.GuestAddr("repro-proxy").String()
	var guests []net.Conn
	for index := 0; index < 4; index += 1 {
		conn, dialErr := net.Dial("tcp", guestAddr)
		if dialErr != nil {
			t.Fatalf("第 %d 个访客拨号失败：%v", index+1, dialErr)
		}
		guests = append(guests, conn)
	}
	defer func() {
		for _, conn := range guests {
			_ = conn.Close()
		}
	}()

	// 等待访客进入暂存队列并完成记账。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		engine.workConns.mu.Lock()
		queued := len(engine.workConns.guests["repro-proxy"])
		engine.workConns.mu.Unlock()
		if queued >= 4 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	engine.mu.Lock()
	before := len(engine.conns)
	engine.mu.Unlock()
	if before < 4 {
		t.Fatalf("暂存访客应已登记活动记账，实际 %d 条", before)
	}

	dropped := engine.workConns.dropGuests("repro-proxy")
	if len(dropped) != 4 {
		t.Fatalf("应释放 4 条暂存访客，实际 %d 条", len(dropped))
	}
	for _, conn := range dropped {
		engine.untrack(conn)
	}

	engine.mu.Lock()
	after := len(engine.conns)
	engine.mu.Unlock()
	if after != before-4 {
		t.Fatalf("释放的访客必须撤销记账：释放前 %d 条，释放后 %d 条，期望 %d 条",
			before, after, before-4)
	}
}

// 越权拒绝路径必须撤销被释放访客的活动记账。
//
// 上一条用例只验证了配对中心返回的连接可被 untrack，不覆盖 Engine 的调用点：
// 调用点若忘记 untrack，测试仍会通过，而引擎长跑时记账表会随每次越权拒绝单调
// 增长。本用例走完整的越权拒绝路径，以活动记账条数作为判定依据。
func TestUnauthorizedTargetPathUntracksGuests(t *testing.T) {
	config, _ := mustServerConfigWithTarget(t)
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
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })

	guestAddr := engine.GuestAddr("repro-proxy").String()
	var guests []net.Conn
	for index := 0; index < 3; index += 1 {
		conn, dialErr := net.Dial("tcp", guestAddr)
		if dialErr != nil {
			t.Fatalf("第 %d 个访客拨号失败：%v", index+1, dialErr)
		}
		guests = append(guests, conn)
	}
	defer func() {
		for _, conn := range guests {
			_ = conn.Close()
		}
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		engine.workConns.mu.Lock()
		queued := len(engine.workConns.guests["repro-proxy"])
		engine.workConns.mu.Unlock()
		if queued >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 发一条目标越权的工作连接声明：目标不在允许集合内，服务端会拒绝并释放访客。
	assertGuestDropUntracks(t, engine, listener.Addr().String())
}

// assertGuestDropUntracks 发送越权声明并断言访客的记账被完整撤销。
func assertGuestDropUntracks(t *testing.T, engine *Engine, controlAddr string) {
	t.Helper()
	engine.mu.Lock()
	before := len(engine.conns)
	engine.mu.Unlock()

	raw, err := net.Dial("tcp", controlAddr)
	if err != nil {
		t.Fatalf("连接控制端口失败：%v", err)
	}
	defer raw.Close()
	payload, err := json.Marshal(map[string]string{
		"client_id":   "repro",
		"token":       "repro-token",
		"proxy_name":  "repro-proxy",
		"target_addr": "127.0.0.1:9",
	})
	if err != nil {
		t.Fatalf("序列化声明失败：%v", err)
	}
	frame, err := wire.EncodeV1Frame(wire.Frame{Type: wire.MessageTypeNewWorkConn, Payload: payload})
	if err != nil {
		t.Fatalf("编码声明失败：%v", err)
	}
	if _, err := raw.Write(frame); err != nil {
		t.Fatalf("发送声明失败：%v", err)
	}

	// 等待访客被释放（队列清空）后检查记账。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		engine.workConns.mu.Lock()
		queued := len(engine.workConns.guests["repro-proxy"])
		engine.workConns.mu.Unlock()
		if queued == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 释放是异步的：给调用点留出撤销记账的时间。
	time.Sleep(50 * time.Millisecond)
	engine.mu.Lock()
	after := len(engine.conns)
	engine.mu.Unlock()
	// 允许控制连接自身占一条记账。
	if after > before-3+1 {
		t.Fatalf("越权拒绝释放访客后记账未撤销：释放前 %d 条，释放后 %d 条", before, after)
	}
}

// 容量拒绝必须是可见事件：既有日志，也有宿主可读的计数。
//
// 回归用例：拒绝路径此前既不写日志，计数也只由一个非导出接收者的方法暴露
// （`workBroker` 全小写，`Engine` 无转发访问器），宿主既看不到也读不到。
// 结果是"访客连不上"会被当成客户端问题，而真实原因是服务端到达了承接上限。
func TestCapacityRejectionIsObservable(t *testing.T) {
	config, _ := mustServerConfigWithTarget(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	logs := &syncLogWriter{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	engine := New(config, WithListener(listener), WithLogger(logger))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })

	guestAddr := engine.GuestAddr("repro-proxy").String()
	// 不启动客户端，因此没有待命工作连接：访客全部进入暂存队列。
	var guests []net.Conn
	for index := 0; index <= maxPendingGuest; index += 1 {
		conn, dialErr := net.Dial("tcp", guestAddr)
		if dialErr != nil {
			t.Fatalf("第 %d 个访客拨号失败：%v", index+1, dialErr)
		}
		guests = append(guests, conn)
	}
	defer func() {
		for _, conn := range guests {
			_ = conn.Close()
		}
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if engine.RejectedGuests() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rejected := engine.RejectedGuests(); rejected < 1 {
		t.Fatalf("宿主应能读到拒绝计数，实际 %d", rejected)
	}
	// 日志应说明拒绝原因，且含代理名以便定位。
	// 拒绝发生在各访客各自的 goroutine 里，等待日志落地再断言。
	var output string
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		output = logs.String()
		if strings.Contains(output, "上限") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(output, "上限") {
		t.Fatalf("容量拒绝应写入日志，实际日志：%q（拒绝计数 %d）", output, engine.RejectedGuests())
	}
	if !strings.Contains(output, "repro-proxy") {
		t.Fatalf("拒绝日志应指明代理，实际日志：%q", output)
	}
}

// syncLogWriter 是并发安全的日志收集器。
//
// 容量拒绝由多个访客 goroutine 各自触发，日志处理器会被并发写入；
// bytes.Buffer 不保证并发安全，直接用它收集会产生数据竞争并丢失内容。
type syncLogWriter struct {
	mu      sync.Mutex
	content strings.Builder
}

func (writer *syncLogWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.content.Write(data)
}

func (writer *syncLogWriter) String() string {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.content.String()
}
