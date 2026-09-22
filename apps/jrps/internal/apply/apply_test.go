package apply

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/wcpe/jrp/apps/jrps/internal/store"
	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/server"
)

// stubEngine 是引擎替身：按预设脚本返回 Apply 结果，并记录收到的部署。
type stubEngine struct {
	result core.ApplyResult
	err    error
	// deployments 记录每次 Apply 收到的 revision，供幂等与并发断言。
	revisions []uint64
}

func (engine *stubEngine) Apply(_ context.Context, deployment server.Deployment) (core.ApplyResult, error) {
	engine.revisions = append(engine.revisions, deployment.Revision)
	return engine.result, engine.err
}

func (engine *stubEngine) ActiveRevision() uint64 { return 0 }

func (engine *stubEngine) LastGoodRevision() uint64 { return 0 }

// stubCredentials 是固定凭证集合的 Provider 替身。
type stubCredentials struct {
	credentials []core.ClientCredential
	err         error
}

func (provider *stubCredentials) DataPlaneCredentials(_ context.Context) ([]core.ClientCredential, error) {
	return provider.credentials, provider.err
}

// newTestService 构造带真实 store 的编排服务，并写入一份合法 desired。
func newTestService(t *testing.T, engine Engine, credentials CredentialProvider) (*Service, *store.Store) {
	t.Helper()
	database, err := store.Open(store.Config{
		Path:   filepath.Join(t.TempDir(), "jrps.db"),
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("打开测试数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	actor := store.ActorAdmin("admin")
	if err := database.Transaction(context.Background(), func(tx *store.Tx) error {
		_, err := tx.SaveProxy(store.Proxy{
			ID: "p1", ClientID: "c1", Name: "ssh", Type: "tcp",
			RemotePort: 26022, Target: "127.0.0.1:22",
		}, actor, store.OriginProxyCreate)
		return err
	}); err != nil {
		t.Fatalf("写入测试代理失败：%v", err)
	}
	return New(database, credentials, engine, slog.New(slog.DiscardHandler)), database
}

func testCredentials() CredentialProvider {
	return &stubCredentials{credentials: []core.ClientCredential{
		{ClientID: "c1", Token: "token-c1"},
	}}
}

// 查询最新 desired revision 供测试驱动。
func latestRevision(t *testing.T, database *store.Store) uint64 {
	t.Helper()
	var revision uint64
	if err := database.View(context.Background(), func(tx *store.Tx) error {
		latest, err := tx.LatestRevision()
		if err != nil {
			return err
		}
		revision = latest.Revision
		return nil
	}); err != nil {
		t.Fatalf("读取最新版本失败：%v", err)
	}
	return revision
}

// 断言落库的阶段结果序列与期望一致。
func assertPhases(t *testing.T, database *store.Store, revision uint64, wantPhaseSucceeded map[string]bool) {
	t.Helper()
	var results []store.ApplyResult
	if err := database.View(context.Background(), func(tx *store.Tx) error {
		var err error
		results, err = tx.ApplyResults(revision)
		return err
	}); err != nil {
		t.Fatalf("读取应用结果失败：%v", err)
	}
	got := make(map[string]bool, len(results))
	for _, result := range results {
		got[result.Phase] = result.Succeeded
	}
	if len(got) != len(wantPhaseSucceeded) {
		t.Fatalf("阶段结果数不符：落库 %v，期望 %v", got, wantPhaseSucceeded)
	}
	for phase, succeeded := range wantPhaseSucceeded {
		if got[phase] != succeeded {
			t.Fatalf("阶段 %s 结果不符：落库 %v，期望 %v", phase, got[phase], succeeded)
		}
	}
}

// 正常路径：四阶段全部成功落库，active 与 last-good 推进到新 revision。
func TestApplyDesiredSuccess(t *testing.T) {
	engine := &stubEngine{result: core.ApplyResult{Revision: 1, Stage: core.StageDrained}}
	service, database := newTestService(t, engine, testCredentials())
	revision := latestRevision(t, database)

	if err := service.ApplyDesired(context.Background(), revision, store.ActorAdmin("admin"), "req-1"); err != nil {
		t.Fatalf("应用失败：%v", err)
	}
	assertPhases(t, database, revision, map[string]bool{
		store.PhaseValidate:    true,
		store.PhasePrepare:     true,
		store.PhaseHealthCheck: true,
		store.PhasePublish:     true,
		store.PhaseDrain:       true,
	})
	var state store.RevisionState
	if err := database.View(context.Background(), func(tx *store.Tx) error {
		var err error
		state, err = tx.RevisionState(store.ScopeServer)
		return err
	}); err != nil {
		t.Fatalf("读取状态失败：%v", err)
	}
	if state.ActiveRevision != revision || state.LastGoodRevision != revision {
		t.Fatalf("active/last-good 未推进：active=%d lastGood=%d 期望 %d",
			state.ActiveRevision, state.LastGoodRevision, revision)
	}
}

// validate 失败（内容非法）：只落 validate 失败记录，不调用引擎，active 不变。
func TestApplyDesiredInvalidContent(t *testing.T) {
	database, err := store.Open(store.Config{
		Path:   filepath.Join(t.TempDir(), "jrps.db"),
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("打开测试数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	// 直接追加非法内容，模拟真源被破坏的路径。
	if err := database.Transaction(context.Background(), func(tx *store.Tx) error {
		_, err := tx.AppendRevision(store.RevisionInput{
			Content: "不是 JSON", Actor: store.ActorAdmin("admin"), Origin: store.OriginProxyCreate,
		})
		return err
	}); err != nil {
		t.Fatalf("写入非法内容失败：%v", err)
	}

	engine := &stubEngine{}
	service := New(database, testCredentials(), engine, slog.New(slog.DiscardHandler))
	revision := latestRevision(t, database)

	err = service.ApplyDesired(context.Background(), revision, store.ActorAdmin("admin"), "req-1")
	if !errors.Is(err, ErrInvalidDesired) {
		t.Fatalf("应返回 ErrInvalidDesired，实际：%v", err)
	}
	if len(engine.revisions) != 0 {
		t.Fatalf("内容非法时不应调用引擎，实际调用 %d 次", len(engine.revisions))
	}
	assertPhases(t, database, revision, map[string]bool{
		store.PhaseValidate: false,
	})
}

// prepare 失败：validate/prepare 落库，失败阶段之后的阶段不落库，active 与 last-good 不变。
func TestApplyDesiredPrepareFailureKeepsActive(t *testing.T) {
	engine := &stubEngine{err: core.NewApplyError(core.StagePrepare, errors.New("绑定端口失败"))}
	service, database := newTestService(t, engine, testCredentials())
	revision := latestRevision(t, database)

	if err := service.ApplyDesired(context.Background(), revision, store.ActorAdmin("admin"), "req-1"); err == nil {
		t.Fatalf("prepare 失败时应用应返回错误")
	}
	assertPhases(t, database, revision, map[string]bool{
		store.PhaseValidate: true,
		store.PhasePrepare:  false,
	})
	var state store.RevisionState
	if err := database.View(context.Background(), func(tx *store.Tx) error {
		var err error
		state, err = tx.RevisionState(store.ScopeServer)
		return err
	}); err != nil {
		t.Fatalf("读取状态失败：%v", err)
	}
	if state.ActiveRevision != 0 || state.LastGoodRevision != 0 {
		t.Fatalf("失败后 active/last-good 应保持不变：active=%d lastGood=%d",
			state.ActiveRevision, state.LastGoodRevision)
	}
}

// health-check 失败：失败点之前的阶段全部落库为成功。
func TestApplyDesiredHealthCheckFailure(t *testing.T) {
	engine := &stubEngine{err: core.NewApplyError(core.StageHealthCheck, errors.New("入口不可用"))}
	service, database := newTestService(t, engine, testCredentials())
	revision := latestRevision(t, database)

	if err := service.ApplyDesired(context.Background(), revision, store.ActorAdmin("admin"), "req-1"); err == nil {
		t.Fatalf("health-check 失败时应用应返回错误")
	}
	assertPhases(t, database, revision, map[string]bool{
		store.PhaseValidate:    true,
		store.PhasePrepare:     true,
		store.PhaseHealthCheck: false,
	})
}

// 请求过期的 revision：拒绝且不触碰引擎（规格 §3.3 禁止最后写入静默覆盖）。
func TestApplyDesiredRejectsStaleRevision(t *testing.T) {
	engine := &stubEngine{result: core.ApplyResult{Stage: core.StageDrained}}
	service, database := newTestService(t, engine, testCredentials())
	actor := store.ActorAdmin("admin")

	// 再追加一个版本，使第一个版本过期。
	if err := database.Transaction(context.Background(), func(tx *store.Tx) error {
		_, err := tx.SaveProxy(store.Proxy{
			ID: "p2", ClientID: "c1", Name: "web", Type: "tcp",
			RemotePort: 26080, Target: "127.0.0.1:80",
		}, actor, store.OriginProxyCreate)
		return err
	}); err != nil {
		t.Fatalf("写入第二个代理失败：%v", err)
	}

	err := service.ApplyDesired(context.Background(), 1, actor, "req-1")
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("应返回 ErrStaleRevision，实际：%v", err)
	}
	if len(engine.revisions) != 0 {
		t.Fatalf("过期请求不应触达引擎，实际调用 %d 次", len(engine.revisions))
	}
}

// 单飞互斥：进行中收到第二个请求返回 ErrApplyInProgress，不排队不合并。
func TestApplyDesiredRejectsConcurrent(t *testing.T) {
	first := make(chan struct{})
	release := make(chan struct{})
	engine := &blockingEngine{started: first, release: release}
	service, database := newTestService(t, engine, testCredentials())
	revision := latestRevision(t, database)
	actor := store.ActorAdmin("admin")

	done := make(chan error, 1)
	go func() {
		done <- engine.run(service, revision, actor)
	}()
	<-first

	err := service.ApplyDesired(context.Background(), revision, actor, "req-2")
	if !errors.Is(err, ErrApplyInProgress) {
		t.Fatalf("并发请求应返回 ErrApplyInProgress，实际：%v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("第一个应用应成功：%v", err)
	}
}

// blockingEngine 用同步点把第一个 Apply 卡在引擎内，制造并发窗口。
type blockingEngine struct {
	started chan struct{}
	release chan struct{}
	entered bool
}

func (engine *blockingEngine) Apply(_ context.Context, _ server.Deployment) (core.ApplyResult, error) {
	if !engine.entered {
		engine.entered = true
		engine.started <- struct{}{}
		<-engine.release
	}
	return core.ApplyResult{Stage: core.StageDrained}, nil
}

func (engine *blockingEngine) ActiveRevision() uint64   { return 0 }
func (engine *blockingEngine) LastGoodRevision() uint64 { return 0 }

func (engine *blockingEngine) run(service *Service, revision uint64, actor store.Actor) error {
	return service.ApplyDesired(context.Background(), revision, actor, "req-1")
}

// 快照转换：合法文档生成包含控制监听与代理绑定的快照，凭证来自 Provider。
func TestBuildSnapshot(t *testing.T) {
	engine := &stubEngine{result: core.ApplyResult{Stage: core.StageDrained}}
	service, database := newTestService(t, engine, testCredentials())
	revision := latestRevision(t, database)

	var content string
	if err := database.View(context.Background(), func(tx *store.Tx) error {
		record, err := tx.Revision(revision)
		if err != nil {
			return err
		}
		content = record.Content
		return nil
	}); err != nil {
		t.Fatalf("读取内容失败：%v", err)
	}

	config, err := service.buildSnapshot(context.Background(), content)
	if err != nil {
		t.Fatalf("构建快照失败：%v", err)
	}
	if config.Listen().Address.Port() != store.DefaultControlListenPort {
		t.Fatalf("控制监听端口不符：%d", config.Listen().Address.Port())
	}
	if len(config.Credentials()) != 1 || config.Credentials()[0].ClientID != "c1" {
		t.Fatalf("凭证集合不符：%+v", config.Credentials())
	}
	bindings := config.Bindings()
	if len(bindings) != 1 || bindings[0].Name != "ssh" || bindings[0].RemotePort != 26022 {
		t.Fatalf("代理绑定不符：%+v", bindings)
	}
}

// 墓碑代理不进入快照：删除动作在应用后表现为入口消失。
func TestBuildSnapshotSkipsTombstones(t *testing.T) {
	engine := &stubEngine{result: core.ApplyResult{Stage: core.StageDrained}}
	service, database := newTestService(t, engine, testCredentials())
	actor := store.ActorAdmin("admin")

	// 删除 p1：文档保留墓碑，快照不含绑定。
	if err := database.Transaction(context.Background(), func(tx *store.Tx) error {
		_, err := tx.DeleteProxy(store.Proxy{ID: "p1", ClientID: "c1", Name: "ssh", Type: "tcp"}, actor)
		return err
	}); err != nil {
		t.Fatalf("删除代理失败：%v", err)
	}
	revision := latestRevision(t, database)
	var content string
	if err := database.View(context.Background(), func(tx *store.Tx) error {
		record, err := tx.Revision(revision)
		if err != nil {
			return err
		}
		content = record.Content
		return nil
	}); err != nil {
		t.Fatalf("读取内容失败：%v", err)
	}

	config, err := service.buildSnapshot(context.Background(), content)
	if err != nil {
		t.Fatalf("构建快照失败：%v", err)
	}
	if len(config.Bindings()) != 0 {
		t.Fatalf("墓碑代理不应进入快照：%+v", config.Bindings())
	}
}

// 引用无凭证客户端的代理在转换期被拒绝：绑定校验前置，避免 Core 侧才报错。
func TestBuildSnapshotRejectsUnknownClient(t *testing.T) {
	engine := &stubEngine{}
	service, database := newTestService(t, engine, testCredentials())
	actor := store.ActorAdmin("admin")

	if err := database.Transaction(context.Background(), func(tx *store.Tx) error {
		_, err := tx.SaveProxy(store.Proxy{
			ID: "p3", ClientID: "ghost", Name: "orphan", Type: "tcp",
			RemotePort: 26100, Target: "127.0.0.1:100",
		}, actor, store.OriginProxyCreate)
		return err
	}); err != nil {
		t.Fatalf("写入孤儿代理失败：%v", err)
	}
	revision := latestRevision(t, database)
	var content string
	if err := database.View(context.Background(), func(tx *store.Tx) error {
		record, err := tx.Revision(revision)
		if err != nil {
			return err
		}
		content = record.Content
		return nil
	}); err != nil {
		t.Fatalf("读取内容失败：%v", err)
	}

	_, err := service.buildSnapshot(context.Background(), content)
	if !errors.Is(err, ErrInvalidDesired) {
		t.Fatalf("应返回 ErrInvalidDesired，实际：%v", err)
	}
}

// RecoverApplier 实现 store.PhaseApplier，供启动恢复注入。
func TestRecoverApplier(t *testing.T) {
	engine := &stubEngine{result: core.ApplyResult{Revision: 3, Stage: core.StageDrained}}
	service, database := newTestService(t, engine, testCredentials())
	revision := latestRevision(t, database)

	applier := service.RecoverApplier(store.ActorAdmin("server"))
	outcome := applier.Apply(context.Background(), "任意内容")
	if len(outcome.Phases) == 0 {
		t.Fatalf("恢复应用应返回阶段结果")
	}
	// RecoverApplier 以当前 desired 为输入：这里验证它调用了引擎且带正确 revision。
	if len(engine.revisions) != 1 || engine.revisions[0] != revision {
		t.Fatalf("恢复应用应带当前 desired revision 调用引擎：%v", engine.revisions)
	}
}
