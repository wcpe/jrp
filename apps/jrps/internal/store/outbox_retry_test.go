package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// 确定性失败必须直接进入失败终态，不消耗重试次数。
//
// 重试只会重复同一个结果，白白拉长失败终态的到达时间。
func TestOutboxPermanentFailureSkipsRetry(t *testing.T) {
	database := openQueryStore(t)
	sender := &classifyingSender{kind: deliveryPermanent, failures: 1}
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.OutboxEnqueue(NotificationOutbox{EventID: "evt-1", EventType: "x", Payload: "{}"})
		return err
	}); err != nil {
		t.Fatalf("写入 outbox 失败：%v", err)
	}

	dispatcher := mustDispatcher(t, database, sender)
	if _, err := dispatcher.DispatchCommitted(context.Background(), 1); err != nil {
		t.Fatalf("派发失败：%v", err)
	}

	entries := mustOutboxEntries(t, database)
	if entries[0].Status != OutboxStatusFailed {
		t.Fatalf("确定性失败应直接进入失败终态：%+v", entries[0])
	}
	if entries[0].Attempts != 1 {
		t.Fatalf("确定性失败只应记录一次尝试：%d", entries[0].Attempts)
	}
	if entries[0].StoppedAt == nil {
		t.Fatal("失败终态应记录停止时间")
	}
}

// 可重试失败按退避重新入队，达到上限后才进入失败终态。
func TestOutboxRetryableFailureKeepsRetrying(t *testing.T) {
	database := openQueryStore(t)
	sender := &classifyingSender{kind: deliveryRetryable, failures: 2}
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.OutboxEnqueue(NotificationOutbox{EventID: "evt-1", EventType: "x", Payload: "{}"})
		return err
	}); err != nil {
		t.Fatalf("写入 outbox 失败：%v", err)
	}

	dispatcher := mustDispatcher(t, database, sender)
	for attempt := 0; attempt < 2; attempt += 1 {
		if _, err := dispatcher.DispatchCommitted(context.Background(), 1); err != nil {
			t.Fatalf("第 %d 次派发失败：%v", attempt+1, err)
		}
		entries := mustOutboxEntries(t, database)
		if entries[0].Status != OutboxStatusRetrying {
			t.Fatalf("可重试失败应进入 retrying：%+v", entries[0])
		}
		if entries[0].StoppedAt != nil {
			t.Fatal("尚未进入终态时不应记录停止时间")
		}
	}

	// 第三次成功。
	if _, err := dispatcher.DispatchCommitted(context.Background(), 1); err != nil {
		t.Fatalf("派发失败：%v", err)
	}
	if entries := mustOutboxEntries(t, database); entries[0].Status != OutboxStatusSent {
		t.Fatalf("恢复后应进入 sent：%+v", entries[0])
	}
}

// 租约过期的 sending 记录应被重新投递：进程崩溃后不能永久卡住。
func TestOutboxReclaimsExpiredLease(t *testing.T) {
	database := openQueryStore(t)
	sender := &classifyingSender{}
	expired := time.Now().UTC().Add(-time.Minute)

	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		// 模拟上一进程崩溃：记录停在 sending 且租约已过期。
		return tx.db.Create(&NotificationOutbox{
			EventID: "evt-1", EventType: "x", Payload: "{}",
			Status: OutboxStatusSending, Attempts: 1, LeaseExpiresAt: &expired,
		}).Error
	}); err != nil {
		t.Fatalf("写入卡住的记录失败：%v", err)
	}

	dispatcher := mustDispatcher(t, database, sender)
	processed, err := dispatcher.DispatchCommitted(context.Background(), 1)
	if err != nil {
		t.Fatalf("派发失败：%v", err)
	}
	if processed != 1 || len(sender.calls) != 1 {
		t.Fatalf("租约过期的记录应被重新投递：processed=%d calls=%d", processed, len(sender.calls))
	}
	if entries := mustOutboxEntries(t, database); entries[0].Status != OutboxStatusSent {
		t.Fatalf("重投后应进入 sent：%+v", entries[0])
	}
}

