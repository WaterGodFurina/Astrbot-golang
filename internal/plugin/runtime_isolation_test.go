package plugin

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMarkCrashRecoveryPreservesIsolation 是「隔离在共享 Runtime 崩溃后仍保留」
// 的回归测试（方案第 7 节；回归测试 3 与 7）：
//   - A/B 中 A=ISOLATION_PENDING、C=ISOLATED 时，共享 Runtime 整体崩溃不得清除
//     它们的隔离要求与失败计数（崩溃只证明 Runtime 失败，不证明插件恢复健康）；
//   - B=NORMAL 进入 RECOVERY_PENDING，重启后重新尝试共享；
//   - 崩溃恢复不改变插件内容身份（SourceHash 保留，区别于「更新换源码」）。
func TestMarkCrashRecoveryPreservesIsolation(t *testing.T) {
	m := newTestManager(t)
	man := &Manifest{Version: 1, Plugins: []ManifestEntry{
		{ID: "pending", Language: "python", RuntimePreferred: "shared", HealthState: HealthIsolationPending, IsolationRequired: true, FailureCount: 2, HealthReason: "plugin_unhealthy"},
		{ID: "isolated", Language: "python", RuntimePreferred: "shared", HealthState: HealthIsolated, IsolationRequired: true, FailureCount: 3},
		{ID: "normal", Language: "python", RuntimePreferred: "shared", HealthState: HealthNormal, SourceHash: "abc"},
		{ID: "recovery", Language: "python", RuntimePreferred: "shared", HealthState: HealthRecoveryPending},
	}}
	if err := man.Save(m.manifestPath()); err != nil {
		t.Fatalf("save manifest: %v", err)
	}

	if err := m.markCrashRecovery([]string{"pending", "isolated", "normal", "recovery"}); err != nil {
		t.Fatalf("markCrashRecovery: %v", err)
	}

	// A（ISOLATION_PENDING）：隔离状态与失败计数原样保留。
	if state, reason, iso := m.PluginHealthStateOf("pending"); state != HealthIsolationPending || !iso || reason != "plugin_unhealthy" {
		t.Errorf("ISOLATION_PENDING 被崩溃清除: state=%q reason=%q iso=%v", state, reason, iso)
	}
	if e := m.cachedManifest().Get("pending"); e == nil || e.FailureCount != 2 {
		t.Errorf("pending FailureCount 被清零: %+v", e)
	}

	// C（ISOLATED）：保持隔离（即使当前不在共享 Runtime 中，也不能被误清）。
	if state, _, iso := m.PluginHealthStateOf("isolated"); state != HealthIsolated || !iso {
		t.Errorf("ISOLATED 被崩溃清除: state=%q iso=%v", state, iso)
	}

	// B（NORMAL）：进入 RECOVERY_PENDING，且 SourceHash 保留。
	if state, _, iso := m.PluginHealthStateOf("normal"); state != HealthRecoveryPending || iso {
		t.Errorf("NORMAL 未进入 RECOVERY_PENDING: state=%q iso=%v", state, iso)
	}
	if e := m.cachedManifest().Get("normal"); e == nil || e.SourceHash != "abc" {
		t.Errorf("崩溃恢复不应改变 SourceHash: %+v", e)
	}

	// RECOVERY_PENDING 的插件仍是 RECOVERY_PENDING（幂等）。
	if state, _, _ := m.PluginHealthStateOf("recovery"); state != HealthRecoveryPending {
		t.Errorf("recovery state=%q, want RECOVERY_PENDING", state)
	}
}

