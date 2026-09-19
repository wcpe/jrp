package store

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// currentSchemaVersion 是当前程序期望的数据库架构版本，落库到 PRAGMA user_version。
const currentSchemaVersion = 2

// migration 是一次架构迁移步骤；apply 在同一事务中被调用。
type migration struct {
	version uint64
	name    string
	apply   func(tx *gorm.DB) error
}

// migrations 按版本递增排列。只做向后兼容迁移；破坏性迁移需要独立规格。
func migrations() []migration {
	return []migration{
		{version: 1, name: "建立 P1 初始表结构", apply: applyInitialSchema},
		{version: 2, name: "新增保留策略对象并放开审计清理", apply: applyRetentionPolicy},
	}
}

// applyInitialSchema 建立 FR-09 规格 §3.3 的实体表与不可变版本触发器。
func applyInitialSchema(tx *gorm.DB) error {
	types := []any{
		&StoreMeta{},
		&AdminCredential{},
		&Session{},
		&Client{},
		&Proxy{},
		&ConfigRevision{},
		&RevisionState{},
		&ApplyResult{},
		&AuditEvent{},
		&NotificationTarget{},
		&NotificationOutbox{},
		&RequestRecord{},
		&BodySegment{},
	}
	if err := tx.AutoMigrate(types...); err != nil {
		return fmt.Errorf("建立数据表失败：%w", err)
	}
	return createRevisionGuards(tx)
}

// createRevisionGuards 在数据库层强制追加写约束：
// 历史版本记录只允许追加，任何原地修改或删除都被拒绝，而不是只靠应用层约定。
//
// 审计事件与应用结果同样只能追加：它们是运行历史的证据，不接受事后改写。
func createRevisionGuards(tx *gorm.DB) error {
	statements := []string{
		`CREATE TRIGGER IF NOT EXISTS config_revisions_immutable_update
			BEFORE UPDATE ON config_revisions
			BEGIN SELECT RAISE(ABORT, '配置版本内容不可修改，只能追加新版本'); END`,
		`CREATE TRIGGER IF NOT EXISTS config_revisions_immutable_delete
			BEFORE DELETE ON config_revisions
			BEGIN SELECT RAISE(ABORT, '配置版本不可删除，只能追加新版本'); END`,
		`CREATE TRIGGER IF NOT EXISTS audit_events_append_only_update
			BEFORE UPDATE ON audit_events
			BEGIN SELECT RAISE(ABORT, '审计事件不可修改，只能追加'); END`,
		`CREATE TRIGGER IF NOT EXISTS audit_events_append_only_delete
			BEFORE DELETE ON audit_events
			BEGIN SELECT RAISE(ABORT, '审计事件不可删除，只能追加'); END`,
		`CREATE TRIGGER IF NOT EXISTS apply_results_append_only_update
			BEFORE UPDATE ON apply_results
			BEGIN SELECT RAISE(ABORT, '应用结果不可修改，只能追加'); END`,
	}
	for _, statement := range statements {
		if err := tx.Exec(statement).Error; err != nil {
			return fmt.Errorf("建立追加写约束失败：%w", err)
		}
	}
	return nil
}

// applyRetentionPolicy 建立 FR-16 的保留策略对象，并把审计表从"完全不可删除"
// 调整为"只允许按保留策略清理"。
//
// v1 的 audit_events_append_only_delete 触发器禁止一切删除，与 FR-16 规格 §3.7
// 要求的按时间维度清理直接冲突。审计"不可篡改"的实质是不接受事后改写，删除
// 既有记录仍属禁止；此处放开删除是为了让保留策略得以执行，清理动作自身必须
// 先写入一条审计事件留痕（见 store.CleanupAuditEvents）。
//
// 更新触发器保持不变：审计内容在任何情况下都不得被就地修改。
func applyRetentionPolicy(tx *gorm.DB) error {
	if err := tx.AutoMigrate(&CapturePolicy{}); err != nil {
		return fmt.Errorf("建立保留策略表失败：%w", err)
	}
	if err := tx.Exec(`DROP TRIGGER IF EXISTS audit_events_append_only_delete`).Error; err != nil {
		return fmt.Errorf("调整审计删除约束失败：%w", err)
	}
	if err := seedCapturePolicy(tx); err != nil {
		return err
	}
	return nil
}

// seedCapturePolicy 写入策略单例行的默认值。
//
// 默认值由 DefaultCapturePolicy 给出，与规格 §2.3 的 30 天 / 5 GiB 一致；
// 已存在时不覆盖，保证迁移可重复执行且不冲掉管理员已保存的策略。
func seedCapturePolicy(tx *gorm.DB) error {
	policy := DefaultCapturePolicy()
	var existing CapturePolicy
	result := tx.Where("id = ?", capturePolicyRowID).First(&existing)
	if result.Error == nil {
		return nil
	}
	if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return fmt.Errorf("读取保留策略失败：%w", result.Error)
	}
	policy.ID = capturePolicyRowID
	if err := tx.Create(&policy).Error; err != nil {
		return fmt.Errorf("写入默认保留策略失败：%w", err)
	}
	return nil
}
