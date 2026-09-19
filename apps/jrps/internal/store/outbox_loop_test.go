package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

// 测试辅助：构造一个测试用发送循环，轮询间隔极短以便快速驱动。
func mustOutboxLoop(t *testing.T, database *Store, sender Sender, config func(*OutboxLoopConfig)) *OutboxLoop {
	t.Helper()
	dispatcher := mustDispatcher(t, database, sender)
	loopConfig := OutboxLoopConfig{Dispatcher: dispatcher, Interval: 5 * time.Millisecond}
	if config != nil {
		config(&loopConfig)
	}
	loop, err := NewOutboxLoop(loopConfig)
	if err != nil {
		t.Fatalf("构造发送循环失败：%v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = loop.Close(ctx)
	})
	return loop
}

// 测试辅助：写入一条待发送记录。
func enqueueOutbox(t *testing.T, database *Store, eventID string) {
	t.Helper()
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.OutboxEnqueue(NotificationOutbox{
			EventID: eventID, EventType: "apply_failure", Payload: `{"结果":"失败"}`,
		})
		return err
	}); err != nil {
		t.Fatalf("写入 outbox 失败：%v", err)
	}
}

// 测试辅助：等待条件成立或超时。
func waitFor(t *testing.T, timeout time.Duration, condition func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

// 后台循环应自动投递已提交的记录，无需外部驱动。
func TestOutboxLoopDeliversCommittedEntries(t *testing.T) {
	database := openQueryStore(t)
	sender := &classifyingSender{}
	loop := mustOutboxLoop(t, database, sender, nil)

	enqueueOutbox(t, database, "evt-1")
	loop.Start()

	if !waitFor(t, 2*time.Second, func() bool { return sender.callCount() == 1 }) {
		t.Fatalf("循环应自动投递记录，实际投递 %d 条", sender.callCount())
	}
	if entries := mustOutboxEntries(t, database); entries[0].Status != OutboxStatusSent {
		t.Fatalf("投递后应进入 sent：%+v", entries[0])
	}
}

// 循环启动时应立即处理积压记录，不空等一个轮询间隔。
func TestOutboxLoopProcessesBacklogOnStart(t *testing.T) {
	database := openQueryStore(t)
	sender := &classifyingSender{}
	// 使用较长间隔：若循环不立即执行，用例会超时失败。
	loop := mustOutboxLoop(t, database, sender, func(config *OutboxLoopConfig) {
		config.Interval = 10 * time.Second
	})

	enqueueOutbox(t, database, "evt-1")
	loop.Start()

	if !waitFor(t, 2*time.Second, func() bool { return sender.callCount() == 1 }) {
		t.Fatal("启动后应立即处理积压记录，而不是等一个轮询间隔")
	}
}

// 重复启动必须无副作用。
func TestOutboxLoopStartIsIdempotent(t *testing.T) {
	database := openQueryStore(t)
	sender := &classifyingSender{}
	loop := mustOutboxLoop(t, database, sender, nil)

	loop.Start()
	loop.Start()
	loop.Start()

	enqueueOutbox(t, database, "evt-1")
	if !waitFor(t, 2*time.Second, func() bool { return sender.callCount() == 1 }) {
		t.Fatalf("重复启动不应影响投递，实际投递 %d 条", sender.callCount())
	}
}

// 重复停止必须无副作用且不 panic。
func TestOutboxLoopCloseIsIdempotent(t *testing.T) {
	database := openQueryStore(t)
	loop := mustOutboxLoop(t, database, &classifyingSender{}, nil)
	loop.Start()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := loop.Close(ctx); err != nil {
		t.Fatalf("首次停止失败：%v", err)
	}
	if err := loop.Close(ctx); err != nil {
		t.Fatalf("重复停止应安全：%v", err)
	}
}

// 未启动时停止必须立即返回，不得悬挂。
func TestOutboxLoopCloseBeforeStart(t *testing.T) {
	database := openQueryStore(t)
	loop := mustOutboxLoop(t, database, &classifyingSender{}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	if err := loop.Close(ctx); err != nil {
		t.Fatalf("未启动时停止应成功：%v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("未启动时停止应立即返回，实际耗时 %v", elapsed)
	}
}

// 停止后不得再投递新记录。
func TestOutboxLoopStopsDeliveringAfterClose(t *testing.T) {
	database := openQueryStore(t)
	sender := &classifyingSender{}
	loop := mustOutboxLoop(t, database, sender, nil)
	loop.Start()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := loop.Close(ctx); err != nil {
		t.Fatalf("停止失败：%v", err)
	}

	enqueueOutbox(t, database, "evt-1")
	time.Sleep(100 * time.Millisecond)
	if sender.callCount() != 0 {
		t.Fatalf("停止后不应投递：实际投递 %d 条", sender.callCount())
	}
}

// 并发投递受上限约束：一批通知不得同时涌向外部服务。
func TestOutboxLoopRespectsConcurrencyLimit(t *testing.T) {
	database := openQueryStore(t)
	// 用带并发计数的发送器观测实际并发。
	sender := &concurrencyProbeSender{}
	loop := mustOutboxLoop(t, database, sender, func(config *OutboxLoopConfig) {
		config.Concurrency = 2
	})

	for index := 0; index < 8; index += 1 {
		enqueueOutbox(t, database, "evt-"+string(rune('a'+index)))
	}
	loop.Start()

	if !waitFor(t, 3*time.Second, func() bool { return sender.completed() >= 8 }) {
		t.Fatalf("应投递全部 8 条，实际 %d 条", sender.completed())
	}
	if peak := sender.peakConcurrency(); peak > 2 {
		t.Fatalf("并发投递数不应超过上限 2，实际峰值 %d", peak)
	}
}

// 单轮处理条数受批量上限约束：长时间占用数据库会让管理请求排队。
func TestOutboxLoopRespectsBatchLimit(t *testing.T) {
	database := openQueryStore(t)
	sender := &classifyingSender{}
	loop := mustOutboxLoop(t, database, sender, func(config *OutboxLoopConfig) {
		config.Batch = 2
		config.Interval = 5 * time.Millisecond
	})

	for index := 0; index < 5; index += 1 {
		enqueueOutbox(t, database, "evt-"+string(rune('a'+index)))
	}
	loop.Start()

	// 分批推进，最终应全部投递完成。
	if !waitFor(t, 3*time.Second, func() bool { return sender.callCount() >= 5 }) {
		t.Fatalf("分批后仍应投递全部记录，实际 %d 条", sender.callCount())
	}
}

// 目标不存在时应触发回退回调，用于把在途记录转入 discarded。
func TestOutboxLoopReportsMissingTarget(t *testing.T) {
	database := openQueryStore(t)
	sender := &classifyingSender{}
	var reported []string
	var mutex sync.Mutex
	loop := mustOutboxLoop(t, database, sender, func(config *OutboxLoopConfig) {
		config.OnTargetMissing = func(_ context.Context, targetID string) error {
			mutex.Lock()
			reported = append(reported, targetID)
			mutex.Unlock()
			return nil
		}
	})

	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.OutboxEnqueue(NotificationOutbox{
			EventID: "evt-1", TargetID: "missing-target",
			EventType: "apply_failure", Payload: "{}",
		})
		return err
	}); err != nil {
		t.Fatalf("写入 outbox 失败：%v", err)
	}
	loop.Start()

	if !waitFor(t, 2*time.Second, func() bool {
		mutex.Lock()
		defer mutex.Unlock()
		return len(reported) == 1
	}) {
		t.Fatal("目标缺失时应触发回调")
	}
	mutex.Lock()
	defer mutex.Unlock()
	if reported[0] != "missing-target" {
		t.Fatalf("回调应带目标标识：%v", reported)
	}
	// 目标缺失时不应调用发送器。
	if sender.callCount() != 0 {
		t.Fatal("目标缺失时不应尝试投递")
	}
}

// 未指定目标的记录不触发缺失回调：由写入方决定其去向。
func TestOutboxLoopIgnoresEmptyTarget(t *testing.T) {
	database := openQueryStore(t)
	sender := &classifyingSender{}
	called := false
	loop := mustOutboxLoop(t, database, sender, func(config *OutboxLoopConfig) {
		config.OnTargetMissing = func(context.Context, string) error {
			called = true
			return nil
		}
	})

	enqueueOutbox(t, database, "evt-1") // 不带 TargetID
	loop.Start()

	if !waitFor(t, 2*time.Second, func() bool { return sender.callCount() == 1 }) {
		t.Fatal("无目标记录仍应投递")
	}
	if called {
		t.Fatal("目标为空时不应触发缺失回调")
	}
}

// 循环的计数器应反映实际投递与失败。
func TestOutboxLoopCounters(t *testing.T) {
	database := openQueryStore(t)
	sender := &classifyingSender{kind: deliveryPermanent, failures: 1}
	loop := mustOutboxLoop(t, database, sender, nil)

	enqueueOutbox(t, database, "evt-1")
	loop.Start()

	if !waitFor(t, 2*time.Second, func() bool { return loop.Dispatched() >= 1 }) {
		t.Fatalf("应记录成功投递数，实际 %d", loop.Dispatched())
	}
	if loop.Rounds() == 0 {
		t.Fatal("应记录执行轮数")
	}
}

// concurrencyProbeSender 观测并发投递的峰值。
type concurrencyProbeSender struct {
	mutex          sync.Mutex
	active         int
	peak           int
	completedCount int
}

func (sender *concurrencyProbeSender) Send(_ context.Context, _ NotificationOutbox) error {
	sender.mutex.Lock()
	sender.active += 1
	if sender.active > sender.peak {
		sender.peak = sender.active
	}
	sender.mutex.Unlock()

	time.Sleep(20 * time.Millisecond)

	sender.mutex.Lock()
	sender.active -= 1
	sender.completedCount += 1
	sender.mutex.Unlock()
	return nil
}

func (sender *concurrencyProbeSender) peakConcurrency() int {
	sender.mutex.Lock()
	defer sender.mutex.Unlock()
	return sender.peak
}

func (sender *concurrencyProbeSender) completed() int {
	sender.mutex.Lock()
	defer sender.mutex.Unlock()
	return sender.completedCount
}
