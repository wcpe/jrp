package store

import (
	"context"
	"errors"
	"fmt"
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

// dueEntries 读取已提交且到达重试时间的记录。
//
// 只有 pending 与到期 retrying 两种状态可被取出；未提交事务的记录在提交前不存在于此表。
func (d *OutboxDispatcher) dueEntries(ctx context.Context, limit int) ([]NotificationOutbox, error) {
	var entries []NotificationOutbox
	err := d.store.View(ctx, func(tx *Tx) error {
		return tx.db.
			Where("status = ? OR (status = ? AND next_attempt_at <= ?)",
				OutboxStatusPending, OutboxStatusRetrying, time.Now().UTC()).
			Order("created_at ASC, id ASC").Limit(limit).Find(&entries).Error
	})
	if err != nil {
		return nil, fmt.Errorf("读取待发送通知失败：%w", err)
	}
	return entries, nil
}

// deliver 先标记发送中，再执行外部副作用，最后记录终态。
//
// 标记与副作用分离，使进程崩溃后可由租约超时重新投递。
func (d *OutboxDispatcher) deliver(ctx context.Context, entry NotificationOutbox) error {
	if err := d.markSending(ctx, entry.ID); err != nil {
		return err
	}
	sendErr := d.sender.Send(ctx, entry)
	return d.recordOutcome(ctx, entry, sendErr)
}

// markSending 标记记录为发送中并写入租约到期时间。
func (d *OutboxDispatcher) markSending(ctx context.Context, id uint64) error {
	lease := time.Now().UTC().Add(defaultOutboxLease)
	err := d.store.Transaction(ctx, func(tx *Tx) error {
		return tx.db.Model(&NotificationOutbox{}).Where("id = ?", id).
			Updates(map[string]any{
				"status":           OutboxStatusSending,
				"lease_expires_at": lease,
				"updated_at":       time.Now().UTC(),
			}).Error
	})
	if err != nil {
		return fmt.Errorf("标记通知发送中失败：%w", err)
	}
	return nil
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

// 默认重试上限与租约时长；退避序列为有限递增，避免同目标重试风暴。
const (
	defaultOutboxMaxAttempts = 5
	defaultOutboxLease       = time.Minute
)

// defaultBackoff 返回带有限增长的退避时长。
func defaultBackoff(attempt int) time.Duration {
	if attempt <= 1 {
		return 30 * time.Second
	}
	if attempt > defaultOutboxMaxAttempts {
		attempt = defaultOutboxMaxAttempts
	}
	return time.Duration(attempt) * 30 * time.Second
}

// updateOutboxOutcome 依据发送结果更新状态、重试次数与退避时间。
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
		updates["last_error"] = sendErr.Error()
		if nextAttempt >= maxAttempts {
			updates["status"] = OutboxStatusFailed
			updates["next_attempt_at"] = nil
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

// OutboxEntries 返回全部 outbox 记录，供测试与运维查询。
func (tx *Tx) OutboxEntries() ([]NotificationOutbox, error) {
	var entries []NotificationOutbox
	if err := tx.db.Order("id ASC").Find(&entries).Error; err != nil {
		return nil, fmt.Errorf("读取 outbox 记录失败：%w", translateSQLError(err))
	}
	return entries, nil
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

// NotificationTargetView 是通知目标的读取视图。
//
// 它不暴露秘密本身，只提供掩码；完整秘密只在写入时接收（FR-15 §3.4）。
type NotificationTargetView struct {
	ID            string
	Type          string
	Name          string
	TargetSummary string
	Enabled       bool
	SecretSuffix  string
}

// MaskedSecret 返回可安全展示的秘密掩码：只保留末四位。
func (v NotificationTargetView) MaskedSecret() string {
	if v.SecretSuffix == "" {
		return "(未设置)"
	}
	return "****" + v.SecretSuffix
}

// NotificationTarget 读取通知目标的脱敏视图；秘密不以明文返回。
func (tx *Tx) NotificationTarget(id string) (NotificationTargetView, error) {
	var record NotificationTarget
	err := tx.db.First(&record, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return NotificationTargetView{}, fmt.Errorf("通知目标 %s 不存在", id)
	}
	if err != nil {
		return NotificationTargetView{}, fmt.Errorf("读取通知目标失败：%w", translateSQLError(err))
	}
	return NotificationTargetView{
		ID:            record.ID,
		Type:          record.Type,
		Name:          record.Name,
		TargetSummary: record.TargetSummary,
		Enabled:       record.Enabled,
		SecretSuffix:  secretSuffix(record.Secret),
	}, nil
}

// secretSuffix 取秘密末四位用于运维识别；秘密过短时整体掩码。
func secretSuffix(secret string) string {
	if len(secret) < 8 {
		return ""
	}
	return secret[len(secret)-4:]
}
