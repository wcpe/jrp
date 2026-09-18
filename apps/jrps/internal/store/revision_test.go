package store

import (
	"context"
	"strings"
	"testing"
)

// 代理变更必须生成新的配置版本，历史版本内容保持不变。
func TestSaveProxyAppendsImmutableRevision(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	actor := ActorAdmin("admin")

	var first, second uint64
	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		var err error
		first, err = tx.SaveProxy(Proxy{
			ID: "proxy-1", Name: "网站", Type: "tcp", LocalPort: 8080, RemotePort: 9080,
		}, actor, OriginProxyCreate)
		return err
	}); err != nil {
		t.Fatalf("创建代理失败：%v", err)
	}
	if first != 1 {
		t.Fatalf("首个版本号应为 1，实际为 %d", first)
	}

	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		var err error
		second, err = tx.SaveProxy(Proxy{
			ID: "proxy-1", Name: "网站", Type: "tcp", LocalPort: 8081, RemotePort: 9080,
		}, actor, OriginProxyUpdate)
		return err
	}); err != nil {
		t.Fatalf("修改代理失败：%v", err)
	}
	if second != 2 {
		t.Fatalf("修改后应生成新版本 2，实际为 %d", second)
	}

	if err := store.View(context.Background(), func(tx *Tx) error {
		snapshot, err := tx.Revision(first)
		if err != nil {
			return err
		}
		if !strings.Contains(snapshot.Content, "8080") {
			t.Fatalf("历史版本内容被改写：%s", snapshot.Content)
		}
		latest, err := tx.LatestRevision()
		if err != nil {
			return err
		}
		if !strings.Contains(latest.Content, "8081") {
			t.Fatalf("最新版本内容未包含新端口：%s", latest.Content)
		}
		return nil
	}); err != nil {
		t.Fatalf("校验历史版本失败：%v", err)
	}
}

// 尝试原地修改已有版本内容必须被拒绝（数据库层强制，不依赖应用层约定）。
func TestUpdateExistingRevisionIsRejected(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.AppendRevision(RevisionInput{
			Content: "原始内容", Actor: ActorAdmin("admin"), Origin: OriginProxyCreate,
		})
		return err
	}); err != nil {
		t.Fatalf("追加版本失败：%v", err)
	}

	err := store.Transaction(context.Background(), func(tx *Tx) error {
		return tx.DB().Exec("UPDATE config_revisions SET content = ? WHERE revision = 1", "被篡改").Error
	})
	if err == nil {
		t.Fatal("修改已有版本内容必须被拒绝")
	}
	if !strings.Contains(err.Error(), "不可修改") {
		t.Fatalf("拒绝原因应说明版本不可修改：%v", err)
	}

	if err := store.View(context.Background(), func(tx *Tx) error {
		snapshot, err := tx.Revision(1)
		if err != nil {
			return err
		}
		if snapshot.Content != "原始内容" {
			t.Fatalf("版本内容被篡改：%s", snapshot.Content)
		}
		return nil
	}); err != nil {
		t.Fatalf("校验版本内容失败：%v", err)
	}
}

// 删除历史版本必须被拒绝：历史版本只能追加，不做删除性改写。
func TestDeleteExistingRevisionIsRejected(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.AppendRevision(RevisionInput{
			Content: "内容", Actor: ActorAdmin("admin"), Origin: OriginProxyCreate,
		})
		return err
	}); err != nil {
		t.Fatalf("追加版本失败：%v", err)
	}

	err := store.Transaction(context.Background(), func(tx *Tx) error {
		return tx.DB().Exec("DELETE FROM config_revisions WHERE revision = 1").Error
	})
	if err == nil {
		t.Fatal("删除历史版本必须被拒绝")
	}
}

