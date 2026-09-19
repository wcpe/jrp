package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// 状态归属：服务端整体配置或某个客户端。
const (
	ScopeServer = "server"
)

// 审计主体类别：P1 只有管理员与客户端两类主体。
// 动作、对象类型与结果的封闭枚举由 audit.go 统一管理（FR-16 规格 §3.2）。
const (
	ActorTypeAdmin  = "admin"
	ActorTypeClient = "client"
)

// ErrNoRevision 表示数据库尚无任何 desired 版本。
var ErrNoRevision = errors.New("尚无配置版本")

// Actor 是审计与版本记录中的操作主体。
type Actor struct {
	Type string
	ID   string
}

// ActorAdmin 构造管理员主体。
func ActorAdmin(id string) Actor { return Actor{Type: ActorTypeAdmin, ID: id} }

// RevisionInput 是追加不可变版本所需的输入。
type RevisionInput struct {
	Content       string
	Actor         Actor
	Origin        string
	ChangeSummary string
}

// AppendRevision 追加一个不可变 desired 版本，并在同一事务推进 desired 与写审计。
//
// 这是 desired 的唯一写入点，只允许控制面适配器调用；Core 的内存快照不存在经由此处
// 成为持久化真源的路径（ADR-0012）。
func (tx *Tx) AppendRevision(input RevisionInput) (uint64, error) {
	var revision uint64
	err := tx.Transaction(func() error {
		if err := tx.appendRevision(input); err != nil {
			return err
		}
		latest, err := tx.LatestRevision()
		if err != nil {
			return err
		}
		revision = latest.Revision
		return nil
	})
	if err != nil {
		return 0, err
	}
	return revision, nil
}

func (tx *Tx) appendRevision(input RevisionInput) error {
	next, err := tx.nextRevision()
	if err != nil {
		return err
	}
	record := ConfigRevision{
		Revision:      next,
		Content:       input.Content,
		Creator:       input.Actor.ID,
		Origin:        input.Origin,
		ChangeSummary: input.ChangeSummary,
		CreatedAt:     time.Now().UTC(),
	}
	if err := tx.db.Create(&record).Error; err != nil {
		return fmt.Errorf("追加配置版本失败：%w", translateSQLError(err))
	}
	if err := tx.advanceDesired(ScopeServer, next); err != nil {
		return err
	}
	return tx.writeAudit(AuditEvent{
		ActorType:       input.Actor.Type,
		ActorID:         input.Actor.ID,
		Action:          ActionRevisionAppend,
		ObjectType:      "config_revision",
		ObjectID:        fmt.Sprintf("%d", next),
		Result:          AuditResultSuccess,
		Context:         input.ChangeSummary,
		DesiredRevision: next,
	})
}

// SaveProxy 写入代理定义并在同一事务追加新的 desired 版本与审计事件。
//
// 任何代理创建、修改或删除都会产生新版本，历史版本内容保持不变。
func (tx *Tx) SaveProxy(proxy Proxy, actor Actor, origin string) (uint64, error) {
	if proxy.ID == "" {
		return 0, errors.New("代理标识不能为空")
	}
	var revision uint64
	err := tx.Transaction(func() error {
		if err := tx.db.Save(&proxy).Error; err != nil {
			return fmt.Errorf("写入代理记录失败：%w", translateSQLError(err))
		}
		if err := tx.appendRevision(RevisionInput{
			Content:       proxySnapshotContent(proxy),
			Actor:         actor,
			Origin:        origin,
			ChangeSummary: fmt.Sprintf("代理 %s（%s）发生变更", proxy.Name, proxy.Type),
		}); err != nil {
			return err
		}
		latest, err := tx.LatestRevision()
		if err != nil {
			return err
		}
		revision = latest.Revision
		return tx.writeAudit(AuditEvent{
			ActorType:       actor.Type,
			ActorID:         actor.ID,
			Action:          proxyActionFor(origin),
			ObjectType:      "proxy",
			ObjectID:        proxy.ID,
			Result:          AuditResultSuccess,
			Context:         fmt.Sprintf("代理 %s 变更已生成版本 %d", proxy.Name, revision),
			DesiredRevision: revision,
		})
	})
	if err != nil {
		return 0, err
	}
	return revision, nil
}

