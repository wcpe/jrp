package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// ErrDialTimeoutMissing 表示拨号器未配置超时。
//
// 规格 §3.4 禁止无超时拨号：零超时会让不可达地址上的拨号无限悬挂。
var ErrDialTimeoutMissing = errors.New("拨号必须配置超时")

// Dialer 是带超时约束的 TCP 拨号器。
//
// 超时是构造参数而非可选修饰：零值拨号器一律拒绝拨号，调用方必须显式给出
// 来自配置快照的超时值。
type Dialer struct {
	// Timeout 是单次拨号的时间上限，必须为正值。
	Timeout time.Duration
}

// Dial 按超时拨号并建立带用途标记的连接句柄。
//
// proxy 只在工作连接场景下传入；控制连接不传或传空串。
// 拨号失败时不重试：重试策略由上层决定（规格 §3.6）。
func (dialer Dialer) Dial(ctx context.Context, address string, purpose Purpose, proxy ...string) (*Conn, error) {
	if dialer.Timeout <= 0 {
		return nil, ErrDialTimeoutMissing
	}
	if purpose != PurposeControl && purpose != PurposeWork {
		return nil, fmt.Errorf("未知的建链用途：%q", string(purpose))
	}

	bound := ""
	if len(proxy) > 0 {
		bound = proxy[0]
	}
	dialCtx, cancel := context.WithTimeout(ctx, dialer.Timeout)
	defer cancel()
	netDialer := &net.Dialer{Timeout: dialer.Timeout}
	conn, err := netDialer.DialContext(dialCtx, "tcp", address)
	if err != nil {
		// 拨号失败返回包装错误：不含地址之外的敏感上下文。
		return nil, fmt.Errorf("拨号 %s 失败：%w", address, err)
	}
	return wrapConn(conn, purpose, bound), nil
}
