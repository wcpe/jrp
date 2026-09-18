package transport

import (
	"context"
	"errors"
	"net"
	"sync"
)

// copyBufferSize 是单次转发读取的缓冲大小。
//
// 每条转发 goroutine 持有一份，因此取值必须兼顾吞吐与内存占用。
const copyBufferSize = 32 * 1024

// ErrPartialWrite 表示一次转发写入未写满读到的字节数。
var ErrPartialWrite = errors.New("转发写入不完整")

// Bridge 双向转发一对已配对的连接，直到任一方向结束或上下文取消。
//
// 半关闭语义：一个方向读完后对另一端执行 CloseWrite，让对端读完剩余数据后再
// 感知 EOF；两端都不是 TCP 连接时退化为直接关闭，绝不无限等待。
// 返回前两端必定已关闭，调用方不得再操作。
func Bridge(ctx context.Context, left, right net.Conn) {
	var copyDone sync.WaitGroup
	copyDone.Add(2)
	go func() {
		defer copyDone.Done()
		_, _ = Copy(right, left)
		_ = halfCloseWrite(right)
	}()
	go func() {
		defer copyDone.Done()
		_, _ = Copy(left, right)
		_ = halfCloseWrite(left)
	}()

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		copyDone.Wait()
	}()
	select {
	case <-finished:
	case <-ctx.Done():
		// 取消或超时：先关闭两端再等待转发 goroutine 退出，避免残留。
		_ = left.Close()
		_ = right.Close()
		<-finished
	}
	_ = left.Close()
	_ = right.Close()
}

// halfCloseWrite 半关闭一条转发端的写方向。
//
// 必须穿透 transport.Conn 封装再判定：封装类型本身不是 *net.TCPConn，直接断言
// 会静默退化为整条关闭，导致对端在读完剩余数据前就收到 EOF。
// 非 TCP 连接确实不支持半关闭，此时才退化为整条关闭。
func halfCloseWrite(conn net.Conn) error {
	if wrapped, ok := conn.(*Conn); ok {
		return wrapped.CloseWrite()
	}
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		return tcpConn.CloseWrite()
	}
	return conn.Close()
}

// Copy 在两个连接之间转发字节，返回转发字节数。
//
// 每次调用独立分配缓冲：转发 goroutine 的生命周期与连接一致，复用缓冲需要
// 额外的池管理，收益不足以引入该复杂度。
func Copy(dst, src net.Conn) (int64, error) {
	buffer := make([]byte, copyBufferSize)
	var total int64
	for {
		read, readErr := src.Read(buffer)
		if read > 0 {
			written, writeErr := dst.Write(buffer[:read])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != read {
				return total, ErrPartialWrite
			}
		}
		if readErr != nil {
			return total, readErr
		}
	}
}
