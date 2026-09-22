package server

import (
	"github.com/wcpe/jrp/core"
)

// Subscribe 创建一条事件订阅并立即开始接收此后产生的事件（FR-27，规格 §3.2）。
//
// 不回放历史。多订阅者相互隔离：一个订阅者溢出或关闭不影响其他订阅者，也不影响
// 引擎与数据面。事件类型与语义由根包 core 定义（ADR-0012：事件能力并入根包，
// 不新增公共导入路径）。
func (engine *Engine) Subscribe(options core.Options) *core.Subscription {
	return engine.events.Subscribe(options)
}

// State 返回当前只读状态快照的深复制值（FR-27，规格 §3.5）。
//
// 任何时刻可调用，与订阅无关。快照是某一时刻的一致性视图：生成在引擎锁内完成，
// 不执行网络 IO；宿主修改返回值不影响 Core 内部。
//
// 内容口径（规格 §3.5）：运行状态、active 与 last-good revision、客户端摘要
// （已登记控制连接的客户端标识）、代理摘要（当前生效代）、活动连接计数。
// 快照不出现 desired 字段：Core 不知道也不持久化 desired（ADR-0004）。
func (engine *Engine) State() core.State {
	engine.mu.Lock()
	defer engine.mu.Unlock()

	state := core.State{
		Running:  engine.state == stateRunning,
		LastGood: engine.lastGood,
	}
	if engine.active != nil {
		state.Active = engine.active.revision
		state.Proxies = engine.active.proxySummaries()
	}

	// 客户端摘要按已登记控制连接聚合：同一客户端标识的多条连接只记一次，
	// 代理数取该客户端在当前代的绑定数。
	state.Clients = engine.activeClientSummaries()
	state.Connections = len(engine.controlConns)
	return state
}

// publishApply 向事件中枢发布一次 Apply 结果（成功与失败都发）。
//
// 错误摘要即 Apply 返回的原错误：ApplyError 的文本由阶段名与下层原因组成，
// 不含 token、密码或正文原文（规格 §3.7 已约束下层错误）。
func (engine *Engine) publishApply(result core.ApplyResult, err error) {
	engine.events.PublishApply(core.ApplyResultEvent{
		Revision:  result.Revision,
		Stage:     result.Stage,
		Err:       err,
		EventMeta: core.NewEventMeta(),
	})
}
