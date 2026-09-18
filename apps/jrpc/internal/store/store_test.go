package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 测试辅助：静默日志器，避免测试输出噪声。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// 测试辅助：打开一个 jrpc 数据库并在测试结束时关闭。
func openClientStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(Config{Path: path, BusyTimeout: time.Second, Logger: quietLogger()})
	if err != nil {
		t.Fatalf("打开 jrpc 数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// 测试辅助：直接用底层驱动改写数据库归属标记，用于模拟指向另一侧外壳的数据库文件。
func rewriteRoleMarker(t *testing.T, path string, role string) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(databaseDSN(path, time.Second)), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("测试改写标记时打开数据库失败：%v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("测试改写标记时获取连接失败：%v", err)
	}
	defer func() { _ = sqlDB.Close() }()

	if err := db.Exec("UPDATE store_meta SET value = ? WHERE key = ?", role, metaKeyRole).Error; err != nil {
		t.Fatalf("测试改写角色标记失败：%v", err)
	}
}

// 空数据库首次启动应完成初始迁移，并登记本侧角色标记。
func TestOpenOnEmptyDatabaseAppliesInitialMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jrpc.db")
	store := openClientStore(t, path)

	if store.SchemaVersion() != currentSchemaVersion {
		t.Fatalf("迁移后架构版本不匹配：%d", store.SchemaVersion())
	}
	if store.Role() != RoleClient {
		t.Fatalf("数据库角色标记不匹配：%s", store.Role())
	}
	for _, table := range []string{
		"store_meta", "identities", "desired_states", "revision_records",
		"apply_results", "audit_events", "runtime_states", "outbox",
	} {
		if !store.DB().Migrator().HasTable(table) {
			t.Fatalf("初始迁移缺少数据表：%s", table)
		}
	}
}

// 指向 jrps 的数据库文件时必须拒绝启动并给出中文错误。
func TestOpenRejectsDatabaseOwnedByJrps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreign.db")
	store := openClientStore(t, path)
	if err := store.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}
	rewriteRoleMarker(t, path, RoleServer)

	_, err := Open(Config{Path: path, BusyTimeout: time.Second, Logger: quietLogger()})
	if err == nil {
		t.Fatal("jrpc 复用 jrps 的数据库文件必须失败")
	}
	if !strings.Contains(err.Error(), "jrps") || !strings.Contains(err.Error(), "独立") {
		t.Fatalf("错误信息应说明数据库归属并要求独立文件：%v", err)
	}
}

// 同一数据库文件被两个实例同时打开时，后启动者必须失败。
func TestOpenRejectsSecondInstanceOnSameDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jrpc.db")
	first := openClientStore(t, path)

	_, err := Open(Config{Path: path, BusyTimeout: 200 * time.Millisecond, Logger: quietLogger()})
	if err == nil {
		t.Fatal("同一数据库被两个实例打开必须失败")
	}
	if !strings.Contains(err.Error(), "占用") {
		t.Fatalf("错误信息应说明数据库已被占用：%v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}
	reopened, err := Open(Config{Path: path, BusyTimeout: time.Second, Logger: quietLogger()})
	if err != nil {
		t.Fatalf("释放独占后重新打开失败：%v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("关闭重新打开的数据库失败：%v", err)
	}
}

// 迁移失败必须拒绝启动，且不得留下半迁移状态。
func TestMigrationFailureRefusesStartAndLeavesNoHalfState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jrpc.db")
	failing := Config{
		Path:        path,
		BusyTimeout: time.Second,
		Logger:      quietLogger(),
		migrations: []migration{
			{version: 1, name: "先建一张表", apply: func(tx *gorm.DB) error {
				return tx.Migrator().CreateTable(&DesiredState{})
			}},
			{version: 2, name: "故意失败", apply: func(tx *gorm.DB) error {
				return errors.New("模拟迁移步骤失败")
			}},
		},
	}
	if _, err := Open(failing); err == nil {
		t.Fatal("迁移失败必须拒绝启动")
	} else if !strings.Contains(err.Error(), "迁移") {
		t.Fatalf("迁移失败错误信息应为中文且指明迁移：%v", err)
	}

	store := openClientStore(t, path)
	if store.SchemaVersion() != currentSchemaVersion {
		t.Fatalf("恢复后架构版本不匹配：%d", store.SchemaVersion())
	}
}

// 数据库架构版本高于当前程序时必须拒绝启动。
func TestOpenRejectsNewerSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jrpc.db")
	store := openClientStore(t, path)
	if err := store.DB().Exec("PRAGMA user_version = 99").Error; err != nil {
		t.Fatalf("写入更高架构版本失败：%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}

	_, err := Open(Config{Path: path, BusyTimeout: time.Second, Logger: quietLogger()})
	if err == nil {
		t.Fatal("数据库架构版本高于程序时必须拒绝启动")
	}
	if !strings.Contains(err.Error(), "版本") {
		t.Fatalf("错误信息应说明架构版本问题：%v", err)
	}
}

// 数据目录不存在时应创建，且超长路径不得导致启动失败。
func TestOpenCreatesDataDirectoryWithLongPath(t *testing.T) {
	deep := t.TempDir()
	for index := 0; index < 6; index++ {
		deep = filepath.Join(deep, "深层次数据目录段"+strings.Repeat("y", 8))
	}
	path := filepath.Join(deep, "jrpc.db")

	store := openClientStore(t, path)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("数据目录与数据库文件应被创建：%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}
}

// 下发版本必须落库为本地 desired 真源，且版本单调递增。
func TestRecordDeliveredDesiredKeepsMonotonicRevisions(t *testing.T) {
	store := openClientStore(t, filepath.Join(t.TempDir(), "jrpc.db"))

	var local uint64
	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		var err error
		local, err = tx.RecordDeliveredDesired(7, `{"代理":[]}`)
		return err
	}); err != nil {
		t.Fatalf("保存下发版本失败：%v", err)
	}
	if local != 1 {
		t.Fatalf("首个本地版本号应为 1，实际为 %d", local)
	}

	state := mustRevisionState(t, store)
	if state.DesiredRevision != 1 {
		t.Fatalf("desired 应推进到 1：%+v", state)
	}
	if state.ActiveRevision != 0 || state.LastGoodRevision != 0 {
		t.Fatalf("收到下发内容不得直接声称 active：%+v", state)
	}

	// 服务端下发旧版本时必须拒绝，客户端不得翻回旧版本。
	err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.RecordDeliveredDesired(3, `{"代理":[]}`)
		return err
	})
	if !errors.Is(err, ErrRevisionOutOfOrder) {
		t.Fatalf("服务端下发旧版本应被拒绝：%v", err)
	}
}

