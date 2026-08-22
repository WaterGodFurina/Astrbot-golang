package plugin

import (
	"fmt"
	"sort"
	"sync/atomic"
	"time"
)

// 本文件实现跨进程会话等待（SessionWaiter）的宿主侧注册表：Python 插件的
// session_waiter 需要用户回复消息才能继续（如 listen_music 插件"1 选歌"）。
// 机制（对齐 Python AstrBot session_manager）：
//
//  1. 插件经 HostService.RegisterSessionWait RPC 通知宿主"等待 umo 的下一条
//     消息"，宿主记录并返回 wait_id（唯一标识，插件注销/查询时使用）。
//  2. 宿主管线最前端（SessionWaitStage）收到该 umo 的消息时，按注册表查询
//     目标插件，经 PluginService.FeedSessionWait RPC 推送事件；插件触发
//     SessionWaiter.trigger 处理，事件不再走正常管线。
//  3. 插件结束等待时经 UnregisterSessionWait 注销；超时或插件卸载时宿主
//     自动兜底清理，防止注册表无限增长。

// sessionWaitEntry 是一条跨进程会话等待记录。
type sessionWaitEntry struct {
	// pluginName 是等待所属插件的注册名（SDK 侧从连接身份注入）。
	pluginName string
	// umo 是等待监听的三段式会话标识（platform_id:MessageType:session_id）。
	umo string
}

// SessionWaitTarget 是 SessionWaitForUmo 的单条查询结果，供管线阶段消费。
type SessionWaitTarget struct {
	// WaitID 是宿主分配的等待标识（UnregisterSessionWait 时使用）。
	WaitID string
	// PluginName 是等待所属插件的注册名（定位运行实例用）。
	PluginName string
}

// maxSessionWaits 是会话等待注册表的上限：插件异常反复注册时防止注册表
// 无限增长（超时/卸载兜底清理只覆盖正常路径）。
const maxSessionWaits = 10000

// RegisterSessionWait 注册插件对 umo 的会话等待并返回 wait_id（subMgr 为
// nil 时返回空串 = 宿主不支持）。timeoutSeconds > 0 时超时自动注销，
// 插件结束等待后应主动 UnregisterSessionWait。
func (m *SubprocessManager) RegisterSessionWait(pluginName, umo string, timeoutSeconds int32) string {
	if m == nil {
		return ""
	}
	// waitID 格式 <插件名>-<序号>：日志/仪表盘可直读归属，序号保证唯一。
	n := sessionWaitSeq.Add(1)
	waitID := fmt.Sprintf("%s-%d", sanitizeID(pluginName), n)

	m.sessionWaitMu.Lock()
	if len(m.sessionWaitReg) >= maxSessionWaits {
		m.sessionWaitMu.Unlock()
		return ""
	}
	m.sessionWaitReg[waitID] = &sessionWaitEntry{pluginName: pluginName, umo: umo}
	m.sessionWaitMu.Unlock()

	if timeoutSeconds > 0 {
		// 超时自动注销：插件可能结束等待后忘了 UnregisterSessionWait，
		// 宿主兜底清理，避免注册表无限增长。
		time.AfterFunc(time.Duration(timeoutSeconds)*time.Second, func() {
			m.UnregisterSessionWait(waitID)
		})
	}
	return waitID
}

// UnregisterSessionWait 注销一条会话等待（插件主动调用或超时兜底）。
func (m *SubprocessManager) UnregisterSessionWait(waitID string) {
	if m == nil || waitID == "" {
		return
	}
	m.sessionWaitMu.Lock()
	delete(m.sessionWaitReg, waitID)
	m.sessionWaitMu.Unlock()
}

// SessionWaitForUmo 返回注册在指定 umo 上的全部会话等待（waitID + 插件名），
// 供管线 SessionWaitStage 逐条 FeedSessionWait 推送。按 waitID 排序保证
// 多条等待时推送顺序稳定可复现。无等待时返回 nil。
func (m *SubprocessManager) SessionWaitForUmo(umo string) []SessionWaitTarget {
	if m == nil || umo == "" {
		return nil
	}
	m.sessionWaitMu.Lock()
	defer m.sessionWaitMu.Unlock()
	var out []SessionWaitTarget
	for waitID, e := range m.sessionWaitReg {
		if e.umo == umo {
			out = append(out, SessionWaitTarget{WaitID: waitID, PluginName: e.pluginName})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WaitID < out[j].WaitID })
	return out
}

// unregisterPluginWaits 注销某插件的全部等待（插件卸载/休眠/失败时调用：
// 子进程已终止，Python 侧 SessionWaiter 状态一并丢失，宿主残留条目只会
// 反复推送失败，无保留意义）。
func (m *SubprocessManager) unregisterPluginWaits(pluginName string) {
	if m == nil || pluginName == "" {
		return
	}
	m.sessionWaitMu.Lock()
	for waitID, e := range m.sessionWaitReg {
		if e.pluginName == pluginName {
			delete(m.sessionWaitReg, waitID)
		}
	}
	m.sessionWaitMu.Unlock()
}

// InstanceByName returns the running instance for a plugin identifier (id or
// name), or nil。跨进程会话等待按插件注册名（RegisterSessionWait 注入的
// 名字）查询实例，须走 id/name 双匹配（实例表按 id = name_language 分键）。
func (m *SubprocessManager) InstanceByName(name string) *PluginInstance {
	return m.instanceByName(name)
}

// sessionWaitSeq 是会话等待 wait_id 的全局序号源（跨实例原子递增）。
var sessionWaitSeq atomic.Uint64
