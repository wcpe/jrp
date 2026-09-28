package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	kcp "github.com/xtaci/kcp-go/v5"
	"golang.org/x/net/websocket"
)

// WebSocketOptions 描述 WebSocket 升级与客户端握手参数。
type WebSocketOptions struct {
	Path            string
	Headers         http.Header
	MaxPayloadBytes int
	TLS             *tls.Config
}

// KCPOptions 描述 KCP 会话参数。
type KCPOptions struct {
	MTU           int
	SendWindow    int
	ReceiveWindow int
	DataShards    int
	ParityShards  int
	NoDelay       bool
	Interval      time.Duration
	Resend        int
	NoCongestion  bool
	blockCrypt    kcp.BlockCrypt
}

// QUICOptions 描述 QUIC 会话参数。
type QUICOptions struct {
	TLS                            *tls.Config
	ALPN                           string
	HandshakeTimeout               time.Duration
	MaxIdleTimeout                 time.Duration
	KeepAlivePeriod                time.Duration
	InitialStreamReceiveWindow     uint64
	MaxStreamReceiveWindow         uint64
	InitialConnectionReceiveWindow uint64
	MaxConnectionReceiveWindow     uint64
	MaxIncomingStreams             int64
}

// DialWebSocket 建立 WebSocket 流连接并封装为 Core 连接。
func DialWebSocket(ctx context.Context, address string, options WebSocketOptions, purpose Purpose, proxy string) (*Conn, error) {
	if options.Path == "" {
		options.Path = "/frp"
	}
	scheme := "ws"
	if options.TLS != nil {
		scheme = "wss"
	}
	target := scheme + "://" + address + options.Path
	parsed, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("解析 WebSocket 地址失败：%w", err)
	}
	config := &websocket.Config{Location: parsed, Origin: parsed, Version: websocket.ProtocolVersionHybi13, Header: options.Headers, TlsConfig: options.TLS, Dialer: &net.Dialer{}}
	if deadline, ok := ctx.Deadline(); ok {
		config.Dialer.Timeout = time.Until(deadline)
	}
	ws, err := websocket.DialConfig(config)
	if err != nil {
		return nil, fmt.Errorf("WebSocket 握手失败：%w", err)
	}
	if options.MaxPayloadBytes > 0 {
		ws.MaxPayloadBytes = options.MaxPayloadBytes
	}
	return wrapConn(ws, purpose, proxy), nil
}

// WebSocketListener 接收 HTTP 升级后的 WebSocket 连接。
type WebSocketListener struct {
	server   *http.Server
	listener net.Listener
	queue    chan net.Conn
	done     chan struct{}
	activeMu sync.Mutex
	active   map[*websocketServerConn]struct{}
	once     sync.Once
}

type websocketServerConn struct {
	*websocket.Conn
	done      chan struct{}
	closeOnce sync.Once
}

func (conn *websocketServerConn) Close() error {
	var err error
	conn.closeOnce.Do(func() {
		close(conn.done)
		err = conn.Conn.Close()
	})
	return err
}

// NewWebSocketListener 在已有 HTTP 监听器上创建 WebSocket 升级入口。
func NewWebSocketListener(listener net.Listener, options WebSocketOptions) *WebSocketListener {
	if options.Path == "" {
		options.Path = "/frp"
	}
	if options.TLS != nil {
		listener = tls.NewListener(listener, options.TLS)
	}
	result := &WebSocketListener{listener: listener, queue: make(chan net.Conn, 64), done: make(chan struct{}), active: make(map[*websocketServerConn]struct{})}
	scheme := "ws"
	if options.TLS != nil {
		scheme = "wss"
	}
	location, _ := url.Parse(scheme + "://" + listener.Addr().String() + options.Path)
	mux := http.NewServeMux()
	mux.Handle(options.Path, websocket.Server{Config: websocket.Config{
		Version:   websocket.ProtocolVersionHybi13,
		Location:  location,
		Origin:    location,
		TlsConfig: options.TLS,
	}, Handler: websocket.Handler(func(ws *websocket.Conn) {
		if options.MaxPayloadBytes > 0 {
			ws.MaxPayloadBytes = options.MaxPayloadBytes
		}
		conn := &websocketServerConn{Conn: ws, done: make(chan struct{})}
		result.activeMu.Lock()
		result.active[conn] = struct{}{}
		result.activeMu.Unlock()
		select {
		case result.queue <- conn:
		case <-result.done:
			_ = conn.Close()
		}
		<-conn.done
		result.activeMu.Lock()
		delete(result.active, conn)
		result.activeMu.Unlock()
	})})
	result.server = &http.Server{Handler: mux}
	go func() { _ = result.server.Serve(listener) }()
	return result
}