// 已接收的下发内容不可原地修改，只能追加新版本。
func TestDeliveredDesiredContentIsImmutable(t *testing.T) {
	store := openClientStore(t, filepath.Join(t.TempDir(), "jrpc.db"))
	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.RecordDeliveredDesired(1, "原始内容")
		return err
	}); err != nil {
		t.Fatalf("保存下发版本失败：%v", err)
	}

	err := store.Transaction(context.Background(), func(tx *Tx) error {
		return tx.DB().Exec("UPDATE desired_states SET content = ? WHERE revision = 1", "被篡改").Error
	})
	if err == nil {
		t.Fatal("修改已接收的下发内容必须被拒绝")
	}
	if !strings.Contains(err.Error(), "不可修改") {
		t.Fatalf("拒绝原因应说明内容不可修改：%v", err)
	}
}

// 三个 revision 分列表达；publish 未成功时 active 与 last-good 不得推进。
func TestRevisionsTrackedSeparatelyOnClient(t *testing.T) {
	store := openClientStore(t, filepath.Join(t.TempDir(), "jrpc.db"))

	err := store.Transaction(context.Background(), func(tx *Tx) error {
		if _, err := tx.RecordDeliveredDesired(4, "内容"); err != nil {
			return err
		}
		if err := tx.RecordApplyResult(ApplyResultInput{
			Revision: 1, Phase: PhasePrepare, Succeeded: true,
		}); err != nil {
			return err
		}
		return tx.RecordApplyResult(ApplyResultInput{
			Revision: 1, Phase: PhaseHealthCheck, Succeeded: false, ErrorDetail: "端口占用",
		})
	})
	if err != nil {
		t.Fatalf("记录应用结果失败：%v", err)
	}

	state := mustRevisionState(t, store)
	if state.DesiredRevision != 1 {
		t.Fatalf("desired 应为 1：%+v", state)
	}
	if state.ActiveRevision != 0 || state.LastGoodRevision != 0 {
		t.Fatalf("publish 未成功时 active/last-good 不得推进：%+v", state)
	}

	err = store.Transaction(context.Background(), func(tx *Tx) error {
		if err := tx.RecordApplyResult(ApplyResultInput{
			Revision: 1, Phase: PhasePublish, Succeeded: true,
		}); err != nil {
			return err
		}
		return tx.MarkApplyResultsReported(1)
	})
	if err != nil {
		t.Fatalf("记录 publish 与回执失败：%v", err)
	}
	state = mustRevisionState(t, store)
	if state.ActiveRevision != 1 || state.LastGoodRevision != 1 {
		t.Fatalf("publish 成功后 active 与 last-good 应推进到 1：%+v", state)
	}
}