// 租约未过期的 sending 记录不得被重新投递：避免拖慢正常投递的重复发送。
func TestOutboxSkipsActiveLease(t *testing.T) {
	database := openQueryStore(t)
	sender := &classifyingSender{}
	active := time.Now().UTC().Add(time.Minute)

	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		return tx.db.Create(&NotificationOutbox{
			EventID: "evt-1", EventType: "x", Payload: "{}",
			Status: OutboxStatusSending, Attempts: 1, LeaseExpiresAt: &active,
		}).Error
	}); err != nil {
		t.Fatalf("写入记录失败：%v", err)
	}

	dispatcher := mustDispatcher(t, database, sender)
	processed, err := dispatcher.DispatchCommitted(context.Background(), 1)
	if err != nil {
		t.Fatalf("派发失败：%v", err)
	}
	if processed != 0 || len(sender.calls) != 0 {
		t.Fatalf("租约生效期间的记录不应被投递：processed=%d calls=%d", processed, len(sender.calls))
	}
}

// 同一记录被并发抢占时只能有一个成功：防并发双发的核心保证。
func TestOutboxClaimIsAtomic(t *testing.T) {
	database := openQueryStore(t)
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.OutboxEnqueue(NotificationOutbox{EventID: "evt-1", EventType: "x", Payload: "{}"})
		return err
	}); err != nil {
		t.Fatalf("写入 outbox 失败：%v", err)
	}
	entries := mustOutboxEntries(t, database)
	dispatcher := mustDispatcher(t, database, &classifyingSender{})

	const contenders = 8
	var waitGroup sync.WaitGroup
	var mutex sync.Mutex
	claimed := 0
	for index := 0; index < contenders; index += 1 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			ok, err := dispatcher.claim(context.Background(), entries[0].ID)
			if err != nil {
				return
			}
			if ok {
				mutex.Lock()
				claimed += 1
				mutex.Unlock()
			}
		}()
	}
	waitGroup.Wait()

	if claimed != 1 {
		t.Fatalf("%d 个并发抢占者中应恰好 1 个成功，实际 %d 个", contenders, claimed)
	}
}

// 退避必须带抖动：同一批失败的重试时间应彼此错开，避免重试风暴。
func TestOutboxBackoffHasJitter(t *testing.T) {
	seen := make(map[time.Duration]int)
	for index := 0; index < 200; index += 1 {
		seen[defaultBackoff(1)] += 1
	}
	if len(seen) < 10 {
		t.Fatalf("退避应有抖动，实际只产生了 %d 种取值", len(seen))
	}

	// 抖动幅度有界：不超过基准的 20%。
	base := outboxBackoffBase
	for value := range seen {
		if value < base || value >= base+base/5+time.Millisecond {
			t.Fatalf("退避取值 %v 超出有界抖动范围", value)
		}
	}
}

// 退避随尝试次数递增且有上限。
func TestOutboxBackoffGrowsAndIsBounded(t *testing.T) {
	first := defaultBackoff(1)
	fifth := defaultBackoff(5)
	if fifth <= first {
		t.Fatalf("退避应随尝试次数递增：第一次 %v，第五次 %v", first, fifth)
	}
	// 超出最大尝试次数时按上限截断，不产生超长等待。
	beyond := defaultBackoff(100)
	maxWait := time.Duration(defaultOutboxMaxAttempts) * outboxBackoffBase
	maxJitter := time.Duration(float64(maxWait) * outboxBackoffJitterRatio)
	if beyond > maxWait+maxJitter {
		t.Fatalf("退避应有上限，实际 %v", beyond)
	}
}

// 失败终态的错误摘要必须单行且有长度上限：它要进入管理查询与日志。
func TestOutboxErrorSummaryIsSanitized(t *testing.T) {
	long := errors.New("第一行\n第二行\r\n第三行\t带制表符 " + strings.Repeat("填充", 100))
	summary := sanitizeOutboxError(long)

	if strings.ContainsAny(summary, "\n\r\t") {
		t.Fatalf("错误摘要应为单行：%q", summary)
	}
	if len([]rune(summary)) > maxOutboxErrorRunes {
		t.Fatalf("错误摘要应限长 %d，实际 %d", maxOutboxErrorRunes, len([]rune(summary)))
	}
}

