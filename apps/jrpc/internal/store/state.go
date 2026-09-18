package store

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ErrNoDesired 表示本地尚无任何 desired 版本。
var ErrNoDesired = errors.New("本地尚无配置版本")

// ErrRevisionOutOfOrder 表示服务端下发的版本早于本地已知版本。
//
// 冲突处理以服务端当前 desired 为准：客户端不得自行翻回旧版本（FR-08 §3.4）。
var ErrRevisionOutOfOrder = errors.New("服务端下发的版本早于本地已知版本")

// StoreIdentity 保存 enrollment 结果；同一时刻只保留一条身份记录。
func (tx *Tx) StoreIdentity(identity Identity) error {
	return tx.Transaction(func() error {
		if identity.ID == 0 {
			identity.ID = 1
		}
		if err := tx.db.Save(&identity).Error; err != nil {
			return fmt.Errorf("写入客户端身份失败：%w", translateSQLError(err))
		}
		return tx.writeAudit(AuditEvent{
			Action:     ActionIdentityStored,
			ObjectType: "identity",
			ObjectID:   identity.ClientID,
			Result:     AuditResultSuccess,
			Context:    "客户端身份与 token 已保存到本地",
		})
	})
}

// Identity 读取当前客户端身份；未 enrollment 时返回 gorm.ErrRecordNotFound。
func (tx *Tx) Identity() (Identity, error) {
	var identity Identity
	err := tx.db.Order("id ASC").First(&identity).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Identity{}, ErrNoDesiredIdentity
	}
	if err != nil {
		return Identity{}, fmt.Errorf("读取客户端身份失败：%w", translateSQLError(err))
	}
	return identity, nil
}

// ErrNoDesiredIdentity 表示本地尚未完成 enrollment。
var ErrNoDesiredIdentity = errors.New("本地尚未完成 enrollment")

// RecordDeliveredDesired 保存服务端下发的 desired 内容，使其成为本地持久化真源。
//
// 版本必须单调：服务端下发旧版本时拒绝，由上层重新拉取当前 desired。
func (tx *Tx) RecordDeliveredDesired(serverRevision uint64, content string) (uint64, error) {
	var stored uint64
	err := tx.Transaction(func() error {
		latest, err := tx.LatestDesired()
		if err != nil && !errors.Is(err, ErrNoDesired) {
			return err
		}
		if err == nil && serverRevision < latest.ServerRevision {
			return fmt.Errorf("%w：服务端版本 %d，本地已知 %d",
				ErrRevisionOutOfOrder, serverRevision, latest.ServerRevision)
		}
		next, err := tx.nextRevision()
		if err != nil {
			return err
		}
		record := DesiredState{
			Revision:       next,
			Content:        content,
			ServerRevision: serverRevision,
			ReceivedAt:     time.Now().UTC(),
		}
		if err := tx.db.Create(&record).Error; err != nil {
			return fmt.Errorf("写入下发配置失败：%w", translateSQLError(err))
		}
		if err := tx.setDesired(next); err != nil {
			return err
		}
		stored = next
		return tx.writeAudit(AuditEvent{
			Action:     ActionDesiredReceived,
			ObjectType: "config_revision",
			ObjectID:   fmt.Sprintf("%d", next),
			Result:     AuditResultSuccess,
			Context:    fmt.Sprintf("服务端版本 %d 已落库为本地版本 %d", serverRevision, next),
			Revision:   next,
		})
	})
	if err != nil {
		return 0, err
	}
	return stored, nil
}

// LatestDesired 返回最新本地 desired 版本记录；不存在时返回 ErrNoDesired。
func (tx *Tx) LatestDesired() (DesiredState, error) {
	var record DesiredState
	err := tx.db.Order("revision DESC").First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return DesiredState{}, ErrNoDesired
	}
	if err != nil {
		return DesiredState{}, fmt.Errorf("读取本地配置版本失败：%w", translateSQLError(err))
	}
	return record, nil
}

// Desired 读取指定本地版本内容，只读不改。
func (tx *Tx) Desired(revision uint64) (DesiredState, error) {
	var record DesiredState
	err := tx.db.First(&record, "revision = ?", revision).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return DesiredState{}, fmt.Errorf("本地配置版本 %d 不存在", revision)
	}
	if err != nil {
		return DesiredState{}, fmt.Errorf("读取本地配置版本失败：%w", translateSQLError(err))
	}
	return record, nil
}