// DeleteProxy 标记代理删除并在同一事务追加新的 desired 版本与审计事件。
func (tx *Tx) DeleteProxy(proxy Proxy, actor Actor) (uint64, error) {
	proxy.Deleted = true
	var revision uint64
	err := tx.Transaction(func() error {
		if err := tx.db.Save(&proxy).Error; err != nil {
			return fmt.Errorf("标记代理删除失败：%w", translateSQLError(err))
		}
		if err := tx.appendRevision(RevisionInput{
			Content:       proxySnapshotContent(proxy),
			Actor:         actor,
			Origin:        OriginProxyDelete,
			ChangeSummary: fmt.Sprintf("删除代理 %s", proxy.Name),
		}); err != nil {
			return err
		}
		latest, err := tx.LatestRevision()
		if err != nil {
			return err
		}
		revision = latest.Revision
		return tx.writeAudit(AuditEvent{
			ActorType:       actor.Type,
			ActorID:         actor.ID,
			Action:          ActionProxyDelete,
			ObjectType:      "proxy",
			ObjectID:        proxy.ID,
			Result:          AuditResultSuccess,
			Context:         fmt.Sprintf("代理 %s 已删除并生成版本 %d", proxy.Name, revision),
			DesiredRevision: revision,
		})
	})
	if err != nil {
		return 0, err
	}
	return revision, nil
}

// RestoreRevision 以历史版本内容创建新的 desired 版本，复用同一状态机入口。
//
// 它只读取历史内容，不修改历史版本记录。
func (tx *Tx) RestoreRevision(sourceRevision uint64, actor Actor) (uint64, error) {
	source, err := tx.Revision(sourceRevision)
	if err != nil {
		return 0, err
	}
	var revision uint64
	err = tx.Transaction(func() error {
		if err := tx.appendRevision(RevisionInput{
			Content:       source.Content,
			Actor:         actor,
			Origin:        OriginRestore,
			ChangeSummary: fmt.Sprintf("从版本 %d 恢复配置", sourceRevision),
		}); err != nil {
			return err
		}
		latest, err := tx.LatestRevision()
		if err != nil {
			return err
		}
		revision = latest.Revision
		return tx.writeAudit(AuditEvent{
			ActorType:       actor.Type,
			ActorID:         actor.ID,
			Action:          ActionRestoreApply,
			ObjectType:      "config_revision",
			ObjectID:        fmt.Sprintf("%d", sourceRevision),
			Result:          AuditResultSuccess,
			Context:         fmt.Sprintf("以版本 %d 的内容创建版本 %d", sourceRevision, revision),
			DesiredRevision: revision,
		})
	})
	if err != nil {
		return 0, err
	}
	return revision, nil
}

// advanceDesired 推进 desired revision；desired 是唯一具备真源性质的部分。
func (tx *Tx) advanceDesired(scope string, revision uint64) error {
	state, err := tx.ensureRevisionState(scope)
	if err != nil {
		return err
	}
	state.DesiredRevision = revision
	state.UpdatedAt = time.Now().UTC()
	if err := tx.db.Save(state).Error; err != nil {
		return fmt.Errorf("更新 desired 版本失败：%w", translateSQLError(err))
	}
	return nil
}

// ApplyResultInput 是一次阶段结果的输入。
type ApplyResultInput struct {
	Revision    uint64
	Scope       string
	Phase       string
	Succeeded   bool
	ErrorDetail string
	Actor       Actor
	RequestID   string
}

// RecordApplyResult 记录一个阶段的 Apply 结果并写审计事件。
//
// 只有 publish 成功才推进 active 与 last-good 的落库记录：这两列保存的是 Apply 结果记录，
// 运行态仍归 Core 内存（ADR-0012），因此 publish 未成功时此处不得推进。
func (tx *Tx) RecordApplyResult(input ApplyResultInput) error {
	scope := input.Scope
	if scope == "" {
		scope = ScopeServer
	}
	return tx.Transaction(func() error {
		result := ApplyResult{
			Revision:    input.Revision,
			Scope:       scope,
			Phase:       input.Phase,
			Succeeded:   input.Succeeded,
			ErrorDetail: input.ErrorDetail,
			OccurredAt:  time.Now().UTC(),
		}
		if err := tx.db.Create(&result).Error; err != nil {
			return fmt.Errorf("写入应用结果失败：%w", translateSQLError(err))
		}
		if input.Phase == PhasePublish && input.Succeeded {
			if err := tx.advanceActive(scope, input.Revision); err != nil {
				return err
			}
		}
		return tx.writeAudit(AuditEvent{
			ActorType:       input.Actor.Type,
			ActorID:         input.Actor.ID,
			Action:          applyActionFor(input),
			ObjectType:      "config_revision",
			ObjectID:        fmt.Sprintf("%d", input.Revision),
			Result:          auditResultFor(input.Succeeded),
			Context:         input.ErrorDetail,
			RequestID:       input.RequestID,
			DesiredRevision: input.Revision,
		})
	})
}

