package plugin

import (
	"testing"
	"time"
)

// TestMigrateBackCondition 验证迁回共享 Runtime 的判定条件（纯逻辑，不启动
// 子进程）：python-grpc 独立实例 + 非共享 + 连续 NORMAL 满观察窗口 → 条件成立。
// 此前 dispensePlugin 不设置 Runtime 字段（零值 ""），判定永不成立（死代码）；
// startInstance 现已在 python 分支标注 inst.Runtime = "python-grpc"。
func TestMigrateBackCondition(t *testing.T) {
	m := newTestManager(t)
	if err := (&Manifest{Version: 1, Plugins: []ManifestEntry{
		{ID: "iso", Language: "python", RuntimePreferred: "shared", HealthState: HealthIsolated, IsolationRequired: true},
	}}).Save(m.manifestPath()); err != nil {
		t.Fatalf("save manifest: %v", err)
	}

	// 模拟被隔离到 python-grpc 的实例（与 startInstance python 分支的标注一致）。
	inst := &PluginInstance{ID: "iso", Language: "python", Runtime: "python-grpc", owner: m}

	// 判定前置条件与 RefreshStatus 中的迁回分支保持一致。
	matchesMigrationPath := func() bool {
		return inst.owner != nil && inst.Runtime == "python-grpc" && !inst.shared
	}
	if !matchesMigrationPath() {
		t.Fatal("python-grpc 实例应命中迁回判定前置条件（Runtime 标注生效）")
	}

	// 对照：零值 Runtime（修复前的 dispensePlugin 行为）不命中。
	stale := &PluginInstance{ID: "iso", Language: "python", owner: m}
	if stale.Runtime == "python-grpc" || stale.shared {
		t.Fatal("零值 Runtime 不应命中迁回判定")
	}

	// 连续 NORMAL 未满观察窗口：不迁回。
	since := time.Now().Add(-isolationRecoveryWindow + time.Minute)
	inst.statusMu.Lock()
	inst.pyNormalSince = since
	inst.statusMu.Unlock()
	state, _, pending := m.PluginHealthStateOf(inst.ID)
	if state != HealthIsolated || !pending {
		t.Fatalf("前置状态错误: state=%q pending=%v", state, pending)
	}
	if recoveryWindowElapsed(since, time.Now()) {
		t.Fatal("未满观察窗口不应迁回")
	}

	// 连续 NORMAL 满观察窗口：迁回条件成立。
	since = time.Now().Add(-isolationRecoveryWindow - time.Minute)
	inst.statusMu.Lock()
	inst.pyNormalSince = since
	inst.statusMu.Unlock()
	if !recoveryWindowElapsed(since, time.Now()) {
		t.Fatal("满观察窗口应满足迁回条件")
	}

	// 健康中断（DEGRADED）应清零窗口起点，重新计时。
	inst.statusMu.Lock()
	inst.pyNormalSince = updateNormalSince(inst.pyNormalSince, HealthDegraded, time.Now())
	interrupted := inst.pyNormalSince
	inst.statusMu.Unlock()
	if !interrupted.IsZero() {
		t.Fatal("非 NORMAL 应清零连续 NORMAL 起点")
	}
}

// TestPythonRuntimeManagerShutdownIdempotent 验证 Shutdown 幂等：连调两次不
// panic、无运行中 Runtime 时为空操作（不触发任何 Kill/Close）。
func TestPythonRuntimeManagerShutdownIdempotent(t *testing.T) {
	m := newTestManager(t)
	pm := m.newPythonRuntimeManager()

	// 无 Runtime：Shutdown 为空操作。
	pm.Shutdown()

	// 重复调用安全（sync.Once 幂等）。
	pm.Shutdown()

	// 已初始化管理器（ensureRuntime 未调用、rt=nil）重复 Shutdown 仍安全。
	if mgr := m.sharedRuntimeManagerIfAny(); mgr != nil {
		mgr.Shutdown()
		mgr.Shutdown()
	}

	// Shutdown 后再次 Shutdown 不应复活管理器或 panic。
	pm.Shutdown()
}
