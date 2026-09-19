package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"gorm.io/gorm"
)

// OutboxEnqueue 在业务事务内写入一条待发送记录。
//
// 它只写库，不执行任何外部副作用；发送由发送器在事务提交后接管（ADR-0004、FR-15 §3.2）。
func (tx *Tx) OutboxEnqueue(entry NotificationOutbox) (uint64, error) {
	if entry.EventID == "" {
		return 0, errors.New("outbox 事件标识不能为空")
	}
	if entry.EventType == "" {
		return 0, errors.New("outbox 事件类型不能为空")
	}
	if entry.Status == "" {
		entry.Status = OutboxStatusPending
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	entry.UpdatedAt = entry.CreatedAt
	if err := tx.db.Create(&entry).Error; err != nil {
		return 0, fmt.Errorf("写入 outbox 记录失败：%w", translateSQLError(err))
	}
	return entry.ID, nil
}

// Sender 是外部副作用执行者；实现方必须只在事务提交后被调用。
type Sender interface {
	Send(ctx context.Context, entry NotificationOutbox) error
}

// RetryClassifier 由投递错误实现，用于区分可重试与确定性失败。
//
// store 不依赖具体渠道实现，因此不直接引用 notify 包的错误类型，只约定这一
// 最小接口。未实现该接口的错误按可重试处理：把未知故障当临时问题比当作永久
// 失败更保守，前者最多多试几次，后者会让本该送达的通知永久丢失。
type RetryClassifier interface {
	Retryable() bool
}

// OutboxDispatcherConfig 是发送器的配置。
//
// 重试上限与退避序列属于策略值，由调用方给出；未给出时使用保守默认值。
type OutboxDispatcherConfig struct {
	Store  *Store
	Sender Sender
	// MaxAttempts 是发送重试上限，达到后进入失败终态。
	MaxAttempts int
	// Backoff 返回第 attempt 次失败后的等待时长；为空时使用默认序列。
	Backoff func(attempt int) time.Duration
	// Lease 是发送中记录的租约时长；为空时使用默认值。
	//
	// 它决定进程崩溃后多久允许重新投递同一条记录：过短会导致正常的慢投递
	// 被重复触发，过长会延迟崩溃恢复。
	Lease time.Duration
}

// OutboxDispatcher 在事务提交后读取 outbox 并执行外部副作用。
//
// 它与业务事务严格分离：Dispatcher 只通过新的数据库读取取得记录，因此未提交或已回滚的
// 记录对它永远不可见（FR-15 §3.2 的硬边界）。
type OutboxDispatcher struct {
	store       *Store
	sender      Sender
	maxAttempts int
	backoff     func(attempt int) time.Duration
	lease       time.Duration
}

// NewOutboxDispatcher 构造发送器。
func NewOutboxDispatcher(cfg OutboxDispatcherConfig) (*OutboxDispatcher, error) {
	if cfg.Store == nil {
		return nil, errors.New("outbox 发送器需要数据库")
	}
	if cfg.Sender == nil {
		return nil, errors.New("outbox 发送器需要外部副作用实现")
	}
	dispatcher := &OutboxDispatcher{store: cfg.Store, sender: cfg.Sender, backoff: cfg.Backoff}
	dispatcher.maxAttempts = cfg.MaxAttempts
	if dispatcher.maxAttempts <= 0 {
		dispatcher.maxAttempts = defaultOutboxMaxAttempts
	}
	if dispatcher.backoff == nil {
		dispatcher.backoff = defaultBackoff
	}
	dispatcher.lease = cfg.Lease
	if dispatcher.lease <= 0 {
		dispatcher.lease = defaultOutboxLease
	}
	return dispatcher, nil
}

// DispatchCommitted 取出已提交且到期的待发送记录并执行发送，返回本次处理的条数。
func (d *OutboxDispatcher) DispatchCommitted(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 50
	}
	entries, err := d.dueEntries(ctx, limit)
	if err != nil {
		return 0, err
	}
	processed := 0
	for _, entry := range entries {
		if err := d.deliver(ctx, entry); err != nil {
			return processed, err
		}
		processed++
	}
	return processed, nil
}