// advanceActive 在 publish 成功后推进 active 与 last-good 的落库记录。
func (tx *Tx) advanceActive(scope string, revision uint64) error {
	state, err := tx.ensureRevisionState(scope)
	if err != nil {
		return err
	}
	state.ActiveRevision = revision
	state.LastGoodRevision = revision
	state.UpdatedAt = time.Now().UTC()
	if err := tx.db.Save(state).Error; err != nil {
		return fmt.Errorf("更新 active 应用结果记录失败：%w", translateSQLError(err))
	}
	return nil
}

// ensureRevisionState 取得（必要时创建）指定归属的 revision 状态行。
func (tx *Tx) ensureRevisionState(scope string) (*RevisionState, error) {
	var state RevisionState
	err := tx.db.First(&state, "scope = ?", scope).Error
	if err == nil {
		return &state, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("读取版本状态失败：%w", translateSQLError(err))
	}
	state = RevisionState{Scope: scope, UpdatedAt: time.Now().UTC()}
	if err := tx.db.Create(&state).Error; err != nil {
		return nil, fmt.Errorf("创建版本状态失败：%w", translateSQLError(err))
	}
	return &state, nil
}

// nextRevision 返回下一个版本号，保证版本单调递增。
func (tx *Tx) nextRevision() (uint64, error) {
	var latest uint64
	err := tx.db.Model(&ConfigRevision{}).Select("COALESCE(MAX(revision), 0)").Scan(&latest).Error
	if err != nil {
		return 0, fmt.Errorf("读取最新配置版本失败：%w", translateSQLError(err))
	}
	return latest + 1, nil
}

// Revision 读取单个历史版本内容，只读不改。
func (tx *Tx) Revision(revision uint64) (ConfigRevision, error) {
	var record ConfigRevision
	err := tx.db.First(&record, "revision = ?", revision).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ConfigRevision{}, fmt.Errorf("配置版本 %d 不存在", revision)
	}
	if err != nil {
		return ConfigRevision{}, fmt.Errorf("读取配置版本失败：%w", translateSQLError(err))
	}
	return record, nil
}

// LatestRevision 返回最新 desired 版本记录；不存在时返回 ErrNoRevision。
func (tx *Tx) LatestRevision() (ConfigRevision, error) {
	var record ConfigRevision
	err := tx.db.Order("revision DESC").First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ConfigRevision{}, ErrNoRevision
	}
	if err != nil {
		return ConfigRevision{}, fmt.Errorf("读取最新配置版本失败：%w", translateSQLError(err))
	}
	return record, nil
}

// RevisionState 读取三个 revision 的当前落库取值。
func (tx *Tx) RevisionState(scope string) (RevisionState, error) {
	var state RevisionState
	err := tx.db.First(&state, "scope = ?", scope).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return RevisionState{Scope: scope}, nil
	}
	if err != nil {
		return RevisionState{}, fmt.Errorf("读取版本状态失败：%w", translateSQLError(err))
	}
	return state, nil
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

// AuditEvents 返回审计事件，按发生时间升序。
func (tx *Tx) AuditEvents() ([]AuditEvent, error) {
	var events []AuditEvent
	if err := tx.db.Order("occurred_at ASC, id ASC").Find(&events).Error; err != nil {
		return nil, fmt.Errorf("读取审计事件失败：%w", translateSQLError(err))
	}
	return events, nil
}

// writeAudit 写入审计事件；审计与业务结果在同一事务内提交，不进入降级路径。
//
// 时间是服务端生成的事实而非调用方输入：无论调用方是否填了 OccurredAt，一律
// 以当前 UTC 时间覆盖（FR-16 规格 §2.1）。校验失败与写入失败都返回错误，由
// 上层事务整体回滚，不留下字段不合法或携带秘密的审计记录。
func (tx *Tx) writeAudit(event AuditEvent) error {
	event.OccurredAt = time.Now().UTC()
	if err := validateAuditEvent(event); err != nil {
		return err
	}
	if err := tx.db.Create(&event).Error; err != nil {
		return fmt.Errorf("写入审计事件失败：%w", translateSQLError(err))
	}
	return nil
}

