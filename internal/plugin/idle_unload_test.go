package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// idleTestPlugin installs one plugin (srcDir 可传独立源以产生不同稳定 id) and
// 返回运行实例。关闭 AutoRestart 避免崩溃重启换实例引入的测试竞态。
func idleTestPlugin(t *testing.T, m *SubprocessManager, srcDir, id string) *PluginInstance {
	t.Helper()
	m.AutoRestart = false
	inst, err := m.InstallFromSource(context.Background(), id, srcDir, InstallOptions{GoChoice: "download"})
	if err != nil {
		t.Fatalf("InstallFromSource(%s): %v", id, err)
	}
	return inst
}

// makePluginSource 复制 testdata/plugin 并改写 metadata.name，产生独立的
// 稳定插件 id（用于"插件 A / 插件 B"双实例场景）。
func makePluginSource(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join("testdata", "plugin")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", src, err)
	}
	dir := t.TempDir()
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", e.Name(), err)
		}
		if e.Name() == "metadata.json" {
			var meta map[string]interface{}
			if err := json.Unmarshal(data, &meta); err != nil {
				t.Fatalf("metadata 解析: %v", err)
			}
			meta["name"] = name
			data, _ = json.Marshal(meta)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", e.Name(), err)
		}
	}
	return dir
}

// idleNow 把当前运行实例的最近活动时间拨到 idleAgo 之前（对当前实例操作，
// 消除"旧实例指针被换"的竞态），并返回该实例。
func idleNow(t *testing.T, m *SubprocessManager, id string, idleAgo time.Duration) *PluginInstance {
	t.Helper()
	cur := m.Get(id)
	if cur == nil {
		t.Fatalf("plugin %s 未在运行，无法拨时间", id)
	}
	cur.lastActiveNano.Store(time.Now().Add(-idleAgo).UnixNano())
	return cur
}

// TestIdleDefaultResidentNeverSwept: 新装插件默认 IdleUnload=false，即便全局
// 已启用闲置清扫、且插件闲置很久，也绝不能因 idle 被回收。
func TestIdleDefaultResidentNeverSwept(t *testing.T) {
	requirePlugin(t)
	m := newTestManager(t)
	m.SetIdleUnload(10 * time.Millisecond) // 全局默认阈值开启

	p := idleTestPlugin(t, m, filepath.Join("testdata", "plugin"), "resident_default")
	if m.PluginIdleUnload(p.ID) {
		t.Fatal("fresh install must default to IdleUnload=false (resident)")
	}
	idleNow(t, m, p.ID, time.Hour)
	m.SweepIdle()
	if m.Get(p.ID) == nil {
		t.Fatal("default-resident plugin must never be idle-swept")
	}
}

// TestIdlePerPluginTimeout: 插件 A 阈值 1 分钟、B 阈值 10 分钟，两者都闲置 5
// 分钟 → A 应被回收、B 必须继续运行。全局默认关闭时各插件独立分钟仍生效。
func TestIdlePerPluginTimeout(t *testing.T) {
	requirePlugin(t)
	m := newTestManager(t)
	m.SetIdleUnload(0) // 全局默认关闭：A/B 只靠自己的分钟配置休眠

	a := idleTestPlugin(t, m, makePluginSource(t, "testplugin"), "pA")
	b := idleTestPlugin(t, m, makePluginSource(t, "testpluginb"), "pB")
	if err := m.SetPluginIdleUnload(a.ID, true); err != nil {
		t.Fatalf("SetPluginIdleUnload(A): %v", err)
	}
	if err := m.SetPluginIdleUnloadMinutes(a.ID, 1); err != nil {
		t.Fatalf("SetPluginIdleUnloadMinutes(A): %v", err)
	}
	if err := m.SetPluginIdleUnload(b.ID, true); err != nil {
		t.Fatalf("SetPluginIdleUnload(B): %v", err)
	}
	if err := m.SetPluginIdleUnloadMinutes(b.ID, 10); err != nil {
		t.Fatalf("SetPluginIdleUnloadMinutes(B): %v", err)
	}
	idleNow(t, m, a.ID, 5*time.Minute)
	idleNow(t, m, b.ID, 5*time.Minute)

	m.SweepIdle()
	if m.Get(a.ID) != nil {
		t.Fatal("插件 A（阈值 1min，闲置 5min）应被回收")
	}
	if m.Get(b.ID) == nil {
		t.Fatal("插件 B（阈值 10min，闲置 5min）必须继续运行")
	}
}

