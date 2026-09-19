package store

import (
	"fmt"
	"time"
)

// maxAuditCleanupBatch 是单轮清理删除的最大条数。
//
// 分批而非一次删净：SQLite 连接池固定为单连接，长时间持有写事务会让管理请求
// 排队等待；分批也让清理进程可被中断而不留下半完成状态。
const maxAuditCleanupBatch = 1000

// AuditCleanupResult 是一次审计清理的结果。
type AuditCleanupResult struct {
	// Deleted 是本次删除的审计事件条数。
	Deleted int
	// Cutoff 是本次清理的截止时间：早于它的事件被删除。
	Cutoff time.Time
	// RetentionDays 是执行本次清理时生效的保留天数。
	RetentionDays int
}

// CleanupAuditEvents 按审计保留策略清理过期审计事件。
//
// 行为边界（FR-16 规格 §3.7）：
//   - 只按时间维度，不按大小维度：审计保留策略没有容量上限。
//   - 清理动作本身先写入一条审计事件再执行删除，保证"清理发生过"这件事
//     有据可查；即便随后删除失败，也不会出现无痕清理。
//   - 清理产生的那条审计事件不会被本次清理删除：它的时间戳晚于截止时间。
//
// 返回本次删除的条数。调用方应按需重复调用直至 Deleted 为 0，从而在不占用
// 长事务的前提下清理任意规模的历史数据。
func (tx *Tx) CleanupAuditEvents(actor Actor) (AuditCleanupResult, error) {
	var result AuditCleanupResult
	err := tx.Transaction(func() error {
		policy, err := tx.CapturePolicy()
		if err != nil {
			return err
		}
		cutoff := time.Now().UTC().AddDate(0, 0, -policy.AuditRetentionDays)
		result.Cutoff = cutoff
		result.RetentionDays = policy.AuditRetentionDays

		// 先留痕：清理动作写入审计后才删除，避免"删了但没记录"。
		if err := tx.writeAudit(AuditEvent{
			ActorType:  actor.Type,
			ActorID:    actor.ID,
			Action:     ActionAuditCleanup,
			ObjectType: ObjectTypeRetentionPolicy,
			ObjectID:   "audit",
			Result:     AuditResultSuccess,
			Context: fmt.Sprintf("按保留策略清理审计事件：保留 %d 天，删除 %s 之前的记录",
				policy.AuditRetentionDays, cutoff.Format(time.RFC3339)),
		}); err != nil {
			return err
		}

		// 按主键升序分批删除：与查询同序，使"随时间推进"与"随主键推进"一致。
		statement := tx.db.Exec(
			"DELETE FROM audit_events WHERE id IN ("+
				"SELECT id FROM audit_events WHERE occurred_at < ? ORDER BY id ASC LIMIT ?)",
			cutoff, maxAuditCleanupBatch,
		)
		if statement.Error != nil {
			return fmt.Errorf("清理审计事件失败：%w", translateSQLError(statement.Error))
		}
		result.Deleted = int(statement.RowsAffected)
		return nil
	})
	if err != nil {
		return AuditCleanupResult{}, err
	}
	return result, nil
}

// AuditEventCount 返回当前审计事件总条数，供运维观察水位。
func (tx *Tx) AuditEventCount() (int64, error) {
	var count int64
	if err := tx.db.Model(&AuditEvent{}).Count(&count).Error; err != nil {
		return 0, fmt.Errorf("统计审计事件失败：%w", translateSQLError(err))
	}
	return count, nil
}