// TestContinuousRecoveryWindow 是「5 分钟连续 NORMAL 恢复窗口」的回归测试
// （方案第 7 节；回归测试 4）：窗口必须从连续 NORMAL 的起点计时，任何
// DEGRADED/UNHEALTHY 立即清零重新计时；不能用 StartedAt（启动即恢复）。
func TestContinuousRecoveryWindow(t *testing.T) {
	t0 := time.Now()

	// NORMAL 首次开始计时。
	if s := updateNormalSince(time.Time{}, HealthNormal, t0); !s.Equal(t0) {
		t.Fatalf("NORMAL 起点 = %v, want %v", s, t0)
	}
	// 持续 NORMAL：起点保持不变（连续计时）。
	if s := updateNormalSince(t0, HealthNormal, t0.Add(time.Minute)); !s.Equal(t0) {
		t.Errorf("持续 NORMAL 应保持起点 %v, got %v", t0, s)
	}
	// DEGRADED / UNHEALTHY：清零。
	if s := updateNormalSince(t0, HealthDegraded, t0.Add(time.Minute)); !s.IsZero() {
		t.Errorf("DEGRADED 应清零窗口, got %v", s)
	}
	if s := updateNormalSince(t0, HealthUnhealthy, t0.Add(time.Minute)); !s.IsZero() {
		t.Errorf("UNHEALTHY 应清零窗口, got %v", s)
	}

	// 窗口时长判定。
	if recoveryWindowElapsed(time.Time{}, t0) {
		t.Error("零值 normalSince 不应视为已满窗口")
	}
	if recoveryWindowElapsed(t0, t0.Add(4*time.Minute)) {
		t.Error("4 分钟 < 5 分钟不应恢复")
	}
	if !recoveryWindowElapsed(t0, t0.Add(isolationRecoveryWindow)) {
		t.Error("满 5 分钟应恢复")
	}

	// 回归核心：NORMAL t0 → UNHEALTHY t0+4m（清零）→ NORMAL t0+4m（重计）。
	// 在 t0+8m 时只有 4 分钟连续健康，不应恢复；旧实现用 StartedAt 会误判为
	// 已满 8 分钟而错误迁回 shared。
	start := updateNormalSince(time.Time{}, HealthNormal, t0)
	cleared := updateNormalSince(start, HealthUnhealthy, t0.Add(4*time.Minute))
	restarted := updateNormalSince(cleared, HealthNormal, t0.Add(4*time.Minute))
	if recoveryWindowElapsed(restarted, t0.Add(8*time.Minute)) {
		t.Error("中断后重计未满 5 分钟，不应恢复（StartedAt 旧实现会误判）")
	}
	if !recoveryWindowElapsed(restarted, t0.Add(4*time.Minute+isolationRecoveryWindow)) {
		t.Error("重计满 5 分钟应恢复")
	}
}

// TestParseRequirementsAndConflict 验证 requirements.txt 解析与依赖冲突检测
// （方案第 9 节；回归测试 8 的服务端基础）：
//   - 规范化包名（PEP 503）、去注释/环境标记、识别扩展与版本约束；
//   - 同一包非空且不同的约束 → 冲突；一致/未固定/不相交 → 不冲突。
func TestParseRequirementsAndConflict(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "requirements.txt")
	content := "# comment\n" +
		"Foo_Bar==1.0\n" +
		"baz>=1.0, <2.0   # inline comment\n" +
		"qux[extra]==3.0\n" +
		"marker==1.0 ; python_version < \"3.9\"\n" +
		"unpinned\n" +
		"-r other.txt\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	reqs := parseRequirements(p)
	if reqs["foo-bar"] != "==1.0" {
		t.Errorf("Foo_Bar → foo-bar==1.0, got %q", reqs["foo-bar"])
	}
	if reqs["baz"] != ">=1.0, <2.0" {
		t.Errorf("baz 约束 = %q", reqs["baz"])
	}
	if reqs["qux"] != "==3.0" {
		t.Errorf("qux 约束 = %q", reqs["qux"])
	}
	if reqs["marker"] != "==1.0" {
		t.Errorf("marker 约束（应去环境标记）= %q", reqs["marker"])
	}
	if spec, ok := reqs["unpinned"]; !ok || spec != "" {
		t.Errorf("unpinned 应存在且约束为空, got %q ok=%v", spec, ok)
	}
	if _, ok := reqs["-r"]; ok {
		t.Error("-r 选项行不应被解析为包")
	}

	// 冲突：同一包非空且不同。
	a := map[string]string{"foo": "==1.0", "bar": ""}
	if pkg, sa, sb, ok := requirementsConflict(a, map[string]string{"foo": "==2.0"}); !ok || pkg != "foo" || sa != "==1.0" || sb != "==2.0" {
		t.Errorf("应检测 foo 冲突, got pkg=%q a=%q b=%q ok=%v", pkg, sa, sb, ok)
	}
	if _, _, _, ok := requirementsConflict(a, map[string]string{"foo": "==1.0"}); ok {
		t.Error("相同约束不应冲突")
	}
	if _, _, _, ok := requirementsConflict(a, map[string]string{"foo": "", "baz": "==9"}); ok {
		t.Error("任一方未固定不应冲突")
	}
	if _, _, _, ok := requirementsConflict(a, map[string]string{"other": "==1"}); ok {
		t.Error("不相交包不应冲突")
	}
}

