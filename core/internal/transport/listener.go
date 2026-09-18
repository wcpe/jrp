package transport

import (
	"errors"
	"net"
	"sync"
	"time"
)

// AcceptAction 是 Accept 错误的处理类别。
//
// 规格 §3.6：临时错误退避后继续，致命错误停止监听并上报。
type AcceptAction int

const (
	// AcceptContinue 表示 Accept 成功，循环继续。
	AcceptContinue AcceptAction = iota
	// AcceptTemporary 表示临时错误：退避后继续 Accept 循环。
	AcceptTemporary
	// AcceptFatal 表示致命错误：停止 Accept 循环并上报。
	AcceptFatal
)

// acceptBackoff 是临时错误后的退避时长。
//
// 取值固定且很短：它的唯一目的是避免临时错误把 Accept 循环变成忙循环，
// 不承载重试策略，重试次数由上层决定。
const acceptBackoff = 20 * time.Millisecond

// AcceptErrorAction 判定一次 Accept 错误属于临时还是致命。
//
// 判定顺序：超时归临时（监听器仍可用）；其余归致命（监听器已关闭或不可用）。
func AcceptErrorAction(err error) AcceptAction {
	if err == nil {
		return AcceptContinue
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return AcceptTemporary
	}
	return AcceptFatal
}

// AcceptBackoff 返回错误类别对应的退避时长；致命错误不产生退避。
func AcceptBackoff(action AcceptAction) time.Duration {
	if action == AcceptTemporary {
		return acceptBackoff
	}
	return 0
}

// Listener 是被 Core 接管的监听句柄。
//
// 接管语义（承 ADR-0005 与 FR-25 §3.3）：Start 成功后 Core 独占该监听器，
// 宿主不得再 Accept 或 Close；释放后无残留监听套接字。重复释放安全。
type Listener struct {
	listener net.Listener

	releaseOnce sync.Once
	releaseErr  error
}

// TakeOverListener 接管一条已成功创建的监听器。
//
// 接管只登记所有权，不做可用性探测：可用性由调用方在 Start 阶段判定，
// 失败时监听器仍归宿主，不进入接管状态。
func TakeOverListener(listener net.Listener) *Listener {
	return &Listener{listener: listener}
}

// Addr 返回监听地址。
func (handle *Listener) Addr() net.Addr {
	return handle.listener.Addr()
}

// Accept 接受一条连接并封装为带用途标记的连接句柄。
func (handle *Listener) Accept(purpose Purpose, proxy ...string) (*Conn, AcceptAction, error) {
	conn, err := handle.listener.Accept()
	if err != nil {
		return nil, AcceptErrorAction(err), err
	}
	bound := ""
	if len(proxy) > 0 {
		bound = proxy[0]
	}
	return wrapConn(conn, purpose, bound), AcceptContinue, nil
}

// Listener 返回底层监听器，供需要 net.Listener 的宿主注入点使用。
func (handle *Listener) Listener() net.Listener {
	return handle.listener
}

// Release 关闭监听器并返回首次关闭的结果；重复释放安全。
func (handle *Listener) Release() error {
	handle.releaseOnce.Do(func() {
		handle.releaseErr = handle.listener.Close()
	})
	return handle.releaseErr
}
