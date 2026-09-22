package client

import (
	"github.com/wcpe/jrp/core"
)

// Subscribe 创建一条事件订阅并立即开始接收此后产生的事件（FR-27，规格 §3.2）。
//
// 不回放历史。多订阅者相互隔离：一个订阅者溢出或关闭不影响其他订阅者，也不影响
// 引擎与数据面。事件类型与语义由根包 core 定义（ADR-0012：事件能力并入根包）。
func (engine *Engine) Subscribe(options core.Options) *core.Subscription {
	return engine.events.Subscribe(options)
}

// State 返回当前只读状态快照的深复制值（FR-27，规格 §3.5）。
//
// 任何时刻可调用，与订阅无关。快照是某一时刻的一致性视图：生成在引擎锁内完成，
// 不执行网络 IO。
//
// 客户端视角的快照内容与服务端不同（规格 §3.5 允许）：代理摘要来自当前生效代的
// 配置声明；客户端摘要恒为空——客户端引擎只代表单一客户端，列表语义在服务端
// 视角才无歧义。
func (engine *Engine) State() core.State {
	engine.mu.Lock()
	defer engine.mu.Unlock()

	state := core.State{
		Running:     engine.state == stateRunning,
		LastGood:    engine.lastGood,
		Connections: engine.connTotal(),
	}
	if engine.active != nil {
		state.Active = engine.active.revision
		state.Proxies = engine.active.proxySummaries()
	}
	return state
}

// publishApply 向事件中枢发布一次 Apply 结果（成功与失败都发）。
func (engine *Engine) publishApply(result core.ApplyResult, err error) {
	engine.events.PublishApply(core.ApplyResultEvent{
		Revision:  result.Revision,
		Stage:     result.Stage,
		Err:       err,
		EventMeta: core.NewEventMeta(),
	})
}