// 三个 revision 必须分列表达，且 prepare 与 health-check 不得推进 active/last-good。
func TestRevisionsAreTrackedSeparately(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	actor := ActorAdmin("admin")

	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.SaveProxy(Proxy{ID: "p1", Name: "代理", Type: "tcp"}, actor, OriginProxyCreate)
		return err
	}); err != nil {
		t.Fatalf("创建代理失败：%v", err)
	}

	// prepare 成功、health-check 失败：desired 前进，active 与 last-good 保持不变。
	err := store.Transaction(context.Background(), func(tx *Tx) error {
		if err := tx.RecordApplyResult(ApplyResultInput{
			Revision: 1, Phase: PhasePrepare, Succeeded: true, Actor: actor,
		}); err != nil {
			return err
		}
		return tx.RecordApplyResult(ApplyResultInput{
			Revision: 1, Phase: PhaseHealthCheck, Succeeded: false,
			ErrorDetail: "端口绑定失败", Actor: actor,
		})
	})
	if err != nil {
		t.Fatalf("记录应用结果失败：%v", err)
	}

	state := mustRevisionState(t, store)
	if state.DesiredRevision != 1 {
		t.Fatalf("desired 应推进到 1，实际为 %d", state.DesiredRevision)
	}
	if state.ActiveRevision != 0 || state.LastGoodRevision != 0 {
		t.Fatalf("publish 未成功时 active/last-good 不得推进：%+v", state)
	}

	// publish 成功后 active 与 last-good 才推进到该版本。
	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		return tx.RecordApplyResult(ApplyResultInput{
			Revision: 1, Phase: PhasePublish, Succeeded: true, Actor: actor,
		})
	}); err != nil {
		t.Fatalf("记录 publish 结果失败：%v", err)
	}
	state = mustRevisionState(t, store)
	if state.ActiveRevision != 1 || state.LastGoodRevision != 1 {
		t.Fatalf("publish 成功后 active 与 last-good 应推进到 1：%+v", state)
	}
}

// publish 失败时 last-good 不得推进（回归保护）。
func TestLastGoodDoesNotAdvanceOnPublishFailure(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	actor := ActorAdmin("admin")

	err := store.Transaction(context.Background(), func(tx *Tx) error {
		if _, err := tx.SaveProxy(Proxy{ID: "p1", Name: "代理", Type: "tcp"}, actor, OriginProxyCreate); err != nil {
			return err
		}
		if err := tx.RecordApplyResult(ApplyResultInput{
			Revision: 1, Phase: PhasePublish, Succeeded: true, Actor: actor,
		}); err != nil {
			return err
		}
		if _, err := tx.SaveProxy(Proxy{ID: "p1", Name: "代理", Type: "tcp", LocalPort: 9}, actor, OriginProxyUpdate); err != nil {
			return err
		}
		return tx.RecordApplyResult(ApplyResultInput{
			Revision: 2, Phase: PhasePublish, Succeeded: false,
			ErrorDetail: "发布失败", Actor: actor,
		})
	})
	if err != nil {
		t.Fatalf("记录应用结果失败：%v", err)
	}

	state := mustRevisionState(t, store)
	if state.LastGoodRevision != 1 {
		t.Fatalf("publish 失败时 last-good 应保持在 1，实际为 %d", state.LastGoodRevision)
	}
	if state.ActiveRevision != 1 {
		t.Fatalf("publish 失败时 active 应保持在 1，实际为 %d", state.ActiveRevision)
	}
	if state.DesiredRevision != 2 {
		t.Fatalf("desired 应为 2，实际为 %d", state.DesiredRevision)
	}
}

// 每个 revision 变化都必须有对应审计事件。
func TestRevisionChangesProduceAuditEvents(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	actor := ActorAdmin("admin")

	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		if _, err := tx.SaveProxy(Proxy{ID: "p1", Name: "代理", Type: "tcp"}, actor, OriginProxyCreate); err != nil {
			return err
		}
		return tx.RecordApplyResult(ApplyResultInput{
			Revision: 1, Phase: PhasePublish, Succeeded: true, Actor: actor,
		})
	}); err != nil {
		t.Fatalf("写入失败：%v", err)
	}

	var events []AuditEvent
	if err := store.View(context.Background(), func(tx *Tx) error {
		var err error
		events, err = tx.AuditEvents()
		return err
	}); err != nil {
		t.Fatalf("读取审计事件失败：%v", err)
	}
	actions := map[string]int{}
	for _, event := range events {
		actions[event.Action]++
		if event.OccurredAt.IsZero() {
			t.Fatal("审计事件必须带服务端时间")
		}
	}
	for _, expected := range []string{ActionRevisionAppend, ActionProxyCreate, ActionApplyPublish} {
		if actions[expected] == 0 {
			t.Fatalf("缺少审计动作 %s，实际为 %v", expected, actions)
		}
	}

	results := mustApplyResults(t, store, 1)
	phases := map[string]bool{}
	for _, result := range results {
		phases[result.Phase] = result.Succeeded
	}
	if !phases[PhasePublish] {
		t.Fatalf("缺少成功的 publish 结果记录：%+v", results)
	}
}

