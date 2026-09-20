package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// ErrDialTimeoutMissing 表示拨号器未配置超时。
//
// 规格 §3.4 禁止无超时拨号：零超时会让不可达地址上的拨号无限悬挂。
var ErrDialTimeoutMissing = errors.New("拨号必须配置超时")

// keepAliveInterval 是工作与控制连接的 TCP 保活间隔。
//
// 待命工作连接可能被 NAT 或中间设备静默回收（连接表项超时后丢弃，两端都
// 不知道对端已不可达，即"半开连接"）。半开连接上读取只会永远超时、写入
// 则被本端发送缓冲吞掉而不报错——应用层无从感知，直到真正的业务数据
// 丢失。保活报文由内核周期性探测：死连接在探测失败后被内核标记关闭，
// 此后读写立即出错，服务端的配对探测与客户端的重建循环都能立刻得到反馈。
//
// 取值 30 秒：显著短于常见 NAT 的 UDP/TCP 空闲回收窗口（家用设备多为
// 数分钟），保证表项在被回收前就有保活流量刷新。
const keepAliveInterval = 30 * time.Second

// Dialer 是带超时约束的 TCP 拨号器。
//
// 超时是构造参数而非可选修饰：零值拨号器一律拒绝拨号，调用方必须显式给出
// 来自配置快照的超时值。
type Dialer struct {
	// Timeout 是单次拨号的时间上限，必须为正值。
	Timeout time.Duration
}

// DialUDP 建立一个指向 UDP 目标的数据报套接字。
//
// UDP 是无连接协议，因此不存在拨号动作：这里只解析地址并绑定一个本地端口。
// 套接字不设截止时间：UDP 会话的存续由会话空闲上限决定，套用拨号超时会让
// 会话在超时后立刻失效（规格 §3.4：UDP 的结束语义是空闲回收，不是连接超时）。
func (dialer Dialer) DialUDP(target netip.AddrPort) (*net.UDPConn, error) {
	if dialer.Timeout <= 0 {
		return nil, ErrDialTimeoutMissing
	}
	if !target.Addr().IsValid() {
		return nil, fmt.Errorf("目标地址必须指定 IP 与端口：%s", target.String())
	}
	socket, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(target))
	if err != nil {
		return nil, fmt.Errorf("建立 UDP 目标套接字 %s 失败：%w", target.String(), err)
	}
	return socket, nil
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
	netDialer := &net.Dialer{Timeout: dialer.Timeout, KeepAlive: keepAliveInterval}
	conn, err := netDialer.DialContext(dialCtx, "tcp", address)
	if err != nil {
		// 拨号失败返回包装错误：不含地址之外的敏感上下文。
		return nil, fmt.Errorf("拨号 %s 失败：%w", address, err)
	}
	return wrapConn(conn, purpose, bound), nil
}
