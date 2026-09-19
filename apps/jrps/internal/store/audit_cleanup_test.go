package store

import (
	"context"
	"testing"
	"time"
)

// 测试辅助：写入一条指定时间的审计事件。
//
// writeAudit 会用服务端当前时间覆盖调用方给的时间，因此这里不走 writeAudit，
// 而是直接写库来构造"历史事件"；用例要验证的正是按时间维度的清理行为。
func insertAuditEventAt(t *testing.T, database *Store, occurredAt time.Time, summary string) {
	t.Helper()
	event := baseAuditEvent()
	event.OccurredAt = occurredAt
	event.Context = summary
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		return tx.db.Create(&event).Error
	}); err != nil {
		t.Fatalf("写入历史审计事件失败：%v", err)
	}
}

// 测试辅助：把审计保留天数设为指定值。
func setAuditRetentionDays(t *testing.T, database *Store, days int) {
	t.Helper()
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.UpdateCapturePolicy(ActorAdmin("admin"), CapturePolicyInput{
			RetentionDays:      DefaultRetentionDays,
			MaxTotalBytes:      DefaultMaxTotalBytes,
			AuditRetentionDays: days,
		})
		return err
	}); err != nil {
		t.Fatalf("设置审计保留天数失败：%v", err)
	}
}

// 测试辅助：执行一次清理。
func runAuditCleanup(t *testing.T, database *Store) AuditCleanupResult {
	t.Helper()
	var result AuditCleanupResult
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		var err error
		result, err = tx.CleanupAuditEvents(ActorAdmin("server"))
		return err
	}); err != nil {
		t.Fatalf("清理审计事件失败：%v", err)
	}
	return result
}

// 超出保留期的审计事件应被删除，且在期内的必须保留。
func TestAuditCleanupDeletesOnlyExpiredEvents(t *testing.T) {
	database := openQueryStore(t)
	now := time.Now().UTC()

	insertAuditEventAt(t, database, now.AddDate(0, 0, -400), "四百天前的旧事件")
	insertAuditEventAt(t, database, now.AddDate(0, 0, -3), "三天前的事件")

	result := runAuditCleanup(t, database)
	if result.Deleted != 1 {
		t.Fatalf("应删除 1 条过期事件，实际 %d 条", result.Deleted)
	}
	if result.RetentionDays != DefaultAuditRetentionDays {
		t.Fatalf("结果应带上生效的保留天数，实际 %d", result.RetentionDays)
	}

	events := mustAuditEvents(t, database)
	for _, event := range events {
		if event.Context == "四百天前的旧事件" {
			t.Fatal("过期事件必须被删除")
		}
	}
	// 在期内的事件与清理自身的留痕都应保留。
	foundRecent := false
	for _, event := range events {
		if event.Context == "三天前的事件" {
			foundRecent = true
		}
	}
	if !foundRecent {
		t.Fatal("在期内的事件不得被删除")
	}
}

// 清理动作自身必须留痕：删除了多少、按什么策略删的都要可查（§3.7）。
func TestAuditCleanupLeavesTrace(t *testing.T) {
	database := openQueryStore(t)
	insertAuditEventAt(t, database, time.Now().UTC().AddDate(0, 0, -500), "远古事件")

	runAuditCleanup(t, database)

	events := mustAuditEvents(t, database)
	assertAuditEvent(t, events, auditExpectation{
		Action:     ActionAuditCleanup,
		ObjectType: ObjectTypeRetentionPolicy,
		Result:     AuditResultSuccess,
	})
}

// 清理留痕不会被同一次清理删掉：它的时间戳晚于截止时间。
func TestAuditCleanupDoesNotDeleteItsOwnTrace(t *testing.T) {
	database := openQueryStore(t)
	insertAuditEventAt(t, database, time.Now().UTC().AddDate(0, 0, -500), "远古事件")

	runAuditCleanup(t, database)

	events := mustAuditEvents(t, database)
	if len(events) != 1 {
		t.Fatalf("清理后应只剩留痕一条，实际 %d 条", len(events))
	}
	if events[0].Action != ActionAuditCleanup {
		t.Fatalf("留下的应是清理留痕，实际 %s", events[0].Action)
	}
}