// nextRevision 返回下一个本地版本号，保证版本单调递增。
func (tx *Tx) nextRevision() (uint64, error) {
	var latest uint64
	err := tx.db.Model(&DesiredState{}).Select("COALESCE(MAX(revision), 0)").Scan(&latest).Error
	if err != nil {
		return 0, fmt.Errorf("读取最新本地版本失败：%w", translateSQLError(err))
	}
	return latest + 1, nil
}

// ApplyResultInput 是一次阶段结果的输入。
type ApplyResultInput struct {
	Revision    uint64
	Phase       string
	Succeeded   bool
	ErrorDetail string
}

// RecordApplyResult 记录一个阶段的本地 Apply 结果并写审计事件。
//
// 只有 publish 成功才推进 active 与 last-good 的落库记录：运行态归 Core 内存（ADR-0012），
// 因此 publish 未成功时此处不得推进。
func (tx *Tx) RecordApplyResult(input ApplyResultInput) error {
	return tx.Transaction(func() error {
		result := ApplyResult{
			Revision:    input.Revision,
			Phase:       input.Phase,
			Succeeded:   input.Succeeded,
			ErrorDetail: input.ErrorDetail,
			OccurredAt:  time.Now().UTC(),
		}
		if err := tx.db.Create(&result).Error; err != nil {
			return fmt.Errorf("写入应用结果失败：%w", translateSQLError(err))
		}
		if input.Phase == PhasePublish && input.Succeeded {
			if err := tx.advanceActive(input.Revision); err != nil {
				return err
			}
		}
		return tx.writeAudit(AuditEvent{
			Action:     applyActionFor(input),
			ObjectType: "config_revision",
			ObjectID:   fmt.Sprintf("%d", input.Revision),
			Result:     auditResultFor(input.Succeeded),
			Context:    input.ErrorDetail,
			Revision:   input.Revision,
		})
	})
}

// advanceActive 在 publish 成功后推进 active 与 last-good 的落库记录。
func (tx *Tx) advanceActive(revision uint64) error {
	record, err := tx.ensureRevisionRecord()
	if err != nil {
		return err
	}
	record.ActiveRevision = revision
	record.LastGoodRevision = revision
	record.UpdatedAt = time.Now().UTC()
	if err := tx.db.Save(record).Error; err != nil {
		return fmt.Errorf("更新 active 应用结果记录失败：%w", translateSQLError(err))
	}
	return nil
}

// setDesired 推进本地 desired revision。
func (tx *Tx) setDesired(revision uint64) error {
	record, err := tx.ensureRevisionRecord()
	if err != nil {
		return err
	}
	record.DesiredRevision = revision
	record.UpdatedAt = time.Now().UTC()
	if err := tx.db.Save(record).Error; err != nil {
		return fmt.Errorf("更新本地 desired 版本失败：%w", translateSQLError(err))
	}
	return nil
}

func (tx *Tx) ensureRevisionRecord() (*RevisionRecord, error) {
	var record RevisionRecord
	err := tx.db.Order("id ASC").First(&record).Error
	if err == nil {
		return &record, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("读取本地版本状态失败：%w", translateSQLError(err))
	}
	record = RevisionRecord{UpdatedAt: time.Now().UTC()}
	if err := tx.db.Create(&record).Error; err != nil {
		return nil, fmt.Errorf("创建本地版本状态失败：%w", translateSQLError(err))
	}
	return &record, nil
}

// RevisionState 返回本地三个 revision 的当前落库取值。
func (tx *Tx) RevisionState() (RevisionRecord, error) {
	record, err := tx.ensureRevisionRecord()
	if err != nil {
		return RevisionRecord{}, err
	}
	return *record, nil
}

// ApplyResults 返回指定版本的应用结果，按发生时间升序。
func (tx *Tx) ApplyResults(revision uint64) ([]ApplyResult, error) {
	var results []ApplyResult
	err := tx.db.Where("revision = ?", revision).Order("occurred_at ASC, id ASC").Find(&results).Error
	if err != nil {
		return nil, fmt.Errorf("读取应用结果失败：%w", translateSQLError(err))
	}
	return results, nil
}

