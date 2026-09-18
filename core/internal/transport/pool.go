package transport

import (
	"context"
	"errors"
	"sync"
)

// ErrPoolFull 表示工作连接池已达到上限，新请求被拒绝。
//
// 规格 §3.5：达到上限时新请求被推迟或拒绝，必须产生可观测事件，不得无限等待。
var ErrPoolFull = errors.New("工作连接池已达上限")

// PoolSlot 是工作连接池中的一个已占用槽位。
//
// 槽位必须显式 Release：持有槽位即占用池容量，忘记释放会让池永久耗尽。
// 重复 Release 安全。
type PoolSlot struct {
	pool    *WorkConnPool
	proxy   string
	release sync.Once
}

// Release 归还槽位；重复调用安全。
func (slot *PoolSlot) Release() {
	slot.release.Do(func() {
		slot.pool.release(slot.proxy)
	})
}

// WorkConnPool 是「每个控制会话一个池」的工作连接容量约束。
//
// 池只记账容量，不持有连接本身：连接的所有权仍归建链方。上限按代理名分别
// 计数，因此不同代理互不挤占。达到上限时立即拒绝并累加可观测事件计数，
// 绝不无限等待（规格 §3.5）。
type WorkConnPool struct {
	limit int

	mu         sync.Mutex
	inUse      map[string]int
	fullEvents int64
	closed     bool
}

// NewWorkConnPool 建立给定上限的工作连接池。
func NewWorkConnPool(limit int) *WorkConnPool {
	return &WorkConnPool{
		limit: limit,
		inUse: make(map[string]int),
	}
}

// Acquire 占用一个槽位；达到上限或池已关闭时返回错误。
//
// ctx 只用于调用方的取消语义：池不做等待队列，因此要么立即成功要么立即失败。
func (pool *WorkConnPool) Acquire(ctx context.Context, proxy string) (*PoolSlot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.closed {
		return nil, errors.New("工作连接池已关闭")
	}
	if pool.inUse[proxy] >= pool.limit {
		pool.fullEvents++
		return nil, ErrPoolFull
	}
	pool.inUse[proxy]++
	return &PoolSlot{pool: pool, proxy: proxy}, nil
}

// release 归还一个槽位。
func (pool *WorkConnPool) release(proxy string) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.inUse[proxy] <= 0 {
		return
	}
	pool.inUse[proxy]--
	if pool.inUse[proxy] == 0 {
		delete(pool.inUse, proxy)
	}
}

// FullEvents 返回累计的「达到上限」事件次数，供可观测性与测试断言使用。
func (pool *WorkConnPool) FullEvents() int64 {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.fullEvents
}

// InUse 返回指定代理当前占用的槽位数。
func (pool *WorkConnPool) InUse(proxy string) int {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.inUse[proxy]
}

// Close 关闭池并拒绝后续申请；重复关闭安全。
//
// 关闭不回收已占用槽位：它们承载活动流，由持有方按各自生命周期释放。
func (pool *WorkConnPool) Close() error {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.closed = true
	return nil
}