// 保留天数放宽后，已过期但尚未清理的事件仍在；清理按当前策略判定。
func TestAuditCleanupUsesCurrentPolicy(t *testing.T) {
	database := openQueryStore(t)
	insertAuditEventAt(t, database, time.Now().UTC().AddDate(0, 0, -40), "四十天前的事件")

	// 默认保留 180 天：40 天前的事件在期内，不该被删。
	first := runAuditCleanup(t, database)
	if first.Deleted != 0 {
		t.Fatalf("默认策略下不应删除任何事件，实际 %d 条", first.Deleted)
	}

	// 把保留期收紧到 30 天：同一条事件现在过期了。
	setAuditRetentionDays(t, database, 30)
	second := runAuditCleanup(t, database)
	if second.RetentionDays != 30 {
		t.Fatalf("第二次清理应使用生效的 30 天策略，实际 %d", second.RetentionDays)
	}
	events := mustAuditEvents(t, database)
	// 留下三条：第一次清理的留痕、策略变更审计、第二次清理的留痕。
	// 四十天前的事件已被收紧后的策略清理掉。
	if len(events) != 3 {
		t.Fatalf("清理后应剩 3 条留痕，实际 %d 条", len(events))
	}
	for _, event := range events {
		if event.Context == "四十天前的事件" {
			t.Fatal("收紧保留期后，过期事件应被删除")
		}
	}
}

// 单轮清理有批次上限，超过部分留待下一轮：避免长事务阻塞管理请求。
func TestAuditCleanupBatchesByLimit(t *testing.T) {
	database := openQueryStore(t)
	expired := time.Now().UTC().AddDate(0, 0, -500)
	for index := 0; index < maxAuditCleanupBatch+50; index += 1 {
		insertAuditEventAt(t, database, expired, "过期事件")
	}

	first := runAuditCleanup(t, database)
	if first.Deleted != maxAuditCleanupBatch {
		t.Fatalf("单轮应删除上限 %d 条，实际 %d 条", maxAuditCleanupBatch, first.Deleted)
	}

	// 继续清理直到收敛，剩余记录应被清空。
	second := runAuditCleanup(t, database)
	if second.Deleted != 50 {
		t.Fatalf("第二轮应删除剩余 50 条，实际 %d 条", second.Deleted)
	}
	third := runAuditCleanup(t, database)
	if third.Deleted != 0 {
		t.Fatalf("收敛后不应再删除，实际 %d 条", third.Deleted)
	}
}

// 无过期事件时清理应为空操作，且仍留下留痕。
func TestAuditCleanupWithNothingExpired(t *testing.T) {
	database := openQueryStore(t)

	result := runAuditCleanup(t, database)
	if result.Deleted != 0 {
		t.Fatalf("无过期事件时应删除 0 条，实际 %d 条", result.Deleted)
	}
	if len(mustAuditEvents(t, database)) != 1 {
		t.Fatal("空清理也应留下留痕")
	}
}

// 审计清理不产生递归留痕：清理留痕本身不再触发新的清理事件。
func TestAuditCleanupIsNotRecursive(t *testing.T) {
	database := openQueryStore(t)
	insertAuditEventAt(t, database, time.Now().UTC().AddDate(0, 0, -500), "远古事件")

	runAuditCleanup(t, database)
	runAuditCleanup(t, database)
	runAuditCleanup(t, database)

	events := mustAuditEvents(t, database)
	cleanupTraces := 0
	for _, event := range events {
		if event.Action == ActionAuditCleanup {
			cleanupTraces += 1
		}
	}
	if cleanupTraces != 3 {
		t.Fatalf("三次清理应产生三条留痕且不再递归，实际 %d 条", cleanupTraces)
	}
}

// 审计清理只按时间维度：不因条数多而多删，也不因总量小而提前删（§3.7）。
func TestAuditCleanupIgnoresVolume(t *testing.T) {
	database := openQueryStore(t)
	// 把正文总量上限设到最小，也不应影响审计清理。
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.UpdateCapturePolicy(ActorAdmin("admin"), CapturePolicyInput{
			RetentionDays:      MinRetentionDays,
			MaxTotalBytes:      MinMaxTotalBytes,
			AuditRetentionDays: DefaultAuditRetentionDays,
		})
		return err
	}); err != nil {
		t.Fatalf("设置策略失败：%v", err)
	}
	insertAuditEventAt(t, database, time.Now().UTC().AddDate(0, 0, -10), "十天前的事件")

	result := runAuditCleanup(t, database)
	if result.Deleted != 0 {
		t.Fatalf("总量上限不应影响审计清理，实际删除 %d 条", result.Deleted)
	}
}

// AuditEventCount 反映当前审计事件水位，供运维观察。
func TestAuditEventCount(t *testing.T) {
	database := openQueryStore(t)
	seedAuditEvents(t, database, 5)

	var count int64
	if err := database.View(context.Background(), func(tx *Tx) error {
		var err error
		count, err = tx.AuditEventCount()
		return err
	}); err != nil {
		t.Fatalf("统计审计事件失败：%v", err)
	}
	if count != 5 {
		t.Fatalf("审计事件应为 5 条，实际 %d 条", count)
	}
}