// 重启后必须由 desired 重新走应用流程，且 active 记录必须来自成功 publish。
func TestClientRecoveryReadsDesiredAndRejectsForgedActive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jrpc.db")
	store := openClientStore(t, path)

	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		if _, err := tx.RecordDeliveredDesired(2, "内容"); err != nil {
			return err
		}
		if err := tx.RecordApplyResult(ApplyResultInput{
			Revision: 1, Phase: PhasePublish, Succeeded: true,
		}); err != nil {
			return err
		}
		return tx.RecordApplyResult(ApplyResultInput{
			Revision: 1, Phase: PhaseDrain, Succeeded: true,
		})
	}); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}

	restarted := openClientStore(t, path)
	input, err := restarted.LoadRevisionForRecovery(context.Background())
	if err != nil {
		t.Fatalf("读取恢复输入失败：%v", err)
	}
	if !input.HasDesired || input.DesiredRevision != 1 {
		t.Fatalf("恢复输入应包含 desired：%+v", input)
	}
	if input.RecordedActiveRevision != 1 || input.RecordedLastGoodRevision != 1 {
		t.Fatalf("恢复输入应带上一次运行的 Apply 结果记录：%+v", input)
	}

	// 伪造一条没有成功 publish 支撑的 active 记录，恢复必须拒绝。
	if err := restarted.DB().Exec("UPDATE revision_records SET active_revision = 0").Error; err != nil {
		t.Fatalf("重置 active 失败：%v", err)
	}
	if err := restarted.DB().Exec("DELETE FROM apply_results").Error; err != nil {
		t.Fatalf("清理应用结果失败：%v", err)
	}
	if err := restarted.DB().Exec("UPDATE revision_records SET active_revision = 1, last_good_revision = 1").Error; err != nil {
		t.Fatalf("制造不一致 active 失败：%v", err)
	}
	if _, err := restarted.LoadRevisionForRecovery(context.Background()); err == nil {
		t.Fatal("active 没有成功 publish 记录时恢复必须失败")
	} else if !strings.Contains(err.Error(), "publish") {
		t.Fatalf("失败原因应指明缺少成功 publish：%v", err)
	}
}

// 本地审计事件是追加写：不接受事后改写或删除。
func TestClientAuditEventsAreAppendOnly(t *testing.T) {
	store := openClientStore(t, filepath.Join(t.TempDir(), "jrpc.db"))
	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.RecordDeliveredDesired(1, "内容")
		return err
	}); err != nil {
		t.Fatalf("写入下发版本失败：%v", err)
	}

	if err := store.DB().Exec("UPDATE audit_events SET context = '篡改'").Error; err == nil {
		t.Fatal("修改审计事件必须被拒绝")
	}
	if err := store.DB().Exec("DELETE FROM audit_events").Error; err == nil {
		t.Fatal("删除审计事件必须被拒绝")
	}
}

// 身份落库后不得在审计或运行状态中出现 token 明文。
func TestIdentityStoredWithAuditRedaction(t *testing.T) {
	store := openClientStore(t, filepath.Join(t.TempDir(), "jrpc.db"))
	const token = "jrpc-token-abcdef1234567890"

	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		return tx.StoreIdentity(Identity{
			ClientID: "client-1", Token: token, ServerAddress: "https://jrps.invalid",
			DeviceSummary: "测试机器", EnrolledAt: time.Now().UTC(),
		})
	}); err != nil {
		t.Fatalf("保存身份失败：%v", err)
	}

	var events []AuditEvent
	if err := store.View(context.Background(), func(tx *Tx) error {
		var err error
		events, err = tx.AuditEvents()
		return err
	}); err != nil {
		t.Fatalf("读取审计失败：%v", err)
	}
	for _, event := range events {
		if strings.Contains(event.Context, token) || strings.Contains(event.ObjectID, token) {
			t.Fatalf("本地审计泄露了 token：%+v", event)
		}
	}

	if err := store.View(context.Background(), func(tx *Tx) error {
		identity, err := tx.Identity()
		if err != nil {
			return err
		}
		if identity.ClientID != "client-1" {
			return fmt.Errorf("身份读取不匹配：%+v", identity)
		}
		return nil
	}); err != nil {
		t.Fatalf("读取身份失败：%v", err)
	}
}

// 回滚的上报记录不得留在待上报队列中。
func TestOutboxRecordsDisappearOnRollback(t *testing.T) {
	store := openClientStore(t, filepath.Join(t.TempDir(), "jrpc.db"))
	rollback := errors.New("业务失败")

	err := store.Transaction(context.Background(), func(tx *Tx) error {
		if _, err := tx.OutboxEnqueue(Outbox{EventID: "evt-1", EventType: "apply_result"}); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("事务应回滚并返回业务错误：%v", err)
	}

	if count := mustPendingOutboxCount(t, store); count != 0 {
		t.Fatalf("回滚后不得存在待上报记录，实际为 %d", count)
	}
}

func mustRevisionState(t *testing.T, store *Store) RevisionRecord {
	t.Helper()
	var state RevisionRecord
	if err := store.View(context.Background(), func(tx *Tx) error {
		var err error
		state, err = tx.RevisionState()
		return err
	}); err != nil {
		t.Fatalf("读取版本状态失败：%v", err)
	}
	return state
}

func mustPendingOutboxCount(t *testing.T, store *Store) int64 {
	t.Helper()
	var count int64
	if err := store.View(context.Background(), func(tx *Tx) error {
		var err error
		count, err = tx.PendingOutboxCount()
		return err
	}); err != nil {
		t.Fatalf("统计待上报记录失败：%v", err)
	}
	return count
}
