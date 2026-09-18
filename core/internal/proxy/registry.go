package proxy

import (
	"net/netip"
	"sync"
)

// Registry 是已注册代理绑定与其允许目标集合的只读快照。
//
// 注册校验的四级顺序（字段、权限、冲突、P1 范围）在配置层完成；本注册表只
// 承载已通过校验的绑定，并在运行期按代理名回答两个问题：目标地址是否被允许、
// 入口归属于哪个代理（FR-06a §3.2、§3.3）。
type Registry map[string]*Binding

// Binding 是一条已注册代理绑定的运行期形态。
type Binding struct {
	// Name 是代理名。
	Name string
	// Targets 是该代理允许转发到的目标地址集合。
	Targets []netip.AddrPort
}

// TargetAllowed 判定目标地址是否在允许集合内。
//
// 目标未解析出有效地址时一律不允许：不可解析的输入不得被当作放行依据。
func (binding *Binding) TargetAllowed(target netip.AddrPort) bool {
	if !target.IsValid() {
		return false
	}
	for _, allowed := range binding.Targets {
		if allowed == target {
			return true
		}
	}
	return false
}

// RegistryView 是代理注册表的并发安全持有者。
//
// 表整体替换而非逐条修改，对应 prepare/publish 的原子发布（ADR-0005）：
// 发布期间旧的查询仍命中旧表，不会观察到半更新的中间态。
type RegistryView struct {
	mu       sync.RWMutex
	registry Registry
}

// Publish 原子替换注册表快照。
func (view *RegistryView) Publish(registry Registry) {
	view.mu.Lock()
	defer view.mu.Unlock()
	view.registry = registry
}

// Binding 返回指定代理的绑定；未注册时返回 nil。
func (view *RegistryView) Binding(name string) *Binding {
	view.mu.RLock()
	defer view.mu.RUnlock()
	return view.registry[name]
}

// TargetAllowed 判定指定代理是否可以转发到给定目标地址。
//
// 未注册的代理一律不允许：代理层不得转发到未声明目标（规格 §3.3）。
func (view *RegistryView) TargetAllowed(name string, target netip.AddrPort) bool {
	binding := view.Binding(name)
	if binding == nil {
		return false
	}
	return binding.TargetAllowed(target)
}