// Accept 接收一个已完成升级的 WebSocket 连接。
//
// 监听器关闭后必须立即返回错误而不是继续阻塞：Accept 循环与引擎 Shutdown 都
// 依赖它退出，永久阻塞会让关闭流程悬挂（实测：关闭后 controlWG.Wait 不返回）。
func (listener *WebSocketListener) Accept() (net.Conn, error) {
	select {
	case conn := <-listener.queue:
		return conn, nil
	case <-listener.done:
		return nil, net.ErrClosed
	}
}

// Addr 返回底层 HTTP 监听地址。
func (listener *WebSocketListener) Addr() net.Addr { return listener.listener.Addr() }

// Close 停止 HTTP 服务并释放监听器。
func (listener *WebSocketListener) Close() error {
	var result error
	listener.once.Do(func() {
		close(listener.done)
		listener.activeMu.Lock()
		for conn := range listener.active {
			_ = conn.Close()
		}
		listener.activeMu.Unlock()
		result = listener.server.Close()
	})
	return result
}

// ListenKCP 创建 KCP 监听器并返回 Core 监听抽象。
func ListenKCP(address string, options KCPOptions) (*Listener, error) {
	listener, err := kcp.ListenWithOptions(address, options.blockCrypt, options.DataShards, options.ParityShards)
	if err != nil {
		return nil, fmt.Errorf("KCP 监听失败：%w", err)
	}
	return TakeOverListener(&configuredKCPListener{listener: listener, options: options}), nil
}

type configuredKCPListener struct {
	listener *kcp.Listener
	options  KCPOptions
}

func (listener *configuredKCPListener) Accept() (net.Conn, error) {
	session, err := listener.listener.AcceptKCP()
	if err != nil {
		return nil, err
	}
	configureKCP(session, listener.options)
	return session, nil
}

func (listener *configuredKCPListener) Close() error   { return listener.listener.Close() }
func (listener *configuredKCPListener) Addr() net.Addr { return listener.listener.Addr() }