// MarkApplyResultsReported 标记某版本的结果已回执服务端。
func (tx *Tx) MarkApplyResultsReported(revision uint64) error {
	now := time.Now().UTC()
	err := tx.db.Model(&ApplyResult{}).
		Where("revision = ? AND reported_at IS NULL", revision).
		Update("reported_at", now).Error
	if err != nil {
		return fmt.Errorf("标记回执状态失败：%w", translateSQLError(err))
	}
	return nil
}

// AuditEvents 返回本地审计事件，按发生时间升序。
func (tx *Tx) AuditEvents() ([]AuditEvent, error) {
	var events []AuditEvent
	if err := tx.db.Order("occurred_at ASC, id ASC").Find(&events).Error; err != nil {
		return nil, fmt.Errorf("读取审计事件失败：%w", translateSQLError(err))
	}
	return events, nil
}

// writeAudit 写入本地审计事件；审计与业务结果在同一事务内提交。
func (tx *Tx) writeAudit(event AuditEvent) error {
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	if err := tx.db.Create(&event).Error; err != nil {
		return fmt.Errorf("写入审计事件失败：%w", translateSQLError(err))
	}
	return nil
}

// SetConnectionState 更新本地连接状态。
func (tx *Tx) SetConnectionState(state string) error {
	record, err := tx.ensureRuntimeState()
	if err != nil {
		return err
	}
	record.ConnectionState = state
	record.UpdatedAt = time.Now().UTC()
	if err := tx.db.Save(record).Error; err != nil {
		return fmt.Errorf("更新连接状态失败：%w", translateSQLError(err))
	}
	return nil
}

// RuntimeState 返回本地运行元数据。
func (tx *Tx) RuntimeState() (RuntimeState, error) {
	record, err := tx.ensureRuntimeState()
	if err != nil {
		return RuntimeState{}, err
	}
	return *record, nil
}

func (tx *Tx) ensureRuntimeState() (*RuntimeState, error) {
	var record RuntimeState
	err := tx.db.Order("id ASC").First(&record).Error
	if err == nil {
		return &record, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("读取运行状态失败：%w", translateSQLError(err))
	}
	record = RuntimeState{ConnectionState: ConnectionStateOffline, UpdatedAt: time.Now().UTC()}
	if err := tx.db.Create(&record).Error; err != nil {
		return nil, fmt.Errorf("创建运行状态失败：%w", translateSQLError(err))
	}
	return &record, nil
}

// OutboxEnqueue 在业务事务内写入一条待上报记录；它不执行任何外部调用。
func (tx *Tx) OutboxEnqueue(entry Outbox) (uint64, error) {
	if entry.EventID == "" {
		return 0, errors.New("上报事件标识不能为空")
	}
	if entry.EventType == "" {
		return 0, errors.New("上报事件类型不能为空")
	}
	if entry.Status == "" {
		entry.Status = OutboxStatusPending
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	entry.UpdatedAt = entry.CreatedAt
	if err := tx.db.Create(&entry).Error; err != nil {
		return 0, fmt.Errorf("写入上报队列失败：%w", translateSQLError(err))
	}
	return entry.ID, nil
}

// PendingOutboxCount 返回当前待上报条数。
func (tx *Tx) PendingOutboxCount() (int64, error) {
	var count int64
	err := tx.db.Model(&Outbox{}).Where("status = ?", OutboxStatusPending).Count(&count).Error
	if err != nil {
		return 0, fmt.Errorf("统计待上报记录失败：%w", translateSQLError(err))
	}
	return count, nil
}

// OutboxEntries 返回全部待上报记录。
func (tx *Tx) OutboxEntries() ([]Outbox, error) {
	var entries []Outbox
	if err := tx.db.Order("id ASC").Find(&entries).Error; err != nil {
		return nil, fmt.Errorf("读取上报队列失败：%w", translateSQLError(err))
	}
	return entries, nil
}

func applyActionFor(input ApplyResultInput) string {
	if !input.Succeeded {
		return ActionApplyFailure
	}
	if input.Phase == PhasePublish {
		return ActionApplyPublish
	}
	return ActionApplyPrepare
}

func auditResultFor(succeeded bool) string {
	if succeeded {
		return AuditResultSuccess
	}
	return AuditResultFailure
}
