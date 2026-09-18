package store

import (
	"context"
	"strings"
	"testing"
)

// stubApplier 是测试用的应用流程替身，按给定结果返回阶段结论。
type stubApplier struct {
	outcome    ApplyOutcome
	content    string
	calledWith int
}

func (a *stubApplier) Apply(_ context.Context, desiredContent string) ApplyOutcome {
	a.calledWith++
	a.content = desiredContent
	return a.outcome
}

// 恢复流程必须以 desired 内容为输入，并在 publish 成功后推进落库的 Apply 结果记录。
func TestRecoverAppliesDesiredAndRecordsPublish(t *testing.T) {
	path := t.TempDir() + "/jrps.db"
	store := openServerStore(t, path)
	actor := ActorAdmin("admin")

	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.SaveProxy(Proxy{ID: "p1", Name: "代理", Type: "tcp", LocalPort: 8080}, actor, OriginProxyCreate)
		return err
	}); err != nil {
		t.Fatalf("写入配置失败：%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}

	restarted := openServerStore(t, path)
	applier := &stubApplier{outcome: ApplyOutcome{Phases: []PhaseOutcome{
		{Phase: PhasePrepare, Succeeded: true},
		{Phase: PhaseHealthCheck, Succeeded: true},
		{Phase: PhasePublish, Succeeded: true},
		{Phase: PhaseDrain, Succeeded: true},
	}}}
	if err := restarted.Recover(context.Background(), applier, actor); err != nil {
		t.Fatalf("恢复失败：%v", err)
	}
	if applier.calledWith != 1 {
		t.Fatalf("恢复必须以 desired 为输入调用应用流程：%d", applier.calledWith)
	}
	if !strings.Contains(applier.content, "8080") {
		t.Fatalf("应用流程应收到 SQLite 中的 desired 内容：%s", applier.content)
	}

	state := mustRevisionState(t, restarted)
	if state.ActiveRevision != 1 || state.LastGoodRevision != 1 {
		t.Fatalf("恢复的 publish 成功后应记录 active 与 last-good：%+v", state)
	}
	results := mustApplyResults(t, restarted, 1)
	if len(results) != 4 {
		t.Fatalf("恢复应记录四个阶段的应用结果：%+v", results)
	}
}

// 恢复流程在 publish 前失败时不得声称 active，也不得推进 last-good。
func TestRecoverFailureBeforePublishDoesNotClaimActive(t *testing.T) {
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
		t.Fatalf("准备基线失败：%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}

	restarted := openServerStore(t, path)
	applier := &stubApplier{outcome: ApplyOutcome{Phases: []PhaseOutcome{
		{Phase: PhasePrepare, Succeeded: true},
		{Phase: PhaseHealthCheck, Succeeded: false, ErrorDetail: "端口绑定失败"},
	}}}
	if err := restarted.Recover(context.Background(), applier, actor); err != nil {
		t.Fatalf("恢复流程本身不应报错，阶段失败已落库：%v", err)
	}

	state := mustRevisionState(t, restarted)
	if state.ActiveRevision != 1 || state.LastGoodRevision != 1 {
		t.Fatalf("publish 前失败不得改变 active/last-good 记录：%+v", state)
	}
	var succeeded bool
	if err := restarted.View(context.Background(), func(tx *Tx) error {
		results, err := tx.ApplyResults(1)
		if err != nil {
			return err
		}
		for _, result := range results {
			if result.Phase == PhaseHealthCheck {
				succeeded = result.Succeeded
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("读取应用结果失败：%v", err)
	}
	if succeeded {
		t.Fatal("health-check 失败必须如实落库")
	}
}

// 空数据库启动时不得声称 active，也不需要调用应用流程。
func TestRecoverOnEmptyDatabaseKeepsActiveEmpty(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	applier := &stubApplier{}

	if err := store.Recover(context.Background(), applier, ActorAdmin("admin")); err != nil {
		t.Fatalf("空数据库恢复失败：%v", err)
	}
	if applier.calledWith != 0 {
		t.Fatalf("无 desired 时不应调用应用流程：%d", applier.calledWith)
	}
	state := mustRevisionState(t, store)
	if state.DesiredRevision != 0 || state.ActiveRevision != 0 || state.LastGoodRevision != 0 {
		t.Fatalf("空数据库三个 revision 均应为空：%+v", state)
	}
}

// 有 desired 但未提供应用流程实现时必须拒绝恢复到"已激活"。
func TestRecoverWithoutApplierIsRejected(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.AppendRevision(RevisionInput{
			Content: "内容", Actor: ActorAdmin("admin"), Origin: OriginProxyCreate,
		})
		return err
	}); err != nil {
		t.Fatalf("写入版本失败：%v", err)
	}

	err := store.Recover(context.Background(), nil, ActorAdmin("admin"))
	if err == nil {
		t.Fatal("存在 desired 但没有应用流程时必须拒绝恢复")
	}
	if !strings.Contains(err.Error(), "应用流程") {
		t.Fatalf("拒绝原因应指明缺少应用流程：%v", err)
	}
}

// 应用流程返回空结果时必须中止，不得静默视为成功。
func TestRecoverRejectsEmptyPhaseOutcome(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.AppendRevision(RevisionInput{
			Content: "内容", Actor: ActorAdmin("admin"), Origin: OriginProxyCreate,
		})
		return err
	}); err != nil {
		t.Fatalf("写入版本失败：%v", err)
	}

	err := store.Recover(context.Background(), &stubApplier{}, ActorAdmin("admin"))
	if err == nil {
		t.Fatal("空阶段结果必须视为异常并中止恢复")
	}
	state := mustRevisionState(t, store)
	if state.ActiveRevision != 0 {
		t.Fatalf("中止的恢复不得声称 active：%+v", state)
	}
}

// 阶段命名固定为 prepare、health-check、publish、drain，与服务端共用同一套命名。
func TestPhaseNamesAreFixed(t *testing.T) {
	expected := map[string]string{
		PhasePrepare:     "prepare",
		PhaseHealthCheck: "health_check",
		PhasePublish:     "publish",
		PhaseDrain:       "drain",
	}
	for actual, want := range expected {
		if actual != want {
			t.Fatalf("阶段命名不匹配：期望 %s，实际 %s", want, actual)
		}
	}
}
