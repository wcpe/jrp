package store

import (
	"context"
	"strings"
	"testing"
)

// stubApplier 是测试用的应用流程替身。
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

// 客户端恢复必须以本地 desired 为输入，并在 publish 成功后记录 Apply 结果。
func TestClientRecoverAppliesDesired(t *testing.T) {
	path := t.TempDir() + "/jrpc.db"
	store := openClientStore(t, path)

	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.RecordDeliveredDesired(9, `{"代理":["tcp"]}`)
		return err
	}); err != nil {
		t.Fatalf("写入下发版本失败：%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}

	restarted := openClientStore(t, path)
	applier := &stubApplier{outcome: ApplyOutcome{Phases: []PhaseOutcome{
		{Phase: PhasePrepare, Succeeded: true},
		{Phase: PhaseHealthCheck, Succeeded: true},
		{Phase: PhasePublish, Succeeded: true},
		{Phase: PhaseDrain, Succeeded: true},
	}}}
	if err := restarted.Recover(context.Background(), applier); err != nil {
		t.Fatalf("恢复失败：%v", err)
	}
	if applier.calledWith != 1 || !strings.Contains(applier.content, "tcp") {
		t.Fatalf("恢复应以本地 desired 调用应用流程：%d %q", applier.calledWith, applier.content)
	}

	state, err := restarted.LoadRevisionState(context.Background())
	if err != nil {
		t.Fatalf("读取版本状态失败：%v", err)
	}
	if state.ActiveRevision != 1 || state.LastGoodRevision != 1 {
		t.Fatalf("publish 成功后应记录 active 与 last-good：%+v", state)
	}
}

// 恢复在 publish 前失败时不得声称 active。
func TestClientRecoverFailureBeforePublishKeepsActiveEmpty(t *testing.T) {
	path := t.TempDir() + "/jrpc.db"
	store := openClientStore(t, path)
	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.RecordDeliveredDesired(1, "内容")
		return err
	}); err != nil {
		t.Fatalf("写入下发版本失败：%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}

	restarted := openClientStore(t, path)
	applier := &stubApplier{outcome: ApplyOutcome{Phases: []PhaseOutcome{
		{Phase: PhasePrepare, Succeeded: false, ErrorDetail: "校验失败"},
	}}}
	if err := restarted.Recover(context.Background(), applier); err != nil {
		t.Fatalf("恢复流程本身不应报错：%v", err)
	}
	state, err := restarted.LoadRevisionState(context.Background())
	if err != nil {
		t.Fatalf("读取版本状态失败：%v", err)
	}
	if state.ActiveRevision != 0 || state.LastGoodRevision != 0 {
		t.Fatalf("publish 未成功时不得声称 active：%+v", state)
	}
}

// 空数据库启动时不需要调用应用流程。
func TestClientRecoverOnEmptyDatabase(t *testing.T) {
	store := openClientStore(t, t.TempDir()+"/jrpc.db")
	applier := &stubApplier{}
	if err := store.Recover(context.Background(), applier); err != nil {
		t.Fatalf("空数据库恢复失败：%v", err)
	}
	if applier.calledWith != 0 {
		t.Fatalf("无 desired 时不应调用应用流程：%d", applier.calledWith)
	}
}
