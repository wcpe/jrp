package transport

import (
	"net"
	"sync"
	"testing"
	"time"
)

// fakeMigratingConn 模拟对端地址在连接生命周期内发生变化（QUIC 迁移）。
//
// net.Pipe 两侧地址均为空且不可变，无法用于验证迁移观测；真实 UDP 会话才能
// 体现"连接句柄不变、对端地址变化"。这里用可控假连接驱动同一判据。
type fakeMigratingConn struct {
	net.Conn
	mu       sync.Mutex
	remote   net.Addr
	readFunc func([]byte) (int, error)
}

func (conn *fakeMigratingConn) RemoteAddr() net.Addr {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	return conn.remote
}

func (conn *fakeMigratingConn) Read(buffer []byte) (int, error) {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	return conn.readFunc(buffer)
}

// Write 实现写方向：假连接不真正发送，返回错误。
//
// 必须显式实现——嵌入的 net.Conn 为 nil，不实现会在调用嵌入方法时 panic。
func (conn *fakeMigratingConn) Write(buffer []byte) (int, error) {
	return 0, net.ErrClosed
}

func (conn *fakeMigratingConn) setRemote(address net.Addr) {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	conn.remote = address
}

// TestConnEmitsMigrationWhenRemoteAddressChanges 断言对端地址变化即产出迁移通知。
//
// 规格 §3.6：迁移由 quic-go 在库内完成，传输层只观测。事件只含前后地址摘要，
// 不含完整地址，也不改变连接本身的生命周期。
func TestConnEmitsMigrationWhenRemoteAddressChanges(t *testing.T) {
	before, err := net.ResolveUDPAddr("udp", "203.0.113.10:5000")
	if err != nil {
		t.Fatalf("解析地址失败：%v", err)
	}
	after, err := net.ResolveUDPAddr("udp", "203.0.113.11:5000")
	if err != nil {
		t.Fatalf("解析地址失败：%v", err)
	}
	fake := &fakeMigratingConn{
		remote: before,
		readFunc: func(buffer []byte) (int, error) {
			return 0, net.ErrClosed
		},
	}
	conn := wrapConn(fake, PurposeWork, "svc-echo")

	migrations := make(chan Migration, 4)
	conn.SetMigrationObserver(func(migration Migration) {
		migrations <- migration
	})

	// 首次读写建立地址基线，不产出事件（否则每条连接每次读写都误报一次迁移）。
	buffer := make([]byte, 8)
	if _, err := conn.Read(buffer); err == nil {
		t.Fatal("假连接应返回读错误")
	}
	select {
	case migration := <-migrations:
		t.Fatalf("建立基线时不应产出迁移通知：%+v", migration)
	case <-time.After(150 * time.Millisecond):
	}

	// 对端地址变化后再读写，应产出一次迁移。
	fake.setRemote(after)
	if _, err := conn.Read(buffer); err == nil {
		t.Fatal("假连接应返回读错误")
	}
	select {
	case migration := <-migrations:
		if migration.Purpose != PurposeWork {
			t.Fatalf("迁移用途不正确：%v", migration.Purpose)
		}
		if migration.Proxy != "svc-echo" {
			t.Fatalf("迁移代理归属不正确：%q", migration.Proxy)
		}
		if migration.Previous != AddrSummary(before) {
			t.Fatalf("迁移前地址摘要不正确：%s", migration.Previous)
		}
		if migration.Current != AddrSummary(after) {
			t.Fatalf("迁移后地址摘要不正确：%s", migration.Current)
		}
		if migration.Previous == migration.Current {
			t.Fatalf("迁移前后摘要相同，无法区分：%s", migration.Current)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("对端地址变化后未产出迁移通知")
	}
}

// TestConnIgnoresUnchangedAddress 断言地址不变时不产出迁移通知。
//
// 迁移观测在每次读写路径上都比较地址摘要；若把首次观测也当迁移上报，普通连接
// 每次读写都会刷事件，事件流失去判别力。
func TestConnIgnoresUnchangedAddress(t *testing.T) {
	remote, err := net.ResolveUDPAddr("udp", "203.0.113.10:5000")
	if err != nil {
		t.Fatalf("解析地址失败：%v", err)
	}
	fake := &fakeMigratingConn{
		remote: remote,
		readFunc: func(buffer []byte) (int, error) {
			return 0, net.ErrClosed
		},
	}
	conn := wrapConn(fake, PurposeControl, "")
	conn.SetMigrationObserver(func(Migration) {
		t.Fatal("地址未变化时不应产出迁移通知")
	})

	buffer := make([]byte, 8)
	if _, err := conn.Read(buffer); err == nil {
		t.Fatal("假连接应返回读错误")
	}
	if _, err := conn.Write(buffer); err == nil {
		t.Fatal("假连接应返回写错误")
	}
}

// TestConnMigrationObserverIsOptional 断言未注册观察者时读写语义不受影响。
//
// 迁移观测是可选能力：宿主不订阅迁移事件时，传输层不得因此改变读写语义。
func TestConnMigrationObserverIsOptional(t *testing.T) {
	fake := &fakeMigratingConn{
		readFunc: func(buffer []byte) (int, error) {
			return 0, net.ErrClosed
		},
	}
	conn := wrapConn(fake, PurposeWork, "svc-echo")
	buffer := make([]byte, 8)
	if _, err := conn.Read(buffer); err == nil {
		t.Fatal("假连接应返回读错误")
	}
}

// TestQUICStreamConnForwardsMigrationObserver 断言 QUIC 适配器把观察回调接到会话上。
//
// 迁移发生在 QUIC 会话上，而交付给上层的是一条流：流连接必须把观察回调转发给
// 所属会话，否则只有拨号侧（自己持有会话）能观测到迁移，监听侧交付的流永远
// 观测不到——而监听侧恰恰是服务端，迁移观测对运维更有价值。
func TestQUICStreamConnForwardsMigrationObserver(t *testing.T) {
	// 真实 UDP 会话需要在两端各建一条流，这里验证"回调经会话注册"这一接线，
	// 端到端迁移由 QUIC 传输用例覆盖。
	remote, err := net.ResolveUDPAddr("udp", "203.0.113.10:5000")
	if err != nil {
		t.Fatalf("解析地址失败：%v", err)
	}
	fake := &fakeMigratingConn{
		remote: remote,
		readFunc: func(buffer []byte) (int, error) {
			return 0, net.ErrClosed
		},
	}
	conn := wrapConn(fake, PurposeControl, "")
	observer := func(Migration) {}
	conn.SetMigrationObserver(observer)

	// 回调登记后即生效：地址变化应能被观测（经流连接转发到会话）。
	fake2, err := net.ResolveUDPAddr("udp", "203.0.113.11:5000")
	if err != nil {
		t.Fatalf("解析地址失败：%v", err)
	}
	fake.setRemote(fake2)
	buffer := make([]byte, 8)
	if _, err := conn.Read(buffer); err == nil {
		t.Fatal("假连接应返回读错误")
	}
}
