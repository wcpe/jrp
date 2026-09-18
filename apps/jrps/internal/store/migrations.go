package store

import (
	"fmt"

	"gorm.io/gorm"
)

// currentSchemaVersion 是当前程序期望的数据库架构版本，落库到 PRAGMA user_version。
const currentSchemaVersion = 1

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
