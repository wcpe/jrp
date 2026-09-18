package store

import (
	"fmt"

	"gorm.io/gorm"
)

// migration 是一次架构迁移步骤；apply 在同一事务中被调用。
type migration struct {
	version uint64
	name    string
	apply   func(tx *gorm.DB) error
}

// migrations 按版本递增排列。只做向后兼容迁移；破坏性迁移需要独立规格。
func migrations() []migration {
	return []migration{
		{version: 1, name: "建立 jrpc 初始表结构", apply: applyInitialSchema},
	}
}

// applyInitialSchema 建立客户端侧的本地表与追加写约束。
func applyInitialSchema(tx *gorm.DB) error {
	types := []any{
		&StoreMeta{},
		&Identity{},
		&DesiredState{},
		&RevisionRecord{},
		&ApplyResult{},
		&AuditEvent{},
		&RuntimeState{},
		&Outbox{},
	}
	if err := tx.AutoMigrate(types...); err != nil {
		return fmt.Errorf("建立数据表失败：%w", err)
	}
	return createAppendOnlyGuards(tx)
}

// createAppendOnlyGuards 在数据库层强制追加写约束。
//
// 已接收的 desired 版本与审计事件都是历史证据，不接受事后改写或删除；
// 只有应用结果记录需要在回执后更新上报状态，因此不设写保护。
func createAppendOnlyGuards(tx *gorm.DB) error {
	statements := []string{
		`CREATE TRIGGER IF NOT EXISTS desired_states_immutable_update
			BEFORE UPDATE ON desired_states
			BEGIN SELECT RAISE(ABORT, '已接收的配置版本内容不可修改，只能追加新版本'); END`,
		`CREATE TRIGGER IF NOT EXISTS desired_states_immutable_delete
			BEFORE DELETE ON desired_states
			BEGIN SELECT RAISE(ABORT, '已接收的配置版本不可删除，只能追加新版本'); END`,
		`CREATE TRIGGER IF NOT EXISTS audit_events_append_only_update
			BEFORE UPDATE ON audit_events
			BEGIN SELECT RAISE(ABORT, '审计事件不可修改，只能追加'); END`,
		`CREATE TRIGGER IF NOT EXISTS audit_events_append_only_delete
			BEFORE DELETE ON audit_events
			BEGIN SELECT RAISE(ABORT, '审计事件不可删除，只能追加'); END`,
	}
	for _, statement := range statements {
		if err := tx.Exec(statement).Error; err != nil {
			return fmt.Errorf("建立追加写约束失败：%w", err)
		}
	}
	return nil
}