// TestRequirementsNormalizationBoundary 验证依赖约束比较的边界语义：
//   - 包名按 PEP503 归一（大小写/下划线/点 → 连字符）后比较；
//   - 版本约束去空白后比较（"== 1.0" == "==1.0"）；
//   - 一致约束 / 任一方未固定 → 不冲突（保守：只有两边都非空且不同才冲突）。
func TestRequirementsNormalizationBoundary(t *testing.T) {
	// 包名归一：Foo_Bar / foo-bar 视为同一包（经 parseRequirements 归一）。
	dir := t.TempDir()
	writeReq := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	reqsA := parseRequirements(writeReq("a.txt", "Foo_Bar==1.0\n"))
	reqsB := parseRequirements(writeReq("b.txt", "foo-bar==2.0\n"))
	if pkg, _, _, ok := requirementsConflict(reqsA, reqsB); !ok || pkg != "foo-bar" {
		t.Errorf("包名应归一后检测冲突, got pkg=%q ok=%v", pkg, ok)
	}
	// 约束去空白相等 → 不冲突。
	if _, _, _, ok := requirementsConflict(
		map[string]string{"foo": "== 1.0"},
		map[string]string{"foo": "==1.0"},
	); ok {
		t.Error("去空白后相等的约束不应冲突")
	}
	// 范围约束相同 → 不冲突。
	if _, _, _, ok := requirementsConflict(
		map[string]string{"foo": ">=1.0, <2"},
		map[string]string{"foo": ">=1.0,<2"},
	); ok {
		t.Error("相同范围约束不应冲突")
	}
	// 一方固定 / 一方范围且不同 → 冲突（保守策略）。
	if _, _, _, ok := requirementsConflict(
		map[string]string{"foo": "==1.0"},
		map[string]string{"foo": ">=2.0"},
	); !ok {
		t.Error("不同约束应冲突")
	}
	// 完全不相交的包集合 → 不冲突。
	if _, _, _, ok := requirementsConflict(
		map[string]string{"foo": "==1.0"},
		map[string]string{"bar": "==1.0"},
	); ok {
		t.Error("不相交包不应冲突")
	}
}

// TestSharedDepsConflictTracking 验证共享 Runtime 成员依赖跟踪：新插件与已加入
// 成员冲突时被识别；自身不参与冲突判定；成员卸载后冲突解除。
func TestSharedDepsConflictTracking(t *testing.T) {
	pm := &PythonRuntimeManager{pluginReqs: map[string]map[string]string{}}
	pm.trackSharedReqs("A", map[string]string{"foo": "==1.0"})

	if _, pkg, _, _, ok := pm.conflictFor("B", map[string]string{"foo": "==2.0"}); !ok || pkg != "foo" {
		t.Errorf("B 应与 A 在 foo 上冲突, got pkg=%q ok=%v", pkg, ok)
	}
	if _, _, _, _, ok := pm.conflictFor("A", map[string]string{"foo": "==1.0"}); ok {
		t.Error("自身不应与自身冲突")
	}
	if _, _, _, _, ok := pm.conflictFor("C", map[string]string{"bar": "==1.0"}); ok {
		t.Error("不相交依赖不应冲突")
	}

	// A 卸载后冲突解除。
	pm.mu.Lock()
	delete(pm.pluginReqs, "A")
	pm.mu.Unlock()
	if _, _, _, _, ok := pm.conflictFor("B", map[string]string{"foo": "==2.0"}); ok {
		t.Error("成员卸载后不应再冲突")
	}
}
