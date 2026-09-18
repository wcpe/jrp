package transport

import (
	"sync"
)

// StagedWorkConns 是按代理名暂存待命工作连接的有界集合。
//
// 规格 §3.5：池内连接按用途复用，空闲连接在空闲上限后被回收，回收时释放缓冲
// 与 goroutine。本集合只承载「已建链但尚未配对」的连接：配对完成后连接移交
// 给桥接，不再回到暂存集合。
//
// 超出空闲上限时回收最早暂存的一条（先进先出），保证容量恒定。
type StagedWorkConns struct {
	limit int

	mu     sync.Mutex
	staged map[string][]*Conn
	closed bool
}

// NewStagedWorkConns 建立给定空闲上限的暂存集合。
func NewStagedWorkConns(limit int) *StagedWorkConns {
	return &StagedWorkConns{
		limit:  limit,
		staged: make(map[string][]*Conn),
	}
}

// Push 暂存一条待命工作连接。
//
// 超出空闲上限时立即回收最早暂存的一条；已关闭的集合中 Push 为空操作，
// 调用方需自行关闭连接以免泄漏。
func (staged *StagedWorkConns) Push(proxy string, conn *Conn) {
	staged.mu.Lock()
	if staged.closed {
		staged.mu.Unlock()
		return
	}
	queue := staged.staged[proxy]
	queue = append(queue, conn)
	// 超出上限：回收队首（最早暂存的连接），关闭即释放其缓冲与转发 goroutine。
	for len(queue) > staged.limit {
		_ = queue[0].Close()
		queue = queue[1:]
	}
	if len(queue) == 0 {
		delete(staged.staged, proxy)
	} else {
		staged.staged[proxy] = queue
	}
	staged.mu.Unlock()
}

// Take 取出指定代理最早暂存的一条连接；队列为空时返回 nil。
func (staged *StagedWorkConns) Take(proxy string) *Conn {
	staged.mu.Lock()
	defer staged.mu.Unlock()
	queue := staged.staged[proxy]
	if len(queue) == 0 {
		return nil
	}
	conn := queue[0]
	queue = queue[1:]
	if len(queue) == 0 {
		delete(staged.staged, proxy)
	} else {
		staged.staged[proxy] = queue
	}
	return conn
}

// Len 返回指定代理当前暂存的连接数。
func (staged *StagedWorkConns) Len(proxy string) int {
	staged.mu.Lock()
	defer staged.mu.Unlock()
	return len(staged.staged[proxy])
}

// Close 关闭并释放全部暂存连接；重复关闭安全。
func (staged *StagedWorkConns) Close() error {
	staged.mu.Lock()
	defer staged.mu.Unlock()
	if staged.closed {
		return nil
	}
	staged.closed = true
	for proxy, queue := range staged.staged {
		for _, conn := range queue {
			_ = conn.Close()
		}
		delete(staged.staged, proxy)
	}
	return nil
}
