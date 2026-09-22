package client

import (
	"context"
	"errors"
	"fmt"

	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/internal/transport"
)

// Deployment 是一次配置应用的完整输入。
//
// 只接受完整快照，不提供增量修改入口：增量 API 会形成第二套配置真源，与
// 「SQLite desired 为唯一真源」的分工冲突（规格 §2）。
type Deployment struct {
	// Revision 由宿主分配并单调递增；Core 只与当前 active 比较。
	Revision uint64
	// Config 是完整不可变配置快照（FR-32 构建器产出）。
	Config core.ClientConfig
	// Bindings 是本次涉及的宿主注入资源；客户端不消费宿主注入资源。
	Bindings []core.Binding
}

// Apply 按 prepare → health-check → publish → drain 应用一份完整快照。
//
// 核心承诺：publish 之前的任何失败都释放本次新建资源，并完整保留旧 active
// 与 last-good，不出现半生效状态（规格 §3.3）。publish 之后切换不可撤销，
// drain 异常只标记在结果里。
//
// 单飞：同一 Engine 同一时间只允许一个 Apply；并发调用返回
// ErrApplyInProgress，不排队也不静默合并——排队与 latest-wins 属宿主的
// 控制面策略（ADR-0005），Core 代它决定会让上层无法表达自己的取舍。
func (engine *Engine) Apply(ctx context.Context, deployment Deployment) (core.ApplyResult, error) {
	if deployment.Revision == core.SnapshotRevisionUnknown {
		return core.ApplyResult{}, core.NewApplyError(core.StageValidate, errors.New("revision 缺失"))
	}

	// 单飞认领：置位即持锁完成，失败者拿不到。
	engine.mu.Lock()
	if engine.applying {
		engine.mu.Unlock()
		return core.ApplyResult{}, fmt.Errorf("同一引擎同一时间只允许一次配置应用：%w", core.ErrApplyInProgress)
	}
	engine.applying = true
	previous := uint64(core.SnapshotRevisionUnknown)
	if engine.active != nil {
		previous = engine.active.revision
	}
	engine.mu.Unlock()

	release := func() {
		engine.mu.Lock()
		engine.applying = false
		engine.mu.Unlock()
	}

	result, err := engine.apply(ctx, deployment, previous)

	// Apply 结果事件在返回前发布（FR-27）：无论成败都发，错误摘要即返回的
	// 原错误（ApplyError 文本由阶段与下层原因组成，不含凭据，规格 §3.7）。
	engine.publishApply(result, err)
	release()
	return result, err
}

// apply 是单飞保护下的状态机本体。
func (engine *Engine) apply(
	ctx context.Context, deployment Deployment, previous uint64,
) (core.ApplyResult, error) {
	// ── validate：结构校验 + 运行态 + 控制会话身份 + revision 比较 ──
	if err := deployment.Config.Validate(); err != nil {
		return core.ApplyResult{}, core.NewApplyError(core.StageValidate, err)
	}
	if len(deployment.Bindings) > 0 {
		return core.ApplyResult{}, core.NewApplyError(core.StageValidate,
			errors.New("客户端 Engine 不接受宿主注入的绑定资源"))
	}

	engine.mu.Lock()
	active := engine.active
	state := engine.state
	controlConfig := engine.config
	engine.mu.Unlock()

	switch state {
	case stateStopped:
		// 引擎已终停：不再接受新配置，否则 Shutdown 与 Apply 会竞争生命周期。
		return core.ApplyResult{}, core.NewApplyError(core.StageValidate, ErrStopped)
	case stateRunning:
	default:
		// 尚未成功 Start：没有控制会话可承载任何一代，应用新配置无从生效。
		return core.ApplyResult{}, core.NewApplyError(core.StageValidate, ErrNotStarted)
	}

	// 控制会话身份不可热更：控制连接在 Start 时按初始快照建立并跨代存活，
	// 服务端端点、客户端标识与令牌决定它拨向谁、以什么身份登录。静默沿用旧
	// 连接会让配置声称的端点与实际连接不一致，因此明确拒绝而不是假装生效。
	if !controlIdentityMatches(controlConfig, deployment.Config) {
		return core.ApplyResult{}, core.NewApplyError(core.StageValidate,
			errors.New("首版不支持热更控制会话身份（服务端端点、客户端标识或令牌）"))
	}

	if active != nil {
		switch {
		case deployment.Revision == active.revision:
			// 幂等：重复提交同一 revision 视为成功，不重建资源、不中断连接。
			return core.ApplyResult{
				Revision: deployment.Revision,
				Previous: previous,
				Stage:    core.StageDrained,
			}, nil
		case deployment.Revision < active.revision:
			// 过期拒绝：宿主可能乱序重放旧版本，直接应用会让配置倒退。
			return core.ApplyResult{}, core.NewApplyError(
				core.StageValidate,
				fmt.Errorf("revision %d 已过期，当前 active 为 %d", deployment.Revision, active.revision),
			)
		}
	}

	// ── prepare：构造新一代，不影响 active ───────────────────────
	gen := newGeneration(engine, deployment.Revision, deployment.Config)
	if err := engine.prepareGeneration(gen); err != nil {
		gen.release()
		return core.ApplyResult{}, core.NewApplyError(core.StagePrepare, err)
	}
	// prepare 之后立即检查取消：prepare 期间取消必须按 publish 前失败处理
	// （释放新代、保留旧版），而不是继续往下切换。
	if err := ctx.Err(); err != nil {
		gen.release()
		return core.ApplyResult{}, core.NewApplyError(core.StagePrepare, err)
	}

	// ── health-check：验证新代可用 ───────────────────────────────
	// 测试钩子在检查之前调用：真实流程下本步不会失败，失败分支若无钩子则无法被
	// 测试触达（服务端侧已实测：删掉失败分支的释放逻辑用例全绿）。
	if engine.healthCheckHook != nil {
		if hookErr := engine.healthCheckHook(gen); hookErr != nil {
			gen.release()
			return core.ApplyResult{}, core.NewApplyError(core.StageHealthCheck, hookErr)
		}
	}
	if err := engine.checkGeneration(gen); err != nil {
		gen.release()
		return core.ApplyResult{}, core.NewApplyError(core.StageHealthCheck, err)
	}
	// publish 前再检查一次取消：health-check 期间取消同样按 publish 前失败处理。
	// 阶段落 health-check（刚完成的阶段），原因透传使 errors.Is 可判定 ctx 起因。
	if err := ctx.Err(); err != nil {
		gen.release()
		return core.ApplyResult{}, core.NewApplyError(core.StageHealthCheck, err)
	}

	// ── atomic publish：一次性切换 active ────────────────────────
	old, err := engine.publishGeneration(gen)
	if err != nil {
		gen.release()
		return core.ApplyResult{}, core.NewApplyError(core.StageValidate, err)
	}
	engine.startGeneration(gen)

	// ── drain：旧代停止维持，已有流跑到结束或上限 ────────────────
	drained := 0
	drainIncomplete := false
	if old != nil {
		// 换代 drain 不是终停：只停旧代的维持循环，在途桥接跑到自然结束。
		old.stop(false)
		drained = countProxies(old)
		// drain 用独立时限：publish 已成功、切换不可撤销，受 Apply 的 ctx
		// 约束会让 Apply 时长不可控（规格 §3.5）。
		limit := engine.drainTimeout
		if snapshotLimit := deployment.Config.DrainTimeout(); snapshotLimit > 0 && snapshotLimit < limit {
			limit = snapshotLimit
		}
		drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), limit)
		defer cancel()
		if err := old.waitDrained(drainCtx, limit); err != nil {
			// 排空异常不回切 active：回切会让新连接二次中断（规格 §3.6）。
			drainIncomplete = true
		}
	}

	stage := core.StageDrained
	if drainIncomplete {
		stage = core.StageApplied
	}
	return core.ApplyResult{
		Revision:        deployment.Revision,
		Previous:        previous,
		Stage:           stage,
		Changed:         countProxies(gen),
		Drained:         drained,
		DrainIncomplete: drainIncomplete,
	}, nil
}

