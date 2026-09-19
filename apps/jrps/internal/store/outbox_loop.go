package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// 发送器后台循环的默认参数。
const (
	// defaultDispatchInterval 是轮询间隔。
	//
	// 轮询而非唤醒：通知是低频操作，秒级延迟可接受；轮询避免了"提交后唤醒"
	// 所需的跨协程信号与漏唤醒处理。ADR-0004 要求发送在事务提交后由发送器
	// 接管，未要求即时。
	defaultDispatchInterval = 5 * time.Second

	// defaultDispatchBatch 是单轮处理的记录上限。
	//
	// 有界批量让单轮占用数据库的时间可控：SQLite 连接池固定为单连接，
	// 长时间持有写事务会让管理请求排队等待。
	defaultDispatchBatch = 50

	// defaultDispatchConcurrency 是单轮内的并发投递上限。
	//
	// 并发是为了不让一个慢目标拖住其他目标；上限则是为了不让一批通知
	// 同时涌向外部服务。
	defaultDispatchConcurrency = 4

	// dispatchErrorBackoff 是遇到数据库或读写错误后的退避时长。
	//
	// 出错后不立即重试：连续失败通常是数据库被占用或磁盘问题，
	// 紧密重试只会加剧竞争。
	dispatchErrorBackoff = 10 * time.Second
)

// OutboxLoopConfig 是后台发送循环的配置。
type OutboxLoopConfig struct {
	Dispatcher *OutboxDispatcher
	// Interval 是轮询间隔；为零时取默认值。
	Interval time.Duration
	// Batch 是单轮处理上限；为零时取默认值。
	Batch int
	// Concurrency 是单轮并发投递上限；为零时取默认值。
	Concurrency int
	// Logger 是外壳日志器；为空时静默。
	Logger *slog.Logger
	// OnTargetMissing 在目标不存在时被调用，用于把该目标的在途记录转入 discarded。
	//
	// 以回调注入而不是让循环直接查目标表：循环只负责"发现目标没了"，
	// 至于如何处理（丢弃、告警、重试）属于编排决策。
	OnTargetMissing func(ctx context.Context, targetID string) error
}

// OutboxLoop 周期性驱动 outbox 发送。
//
// 它是 FR-15 §3.2 中"提交后触发"的落地：循环只通过数据库读取取得记录，
// 不持有任何业务事务句柄，因此未提交或已回滚的记录对它永远不可见。
type OutboxLoop struct {
	dispatcher      *OutboxDispatcher
	interval        time.Duration
	batch           int
	concurrency     int
	logger          *slog.Logger
	onTargetMissing func(ctx context.Context, targetID string) error

	started   atomic.Bool
	stop      chan struct{}
	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once

	dispatched atomic.Uint64
	failed     atomic.Uint64
	rounds     atomic.Uint64
}

