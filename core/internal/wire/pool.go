package wire

import (
	"errors"
	"fmt"
	"sync"
)

var (
	// ErrInvalidLimit 表示配置的载荷上限不是正数。
	ErrInvalidLimit = errors.New("载荷上限必须为正数")
	// ErrInvalidPoolSlots 表示缓冲池槽位数不是正数。
	ErrInvalidPoolSlots = errors.New("缓冲池槽位必须为正数")
	// ErrBufferTooLarge 表示请求的缓冲区超过有界池上限。
	ErrBufferTooLarge = errors.New("请求的缓冲区超过有界池上限")
)

// BufferPool 是有界缓冲区池。
//
// wire 热路径从池中取用载荷缓冲并显式归还：既避免每条消息都做独立的大块分配，
// 也保证缓冲总量有界，不会按对端声明的长度无界增长。
// 池本身不持有任何协议语义，只负责复用与限额。
type BufferPool struct {
	limit int
	free  chan []byte

	mu          sync.Mutex
	outstanding int
}

// NewBufferPool 建立单块上限为 limit 字节、最多缓存 slots 个缓冲的池。
func NewBufferPool(limit, slots int) (*BufferPool, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w：实际 %d", ErrInvalidLimit, limit)
	}
	if slots <= 0 {
		return nil, fmt.Errorf("%w：实际 %d", ErrInvalidPoolSlots, slots)
	}
	return &BufferPool{limit: limit, free: make(chan []byte, slots)}, nil
}

// Outstanding 返回尚未归还的缓冲数量，用于断言拒绝路径不泄漏缓冲。
func (pool *BufferPool) Outstanding() int {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.outstanding
}

// Get 返回一块长度恰为 size 的缓冲。
// size 超出上限时为不可能取值，返回错误且不做任何分配。
func (pool *BufferPool) Get(size int) ([]byte, error) {
	if size < 0 || size > pool.limit {
		return nil, fmt.Errorf("%w：请求 %d 字节，上限 %d 字节", ErrBufferTooLarge, size, pool.limit)
	}
	pool.mu.Lock()
	pool.outstanding++
	pool.mu.Unlock()

	select {
	case buf := <-pool.free:
		if cap(buf) >= size {
			return buf[:size], nil
		}
		// 归还的缓冲偏小，放回池中并新分配一块，避免池被小缓冲占满。
		select {
		case pool.free <- buf[:cap(buf)]:
		default:
		}
	default:
	}
	return make([]byte, size), nil
}

// Put 显式归还缓冲。池已满时直接丢弃，保证池自身有界。
func (pool *BufferPool) Put(buf []byte) {
	pool.mu.Lock()
	if pool.outstanding > 0 {
		pool.outstanding--
	}
	pool.mu.Unlock()

	if buf == nil || cap(buf) > pool.limit {
		return
	}
	select {
	case pool.free <- buf[:cap(buf)]:
	default:
	}
}
