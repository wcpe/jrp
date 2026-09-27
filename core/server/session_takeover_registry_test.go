package server

import (
	"net"
	"testing"

	"github.com/wcpe/jrp/core/internal/transport"
)

// 会话接管不得抹掉新会话的官方会话登记。
//
// 复现路径：客户端重连（或客户端进程重启）时，新会话先完成登录登记，被接管的
// 旧连接随后才执行自己的清理；若清理不校验登记归属，就会把新会话的写出通道与
// 运行 ID 索引一并删除，表现为「登录与代理注册都成功，但访客永远等不到工作连接」。
// 真实链路（反向代理 + 双 wire 连续执行）已复现过这一现象。
func TestUntrackControlKeepsNewSessionRegistration(t *testing.T) {
	engine := New(mustServerConfig(t))

	oldConn := dialTransportConnForTest(t)
	newConn := dialTransportConnForTest(t)

	engine.mu.Lock()
	engine.controlConns[oldConn] = struct{}{}
	engine.controlConns[newConn] = struct{}{}
	engine.mu.Unlock()

	// 旧会话先登录（登记索引与官方会话），随后新会话登录并接管。
	engine.bindControlClient(oldConn, "client-a")
	engine.registerOfficialSession("client-a", sessionWriter{conn: oldConn, version: "v1"}, "run-old")
	if replaced := engine.bindControlClient(newConn, "client-a"); replaced != oldConn {
		t.Fatalf("接管应返回旧连接，实际 %v", replaced)
	}
	engine.registerOfficialSession("client-a", sessionWriter{conn: newConn, version: "v1"}, "run-new")

	// 旧连接此时才退出：不得影响新会话的登记。
	engine.untrackControl(oldConn)

	if _, _, ok := engine.sessionByRunID("run-new"); !ok {
		t.Fatal("旧会话清理后，新会话的运行 ID 索引被误删")
	}
	if _, _, ok := engine.sessionByRunID("run-old"); ok {
		t.Fatal("旧会话的运行 ID 索引应被清理")
	}
}

// dialTransportConnForTest 建立一条真实的回环连接并封装为带用途标记的句柄。
func dialTransportConnForTest(t *testing.T) *transport.Conn {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听回环端口失败：%v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("回环连接失败：%v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	handle := transport.TakeOverListener(listener)
	t.Cleanup(func() { _ = handle.Release() })
	conn, _, err := handle.Accept(transport.PurposeControl)
	if err != nil {
		t.Fatalf("接受回环连接失败：%v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
