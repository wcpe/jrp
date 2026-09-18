package transport

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// PacketListener 是被 Core 接管的数据报监听句柄。
//
// 与面向流的 Listener 对应：它承载无连接传输的入口，只提供按上限读取单个
// 数据报与释放两个动作，不引入连接语义（规格 §3.4：UDP 的「关闭」对应会话
// 空闲回收，不得用 TCP 关闭语义推断）。
// 释放语义与 Listener 一致：接管后归 Core 所有，释放幂等。
type PacketListener struct {
	socket net.PacketConn

	releaseOnce sync.Once
	releaseErr  error
}

// ListenUDP 在给定地址上打开一个 UDP 数据报入口并返回接管句柄。
func ListenUDP(address netip.AddrPort) (*PacketListener, error) {
	socket, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(address))
	if err != nil {
		return nil, err
	}
	return &PacketListener{socket: socket}, nil
}

// Addr 返回入口地址。
func (handle *PacketListener) Addr() net.Addr {
	return handle.socket.LocalAddr()
}

// Read 读取单个数据报，返回对端地址与载荷。
//
// 缓冲由调用方按配置上限提供：本方法不按声明长度分配，因此超上限的数据报
// 只会截断到缓冲长度，绝不无界增长（规格 §3.4）。
func (handle *PacketListener) Read(buffer []byte) (int, netip.AddrPort, error) {
	read, addr, err := handle.socket.ReadFrom(buffer)
	if err != nil {
		return 0, netip.AddrPort{}, err
	}
	peer, err := addrPortFromNetAddr(addr)
	if err != nil {
		return read, netip.AddrPort{}, err
	}
	return read, peer, nil
}

// addrPortFromNetAddr 把标准库地址转换为 netip.AddrPort。
//
// 只读 UDP/TCP 地址的具体类型：数据报入口只服务 UDP，非 UDP 地址按非法地址处理，
// 避免为不存在的场景构造兜底路径。
func addrPortFromNetAddr(addr net.Addr) (netip.AddrPort, error) {
	switch typed := addr.(type) {
	case *net.UDPAddr:
		return typed.AddrPort(), nil
	case *net.TCPAddr:
		return typed.AddrPort(), nil
	default:
		return netip.AddrPort{}, net.UnknownNetworkError(addr.Network())
	}
}

// Write 向指定对端写出一个数据报。
func (handle *PacketListener) Write(datagram []byte, peer netip.AddrPort) (int, error) {
	return handle.socket.WriteTo(datagram, net.UDPAddrFromAddrPort(peer))
}

// SetReadDeadline 设置读截止时间，使读取循环可被停止流程中断。
func (handle *PacketListener) SetReadDeadline(deadline time.Time) error {
	return handle.socket.SetReadDeadline(deadline)
}

// Release 关闭入口并返回首次关闭的结果；重复释放安全。
func (handle *PacketListener) Release() error {
	handle.releaseOnce.Do(func() {
		handle.releaseErr = handle.socket.Close()
	})
	return handle.releaseErr
}
