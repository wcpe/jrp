package server

import (
	"context"
	"net"
	"testing"
	"time"
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
