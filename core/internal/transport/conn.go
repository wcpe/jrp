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

// Received 返回本连接累计收到的下行字节数。
//
// 原子读取，可在桥接运行期间调用；判据语义见字段注释。
func (conn *Conn) Received() int64 {
	return conn.received.Load()
}

// Read 覆盖内嵌的 net.Conn.Read：读取后累加下行字节计数。
//
// 读错误时也按实际读到的字节数累加（读到的数据仍有效），错误本身交给调用方。
func (conn *Conn) Read(buffer []byte) (int, error) {
	read, err := conn.Conn.Read(buffer)
	if read > 0 {
		conn.received.Add(int64(read))
	}
	return read, err
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
