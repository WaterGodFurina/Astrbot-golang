package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/v2"
)

// TestPluginWantsSharedRuntime 验证运行方式分流与隔离迁移的判定逻辑（纯逻辑，
// 不启动子进程）：Runtime/RuntimePreferred 决定是否接入共享 Runtime；隔离
// （ISOLATION_PENDING/ISOLATED/isolation_required）与首选 grpc（独立进程）
// 一律走独立进程；RECOVERY_PENDING 按首选（shared）重新评估。
func TestPluginWantsSharedRuntime(t *testing.T) {
	m := newTestManager(t)
	man := &Manifest{Version: 1}
	man.Plugins = []ManifestEntry{
		{ID: "shared", Language: "python", RuntimePreferred: "shared"},
		{ID: "grpc", Language: "python", RuntimePreferred: "grpc"},
		{ID: "legacy_iso", Language: "python", RuntimePreferred: "python-isolated"},
		{ID: "pref_empty", Language: "python", RuntimePreferred: ""},
		{ID: "pending", Language: "python", RuntimePreferred: "shared", HealthState: HealthIsolationPending, IsolationRequired: true},
		{ID: "isolated", Language: "python", RuntimePreferred: "shared", HealthState: HealthIsolated, IsolationRequired: true},
		{ID: "recover", Language: "python", RuntimePreferred: "shared", HealthState: HealthRecoveryPending},
	}
	if err := man.Save(m.manifestPath()); err != nil {
		t.Fatalf("save manifest: %v", err)
	}

	cases := map[string]bool{
		"shared":     true,
		"grpc":       false,
		"legacy_iso": false,
		"pref_empty": true,  // 空 → 默认 shared
		"pending":    false, // 隔离中：走 python-grpc
		"isolated":   false,
		"recover":    true, // 更新后按首选（shared）重新尝试共享
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

// TestSetPluginRuntimePreference 验证运行方式偏好的持久化与校验（preferred 取值
// 只有 shared/grpc；isolated 是派生状态，不作为用户选项）。
func TestSetPluginRuntimePreference(t *testing.T) {
	m := newTestManager(t)
	man := &Manifest{Version: 1, Plugins: []ManifestEntry{{ID: "p", Language: "python"}}}
	if err := man.Save(m.manifestPath()); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := m.SetPluginRuntimePreference("p", "shared"); err != nil {
		t.Fatalf("set shared: %v", err)
	}
	pref, cur := m.PluginRuntimeOf("p")
	if pref != "shared" || cur != "python-shared" {
		t.Errorf("runtime pref/cur = %q/%q, want shared/python-shared", pref, cur)
	}
	if err := m.SetPluginRuntimePreference("p", "grpc"); err != nil {
		t.Fatalf("set grpc: %v", err)
	}
	pref, cur = m.PluginRuntimeOf("p")
	if pref != "grpc" || cur != "python-grpc" {
		t.Errorf("runtime pref/cur = %q/%q, want grpc/python-grpc", pref, cur)
	}
	if err := m.SetPluginRuntimePreference("p", "bogus"); err == nil {
		t.Error("invalid runtime should error")
	}
	if err := m.SetPluginRuntimePreference("missing", "grpc"); err == nil {
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

// TestContentFingerprint 验证内容指纹：同内容稳定、内容变更后改变、缓存目录
// 不影响指纹（避免 __pycache__ 造成误判）。
func TestContentFingerprint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "__pycache__"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "__pycache__", "main.cpython-312.pyc"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	h1 := contentFingerprint(dir)
	if h1 == "" {
		t.Fatal("fingerprint 为空")
	}
	// 改变 __pycache__ 内容不影响指纹
	if err := os.WriteFile(filepath.Join(dir, "__pycache__", "main.cpython-312.pyc"), []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if h2 := contentFingerprint(dir); h2 != h1 {
		t.Errorf("__pycache__ 变化不应改变指纹: %s != %s", h1, h2)
	}
	// 改变源码内容改变指纹
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print(2)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if h3 := contentFingerprint(dir); h3 == h1 {
		t.Error("源码变化应改变指纹")
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

// fakeSharedClient 是 pluginsdk.PluginClient 的最小桩：仅 ManagePlugin/Close
// 被恢复闭环触达（disposeRuntime 调 Close；UnloadPlugin 回滚调 ManagePlugin）。
type fakeSharedClient struct {
	mu         sync.Mutex
	closeCalls int
	unloads    []string
}

func (f *fakeSharedClient) ManagePlugin(ctx context.Context, req pluginsdk.ManagePluginRequest) (pluginsdk.ManagePluginResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Action == "unload" {
		f.unloads = append(f.unloads, req.PluginID)
	}
	return pluginsdk.ManagePluginResponse{OK: true}, nil
}

func (f *fakeSharedClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	return nil
}

func (f *fakeSharedClient) Register(ctx context.Context) (pluginsdk.PluginInfo, error) {
	return pluginsdk.PluginInfo{}, errors.New("not implemented")
}
func (f *fakeSharedClient) HandleCommand(ctx context.Context, name string, args []string, event *pluginsdk.Event) (pluginsdk.HandleCommandResult, error) {
	return pluginsdk.HandleCommandResult{}, errors.New("not implemented")
}
func (f *fakeSharedClient) HandleFilter(ctx context.Context, name string, event *pluginsdk.Event) (pluginsdk.HandleFilterResult, error) {
	return pluginsdk.HandleFilterResult{}, errors.New("not implemented")
}
func (f *fakeSharedClient) HandleHook(ctx context.Context, name string, event *pluginsdk.Event, chain []pluginsdk.Component) (pluginsdk.HandleHookResult, error) {
	return pluginsdk.HandleHookResult{}, errors.New("not implemented")
}
func (f *fakeSharedClient) HandleHookWithPayload(ctx context.Context, name string, event *pluginsdk.Event, chain []pluginsdk.Component, payload any) (pluginsdk.HandleHookResult, error) {
	return pluginsdk.HandleHookResult{}, errors.New("not implemented")
}
func (f *fakeSharedClient) HandleLLMRequest(ctx context.Context, name string, event *pluginsdk.Event, systemPrompt, userPrompt string) (pluginsdk.HandleLLMRequestResult, error) {
	return pluginsdk.HandleLLMRequestResult{}, errors.New("not implemented")
}
func (f *fakeSharedClient) ListTools(ctx context.Context) ([]pluginsdk.ToolDesc, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeSharedClient) ListWebApis(ctx context.Context) ([]pluginsdk.WebAPIDesc, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeSharedClient) GetConfigSchema(ctx context.Context) ([]byte, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeSharedClient) HandleTool(ctx context.Context, name string, args map[string]any, event *pluginsdk.Event) (pluginsdk.HandleToolResult, error) {
	return pluginsdk.HandleToolResult{}, errors.New("not implemented")
}
func (f *fakeSharedClient) HandleWebRequest(ctx context.Context, req pluginsdk.HandleWebRequest) (pluginsdk.HandleWebResponse, error) {
	return pluginsdk.HandleWebResponse{}, errors.New("not implemented")
}
func (f *fakeSharedClient) HealthCheck(ctx context.Context) (pluginsdk.HealthInfo, error) {
	return pluginsdk.HealthInfo{}, errors.New("not implemented")
}
func (f *fakeSharedClient) Cleanup(ctx context.Context) error {
	return errors.New("not implemented")
}
func (f *fakeSharedClient) SetLogLevel(ctx context.Context, level string) error {
	return errors.New("not implemented")
}
func (f *fakeSharedClient) FeedSessionWait(ctx context.Context, event *pluginsdk.Event) (bool, error) {
	return false, errors.New("not implemented")
}
func (f *fakeSharedClient) FeedCronJob(ctx context.Context, req pluginsdk.FeedCronJobRequest) (pluginsdk.FeedCronJobResponse, error) {
	return pluginsdk.FeedCronJobResponse{}, errors.New("not implemented")
}
func (f *fakeSharedClient) ForPlugin(pluginID string) pluginsdk.PluginClient { return f }
func (f *fakeSharedClient) PluginID() string                                 { return "" }

// TestRecoverSharedRuntimeReloadsPlugins 验证崩溃恢复闭环（方案第 6 节）：
// 模拟 Runtime 退出后调 handleSharedExit（与 watchSharedRuntime 检测路径一致），
// 异步恢复应：清理死 runtime 句柄/端口段、pm.pluginReqs 清空、gen 自增、
// 对崩溃前 rt.plugins 中的每个插件调用一次 Reload 回调；单个插件失败不
// 中断其余恢复。
func TestRecoverSharedRuntimeReloadsPlugins(t *testing.T) {
	m := newTestManager(t)
	pm := m.newPythonRuntimeManager()
	// 挂到管理器上：handleSharedExit 经 sharedRuntimeManagerIfAny 取恢复入口
	//（与生产 sharedManager() 懒初始化后的形态一致）。
	m.mu.Lock()
	m.sharedRTMgr = pm
	m.mu.Unlock()
	t.Cleanup(func() {
		m.mu.Lock()
		m.sharedRTMgr = nil
		m.mu.Unlock()
	})

	// 构造崩溃前的共享 Runtime：两个插件挂载、一段已分配端口、注入依赖跟踪。
	fake := &fakeSharedClient{}
	minp, _ := allocPluginPort(30000, 40000)
	if minp == 0 {
		t.Fatal("alloc port for test")
	}
	rt := &sharedRuntime{
		mgr:     m,
		client:  fake,
		pid:     424242,
		minp:    minp,
		abs:     t.TempDir(),
		plugins: map[string]bool{"alpha": true, "beta": true},
	}
	pm.mu.Lock()
	pm.rt = rt
	pm.pluginReqs = map[string]map[string]string{"alpha": {"foo": "==1.0"}}
	beforeGen := pm.gen
	pm.mu.Unlock()

	// Reload 回调桩：alpha 失败（验证失败不中断其余恢复），beta 成功；记录调用。
	// 注意恢复语义：单插件 Reload 失败仅告警跳过，不做同步重试（失败状态由
	// markFailed 记录，避免故障插件在恢复循环里反复重载）。
	var reloadMu sync.Mutex
	var reloaded []string
	pm.reloadFunc = func(ctx context.Context, id string) error {
		reloadMu.Lock()
		defer reloadMu.Unlock()
		reloaded = append(reloaded, id)
		if id == "alpha" {
			return errors.New("boom")
		}
		return nil
	}

	// 模拟 watch goroutine 检测到进程退出。
	m.handleSharedExit(rt)

	// 等待异步恢复完成：每个插件恰好调用一次（alpha 失败也只调一次）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		reloadMu.Lock()
		done := len(reloaded) >= 2
		reloadMu.Unlock()
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // 留出多余调用暴露窗口
	reloadMu.Lock()
	calls := append([]string(nil), reloaded...)
	reloadMu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("Reload 回调调用次数 = %d (%v), want 2（每插件一次，失败不重试）", len(calls), calls)
	}

	// 死 runtime 已清理：client 关闭、端口段归还、gen 自增、依赖跟踪清空。
	if got := fake.closeCalls; got < 1 {
		t.Errorf("disposeRuntime 应关闭死 client, closeCalls = %d", got)
	}
	releasePluginPort(minp) // 测试自身归还，避免污染全局分配器
	_ = minp
	pm.mu.Lock()
	genAfter := pm.gen
	rtCleared := pm.rt != rt
	reqsCleared := pm.pluginReqs == nil
	pm.mu.Unlock()
	if genAfter != beforeGen+1 {
		t.Errorf("恢复应 gen++: before=%d after=%d", beforeGen, genAfter)
	}
	if !rtCleared {
		t.Error("恢复应把 pm.rt 指向的死 runtime 清掉")
	}
	if !reqsCleared {
		t.Error("恢复应清空 pm.pluginReqs")
	}

	// handleSharedExit 幂等：重复触发（同一 rt）不再产生新 Reload 调用。
	n := len(calls)
	m.handleSharedExit(rt)
	time.Sleep(100 * time.Millisecond)
	reloadMu.Lock()
	after := len(reloaded)
	reloadMu.Unlock()
	if after != n {
		t.Errorf("重复 handleSharedExit 不应重复恢复: %d -> %d", n, after)
	}
}

// TestRecoverSharedRuntimeSerializesRounds 验证可重入安全：恢复过程中 Runtime
// 再次崩溃触发第二轮恢复时，两轮串行执行且各自完成全部重载。
func TestRecoverSharedRuntimeSerializesRounds(t *testing.T) {
	m := newTestManager(t)
	pm := m.newPythonRuntimeManager()
	m.mu.Lock()
	m.sharedRTMgr = pm
	m.mu.Unlock()
	t.Cleanup(func() {
		m.mu.Lock()
		m.sharedRTMgr = nil
		m.mu.Unlock()
	})

	newRt := func() *sharedRuntime {
		return &sharedRuntime{
			mgr:     m,
			client:  &fakeSharedClient{},
			pid:     1,
			plugins: map[string]bool{"p1": true},
		}
	}
	rt1, rt2 := newRt(), newRt()

	var reloadMu sync.Mutex
	inFlight, maxInFlight, total := 0, 0, 0
	// reloadFunc 慢速执行，制造两轮恢复重叠窗口。
	pm.reloadFunc = func(ctx context.Context, id string) error {
		reloadMu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		reloadMu.Unlock()
		time.Sleep(30 * time.Millisecond)
		reloadMu.Lock()
		inFlight--
		total++
		reloadMu.Unlock()
		return nil
	}

	// 第一轮经 handleSharedExit 异步触发；第二轮经 handleSharedExit 触发会被
	// rt1.stopped 挡住吗？——不会：rt2 是新句柄。这里直接调 recoverSharedRuntime
	//（等价于恢复期间再次 handleSharedExit 的触发路径）。两轮在 recoveryMu 上
	// 串行：第二轮会等第一轮（含 30ms 慢速重载）完成才开始。
	m.handleSharedExit(rt1)
	pm.recoverSharedRuntime(rt2, []string{"p1"})

	// 等待第二轮（排队于 recoveryMu 之后）完成。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		reloadMu.Lock()
		done := total >= 2
		reloadMu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 两轮串行：重载并发度不超过 1，总量 = 2 轮 × 1 插件。
	reloadMu.Lock()
	defer reloadMu.Unlock()
	if maxInFlight > 1 {
		t.Errorf("两轮恢复应串行，重载最大并发 = %d", maxInFlight)
	}
	if total != 2 {
		t.Errorf("两轮恢复应各重载 1 个插件, total = %d", total)
	}
}

// TestUnloadPluginKeepsEntryOnRPCFailure 验证 UnloadPlugin 失败重试语义：
// ManagePlugin(unload) RPC 失败时 rt.plugins 条目保留（可重试）；成功后才删除。
func TestUnloadPluginKeepsEntryOnRPCFailure(t *testing.T) {
	m := newTestManager(t)
	pm := m.newPythonRuntimeManager()

	fail := true
	rt := &sharedRuntime{
		mgr:     m,
		client:  &failingUnloadClient{fail: &fail},
		pid:     1,
		plugins: map[string]bool{"p": true},
	}
	pm.mu.Lock()
	pm.rt = rt
	pm.pluginReqs = map[string]map[string]string{"p": {"foo": "==1.0"}}
	pm.mu.Unlock()

	// RPC 失败：条目保留。
	if err := pm.UnloadPlugin(context.Background(), "p"); err == nil {
		t.Fatal("RPC 失败应返回错误")
	}
	rt.mu.Lock()
	kept := rt.plugins["p"]
	rt.mu.Unlock()
	if !kept {
		t.Fatal("RPC 失败后 rt.plugins 条目应保留以便重试")
	}
	pm.mu.Lock()
	reqsKept := pm.pluginReqs["p"] != nil
	pm.mu.Unlock()
	if !reqsKept {
		t.Fatal("RPC 失败后依赖贡献应保留")
	}

	// RPC 成功：条目删除。
	fail = false
	if err := pm.UnloadPlugin(context.Background(), "p"); err != nil {
		t.Fatalf("retry unload: %v", err)
	}
	rt.mu.Lock()
	deleted := !rt.plugins["p"]
	rt.mu.Unlock()
	if !deleted {
		t.Fatal("RPC 成功后 rt.plugins 条目应删除")
	}
	pm.mu.Lock()
	_, reqsGone := pm.pluginReqs["p"]
	pm.mu.Unlock()
	if reqsGone {
		t.Fatal("RPC 成功后依赖贡献应删除")
	}
}

// failingUnloadClient 按 fail 标志决定 ManagePlugin(unload) 是否失败。
type failingUnloadClient struct {
	fakeSharedClient
	fail *bool
}

func (f *failingUnloadClient) ManagePlugin(ctx context.Context, req pluginsdk.ManagePluginRequest) (pluginsdk.ManagePluginResponse, error) {
	if *f.fail {
		return pluginsdk.ManagePluginResponse{}, fmt.Errorf("rpc down")
	}
	return pluginsdk.ManagePluginResponse{OK: true}, nil
}

// ForPlugin 必须覆写：继承自 fakeSharedClient 的实现会返回内嵌 fake（永远成功），
// 而非本桩——UnloadPlugin 经 ForPlugin(id) 取调用视图，失败注入才能生效。
func (f *failingUnloadClient) ForPlugin(pluginID string) pluginsdk.PluginClient { return f }

// TestEnsurePluginRegisterRollback 验证 Register 失败回滚：ManagePlugin(unload)
// 被尽力调用，插件条目不进 rt.plugins / pluginReqs。
func TestEnsurePluginRegisterRollback(t *testing.T) {
	m := newTestManager(t)
	pm := m.newPythonRuntimeManager()

	fake := &failingRegisterClient{fakeSharedClient: fakeSharedClient{}}
	rt := &sharedRuntime{mgr: m, client: fake, pid: 1, plugins: map[string]bool{}}
	pm.mu.Lock()
	pm.rt = rt
	pm.mu.Unlock()

	// 带入口的插件目录：ManagePlugin(load) 在真实共享进程中成功，
	// Register 由桩返回失败，触发回滚路径。
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("# test plugin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := pm.EnsurePlugin(context.Background(), "p", dir, "p", "1.0")
	if err == nil {
		t.Fatal("Register 失败应上抛错误")
	}
	if len(fake.unloads) != 1 || fake.unloads[0] != "p" {
		t.Errorf("Register 失败应尽力回滚 ManagePlugin(unload), unloads = %v", fake.unloads)
	}
	rt.mu.Lock()
	entered := rt.plugins["p"]
	rt.mu.Unlock()
	if entered {
		t.Error("Register 失败的插件不应进入 rt.plugins")
	}
	pm.mu.Lock()
	_, tracked := pm.pluginReqs["p"]
	pm.mu.Unlock()
	if tracked {
		t.Error("Register 失败的插件不应进入 pluginReqs")
	}
}

// failingRegisterClient 在 Register 时失败（模拟共享进程内注册报错）。
type failingRegisterClient struct {
	fakeSharedClient
}

func (f *failingRegisterClient) Register(ctx context.Context) (pluginsdk.PluginInfo, error) {
	return pluginsdk.PluginInfo{}, fmt.Errorf("register failed")
}
