package core

import (
	"context"
	"sync"
)

// ActiveEventRegistry 维护 unified_msg_origin 到活跃事件的映射，用于 /stop、
// /reset、/new 等场景终止该会话正在处理的事件。
// 对齐 Python astrbot/core/utils/active_event_registry.py。
type ActiveEventRegistry struct {
	mu sync.Mutex
	// events: umo -> 活跃事件集合。
	events map[string]map[*Event]struct{}
	// stopCallbacks: 事件 -> 取消该事件 Agent 执行的回调（通常是派生 context 的
	// CancelFunc）。RequestAgentStopAll 会调用它立即中断 LLM/工具循环。
	stopCallbacks map[*Event]context.CancelFunc
}

// activeEventRegistry 是进程级单例（对齐 Python 模块级 active_event_registry）。
var activeEventRegistry = &ActiveEventRegistry{
	events:        make(map[string]map[*Event]struct{}),
	stopCallbacks: make(map[*Event]context.CancelFunc),
}

// ActiveEventRegistry 返回全局活跃事件注册表。
func ActiveRegistry() *ActiveEventRegistry { return activeEventRegistry }

// Register 登记一个开始处理的事件。
func (r *ActiveEventRegistry) Register(event *Event) {
	if event == nil {
		return
	}
	umo := event.UnifiedMsgOrigin()
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.events[umo]
	if set == nil {
		set = make(map[*Event]struct{})
		r.events[umo] = set
	}
	set[event] = struct{}{}
}

// Unregister 注销一个处理结束的事件。
func (r *ActiveEventRegistry) Unregister(event *Event) {
	if event == nil {
		return
	}
	umo := event.UnifiedMsgOrigin()
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.stopCallbacks, event)
	if set := r.events[umo]; set != nil {
		delete(set, event)
		if len(set) == 0 {
			delete(r.events, umo)
		}
	}
}

// RegisterStopCallback 为活跃事件登记 Agent 取消回调。
func (r *ActiveEventRegistry) RegisterStopCallback(event *Event, cancel context.CancelFunc) {
	if event == nil || cancel == nil {
		return
	}
	r.mu.Lock()
	r.stopCallbacks[event] = cancel
	r.mu.Unlock()
}

// StopAll 终止指定 UMO 的所有活跃事件（调用 Event.Stop，后续阶段不再执行），
// 返回被终止的事件数量。exclude 通常传入发起指令的事件本身。
// 对齐 Python stop_all。
func (r *ActiveEventRegistry) StopAll(umo string, exclude *Event) int {
	r.mu.Lock()
	events := make([]*Event, 0, len(r.events[umo]))
	for e := range r.events[umo] {
		if e != exclude {
			events = append(events, e)
		}
	}
	r.mu.Unlock()
	for _, e := range events {
		e.Stop()
	}
	return len(events)
}

// RequestAgentStopAll 请求停止指定 UMO 的所有活跃事件的 Agent 运行：置
// agent_stop_requested 标记并触发取消回调，但不调用 Event.Stop，因此不会中断
// 事件传播，历史保存等后续流程仍可继续。返回受影响的事件数量。
// 对齐 Python request_agent_stop_all。
func (r *ActiveEventRegistry) RequestAgentStopAll(umo string, exclude *Event) int {
	r.mu.Lock()
	events := make([]*Event, 0, len(r.events[umo]))
	cancels := make([]context.CancelFunc, 0, len(r.events[umo]))
	for e := range r.events[umo] {
		if e == exclude {
			continue
		}
		events = append(events, e)
		if c := r.stopCallbacks[e]; c != nil {
			cancels = append(cancels, c)
		}
	}
	r.mu.Unlock()
	for _, e := range events {
		e.SetExtra("agent_stop_requested", true)
	}
	for _, c := range cancels {
		c()
	}
	return len(events)
}