// prepareGeneration 构造新一代的依赖资源。
//
// 客户端没有需要绑定的监听端口：工作连接由 publish 后的维持循环按需拨出，本地
// 目标由每条桥接各自拨号。因此本步只建立工作连接池与代上下文，不触碰网络。
func (engine *Engine) prepareGeneration(gen *generation) error {
	gen.pool = transport.NewWorkConnPool(gen.config.WorkConnPoolSize())
	return nil
}

// checkGeneration 校验新一代可用：引擎仍在运行且控制会话未断开。
//
// 客户端没有需要探测的本地入口，因此健康检查的实质是"这一代还能不能被维持"：
// 控制会话一旦断开，新代即使发布也无人能把它的代理声明给服务端。
func (engine *Engine) checkGeneration(gen *generation) error {
	engine.mu.Lock()
	state := engine.state
	control := engine.control
	done := engine.done
	engine.mu.Unlock()

	if state != stateRunning {
		return ErrStopped
	}
	if control == nil {
		return errors.New("控制会话缺失")
	}
	if gen.pool == nil {
		return errors.New("工作连接池未建立")
	}
	select {
	case <-done:
		return errors.New("控制会话已断开")
	default:
	}
	return nil
}

// publishGeneration 原子切换 active 并返回被替换的旧代。
//
// 切换与"引擎是否已停"的判断在同一把锁内完成：Shutdown 与 publish 并发时，
// 若 publish 发生在 Shutdown 快照之后，新代会无人停也无人等待而泄漏。
func (engine *Engine) publishGeneration(gen *generation) (*generation, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.state != stateRunning {
		return nil, ErrStopped
	}
	old := engine.active
	engine.active = gen
	engine.lastGood = gen.revision
	return old, nil
}

// ActiveRevision 返回当前生效的 revision；尚未应用任何配置时为 0。
func (engine *Engine) ActiveRevision() uint64 {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.active == nil {
		return core.SnapshotRevisionUnknown
	}
	return engine.active.revision
}

// LastGoodRevision 返回最近一次成功 publish 的 revision。
func (engine *Engine) LastGoodRevision() uint64 {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.lastGood
}

// controlIdentityMatches 判断新快照是否仍指向同一条控制会话。
//
// 判定范围只覆盖决定「拨向谁、以什么身份登录」的字段：服务端端点、客户端标识
// 与鉴权材料。心跳、排水上限、代理集合与池参数都随代生效，不属于控制会话身份。
func controlIdentityMatches(base, next core.ClientConfig) bool {
	return base.ServerEndpoint() == next.ServerEndpoint() &&
		base.ClientID() == next.ClientID() &&
		base.Auth() == next.Auth()
}
