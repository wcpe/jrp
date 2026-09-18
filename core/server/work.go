package server

import (
	"context"
	"errors"
	"net"
	"sync"
)

// workBroker 按代理名管理访客与工作连接的双向暂存配对。
//
// 配对语义：访客与工作连接到达顺序不确定，任一方先到都暂存，另一方到达时
// 立即配对桥接。桥接纳入 Engine 的 WaitGroup 管理，Shutdown 时按排水上限
// 等待；关闭后暂存连接全部关闭，不留悬挂 goroutine。
type workBroker struct {
	mu     sync.Mutex
	guests map[string][]net.Conn
	works  map[string][]net.Conn
	closed bool
}

// newWorkBroker 建立空的配对中心。
func newWorkBroker() *workBroker {
	return &workBroker{
		guests: make(map[string][]net.Conn),
		works:  make(map[string][]net.Conn),
	}
}

// parkGuest 暂存一个访客；若已有待命工作连接，立即配对并返回配对结果。
//
// 返回的布尔值表示是否完成配对。未配对时访客已暂存，调用方不得关闭它；
// 已配对时访客已移交给桥接，调用方同样不得再操作。
// 配对成功后桥接纳入 Engine 的 WaitGroup，由 Shutdown 按排水上限等待。
func (broker *workBroker) parkGuest(proxyName string, guest net.Conn, track func(net.Conn), add func(int)) bool {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.closed {
		return false
	}
	if len(broker.works[proxyName]) > 0 {
		work := broker.works[proxyName][0]
		broker.works[proxyName] = broker.works[proxyName][1:]
		track(guest)
		track(work)
		add(1)
		go func() {
			defer add(-1)
			defer untrackBoth(track, guest, work)
			bridgeWorkConn(context.Background(), guest, work)
		}()
		return true
	}
	broker.guests[proxyName] = append(broker.guests[proxyName], guest)
	return false
}

// untrackBoth 从活动集合移除一对已桥接的连接。
func untrackBoth(untrack func(net.Conn), guest, work net.Conn) {
	untrack(guest)
	untrack(work)
}

// park 暂存一条待命工作连接；若已有等待访客，立即配对并返回真。
func (broker *workBroker) park(proxyName string, work net.Conn, track func(net.Conn), add func(int)) bool {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.closed {
		return false
	}
	if len(broker.guests[proxyName]) > 0 {
		guest := broker.guests[proxyName][0]
		broker.guests[proxyName] = broker.guests[proxyName][1:]
		track(guest)
		track(work)
		add(1)
		go func() {
			defer add(-1)
			defer untrackBoth(track, guest, work)
			bridgeWorkConn(context.Background(), guest, work)
		}()
		return true
	}
	broker.works[proxyName] = append(broker.works[proxyName], work)
	return true
}

// closeStaged 关闭尚未配对的暂存连接并拒绝后续配对。
//
// 已配对并进入桥接的连接不在此处理：它们承载活动流，由 Engine 按排水上限
// 等待自然结束。
func (broker *workBroker) closeStaged() {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.closed {
		return
	}
	broker.closed = true
	for _, guests := range broker.guests {
		for _, guest := range guests {
			_ = guest.Close()
		}
	}
	for _, works := range broker.works {
		for _, work := range works {
			_ = work.Close()
		}
	}
	broker.guests = make(map[string][]net.Conn)
	broker.works = make(map[string][]net.Conn)
}

// workConnRequest 是客户端向服务端声明新建工作连接的载荷。
type workConnRequest struct {
	RunID string `json:"run_id"`
	Proxy string `json:"proxy_name"`
}

// bridgeWorkConn 把已配对的访客与工作连接双向桥接，直到任一方向结束。
func bridgeWorkConn(ctx context.Context, guest, work net.Conn) {
	var done sync.WaitGroup
	done.Add(2)
	go func() {
		defer done.Done()
		_, _ = copyBuffer(work, guest)
		if closer, ok := work.(*net.TCPConn); ok {
			_ = closer.CloseWrite()
		} else {
			_ = work.Close()
		}
	}()
	go func() {
		defer done.Done()
		_, _ = copyBuffer(guest, work)
		if closer, ok := guest.(*net.TCPConn); ok {
			_ = closer.CloseWrite()
		} else {
			_ = guest.Close()
		}
	}()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done.Wait()
	}()
	select {
	case <-finished:
	case <-ctx.Done():
		_ = guest.Close()
		_ = work.Close()
		<-finished
	}
	_ = guest.Close()
	_ = work.Close()
}

// copyBuffer 在两个连接之间转发字节，返回转发字节数。
func copyBuffer(dst, src net.Conn) (int64, error) {
	buffer := make([]byte, 32*1024)
	return copyWithBuffer(dst, src, buffer)
}

// copyWithBuffer 用给定缓冲转发，返回转发字节数。
func copyWithBuffer(dst net.Conn, src net.Conn, buffer []byte) (int64, error) {
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
				return total, errors.New("转发写入不完整")
			}
		}
		if readErr != nil {
			return total, readErr
		}
	}
}