// 重启恢复必须读取 desired，并把 active/last-good 标记为仅用于展示的记录值。
func TestRecoveryReadsDesiredAndLeavesActiveEmpty(t *testing.T) {
	path := t.TempDir() + "/jrps.db"
	store := openServerStore(t, path)
	actor := ActorAdmin("admin")

	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		if _, err := tx.SaveProxy(Proxy{ID: "p1", Name: "代理", Type: "tcp"}, actor, OriginProxyCreate); err != nil {
			return err
		}
		return tx.RecordApplyResult(ApplyResultInput{
			Revision: 1, Phase: PhasePublish, Succeeded: true, Actor: actor,
		})
	}); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}

	restarted := openServerStore(t, path)
	input, err := restarted.LoadRevisionForRecovery(context.Background())
	if err != nil {
		t.Fatalf("读取恢复输入失败：%v", err)
	}
	if !input.HasDesired || input.DesiredRevision != 1 {
		t.Fatalf("恢复输入应包含 desired 版本 1：%+v", input)
	}
	if input.RecordedActiveRevision != 1 || input.RecordedLastGoodRevision != 1 {
		t.Fatalf("恢复输入应带上一次运行留下的 Apply 结果记录：%+v", input)
	}
}

// 空数据库首次启动时不得存在 desired，恢复输入必须明确表达这一点。
func TestRecoveryOnEmptyDatabaseHasNoDesired(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	input, err := store.LoadRevisionForRecovery(context.Background())
	if err != nil {
		t.Fatalf("读取恢复输入失败：%v", err)
	}
	if input.HasDesired {
		t.Fatalf("空数据库不应存在 desired：%+v", input)
	}
	if input.RecordedActiveRevision != 0 {
		t.Fatalf("空数据库的 active 记录应为空：%+v", input)
	}
}

// restore 只能以历史内容创建新版本，不得改写历史版本记录。
func TestRestoreCreatesNewRevisionFromHistory(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	actor := ActorAdmin("admin")

	var restored uint64
	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		if _, err := tx.AppendRevision(RevisionInput{
			Content: "原始内容", Actor: actor, Origin: OriginProxyCreate, ChangeSummary: "初始版本",
		}); err != nil {
			return err
		}
		if _, err := tx.AppendRevision(RevisionInput{
			Content: "后续内容", Actor: actor, Origin: OriginProxyUpdate, ChangeSummary: "后续版本",
		}); err != nil {
			return err
		}
		var err error
		restored, err = tx.RestoreRevision(1, actor)
		return err
	}); err != nil {
		t.Fatalf("恢复失败：%v", err)
	}
	if restored != 3 {
		t.Fatalf("恢复应生成版本 3，实际为 %d", restored)
	}

	if err := store.View(context.Background(), func(tx *Tx) error {
		record, err := tx.Revision(3)
		if err != nil {
			return err
		}
		if record.Content != "原始内容" {
			t.Fatalf("恢复版本内容应来自历史版本：%s", record.Content)
		}
		return nil
	}); err != nil {
		t.Fatalf("校验恢复版本失败：%v", err)
	}
}

// 审计失败必须让业务一并回滚，不留"发生了但没有记录"的操作。
func TestAuditFailureRollsBackBusinessChange(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	if err := store.DB().Exec("DROP TABLE audit_events").Error; err != nil {
		t.Fatalf("准备失败场景时删除审计表失败：%v", err)
	}

	err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.SaveProxy(Proxy{ID: "p1", Name: "代理", Type: "tcp"}, ActorAdmin("admin"), OriginProxyCreate)
		return err
	})
	if err == nil {
		t.Fatal("审计写入失败时业务必须回滚")
	}

	if err := store.View(context.Background(), func(tx *Tx) error {
		var count int64
		if err := tx.DB().Model(&ConfigRevision{}).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("回滚后不应留下配置版本记录，实际为 %d", count)
		}
		return nil
	}); err != nil {
		t.Fatalf("校验回滚结果失败：%v", err)
	}
}

func mustRevisionState(t *testing.T, store *Store) RevisionState {
	t.Helper()
	var state RevisionState
	if err := store.View(context.Background(), func(tx *Tx) error {
		var err error
		state, err = tx.RevisionState(ScopeServer)
		return err
	}); err != nil {
		t.Fatalf("读取版本状态失败：%v", err)
	}
	return state
}

func mustApplyResults(t *testing.T, store *Store, revision uint64) []ApplyResult {
	t.Helper()
	var results []ApplyResult
	if err := store.View(context.Background(), func(tx *Tx) error {
		var err error
		results, err = tx.ApplyResults(revision)
		return err
	}); err != nil {
		t.Fatalf("读取应用结果失败：%v", err)
	}
	return results
}
