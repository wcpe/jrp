package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/wcpe/jrp/core"
)

// Deployment 是一次配置应用的完整输入。
//
// 只接受完整快照，不提供增量修改入口：增量 API 会形成第二套配置真源，与
// 「SQLite desired 为唯一真源」的分工冲突（规格 §2）。
type Deployment struct {
	// Revision 由宿主分配并单调递增；Core 只与当前 active 比较。
	Revision uint64
	// Config 是完整不可变配置快照（FR-32 构建器产出）。
	Config core.ServerConfig
	// Bindings 是本次涉及的宿主注入资源，按绑定标识跨版本对齐。
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
	// Apply 结果事件在返回前发布（FR-27）：无论成败都发。
	engine.publishApply(result, err)
	release()
	return result, err
}

// apply 是单飞保护下的状态机本体。
func (engine *Engine) apply(
	ctx context.Context, deployment Deployment, previous uint64,
) (core.ApplyResult, error) {
	// ── validate：结构校验 + revision 比较 ─────────────────────────
	if err := deployment.Config.Validate(); err != nil {
		return core.ApplyResult{}, core.NewApplyError(core.StageValidate, err)
	}
	// 首版不支持宿主注入的绑定资源：控制监听器跨代复用，访客入口由 Core 按
	// 配置自建，Bindings 当前没有可承载的语义。静默忽略会让宿主误以为注入
	// 生效（泄漏在宿主侧、归属承诺落空），因此直接拒绝并说明。
	if len(deployment.Bindings) > 0 {
		return core.ApplyResult{}, core.NewApplyError(core.StageValidate,
			errors.New("首版不支持宿主注入的绑定资源"))
	}
	engine.mu.Lock()
	active := engine.active
	stopped := engine.state == stateStopped
	engine.mu.Unlock()

	if stopped {
		// 引擎已终停：不再接受新配置，否则 Shutdown 与 Apply 会竞争生命周期。
		return core.ApplyResult{}, core.NewApplyError(core.StageValidate, ErrStopped)
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
	// 未变化的入口在 prepare 里接管旧代的同一个监听套接字，因此不会出现
	// "关旧再开新"的端口空窗，也不会因端口仍被旧代占用而绑定失败。
	engine.mu.Lock()
	previousGen := engine.active
	engine.mu.Unlock()

	gen := newGeneration(engine, deployment.Revision, deployment.Config)
	if err := engine.prepareGeneration(ctx, gen, deployment, previousGen); err != nil {
		gen.release()
		return core.ApplyResult{}, core.NewApplyError(core.StagePrepare, err)
	}
	// prepare 之后立即检查取消：prepare 期间取消必须按 publish 前失败处理
	// （释放新代、保留旧版），而不是继续往下切换。
	if err := ctx.Err(); err != nil {
		gen.release()
		return core.ApplyResult{}, core.NewApplyError(core.StagePrepare, err)
	}

	// ── health-check：验证新资源可用 ─────────────────────────────
	// 测试钩子在检查之前调用：prepare 成功即端口可用，真实流程中 health-check
	// 不会失败，失败分支若无钩子则无法被测试触达（实测删掉 release 用例全绿）。
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

	// ── atomic publish：一次性切换 active ────────────────────────
	// publish 内部复查停止状态：与 Shutdown 并发时，若切换发生在 Shutdown
	// 快照之后，新代会无人 stop 也无人等待而泄漏。此时释放新代并按停止拒绝。
	//
	// publish 前再检查一次取消：health-check 期间取消同样按 publish 前失败处理。
	// 阶段落 health-check（刚完成的阶段），原因透传使 errors.Is 可判定 ctx 起因。
	if err := ctx.Err(); err != nil {
		gen.release()
		return core.ApplyResult{}, core.NewApplyError(core.StageHealthCheck, err)
	}
	old, err := engine.publishGeneration(gen)
	if err != nil {
		gen.release()
		return core.ApplyResult{}, core.NewApplyError(core.StageValidate, err)
	}

	// 交接顺序是关键：先让旧代停止接收，再启动新代的接收循环。
	// 反过来的话两个接收循环会同时挂在被复用的监听器上，谁抢到连接就按谁的代
	// 处理——同一个端口上会出现新旧两套配置并存。
	// 被接管的入口只是解除 Accept，套接字不关，因此这个窗口内到达的连接留在
	// 内核 backlog 里，由新代的循环接走，不会丢。
	drained := 0
	drainIncomplete := false
	if old != nil {
		old.donate(gen.reused)
		old.stopAccepting()
		drained = countEntries(old)
	}
	engine.startGeneration(gen)

	// ── drain：旧代的已有流跑到结束或上限 ────────────────────────
	if old != nil {
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
		Changed:         countEntries(gen),
		Drained:         drained,
		DrainIncomplete: drainIncomplete,
	}, nil
}

// prepareGeneration 为新一代构造监听器、入口与注册表，不触碰 active。
func (engine *Engine) prepareGeneration(
	ctx context.Context, gen *generation, deployment Deployment, previous *generation,
) error {
	engine.workConns = newWorkBroker(deployment.Config.IdleWorkConnLimit())

	if err := engine.openGuestEntries(gen, deployment.Config, previous); err != nil {
		return err
	}
	gen.publishRegistry()

	// 健康检查放在 prepare 之内完成最小闭环：入口地址非空即证明端口已绑定。
	for name, addr := range gen.guestAddr {
		if addr == nil {
			return fmt.Errorf("代理 %s 的入口未绑定", name)
		}
	}
	_ = ctx
	return nil
}

// checkGeneration 校验新代资源可用；当前实现下端口绑定已在 prepare 内隐式完成。
func (engine *Engine) checkGeneration(gen *generation) error {
	if len(gen.guestLns) == 0 && len(gen.udpEntries) == 0 && len(gen.config.Bindings()) == 0 &&
		len(gen.config.HTTPBindings()) == 0 && len(gen.config.UDPBindings()) == 0 &&
		len(gen.config.HTTPSBindings()) == 0 {
		return nil
	}
	for name, listener := range gen.guestLns {
		if listener == nil || !listenerUsable(listener.Listener()) {
			return fmt.Errorf("代理 %s 的入口不可用", name)
		}
	}
	return nil
}

// publishGeneration 原子切换 active 并返回被替换的旧代。
//
// 切换与"引擎是否已停"的判断在同一把锁内完成：Shutdown 与 publish 并发时，
// 若 publish 发生在 Shutdown 快照之后，新代会无人 stop 也无人等待而泄漏。
// 因此 publish 时复查 state，已停则释放新代并返回 ErrStopped。
func (engine *Engine) publishGeneration(gen *generation) (*generation, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.state == stateStopped {
		return nil, ErrStopped
	}
	old := engine.active
	engine.active = gen
	engine.lastGood = gen.revision
	return old, nil
}

// countEntries 统计一代拥有的入口数量，供 Apply 结果报告变更规模。
//
// 扣除被接管的入口：它们没有变化，不属于「本次新增或变化的资源」。
func countEntries(gen *generation) int {
	total := len(gen.guestLns) + len(gen.udpEntries)
	return total - len(gen.reused)
}

// release 释放 prepare 阶段新建、但 publish 之前失败的资源。
//
// Engine 自行创建的入口归 Core 所有，必须释放。从上一代接管的入口（reused）
// 同样不释放：所有权要到 publish 成功才转移，此刻它们仍归上一代，关闭会把正在
// 服务的入口一并关掉。
func (gen *generation) release() {
	for name, guestListener := range gen.guestLns {
		if gen.reused[name] {
			continue
		}
		_ = guestListener.Release()
	}
	for name, entry := range gen.udpEntries {
		if gen.reused[name] {
			continue
		}
		_ = entry.Close()
	}
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
