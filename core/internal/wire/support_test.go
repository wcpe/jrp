package wire

import (
	"bytes"
	"io"
	"net"
	"runtime"
	"sync"
	"time"
)

// bufferedPoolSize 是测试与默认配置使用的有界池槽位数。
const bufferedPoolSize = 8

// allocationSnapshot 返回当前累计分配字节数，用于断言「先校验长度再分配」。
func allocationSnapshot() uint64 {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.TotalAlloc
}

// bufferConn 是最小可写连接，只把写入内容收集到缓冲。
type bufferConn struct {
	buffer *bytes.Buffer
}

func (conn *bufferConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (conn *bufferConn) Write(p []byte) (int, error)      { return conn.buffer.Write(p) }
func (conn *bufferConn) Close() error                     { return nil }
func (conn *bufferConn) LocalAddr() net.Addr              { return stubAddr("local") }
func (conn *bufferConn) RemoteAddr() net.Addr             { return stubAddr("remote") }
func (conn *bufferConn) SetDeadline(time.Time) error      { return nil }
func (conn *bufferConn) SetReadDeadline(time.Time) error  { return nil }
func (conn *bufferConn) SetWriteDeadline(time.Time) error { return nil }

// stubAddr 是脱敏地址，避免测试断言依赖真实网络地址。
type stubAddr string

func (addr stubAddr) Network() string { return "stub" }
func (addr stubAddr) String() string  { return string(addr) }

// recordingConn 记录连接是否被关闭。
type recordingConn struct {
	net.Conn
	mu     sync.Mutex
	closed bool
}

func (conn *recordingConn) Close() error {
	conn.mu.Lock()
	conn.closed = true
	conn.mu.Unlock()
	return conn.Conn.Close()
}

func (conn *recordingConn) wasClosed() bool {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	return conn.closed
}

// clientHelloWith 构造测试用客户端 hello：能力集合按线上嵌套结构分组。
func clientHelloWith(codecs, cryptoAlgorithms, compressionAlgorithms []string, maxPayload int) ClientHello {
	hello := ClientHello{
		Bootstrap: BootstrapSummary{Transport: "tcp"},
		Capabilities: ClientCaps{
			Message:    MessageCodecs{Codecs: codecs},
			Crypto:     CryptoOffer{Algorithms: cryptoAlgorithms},
			MaxPayload: maxPayload,
		},
	}
	if len(compressionAlgorithms) > 0 {
		hello.Capabilities.Compression = &CompressionOffer{Algorithms: compressionAlgorithms}
	}
	return hello
}
