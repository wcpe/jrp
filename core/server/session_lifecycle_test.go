package server

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/wcpe/jrp/core/internal/wire"
)

// 心跳失活：超过失活窗口未收到任何心跳，服务端关闭会话。
//
// 失活窗口 = 心跳周期 × 3（startEngineForRuntimeProxy 配 300ms，收敛到
// 下限 1 秒）。连接被服务端关闭时客户端读侧应立即得到 EOF 而非超时。
func TestSessionLivenessClosesIdleConnection(t *testing.T) {
	engine, listener := startEngineForRuntimeProxy(t)
	_ = engine
	session := openTestSession(t, listener, "rt", "rt-token")
	defer session.raw.Close()

	_ = session.raw.SetReadDeadline(time.Now().Add(6 * time.Second))
	buffer := make([]byte, 1)
	_, readErr := session.raw.Read(buffer)
	if readErr == nil {
		t.Fatal("失活会话不应继续收到数据")
	}
	if isReadDeadline(readErr) {
		t.Fatal("失活窗口内会话未被服务端关闭（客户端读超时而非收到 EOF）")
	}
}

// 会话替换：同一客户端的新会话接管，旧会话被关闭且其运行时代理被清理；
// 旧会话的清理不误伤新会话注册的代理（FR-03 §3.4）。
func TestReplacementSessionTakesOver(t *testing.T) {
	engine, listener := startEngineForRuntimeProxy(t)
	target, stopEcho := startTestEcho(t)
	defer stopEcho()

	first := openTestSession(t, listener, "rt", "rt-token")
	defer first.raw.Close()
	firstPort := reserveTestPortForRuntime(t)
	if err := first.registerProxy("old-proxy", firstPort, target); err != nil {
		t.Fatalf("旧会话注册失败：%v", err)
	}

	// 新会话登录：服务端关闭旧会话。
	second := openTestSession(t, listener, "rt", "rt-token")
	defer second.raw.Close()

	_ = first.raw.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := first.raw.Read(make([]byte, 1)); err == nil {
		t.Fatal("旧会话应被服务端关闭")
	}

	// 旧会话的运行时代理随会话结束被清理。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if engine.GuestAddr("old-proxy") == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if engine.GuestAddr("old-proxy") != nil {
		t.Fatal("旧会话被替换后其运行时代理应被清理")
	}

	// 新会话可正常注册，且旧会话的清理不误伤新会话的注册。
	secondPort := reserveTestPortForRuntime(t)
	if err := second.registerProxy("new-proxy", secondPort, target); err != nil {
		t.Fatalf("新会话注册失败：%v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if engine.GuestAddr("new-proxy") == nil {
		t.Fatal("旧会话的清理误伤新会话注册的代理")
	}
}

// 过期会话：控制会话已结束时，其残留在途的工作连接声明被拒绝（FR-03 §7.5）。
func TestWorkDeclarationRejectedWithoutActiveSession(t *testing.T) {
	_, listener := startEngineForRuntimeProxy(t)

	// 凭证有效（rt/rt-token），但没有任何活跃控制会话。
	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("拨号失败：%v", err)
	}
	defer raw.Close()
	payload, err := json.Marshal(map[string]string{
		"client_id":   "rt",
		"token":       "rt-token",
		"proxy_name":  "whatever",
		"target_addr": "127.0.0.1:9",
		"run_id":      "whatever",
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

	// 被拒绝的连接应被服务端关闭：读侧得到 EOF（或平台相关的关闭错误），
	// 而不是超时——超时意味着连接仍开着、声明被静默接纳。
	_ = raw.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, readErr := raw.Read(make([]byte, 1))
	if readErr == nil {
		t.Fatal("无活跃会话的声明不应被接纳")
	}
	if isReadDeadline(readErr) {
		t.Fatal("无活跃会话的声明应被关闭连接（读超时说明连接仍开着）")
	}
}