// NewOutboxLoop 构造后台发送循环。
func NewOutboxLoop(cfg OutboxLoopConfig) (*OutboxLoop, error) {
	if cfg.Dispatcher == nil {
		return nil, errors.New("outbox 发送循环需要发送器")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	loop := &OutboxLoop{
		dispatcher:      cfg.Dispatcher,
		interval:        cfg.Interval,
		batch:           cfg.Batch,
		concurrency:     cfg.Concurrency,
		logger:          logger,
		onTargetMissing: cfg.OnTargetMissing,
		stop:            make(chan struct{}),
		done:            make(chan struct{}),
	}
	if loop.interval <= 0 {
		loop.interval = defaultDispatchInterval
	}
	if loop.batch <= 0 {
		loop.batch = defaultDispatchBatch
	}
	if loop.concurrency <= 0 {
		loop.concurrency = defaultDispatchConcurrency
	}
	return loop, nil
}

// Start 启动后台循环；重复调用无副作用。
func (loop *OutboxLoop) Start() {
	if !loop.started.CompareAndSwap(false, true) {
		return
	}
	loop.wg.Add(1)
	go loop.run()
	loop.logger.Info("通知发送循环已启动", "轮询间隔", loop.interval.String(), "单轮上限", loop.batch)
}

// Done 返回循环完全停止的信号通道。
func (loop *OutboxLoop) Done() <-chan struct{} { return loop.done }

// Close 停止循环并等待当前轮结束。
//
// 幂等；未启动时立即返回。等待当前轮结束而不是强行中断，是为了让正在进行的
// 投递把结果落库——中断会让记录停在 sending，只能等租约过期后重投。
func (loop *OutboxLoop) Close(ctx context.Context) error {
	var closeErr error
	loop.closeOnce.Do(func() {
		if !loop.started.Load() {
			close(loop.done)
			return
		}
		close(loop.stop)
		waitDone := make(chan struct{})
		go func() {
			loop.wg.Wait()
			close(waitDone)
		}()
		select {
		case <-waitDone:
		case <-ctx.Done():
			closeErr = ctx.Err()
			return
		}
		close(loop.done)
	})
	return closeErr
}

// run 是循环主体：按间隔驱动一轮派发，直到收到停止信号。
func (loop *OutboxLoop) run() {
	defer loop.wg.Done()
	timer := time.NewTimer(loop.interval)
	defer timer.Stop()
	// 启动后立即跑一轮：进程重启时可能已有积压记录，等一个间隔才开始
	// 会让这些记录白白延迟。
	loop.dispatchRound(context.Background())

	for {
		select {
		case <-loop.stop:
			return
		case <-timer.C:
			loop.dispatchRound(context.Background())
			timer.Reset(loop.interval)
		}
	}
}

// dispatchRound 执行一轮派发：读取一批到期记录并并发投递。
func (loop *OutboxLoop) dispatchRound(ctx context.Context) {
	entries, err := loop.dispatcher.dueEntries(ctx, loop.batch)
	if err != nil {
		loop.failed.Add(1)
		loop.logger.Error("读取待发送通知失败", "错误", err)
		time.Sleep(dispatchErrorBackoff)
		return
	}
	loop.rounds.Add(1)
	if len(entries) == 0 {
		return
	}
	loop.deliverBatch(ctx, entries)
}

// deliverBatch 并发投递一批记录，并发量受上限约束。
func (loop *OutboxLoop) deliverBatch(ctx context.Context, entries []NotificationOutbox) {
	semaphore := make(chan struct{}, loop.concurrency)
	var waitGroup sync.WaitGroup
	for _, entry := range entries {
		waitGroup.Add(1)
		semaphore <- struct{}{}
		go func(item NotificationOutbox) {
			defer waitGroup.Done()
			defer func() { <-semaphore }()
			loop.deliverOne(ctx, item)
		}(entry)
	}
	waitGroup.Wait()
}

// deliverOne 投递单条记录并处理目标缺失。
func (loop *OutboxLoop) deliverOne(ctx context.Context, entry NotificationOutbox) {
	if loop.targetMissing(ctx, entry.TargetID) {
		return
	}
	if err := loop.dispatcher.deliver(ctx, entry); err != nil {
		loop.failed.Add(1)
		// 不记录目标地址或载荷：它们可能含秘密（FR-15 §3.5）。
		loop.logger.Error("投递通知失败", "事件标识", entry.EventID, "错误", err)
		return
	}
	loop.dispatched.Add(1)
}

// targetMissing 判断目标是否已不存在，并在缺失时交给回调处理。
//
// 目标为空的记录（未指定目标）视为存在：这类记录由写入方决定去向，
// 不因缺少目标标识而被丢弃。
func (loop *OutboxLoop) targetMissing(ctx context.Context, targetID string) bool {
	if targetID == "" || loop.onTargetMissing == nil {
		return false
	}
	// 只把"目标确实不存在"当作缺失。
	//
	// 此前把查询返回的任何错误都判为缺失，于是数据库瞬时故障或上下文取消都会
	// 触发回调——生产路径下会把该目标的全部在途通知转入 discarded 并写一条内容
	// 错误的审计（"目标已不存在"），而目标其实完好。查询失败应当让本轮跳过该
	// 记录、留待下次重试，而不是据错误的存在推断目标的状态。
	missing := false
	queryErr := loop.dispatcher.store.View(ctx, func(tx *Tx) error {
		if _, err := tx.NotificationTargetByID(targetID); err != nil {
			// 只有哨兵错误表示目标不存在；其余错误向上抛出，由调用方按查询
			// 失败处理，不进入缺失分支。
			if errors.Is(err, ErrNotificationTargetMissing) {
				missing = true
				return nil
			}
			return err
		}
		return nil
	})
	if queryErr != nil {
		loop.failed.Add(1)
		loop.logger.Error("读取通知目标失败，本轮跳过", "目标标识", targetID, "错误", queryErr)
		return false
	}
	if !missing {
		return false
	}
	if err := loop.onTargetMissing(ctx, targetID); err != nil {
		loop.logger.Error("处理已缺失目标的在途通知失败", "目标标识", targetID, "错误", err)
		return true
	}
	loop.logger.Warn("通知目标已不存在，其待发送记录已转入 discarded", "目标标识", targetID)
	return true
}

// Dispatched 返回累计成功投递的条数。
func (loop *OutboxLoop) Dispatched() uint64 {
	if loop == nil {
		return 0
	}
	return loop.dispatched.Load()
}

// Failed 返回累计失败的条数。
func (loop *OutboxLoop) Failed() uint64 {
	if loop == nil {
		return 0
	}
	return loop.failed.Load()
}

// Rounds 返回累计执行的轮数。
func (loop *OutboxLoop) Rounds() uint64 {
	if loop == nil {
		return 0
	}
	return loop.rounds.Load()
}

// Dispatcher 返回底层发送器，供调用方在事务提交后主动触发一轮。
func (loop *OutboxLoop) Dispatcher() *OutboxDispatcher { return loop.dispatcher }
