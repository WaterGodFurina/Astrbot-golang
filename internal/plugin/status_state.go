package plugin

import (
	"fmt"
	"time"
)

// Python 插件故障/隔离状态机与持久化（板块 6，方案用户修订版）。
//
// 职责边界：**Python Watchdog 管插件级健康；Go Runtime Manager 管进程级**
// （隔离迁移、状态持久化、Runtime 重启恢复）。本文件只负责 Go 侧的
// **持久化与状态流转标记**——不真正执行共享 Runtime 迁移（那需要
// PythonRuntimeManager，属后续板块）。
//
// 状态机：
//
//	NORMAL → DEGRADED → UNHEALTHY → ISOLATION_PENDING → ISOLATED
//	                   ↗ 恢复 NORMAL / 卸载 REMOVED / 更新 RECOVERY_PENDING
//
// 关键原则（方案明确）：
//   - ISOLATED 是「当前插件实例的运行状态」，不是永久处罚；恢复/卸载/更新
//     后必须有明确的清理/重新评估机制。
//   - 更新（version/source_hash 变化）后不继承旧故障状态 → RECOVERY_PENDING。
//   - Runtime 崩溃不得凭猜测指定「罪魁祸首」；只有崩溃前已持久化
//     ISOLATION_PENDING 的插件才在重启后优先隔离。

const (
	// HealthNormal 正常运行。
	HealthNormal = "NORMAL"
	// HealthDegraded 出现一定异常但仍可继续运行。
	HealthDegraded = "DEGRADED"
	// HealthUnhealthy 已达到故障判定条件（Python Watchdog 上报）。
	HealthUnhealthy = "UNHEALTHY"
	// HealthIsolationPending 已确认需要脱离 Shared Runtime（Go 持久化）。
	HealthIsolationPending = "ISOLATION_PENDING"
	// HealthIsolated 已迁移到独立 python-grpc 进程。
	HealthIsolated = "ISOLATED"
	// HealthRecoveryPending 更新/恢复后待重新评估（观察窗口内）。
	HealthRecoveryPending = "RECOVERY_PENDING"
	// HealthRemoved 已卸载，从运行状态中删除。
	HealthRemoved = "REMOVED"
)

// SetPluginHealthState 持久化插件的健康状态与原因（板块 6）。
// Python 是状态的权威来源，Go 仅记录决策结果（mirror + 恢复依据）。
func (m *SubprocessManager) SetPluginHealthState(id, state, reason string) error {
	m.manifestMu.Lock()
	defer m.manifestMu.Unlock()
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		return err
	}
	e := man.Get(id)
	if e == nil {
		return fmt.Errorf("插件 %s 未安装", id)
	}
	e.HealthState = state
	e.HealthReason = reason
	if state == HealthUnhealthy || state == HealthIsolationPending {
		e.FailureCount++
		e.LastFailureTime = time.Now().Unix()
	}
	if state == HealthIsolationPending {
		e.IsolationRequired = true
	}
	return m.saveManifest(man)
}

// MarkIsolationPending 由 Go Runtime Manager 在收到 Python Watchdog 的
// UNHEALTHY 上报后**立即持久化** ISOLATION_PENDING（方案：不能等迁移完成
// 再写——否则 Runtime 随后崩溃会丢失该状态）。Runtime 重启后据此决定哪些
// 插件进入 Shared、哪些保持隔离。
func (m *SubprocessManager) MarkIsolationPending(id, reason string) error {
	return m.SetPluginHealthState(id, HealthIsolationPending, reason)
}

// ClearPluginIsolation 清除插件的隔离标记并恢复为 NORMAL（恢复检查通过 /
// 卸载 / 更新后调用）。ISOLATED 不是永久处罚：不保留「曾经异常」作为当前
// 运行依据。
func (m *SubprocessManager) ClearPluginIsolation(id string) error {
	m.manifestMu.Lock()
	defer m.manifestMu.Unlock()
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		return err
	}
	e := man.Get(id)
	if e == nil {
		return fmt.Errorf("插件 %s 未安装", id)
	}
	e.HealthState = HealthNormal
	e.HealthReason = ""
	e.IsolationRequired = false
	e.FailureCount = 0
	e.RecoveryGeneration++
	return m.saveManifest(man)
}

