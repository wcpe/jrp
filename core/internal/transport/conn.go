package transport

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Purpose 标记一条连接的建链用途。
//
// 用途在建链时确定且不可变更：控制连接不承载用户数据，工作连接只服务绑定的
// 代理。差异只在生命周期归属，不体现在读写语义上。
type Purpose string

const (
	// PurposeControl 是控制连接用途。
	PurposeControl Purpose = "control"
	// PurposeWork 是工作连接用途。
	PurposeWork Purpose = "work"
)

// MigrationObserver 接收一条连接的对端地址迁移通知（规格 §3.6）。
//
// 只用于观测：对端身份由会话层鉴权决定，迁移事件不参与身份判定，因此地址伪装
// 无法被当作合法身份。回调在传输包的读写路径上同步触发，不得阻塞或返回错误。
type MigrationObserver func(Migration)

// migratable 是底层连接对迁移观测的支持接口。
//
// 只有 QUIC 连接实现它（quicStreamConn）；TCP/WebSocket 连接的对端地址在连接
// 生命周期内固定，无需实现。这样 Conn 不需要按传输类型分支，类型隔离仍由
// 传输包内部消化。
type migratable interface {
	SetMigrationObserver(MigrationObserver)
}

// Migration 描述一条连接的对端地址变化，载荷只含脱敏后的地址摘要。
type Migration struct {
	// Purpose 是发生迁移的连接用途。
	Purpose Purpose
	// Proxy 是工作连接绑定的代理名；控制连接为空字符串。
	Proxy string
	// Previous 是迁移前的对端地址摘要。
	Previous string
	// Current 是迁移后的对端地址摘要。
	Current string
}

// Conn 是一条传输连接的句柄。
//
// 它在 net.Conn 之上附加三件少吃状态：用途标记、代理归属与建链时间。
// 读写与关闭语义与 net.Conn 完全一致；Close 幂等，重复调用返回同一结果。
type Conn struct {
	net.Conn
	purpose       Purpose
	proxy         string
	establishedAt time.Time

	closeOnce sync.Once
	closeErr  error

	// received 记录本连接收到的下行字节数（原子累加）。
	//
	// 工作连接的下行字节即访客数据：声明后的待命连接在服务端配对之前不会有
	// 任何下行字节，因此「received == 0」是"未承载访客数据"的协议层判据，
	// 供客户端换代排水区分待命桥接与活动桥接。
	received atomic.Int64

	// migrationObserver 是对端地址迁移的观察回调，可为 nil（不观测）。
	//
	// 只有 QUIC 会出现迁移：TCP/WebSocket 的地址在连接生命周期内固定。
	migrationObserver MigrationObserver
	// lastRemote 是上次观测到的对端地址摘要，用于判定是否发生变化。
	lastRemote string
	// hasBaseline 表示是否已建立地址基线。
	//
	// 首次观测只建立基线不上报：否则每条连接的每次读写都会刷一次"迁移"，
	// 事件流失去判别力。
	hasBaseline bool
	// migrationMu 保护迁移观测状态。
	//
	// 迁移观测在读写路径上触发，而一条连接的读写可能来自多个 goroutine
	//（如心跳写与数据写并发），无锁会构成数据竞争。
	migrationMu sync.Mutex
}

// wrapConn 把一条标准库连接封装为带用途标记的连接句柄。
//
// proxy 只在用途为工作连接时有意义；控制连接必须传空字符串。
func wrapConn(conn net.Conn, purpose Purpose, proxy string) *Conn {
	if purpose == PurposeControl {
		proxy = ""
	}
	return &Conn{
		Conn:          conn,
		purpose:       purpose,
		proxy:         proxy,
		establishedAt: time.Now(),
	}
}

// Purpose 返回连接的用途标记。
func (conn *Conn) Purpose() Purpose {
	return conn.purpose
}

// String 返回用途的可读标识，用于日志与事件载荷。
func (purpose Purpose) String() string {
	return string(purpose)
}

// Proxy 返回工作连接绑定的代理名；控制连接返回空字符串。
func (conn *Conn) Proxy() string {
	return conn.proxy
}

// EstablishedAt 返回连接的建链时刻。
func (conn *Conn) EstablishedAt() time.Time {
	return conn.establishedAt
}