// dueEntries 读取已提交且到达重试时间、或租约已过期的记录。
//
// 三类记录可被取出：
//   - pending：事务已提交，等待首次发送。
//   - retrying：到达退避时间，等待重试。
//   - sending 且租约已过期：上次发送的进程崩溃或卡死，租约超时后允许重新投递。
//
// 未提交事务的记录在提交前不存在于此表，发送器永远不会拿到它们。
func (d *OutboxDispatcher) dueEntries(ctx context.Context, limit int) ([]NotificationOutbox, error) {
	var entries []NotificationOutbox
	now := time.Now().UTC()
	err := d.store.View(ctx, func(tx *Tx) error {
		return tx.db.
			Where("status = ? OR (status = ? AND next_attempt_at <= ?) OR (status = ? AND lease_expires_at <= ?)",
				OutboxStatusPending, OutboxStatusRetrying, now, OutboxStatusSending, now).
			Order("created_at ASC, id ASC").Limit(limit).Find(&entries).Error
	})
	if err != nil {
		return nil, fmt.Errorf("读取待发送通知失败：%w", err)
	}
	return entries, nil
}

// deliver 抢占记录、执行外部副作用、记录终态。
//
// 抢占（claim）与副作用分离，使进程崩溃后可由租约超时重新投递；
// 抢占本身是条件更新，保证同一记录不会被两个发送器并发投递。
func (d *OutboxDispatcher) deliver(ctx context.Context, entry NotificationOutbox) error {
	claimed, err := d.claim(ctx, entry.ID)
	if err != nil {
		return err
	}
	if !claimed {
		// 记录已被其他发送器抢占或状态已变化，本次跳过而不视为失败。
		return nil
	}
	sendErr := d.sender.Send(ctx, entry)
	return d.recordOutcome(ctx, entry, sendErr)
}

// claim 原子抢占一条记录：只有状态仍为可发送且未被他人持有时才成功。
//
// 条件更新是防并发双发的关键：读—判断—写若分成两步，两个发送器可能都读到
// pending 并各自投递一次。把状态判断放进 UPDATE 的 WHERE 子句后，数据库保证
// 只有一个更新能影响到行，另一个的 RowsAffected 为 0。
func (d *OutboxDispatcher) claim(ctx context.Context, id uint64) (bool, error) {
	now := time.Now().UTC()
	lease := now.Add(d.lease)
	affected := int64(0)
	err := d.store.Transaction(ctx, func(tx *Tx) error {
		statement := tx.db.Model(&NotificationOutbox{}).
			Where("id = ? AND (status = ? OR (status = ? AND next_attempt_at <= ?) OR (status = ? AND lease_expires_at <= ?))",
				id, OutboxStatusPending, OutboxStatusRetrying, now, OutboxStatusSending, now).
			Updates(map[string]any{
				"status":           OutboxStatusSending,
				"lease_expires_at": lease,
				"updated_at":       now,
			})
		if statement.Error != nil {
			return statement.Error
		}
		affected = statement.RowsAffected
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("抢占待发送通知失败：%w", translateSQLError(err))
	}
	return affected > 0, nil
}

// recordOutcome 把发送结果落库。
//
// 发送失败是业务既定结果，已记录后不再向上层报错；只有落库本身失败才返回错误。
func (d *OutboxDispatcher) recordOutcome(ctx context.Context, entry NotificationOutbox, sendErr error) error {
	next := entry.Attempts + 1
	return d.store.Transaction(ctx, func(tx *Tx) error {
		return tx.updateOutboxOutcome(entry.ID, next, d.maxAttempts, d.backoff, sendErr)
	})
}

// 默认重试上限、租约时长与退避基准。
const (
	defaultOutboxMaxAttempts = 5
	defaultOutboxLease       = time.Minute
	outboxBackoffBase        = 30 * time.Second
	// outboxBackoffJitterRatio 是退避抖动比例：实际等待落在 [base, base*(1+ratio)) 区间。
	//
	// 抖动用于打散同一时刻失败的一批记录：若退避是确定值，多个目标同时失败后
	// 会在同一时刻集中重试，形成重试风暴。抖动让它们的重试时间彼此错开。
	outboxBackoffJitterRatio = 0.2
)

// defaultBackoff 返回带抖动的有限递增退避时长。
//
// 退避随尝试次数线性增长，并叠加上限为基准 20% 的随机抖动。上限固定在
// 最大尝试次数，避免调用方传入更大的 attempt 时算出超长等待。
func defaultBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > defaultOutboxMaxAttempts {
		attempt = defaultOutboxMaxAttempts
	}
	base := time.Duration(attempt) * outboxBackoffBase
	return base + jitterDuration(base)
}

// jitterDuration 返回 [0, base*outboxBackoffJitterRatio) 区间内的随机时长。
func jitterDuration(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	span := int64(float64(base) * outboxBackoffJitterRatio)
	if span <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(span))
}