// MarkRecoveryPending 在插件更新（version/source_hash 变化）后调用：清除旧
// 版本的隔离标记并进入 RECOVERY_PENDING，下次加载先尝试 python-shared，
// 观察窗口内正常 → NORMAL，再次异常 → ISOLATED。避免「半年前出过一次问题
// 就永远单独跑」。
func (m *SubprocessManager) MarkRecoveryPending(id, sourceHash string) error {
	m.manifestMu.Lock()
	defer m.manifestMu.Unlock()
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		return err
	}
	e := man.Get(id)
	if e == nil {
		return fmt.Errorf("插件 %s 未安装", id)
	}
	e.HealthState = HealthRecoveryPending
	e.HealthReason = ""
	e.IsolationRequired = false
	e.FailureCount = 0
	e.SourceHash = sourceHash
	e.RecoveryGeneration++
	return m.saveManifest(man)
}

// markCrashRecovery 在共享 Runtime 整体崩溃后对崩溃时仍挂载的插件标记
// RECOVERY_PENDING（重启后重新尝试 shared）。shared process 崩溃只证明
// Runtime 失败，不证明任何插件有罪（方案第 7 节）——因此：
//
//   - **保留已有隔离要求的插件**（ISOLATION_PENDING / ISOLATED /
//     IsolationRequired）：不能因整体崩溃被清掉隔离决策，否则会丢失
//     "该插件曾故障、应隔离"的持久化结果。
//   - 仅对非隔离插件写 RECOVERY_PENDING（IsolationRequired=false、
//     FailureCount 归零、RecoveryGeneration++），并**保留 SourceHash**
//     （崩溃恢复与"更新换源码"不同，不改变插件内容身份）。
//
// 单次 Load→改→Save 原子完成（持 manifestMu），避免逐插件多次 Load/Save
// 及中间被并发修改的一致性窗口。
func (m *SubprocessManager) markCrashRecovery(ids []string) error {
	m.manifestMu.Lock()
	defer m.manifestMu.Unlock()
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		return err
	}
	changed := false
	for _, id := range ids {
		e := man.Get(id)
		if e == nil {
			continue
		}
		if e.HealthState == HealthIsolationPending || e.HealthState == HealthIsolated || e.IsolationRequired {
			continue
		}
		e.HealthState = HealthRecoveryPending
		e.HealthReason = ""
		e.IsolationRequired = false
		e.FailureCount = 0
		e.RecoveryGeneration++
		changed = true
	}
	if !changed {
		return nil
	}
	return m.saveManifest(man)
}

// ClearPluginRuntimeState 卸载/移除时清理该插件的运行状态与恢复标记。
func (m *SubprocessManager) ClearPluginRuntimeState(id string) error {
	m.manifestMu.Lock()
	defer m.manifestMu.Unlock()
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		return err
	}
	e := man.Get(id)
	if e == nil {
		return nil // 已不在 manifest：幂等
	}
	e.HealthState = ""
	e.HealthReason = ""
	e.IsolationRequired = false
	e.FailureCount = 0
	e.LastFailureTime = 0
	e.RecoveryGeneration = 0
	e.RuntimePreferred = ""
	e.RuntimeCurrent = ""
	return m.saveManifest(man)
}

// SetPluginRuntime 持久化插件的运行时偏好与当前运行方式（方案第 5 节）：
// preferred = 插件正常应运行的方式（如 python-shared）；current = 当前实际
// 运行方式（因故障被隔离后暂用 python-grpc）。二者分离，恢复后 current 归位。
func (m *SubprocessManager) SetPluginRuntime(id, preferred, current string) error {
	m.manifestMu.Lock()
	defer m.manifestMu.Unlock()
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		return err
	}
	e := man.Get(id)
	if e == nil {
		return fmt.Errorf("插件 %s 未安装", id)
	}
	e.RuntimePreferred = preferred
	e.RuntimeCurrent = current
	return m.saveManifest(man)
}

// PluginRuntimeOf 读取插件持久化的 (preferred, current) 运行方式。
func (m *SubprocessManager) PluginRuntimeOf(id string) (preferred, current string) {
	if e := m.cachedManifest().Get(id); e != nil {
		return e.RuntimePreferred, e.RuntimeCurrent
	}
	return "", ""
}

// PluginHealthStateOf 读取插件的持久化健康状态（供 ListInfo/前端/恢复决策）。
func (m *SubprocessManager) PluginHealthStateOf(id string) (state, reason string, isolationRequired bool) {
	if e := m.cachedManifest().Get(id); e != nil {
		return e.HealthState, e.HealthReason, e.IsolationRequired
	}
	return "", "", false
}

// IsIsolationPending 报告插件在最近一次持久化中是否被标记为待隔离
// （Runtime 重启恢复时使用：只恢复崩溃前已持久化 ISOLATION_PENDING 的插件）。
func (m *SubprocessManager) IsIsolationPending(id string) bool {
	if e := m.cachedManifest().Get(id); e != nil {
		return e.HealthState == HealthIsolationPending || e.HealthState == HealthIsolated
	}
	return false
}