// 目标被删除时，其待发送记录必须转入 discarded 并留下停止时间（规格 §3.6）。
func TestOutboxDiscardsEntriesOfRemovedTarget(t *testing.T) {
	database := openQueryStore(t)
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		if _, err := tx.OutboxEnqueue(NotificationOutbox{
			EventID: "evt-1", TargetID: "target-1", EventType: "x", Payload: "{}",
		}); err != nil {
			return err
		}
		_, err := tx.OutboxEnqueue(NotificationOutbox{
			EventID: "evt-2", TargetID: "target-2", EventType: "x", Payload: "{}",
		})
		return err
	}); err != nil {
		t.Fatalf("写入 outbox 失败：%v", err)
	}

	var discarded int
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		var err error
		discarded, err = tx.DiscardOutboxForTarget("target-1")
		return err
	}); err != nil {
		t.Fatalf("丢弃记录失败：%v", err)
	}
	if discarded != 1 {
		t.Fatalf("应丢弃 1 条记录，实际 %d 条", discarded)
	}

	entries := mustOutboxEntries(t, database)
	for _, entry := range entries {
		if entry.TargetID == "target-1" {
			if entry.Status != OutboxStatusDiscarded {
				t.Fatalf("已删除目标的记录应转入 discarded：%+v", entry)
			}
			if entry.StoppedAt == nil {
				t.Fatal("discarded 记录应留下停止时间")
			}
		}
		if entry.TargetID == "target-2" && entry.Status != OutboxStatusPending {
			t.Fatalf("其他目标的记录不应受影响：%+v", entry)
		}
	}
}

// discarded 记录不得再被投递。
func TestOutboxDoesNotDispatchDiscarded(t *testing.T) {
	database := openQueryStore(t)
	sender := &classifyingSender{}
	stopped := time.Now().UTC()
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		return tx.db.Create(&NotificationOutbox{
			EventID: "evt-1", EventType: "x", Payload: "{}",
			Status: OutboxStatusDiscarded, StoppedAt: &stopped,
		}).Error
	}); err != nil {
		t.Fatalf("写入记录失败：%v", err)
	}

	dispatcher := mustDispatcher(t, database, sender)
	processed, err := dispatcher.DispatchCommitted(context.Background(), 1)
	if err != nil {
		t.Fatalf("派发失败：%v", err)
	}
	if processed != 0 || len(sender.calls) != 0 {
		t.Fatalf("discarded 记录不应被投递：processed=%d calls=%d", processed, len(sender.calls))
	}
}

// deliveryKind 与 notify 包的分类保持一致的语义。
type deliveryKind int

const (
	deliveryRetryable deliveryKind = iota
	deliveryPermanent
)

// classifyingSender 返回带分类的投递错误，并可指定前若干次失败。
//
// 必须并发安全：后台发送循环会并发调用 Send，未加保护的切片写入会构成数据竞争。
type classifyingSender struct {
	mutex    sync.Mutex
	calls    []NotificationOutbox
	failures int
	kind     deliveryKind
}

func (sender *classifyingSender) Send(_ context.Context, entry NotificationOutbox) error {
	sender.mutex.Lock()
	defer sender.mutex.Unlock()
	sender.calls = append(sender.calls, entry)
	if sender.failures > 0 {
		sender.failures -= 1
		return classifiedError{retryable: sender.kind == deliveryRetryable}
	}
	return nil
}

// callCount 返回已投递的条数。
func (sender *classifyingSender) callCount() int {
	sender.mutex.Lock()
	defer sender.mutex.Unlock()
	return len(sender.calls)
}

// classifiedError 实现 RetryClassifier，供 store 层判定可重试性。
type classifiedError struct {
	retryable bool
}

func (err classifiedError) Error() string { return "模拟投递失败" }

func (err classifiedError) Retryable() bool { return err.retryable }