// updateOutboxOutcome 依据发送结果更新状态、重试次数与退避时间。
//
// 失败的归类决定走向：确定性失败（目标地址无效、鉴权失败等）直接进入 failed，
// 不再消耗重试次数——重试只会重复同一个确定性结果，白白拉长失败终态的到达
// 时间。可重试失败按退避重新入队，达到上限后进入 failed 并记录停止时间。
func (tx *Tx) updateOutboxOutcome(
	id uint64,
	nextAttempt int,
	maxAttempts int,
	backoff func(int) time.Duration,
	sendErr error,
) error {
	var entry NotificationOutbox
	if err := tx.db.First(&entry, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("待发送通知 %d 不存在", id)
		}
		return fmt.Errorf("读取待发送通知失败：%w", translateSQLError(err))
	}

	now := time.Now().UTC()
	updates := map[string]any{"updated_at": now, "lease_expires_at": nil}
	if sendErr == nil {
		updates["status"] = OutboxStatusSent
		updates["last_error"] = ""
		updates["next_attempt_at"] = nil
	} else {
		updates["attempts"] = nextAttempt
		updates["last_error"] = sanitizeOutboxError(sendErr)
		if !retryableSendError(sendErr) || nextAttempt >= maxAttempts {
			updates["status"] = OutboxStatusFailed
			updates["next_attempt_at"] = nil
			updates["stopped_at"] = now
		} else {
			updates["status"] = OutboxStatusRetrying
			updates["next_attempt_at"] = now.Add(backoff(nextAttempt))
		}
	}
	if err := tx.db.Model(&NotificationOutbox{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return fmt.Errorf("更新通知发送结果失败：%w", translateSQLError(err))
	}
	return nil
}

// retryableSendError 判断发送错误是否应当在退避后重试。
func retryableSendError(err error) bool {
	var classifier RetryClassifier
	if errors.As(err, &classifier) {
		return classifier.Retryable()
	}
	return true
}

// sanitizeOutboxError 提取可安全落库的错误摘要。
//
// LastError 会进入管理查询与日志，因此只保留一行、限长，并去掉换行与制表符：
// 底层错误可能含完整 URL（含查询串凭据）、多行堆栈或响应体片段。
func sanitizeOutboxError(err error) string {
	summary := err.Error()
	replacer := strings.NewReplacer("\r", " ", "\n", " ", "\t", " ")
	summary = replacer.Replace(summary)
	if len([]rune(summary)) > maxOutboxErrorRunes {
		summary = string([]rune(summary)[:maxOutboxErrorRunes])
	}
	return summary
}

// maxOutboxErrorRunes 限制错误摘要长度；模型列宽 255 字节，中文按 3 字节预留余量。
const maxOutboxErrorRunes = 80

// OutboxEntries 返回全部 outbox 记录，供测试与运维查询。
func (tx *Tx) OutboxEntries() ([]NotificationOutbox, error) {
	var entries []NotificationOutbox
	if err := tx.db.Order("id ASC").Find(&entries).Error; err != nil {
		return nil, fmt.Errorf("读取 outbox 记录失败：%w", translateSQLError(err))
	}
	return entries, nil
}

// DiscardOutboxForTarget 把某目标的全部在途记录转入 discarded。
//
// 目标被禁用或删除后，其待发送记录已无投递意义：继续重试只会对着一个不存在
// 的目标反复失败，最终仍会进入 failed，而 failed 会被误读为"投递出了问题"。
// discarded 表达的是"这条通知不再需要投递"，与投递失败是两回事（规格 §3.3）。
//
// 只处理尚未进入终态的记录：已 sent 或已 failed 的记录是历史事实，不改写。
// 返回转入 discarded 的条数。
func (tx *Tx) DiscardOutboxForTarget(targetID string) (int, error) {
	now := time.Now().UTC()
	affected := int64(0)
	err := tx.Transaction(func() error {
		statement := tx.db.Model(&NotificationOutbox{}).
			Where("target_id = ? AND status IN ?", targetID,
				[]string{OutboxStatusPending, OutboxStatusRetrying, OutboxStatusSending}).
			Updates(map[string]any{
				"status":           OutboxStatusDiscarded,
				"next_attempt_at":  nil,
				"lease_expires_at": nil,
				"stopped_at":       now,
				"updated_at":       now,
				"last_error":       "目标已停用或删除，在途通知不再投递",
			})
		if statement.Error != nil {
			return fmt.Errorf("转入 discarded 失败：%w", translateSQLError(statement.Error))
		}
		affected = statement.RowsAffected
		return nil
	})
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

// PendingOutboxCount 返回当前可发送记录条数。
func (tx *Tx) PendingOutboxCount() (int64, error) {
	var count int64
	err := tx.db.Model(&NotificationOutbox{}).
		Where("status = ? OR status = ?", OutboxStatusPending, OutboxStatusRetrying).
		Count(&count).Error
	if err != nil {
		return 0, fmt.Errorf("统计待发送通知失败：%w", translateSQLError(err))
	}
	return count, nil
}