// DialKCP 建立 KCP 会话并返回 Core 连接。
func DialKCP(ctx context.Context, address string, options KCPOptions, purpose Purpose, proxy string) (*Conn, error) {
	result := make(chan struct {
		conn net.Conn
		err  error
	}, 1)
	go func() {
		conn, err := kcp.DialWithOptions(address, options.blockCrypt, options.DataShards, options.ParityShards)
		result <- struct {
			conn net.Conn
			err  error
		}{conn, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case outcome := <-result:
		if outcome.err != nil {
			return nil, fmt.Errorf("KCP 拨号失败：%w", outcome.err)
		}
		if session, ok := outcome.conn.(*kcp.UDPSession); ok {
			configureKCP(session, options)
		}
		return wrapConn(outcome.conn, purpose, proxy), nil
	}
}

func configureKCP(session *kcp.UDPSession, options KCPOptions) {
	session.SetStreamMode(true)
	session.SetWriteDelay(true)
	session.SetACKNoDelay(false)
	if options.MTU > 0 {
		session.SetMtu(options.MTU)
	}
	if options.SendWindow > 0 || options.ReceiveWindow > 0 {
		send, receive := options.SendWindow, options.ReceiveWindow
		if send == 0 {
			send = 32
		}
		if receive == 0 {
			receive = 32
		}
		session.SetWindowSize(send, receive)
	}
	interval := int(options.Interval / time.Millisecond)
	if interval <= 0 {
		interval = 100
	}
	resend := options.Resend
	if resend < 0 {
		resend = 0
	}
	nodelay := 0
	if options.NoDelay {
		nodelay = 1
	}
	noCongestion := 0
	if options.NoCongestion {
		noCongestion = 1
	}
	session.SetNoDelay(nodelay, interval, resend, noCongestion)
}

// ListenQUIC 创建 QUIC 监听器并把它接受的每条双向流映射为一条连接。
//
// 映射单位是**流**而不是会话：官方客户端把控制连接与每条工作连接都作为
// 同一条 QUIC 会话上的流打开（会话建立后反复 OpenStreamSync），按会话映射
// 会让第二条流永远不被接受，表现为"控制连接可用但代理流量不通"。
func ListenQUIC(address string, options QUICOptions) (net.Listener, error) {
	listener, err := quic.ListenAddr(address, options.TLS, quicConfig(options))
	if err != nil {
		return nil, fmt.Errorf("QUIC 监听失败：%w", err)
	}
	return newQUICListener(listener), nil
}

// DialQUIC 建立 QUIC 会话并打开一条双向流。
func DialQUIC(ctx context.Context, address string, options QUICOptions, purpose Purpose, proxy string) (*Conn, error) {
	session, err := quic.DialAddr(ctx, address, options.TLS, quicConfig(options))
	if err != nil {
		return nil, fmt.Errorf("QUIC 拨号失败：%w", err)
	}
	stream, err := session.OpenStreamSync(ctx)
	if err != nil {
		_ = session.CloseWithError(0, "流建立失败")
		return nil, fmt.Errorf("QUIC 流建立失败：%w", err)
	}
	// 本端自己建立的会话归本条连接所有：连接结束后会话不再有用，随连接一起关闭。
	return wrapConn(&quicStreamConn{stream: stream, session: session, ownsSession: true}, purpose, proxy), nil
}

type quicListener struct {
	listener *quic.Listener
	queue    chan net.Conn
	closed   chan struct{}
	once     sync.Once
}

func newQUICListener(listener *quic.Listener) *quicListener {
	result := &quicListener{
		listener: listener,
		queue:    make(chan net.Conn, 64),
		closed:   make(chan struct{}),
	}
	go result.acceptSessions()
	return result
}

func (listener *quicListener) acceptSessions() {
	for {
		session, err := listener.listener.Accept(context.Background())
		if err != nil {
			_ = listener.Close()
			return
		}
		// 每条会话独立接收流：会话内的后续流同样要作为连接交付给上层。
		go listener.acceptStreams(session)
	}
}

func (listener *quicListener) acceptStreams(session *quic.Conn) {
	for {
		stream, err := session.AcceptStream(context.Background())
		if err != nil {
			return
		}
		conn := &quicStreamConn{stream: stream, session: session}
		select {
		case listener.queue <- conn:
		case <-listener.closed:
			_ = conn.stream.Close()
			return
		}
	}
}

// Accept 交付一条已建立的双向流；监听器关闭后立即返回错误。
func (listener *quicListener) Accept() (net.Conn, error) {
	select {
	case conn := <-listener.queue:
		return conn, nil
	case <-listener.closed:
		return nil, net.ErrClosed
	}
}
func (listener *quicListener) Addr() net.Addr { return listener.listener.Addr() }

func (listener *quicListener) Close() error {
	var result error
	listener.once.Do(func() {
		close(listener.closed)
		result = listener.listener.Close()
	})
	return result
}

// quicStreamConn 把一条 QUIC 双向流适配为面向流的连接。
//
// ownsSession 区分两种来源：拨号侧自己建立的会话随连接关闭，监听侧交付的流
// 属于对端会话——关闭一条工作连接绝不能顺手关掉承载控制连接的会话。
type quicStreamConn struct {
	stream      *quic.Stream
	session     *quic.Conn
	ownsSession bool
	closeOnce   sync.Once

	// migrationObserver 是对端地址迁移的观察回调，可为 nil（不观测）。
	//
	// 只有 QUIC 会出现迁移：TCP/WebSocket 的地址在连接生命周期内固定。
	migrationObserver MigrationObserver
	// lastRemote 是上次观测到的对端地址摘要，用于判定是否发生变化。
	lastRemote string
}

// SetMigrationObserver 启用对端地址迁移观测（规格 §3.6）。
//
// 迁移由 quic-go 在库内完成：路径变化时连接 ID 与 TLS 状态保持不变，
// RemoteAddr() 自动反映新路径。因此本方法只需登记回调，quic-go 不提供也不
// 需要迁移回调 API——观测在 Conn 的读写路径上比较地址摘要完成，不引入后台
// goroutine。TCP 与 WebSocket 的地址在连接生命周期内固定，无需登记。
func (conn *quicStreamConn) SetMigrationObserver(observer MigrationObserver) {
	conn.migrationObserver = observer
	conn.lastRemote = ""
}

func (conn *quicStreamConn) Read(buffer []byte) (int, error)  { return conn.stream.Read(buffer) }
func (conn *quicStreamConn) Write(buffer []byte) (int, error) { return conn.stream.Write(buffer) }
func (conn *quicStreamConn) Close() error {
	var result error
	conn.closeOnce.Do(func() {
		result = conn.stream.Close()
		if conn.ownsSession {
			_ = conn.session.CloseWithError(0, "连接关闭")
		}
	})
	return result
}
func (conn *quicStreamConn) LocalAddr() net.Addr  { return conn.session.LocalAddr() }
func (conn *quicStreamConn) RemoteAddr() net.Addr { return conn.session.RemoteAddr() }
func (conn *quicStreamConn) SetDeadline(deadline time.Time) error {
	return conn.stream.SetDeadline(deadline)
}
func (conn *quicStreamConn) SetReadDeadline(deadline time.Time) error {
	return conn.stream.SetReadDeadline(deadline)
}
func (conn *quicStreamConn) SetWriteDeadline(deadline time.Time) error {
	return conn.stream.SetWriteDeadline(deadline)
}

func quicConfig(options QUICOptions) *quic.Config {
	config := &quic.Config{HandshakeIdleTimeout: options.HandshakeTimeout, MaxIdleTimeout: options.MaxIdleTimeout, KeepAlivePeriod: options.KeepAlivePeriod, InitialStreamReceiveWindow: options.InitialStreamReceiveWindow, MaxStreamReceiveWindow: options.MaxStreamReceiveWindow, InitialConnectionReceiveWindow: options.InitialConnectionReceiveWindow, MaxConnectionReceiveWindow: options.MaxConnectionReceiveWindow, MaxIncomingStreams: options.MaxIncomingStreams, Allow0RTT: false}
	if options.ALPN != "" && options.TLS != nil {
		options.TLS.NextProtos = []string{options.ALPN}
	}
	return config
}

// TLSConfigFromPEM 将 Core 自有 TLS 配置转换为标准库配置。
func TLSConfigFromPEM(serverName string, rootPEM, certPEM, keyPEM []byte, fingerprint string, server bool) (*tls.Config, error) {
	config := &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12, InsecureSkipVerify: false} //nolint:gosec
	if len(certPEM) != 0 || len(keyPEM) != 0 {
		certificate, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("解析 TLS 证书失败：%w", err)
		}
		config.Certificates = []tls.Certificate{certificate}
	}
	if len(rootPEM) != 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(rootPEM) {
			return nil, errors.New("解析 TLS 信任根失败")
		}
		if server {
			config.ClientCAs = pool
		} else {
			config.RootCAs = pool
		}
	}
	if !server {
		normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(fingerprint), ":", ""))
		if normalized != "" {
			pinned, err := hex.DecodeString(normalized)
			if err != nil || len(pinned) != sha256.Size {
				return nil, errors.New("TLS 证书指纹格式无效")
			}
			config.InsecureSkipVerify = true // 仅在后续证书指纹精确匹配时跳过链验证
			config.VerifyConnection = func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 {
					return errors.New("TLS 对端未提供证书")
				}
				digest := sha256.Sum256(state.PeerCertificates[0].Raw)
				if !bytes.Equal(digest[:], pinned) {
					return errors.New("TLS 证书指纹不匹配")
				}
				return nil
			}
		}
	}
	return config, nil
}

var _ net.Conn = (*quicStreamConn)(nil)
var _ io.ReadWriteCloser = (*websocket.Conn)(nil)