// WriteAudit 是 writeAudit 的导出入口，供外壳层在同一事务内写入审计。
//
// 仍然只提供这一个入口而不开放直写表：审计的字段校验、脱敏检查与时间覆盖
// 必须对所有权调用方一致生效，否则绕过入口的写入会让这些约束形同虚设。
func (tx *Tx) WriteAudit(event AuditEvent) error {
	return tx.writeAudit(event)
}

// Transaction 在已有事务句柄上执行嵌套事务，失败整体回滚。
func (tx *Tx) Transaction(fn func() error) error {
	return tx.db.Transaction(func(*gorm.DB) error { return fn() })
}

// LoadRevisionForRecovery 读取重启恢复所需输入。
//
// 返回的 active 与 last-good 是上一次运行留下的 Apply 结果记录，只用于展示与比对；
// 恢复流程必须以 desired 为输入重新走完整应用流程，不得把它当作 active 使用。
//
// 若落库的 active/last-good 找不到对应的成功 publish 结果，说明存在绕过 publish 的写入
// （即把 desired 或内存快照直接冒充为 active），此时拒绝恢复并报告不一致。
func (s *Store) LoadRevisionForRecovery(ctx context.Context) (RecoveryInput, error) {
	var input RecoveryInput
	err := s.View(ctx, func(tx *Tx) error {
		latest, err := tx.LatestRevision()
		if errors.Is(err, ErrNoRevision) {
			return nil
		}
		if err != nil {
			return err
		}
		state, err := tx.RevisionState(ScopeServer)
		if err != nil {
			return err
		}
		if err := verifyActiveRecord(tx, state.ActiveRevision); err != nil {
			return err
		}
		if err := verifyActiveRecord(tx, state.LastGoodRevision); err != nil {
			return err
		}
		input.HasDesired = true
		input.DesiredRevision = latest.Revision
		input.DesiredContent = latest.Content
		input.RecordedActiveRevision = state.ActiveRevision
		input.RecordedLastGoodRevision = state.LastGoodRevision
		return nil
	})
	if err != nil {
		return RecoveryInput{}, err
	}
	return input, nil
}

// verifyActiveRecord 校验 active 类记录必须来自一次成功的 publish。
// 版本号 0 表示尚无记录，直接通过。
func verifyActiveRecord(tx *Tx, revision uint64) error {
	if revision == 0 {
		return nil
	}
	var count int64
	err := tx.db.Model(&ApplyResult{}).
		Where("revision = ? AND phase = ? AND succeeded = ?", revision, PhasePublish, true).
		Count(&count).Error
	if err != nil {
		return fmt.Errorf("校验 active 记录失败：%w", translateSQLError(err))
	}
	if count == 0 {
		return fmt.Errorf(
			"数据库中的 active/last-good 记录（版本 %d）没有对应的成功 publish 结果，"+
				"存在绕过 publish 的写入，拒绝恢复", revision)
	}
	return nil
}

// RecoveryInput 是重启恢复的输入。
type RecoveryInput struct {
	HasDesired               bool
	DesiredRevision          uint64
	DesiredContent           string
	RecordedActiveRevision   uint64
	RecordedLastGoodRevision uint64
}

func proxyActionFor(origin string) string {
	switch origin {
	case OriginProxyUpdate:
		return ActionProxyUpdate
	case OriginProxyDelete:
		return ActionProxyDelete
	default:
		return ActionProxyCreate
	}
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

// proxySnapshotContent 生成代理的不可变内容快照。
func proxySnapshotContent(proxy Proxy) string {
	return fmt.Sprintf(
		`{"id":%q,"clientId":%q,"name":%q,"type":%q,"localPort":%d,"remotePort":%d,`+
			`"target":%q,"transport":%q,"captureEnabled":%t,"deleted":%t}`,
		proxy.ID, proxy.ClientID, proxy.Name, proxy.Type, proxy.LocalPort, proxy.RemotePort,
		proxy.Target, proxy.Transport, proxy.CaptureEnabled, proxy.Deleted,
	)
}
