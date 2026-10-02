package plugin

import (
	"testing"
)

// TestPluginWantsSharedRuntime 验证运行方式分流与隔离迁移的判定逻辑（纯逻辑，
// 不启动子进程）：Runtime/RuntimePreferred 决定是否接入共享 Runtime；隔离
// （ISOLATION_PENDING/ISOLATED/isolation_required）与 python-isolated 一律
// 走独立进程；RECOVERY_PENDING 按偏好重新评估。
func TestPluginWantsSharedRuntime(t *testing.T) {
	m := newTestManager(t)
	man := &Manifest{Version: 1}
	man.Plugins = []ManifestEntry{
		{ID: "shared", Runtime: "python-shared"},
		{ID: "grpc", Runtime: "python-grpc"},
		{ID: "iso", Runtime: "python-isolated"},
		{ID: "pref", RuntimePreferred: "python-shared"},
		{ID: "pending", Runtime: "python-shared", HealthState: HealthIsolationPending, IsolationRequired: true},
		{ID: "isolated", Runtime: "python-shared", HealthState: HealthIsolated, IsolationRequired: true},
		{ID: "recover", RuntimePreferred: "python-shared", HealthState: HealthRecoveryPending},
	}
	if err := man.Save(m.manifestPath()); err != nil {
		t.Fatalf("save manifest: %v", err)
	}

	cases := map[string]bool{
		"shared":   true,
		"grpc":     false,
		"iso":      false,
		"pref":     true,
		"pending":  false, // 隔离中：走 python-grpc
		"isolated": false,
		"recover":  true, // 更新后按偏好重新尝试共享
	}
	for id, want := range cases {
		if got := m.pluginWantsSharedRuntime(id); got != want {
			t.Errorf("pluginWantsSharedRuntime(%s) = %v, want %v", id, got, want)
		}
	}
	// 未安装插件 → false
	if m.pluginWantsSharedRuntime("nope") {
		t.Error("unknown plugin should not want shared runtime")
	}
}

// TestSetPluginRuntimePreference 验证运行方式偏好的持久化与校验。
func TestSetPluginRuntimePreference(t *testing.T) {
	m := newTestManager(t)
	man := &Manifest{Version: 1, Plugins: []ManifestEntry{{ID: "p", Language: "python"}}}
	if err := man.Save(m.manifestPath()); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := m.SetPluginRuntimePreference("p", "python-shared"); err != nil {
		t.Fatalf("set shared: %v", err)
	}
	pref, cur := m.PluginRuntimeOf("p")
	if pref != "python-shared" || cur != "python-shared" {
		t.Errorf("runtime pref/cur = %q/%q, want python-shared/python-shared", pref, cur)
	}
	if err := m.SetPluginRuntimePreference("p", "bogus"); err == nil {
		t.Error("invalid runtime should error")
	}
	if err := m.SetPluginRuntimePreference("missing", "python-grpc"); err == nil {
		t.Error("unknown plugin should error")
	}
}

// TestIsolationStateMachine 验证隔离/恢复状态机的持久化流转。
func TestIsolationStateMachine(t *testing.T) {
	m := newTestManager(t)
	man := &Manifest{Version: 1, Plugins: []ManifestEntry{{ID: "p", Language: "python"}}}
	if err := man.Save(m.manifestPath()); err != nil {
		t.Fatalf("save: %v", err)
	}

	// UNHEALTHY 上报 → ISOLATION_PENDING（立即持久化）。
	if err := m.MarkIsolationPending("p", "plugin_unhealthy"); err != nil {
		t.Fatalf("mark pending: %v", err)
	}
	if state, reason, iso := m.PluginHealthStateOf("p"); state != HealthIsolationPending || !iso || reason != "plugin_unhealthy" {
		t.Fatalf("after pending: state=%q reason=%q iso=%v", state, reason, iso)
	}
	if !m.IsIsolationPending("p") {
		t.Error("IsIsolationPending should be true")
	}

	// 迁移完成 → ISOLATED。
	if err := m.SetPluginHealthState("p", HealthIsolated, "plugin_unhealthy"); err != nil {
		t.Fatalf("set isolated: %v", err)
	}
	if !m.IsIsolationPending("p") {
		t.Error("IsIsolationPending should stay true while ISOLATED")
	}

	// 恢复 → NORMAL（清隔离）。
	if err := m.ClearPluginIsolation("p"); err != nil {
		t.Fatalf("clear isolation: %v", err)
	}
	if state, _, iso := m.PluginHealthStateOf("p"); state != HealthNormal || iso {
		t.Fatalf("after clear: state=%q iso=%v", state, iso)
	}
	if m.IsIsolationPending("p") {
		t.Error("IsIsolationPending should be false after clear")
	}
}

// TestSetIdleTimeouts 验证全局休眠/卸载默认阈值的注入与回退。
func TestSetIdleTimeouts(t *testing.T) {
	m := newTestManager(t)
	// 默认回退内置常量。
	if got := m.defaultIdleUnloadMinutes(); got != DefaultIdleUnloadMinutes {
		t.Errorf("default = %d, want %d", got, DefaultIdleUnloadMinutes)
	}
	m.SetIdleTimeouts(1, 20)
	if got := m.defaultIdleUnloadMinutes(); got != 20 {
		t.Errorf("after set = %d, want 20", got)
	}
	// 负值归零 → 回退内置默认。
	m.SetIdleTimeouts(-5, -5)
	if got := m.defaultIdleUnloadMinutes(); got != DefaultIdleUnloadMinutes {
		t.Errorf("after negative = %d, want %d", got, DefaultIdleUnloadMinutes)
	}
}