// String 返回用于日志与事件的连接摘要，不含任何凭证或正文。
func (conn *Conn) String() string {
	if conn.proxy == "" {
		return string(conn.purpose) + "://" + conn.RemoteAddr().String()
	}
	return string(conn.purpose) + "://" + conn.RemoteAddr().String() + "[" + conn.proxy + "]"
}

// SetMigrationObserver 注册对端地址迁移的观察回调，重复注册覆盖前者。
//
// 回调经内嵌连接转发给真正持有会话的类型：只有 QUIC 连接实现该接口，
// TCP/WebSocket 连接忽略注册（它们的对端地址在连接生命周期内固定）。
// 这样迁移观测不需要上层按传输类型分支，类型隔离仍由传输包内部消化。
func (conn *Conn) SetMigrationObserver(observer MigrationObserver) {
	conn.migrationMu.Lock()
	defer conn.migrationMu.Unlock()
	conn.migrationObserver = observer
	conn.lastRemote = ""
	conn.hasBaseline = false
	if target, ok := conn.Conn.(migratable); ok {
		target.SetMigrationObserver(observer)
		return
	}
}

// Received 返回本连接累计收到的下行字节数。
//
// 原子读取，可在桥接运行期间调用；判据语义见字段注释。
func (conn *Conn) Received() int64 {
	return conn.received.Load()
}

// observeMigration 在读写路径上比较对端地址摘要，变化即通知观察者。
//
// 摘要而非原值：迁移事件用于观测路径变化，完整地址会把网络拓扑带进事件流，
// 而摘要已足以区分"是否变化"与"变化成什么"。解析失败按整串摘要处理，
// 保证任何地址形态都不会漏报。
func (conn *Conn) observeMigration() {
	conn.migrationMu.Lock()
	observer := conn.migrationObserver
	if observer == nil {
		conn.migrationMu.Unlock()
		return
	}
	current := AddrSummary(conn.RemoteAddr())
	if !conn.hasBaseline {
		// 首次观测只建立基线：此后地址变化才算迁移。
		conn.lastRemote = current
		conn.hasBaseline = true
		conn.migrationMu.Unlock()
		return
	}
	if current == conn.lastRemote {
		conn.migrationMu.Unlock()
		return
	}
	previous := conn.lastRemote
	conn.lastRemote = current
	conn.migrationMu.Unlock()

	observer(Migration{
		Purpose:  conn.purpose,
		Proxy:    conn.proxy,
		Previous: previous,
		Current:  current,
	})
}

// Read 覆盖内嵌的 net.Conn.Read：读取后累加下行字节计数，并观测对端地址变化。
//
// 读错误时也按实际读到的字节数累加（读到的数据仍有效），错误本身交给调用方。
func (conn *Conn) Read(buffer []byte) (int, error) {
	read, err := conn.Conn.Read(buffer)
	if read > 0 {
		conn.received.Add(int64(read))
	}
	conn.observeMigration()
	return read, err
}

// Write 覆盖内嵌的 net.Conn.Write：写入后观测对端地址变化。
//
// QUIC 的路径迁移由对端发起，本端在下一个数据报往返时才会观测到新地址，
// 因此 Write 与 Read 两侧都要检查。
func (conn *Conn) Write(buffer []byte) (int, error) {
	written, err := conn.Conn.Write(buffer)
	conn.observeMigration()
	return written, err
}

// CloseWrite 半关闭连接的写方向，是上层表达「已写完」的唯一入口。
//
// 非 TCP 连接不支持半关闭，退化为整条关闭：对端读到 EOF 的时机略有提前，
// 但语义仍然正确。调用后连接仍可读，直到对端也结束。
func (conn *Conn) CloseWrite() error {
	if tcpConn, ok := conn.Conn.(*net.TCPConn); ok {
		return tcpConn.CloseWrite()
	}
	return conn.Close()
}

// Close 关闭连接并返回首次关闭的结果；重复调用返回同一结果且不 panic。
func (conn *Conn) Close() error {
	conn.closeOnce.Do(func() {
		conn.closeErr = conn.Conn.Close()
	})
	return conn.closeErr
}