// TestIdleMinutesZeroFallsBackToGlobal: 插件未设独立分钟（0）时回退全局默认；
// 全局默认 0 时不允许回收（避免立即反复休眠/唤醒）。
func TestIdleMinutesZeroFallsBackToGlobal(t *testing.T) {
	requirePlugin(t)
	m := newTestManager(t)
	p := idleTestPlugin(t, m, filepath.Join("testdata", "plugin"), "pzero")
	if err := m.SetPluginIdleUnload(p.ID, true); err != nil {
		t.Fatalf("SetPluginIdleUnload: %v", err)
	}

	// 全局默认 0 → 无有效超时 → 不回收。
	idleNow(t, m, p.ID, time.Hour)
	m.SweepIdle()
	if m.Get(p.ID) == nil {
		t.Fatal("minutes=0 且全局默认 0 时不应回收")
	}
	// 全局默认开启 → minutes=0 回退全局。
	m.SetIdleUnload(10 * time.Millisecond)
	idleNow(t, m, p.ID, time.Hour)
	m.SweepIdle()
	if m.Get(p.ID) != nil {
		t.Fatal("minutes=0 应回退全局默认并被回收")
	}
}

// TestIdleTimerResetOnRPC: 有 RPC 活动（Touch）会重置闲置计时，连续活动期间
// 不得被误回收。
func TestIdleTimerResetOnRPC(t *testing.T) {
	requirePlugin(t)
	m := newTestManager(t)
	p := idleTestPlugin(t, m, filepath.Join("testdata", "plugin"), "p_reset")
	if err := m.SetPluginIdleUnload(p.ID, true); err != nil {
		t.Fatalf("SetPluginIdleUnload: %v", err)
	}
	if err := m.SetPluginIdleUnloadMinutes(p.ID, 1); err != nil {
		t.Fatalf("SetPluginIdleUnloadMinutes: %v", err)
	}
	idleNow(t, m, p.ID, 5*time.Minute)
	p.Touch() // 刚有 RPC 活动 → 计时重置
	m.SweepIdle()
	if m.Get(p.ID) == nil {
		t.Fatal("刚有 RPC 活动的插件不应被回收")
	}
}

// TestIdleInFlightRPCPreventsSweep: 进行中的 RPC（activeRPC>0）即使已闲置
// 也不得被回收，防止执行中的命令/工具被误杀。
func TestIdleInFlightRPCPreventsSweep(t *testing.T) {
	requirePlugin(t)
	m := newTestManager(t)
	p := idleTestPlugin(t, m, filepath.Join("testdata", "plugin"), "p_inflight")
	if err := m.SetPluginIdleUnload(p.ID, true); err != nil {
		t.Fatalf("SetPluginIdleUnload: %v", err)
	}
	if err := m.SetPluginIdleUnloadMinutes(p.ID, 1); err != nil {
		t.Fatalf("SetPluginIdleUnloadMinutes: %v", err)
	}
	release := p.RPCGuard() // 进行中 RPC
	idleNow(t, m, p.ID, 5*time.Minute)
	m.SweepIdle()
	if m.Get(p.ID) == nil {
		t.Fatal("进行中 RPC 的插件不应被清扫")
	}
	release()
	idleNow(t, m, p.ID, 5*time.Minute)
	m.SweepIdle()
	if m.Get(p.ID) != nil {
		t.Fatal("RPC 结束后闲置插件应可被回收")
	}
}

// TestIdleLazyLoadAfterSweep: 休眠（进程回收）后再次触发能 Lazy Load 拉起，
// 重新建立实例与 RPC，且不会双重启动。
func TestIdleLazyLoadAfterSweep(t *testing.T) {
	requirePlugin(t)
	m := newTestManager(t)
	p := idleTestPlugin(t, m, filepath.Join("testdata", "plugin"), "p_lazy")
	if err := m.SetPluginIdleUnload(p.ID, true); err != nil {
		t.Fatalf("SetPluginIdleUnload: %v", err)
	}
	if err := m.SetPluginIdleUnloadMinutes(p.ID, 1); err != nil {
		t.Fatalf("SetPluginIdleUnloadMinutes: %v", err)
	}
	idleNow(t, m, p.ID, 5*time.Minute)
	m.SweepIdle()
	if m.Get(p.ID) != nil {
		t.Fatal("闲置插件应先被回收")
	}

	inst, err := m.EnsureLoaded(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("EnsureLoaded: %v", err)
	}
	if inst == nil || inst.Client == nil {
		t.Fatal("Lazy Load 后实例/RPC 客户端必须可用")
	}
	if m.Get(p.ID) != inst {
		t.Fatal("Lazy Load 不得产生双重实例")
	}
}