// 已转入 discarded 的记录不得被发送结果覆盖。
//
// 回归用例：`recordOutcome` 的 UPDATE 只带 `id`，于是投递期间被丢弃的记录会在
// 投递结束后被改写成 sent 或 retrying——已明确不再投递的通知会被重新发出，且
// 留下 retrying 与 stopped_at 并存的自相矛盾记录。
func TestOutboxDiscardedStateSurvivesDeliveryOutcome(t *testing.T) {
	database := openQueryStore(t)
	sender := &classifyingSender{}
	stopped := time.Now().UTC()

	var entryID uint64
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		var err error
		entryID, err = tx.OutboxEnqueue(NotificationOutbox{
			EventID: "evt-1", TargetID: "target-1", EventType: "x", Payload: "{}",
		})
		return err
	}); err != nil {
		t.Fatalf("写入 outbox 失败：%v", err)
	}

	// 模拟投递进行中：先抢占（置为 sending），再把记录转入 discarded。
	dispatcher := mustDispatcher(t, database, sender)
	claimed, err := dispatcher.claim(context.Background(), entryID)
	if err != nil || !claimed {
		t.Fatalf("抢占应成功：claimed=%v err=%v", claimed, err)
	}
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		return tx.db.Model(&NotificationOutbox{}).Where("id = ?", entryID).
			Updates(map[string]any{
				"status":     OutboxStatusDiscarded,
				"stopped_at": stopped,
			}).Error
	}); err != nil {
		t.Fatalf("转入 discarded 失败：%v", err)
	}

	// 投递返回成功：结果不得覆盖已确定的终态。
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		return tx.updateOutboxOutcome(entryID, 1, 5, func(int) time.Duration { return 0 }, nil)
	}); err != nil {
		t.Fatalf("写入投递结果失败：%v", err)
	}

	entries := mustOutboxEntries(t, database)
	if entries[0].Status != OutboxStatusDiscarded {
		t.Fatalf("已丢弃的终态被投递结果覆盖为 %s", entries[0].Status)
	}
}

// 目标查询失败不得被当作"目标不存在"。
//
// 回归用例：`NotificationTargetByID` 把任何数据库错误都包装成"目标不存在"，
// 调用方据此把查询失败与真正缺失混为一谈——生产路径下会把完好目标的在途通知
// 全部转入 discarded 并写一条内容错误的审计。
func TestTargetLookupFailureIsNotMissing(t *testing.T) {
	database := openQueryStore(t)

	// 先建一个目标，确认正常查询不报缺失。
	var targetID string
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		id, err := NewNotificationTargetID()
		if err != nil {
			return err
		}
		targetID = id
		view, err := tx.CreateNotificationTarget(ActorAdmin("admin"), NotificationTargetInput{
			Name: "目标", Type: NotificationTypeWebhook, Enabled: true,
			WebhookURL: "https://hooks.example.com/hook",
		})
		if err != nil {
			return err
		}
		targetID = view.ID
		return nil
	}); err != nil {
		t.Fatalf("创建目标失败：%v", err)
	}

	// 用已取消的上下文查询：这是"查询失败"而非"目标不存在"。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	var lookupErr error
	_ = database.View(cancelled, func(tx *Tx) error {
		_, lookupErr = tx.NotificationTargetByID(targetID)
		return nil
	})
	if lookupErr == nil {
		// 前提不成立就明确失败，而不是跳过：静默跳过会让该分支在别的环境下
		// 悄悄失去覆盖，而它保护的是一条会把完好目标的在途通知误丢弃的路径。
		t.Fatal("已取消的上下文未使查询失败，用例前提不成立")
	}
	if errors.Is(lookupErr, ErrNotificationTargetMissing) {
		t.Fatalf("查询失败被误判为目标不存在：%v", lookupErr)
	}

	// 真正的缺失仍应返回该哨兵，否则调用方的 404 分支会失效。
	var missingErr error
	_ = database.View(context.Background(), func(tx *Tx) error {
		_, missingErr = tx.NotificationTargetByID("不存在的目标标识")
		return nil
	})
	if !errors.Is(missingErr, ErrNotificationTargetMissing) {
		t.Fatalf("真正缺失的目标应返回哨兵错误：%v", missingErr)
	}
}
