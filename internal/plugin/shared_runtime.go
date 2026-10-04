package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
	sdkv1 "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/gen/sdkv1"
	goplugin "github.com/hashicorp/go-plugin"
)

// PythonRuntimeManager 管理 python-shared 共享 Runtime 进程：单 CPython 进程
// 承载 N 个 Python 插件（方案第 2/3 节）。与 python-grpc（一插件一进程）共存：
// python-grpc 走 SubprocessManager 的既有 startInstance；python-shared 走本
// 管理器——启动/复用一个共享进程，按插件经 ManagePlugin 加载/卸载，RPC 带
// plugin_id 多租户路由。
//
// 职责边界（方案第 3 节）：Python 是插件健康状态权威；本管理器只负责
// **进程级**监督（连接、崩溃重启、隔离迁移的持久化标记）。
type PythonRuntimeManager struct {
	mgr *SubprocessManager

	mu sync.Mutex
	rt *sharedRuntime
	// gen 在每次 Runtime 重建（崩溃重启）时自增，供恢复流程区分新旧实例。
	gen uint64
}

// sharedRuntime 是单个共享 Runtime 进程的运行时句柄。
type sharedRuntime struct {
	mgr    *SubprocessManager
	raw    *goplugin.Client
	client pluginsdk.PluginClient
	cmd    *exec.Cmd
	pid    int
	minp   uint
	abs    string

	mu      sync.Mutex
	plugins map[string]bool
	stopped bool
}

// sharedRuntimeEnvPlugins 注入初始插件清单（JSON 数组），供共享 Runtime 启动
// 时预加载；动态增删走 ManagePlugin RPC。
const sharedRuntimeEnvPlugins = "ASTRBOT_SHARED_PLUGINS"

// newPythonRuntimeManager 创建共享 Runtime 管理器，并注入共享连接的 per-request
// 身份解析器（注册名 → manifest id，方案第 8 节身份绑定 per-request）。
func (m *SubprocessManager) newPythonRuntimeManager() *PythonRuntimeManager {
	pluginsdk.SetSharedPluginResolver(m.resolveSharedPluginID)
	return &PythonRuntimeManager{mgr: m}
}

// resolveSharedPluginID 把共享 Runtime 反调用携带的注册名严格解析为 manifest
// id（fail-closed：未知名字返回 ""）；供 SDK 的 per-request 身份鉴权/配置归属
// 使用。与 pluginConfigID 不同，零命中时**不**回退注册名。
func (m *SubprocessManager) resolveSharedPluginID(name string) string {
	if m == nil || name == "" {
		return ""
	}
	if inst := m.Get(name); inst != nil && inst.ID != "" {
		return inst.ID
	}
	for _, inst := range m.List() {
		if inst.Name == name && inst.ID != "" {
			return inst.ID
		}
	}
	if man := m.cachedManifest(); man != nil {
		for i := range man.Plugins {
			if man.Plugins[i].Name == name && man.Plugins[i].ID != "" {
				return man.Plugins[i].ID
			}
		}
	}
	return ""
}

// sharedManager 懒初始化并返回共享 Runtime 管理器。
func (m *SubprocessManager) sharedManager() *PythonRuntimeManager {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sharedRTMgr == nil {
		m.sharedRTMgr = m.newPythonRuntimeManager()
	}
	return m.sharedRTMgr
}

// startSharedProcess 启动共享 Runtime 进程并完成 go-plugin 握手，返回运行句柄。
func startSharedProcess(ctx context.Context, m *SubprocessManager) (*sharedRuntime, error) {
	env, err := m.pythonRuntime()
	if err != nil {
		return nil, fmt.Errorf("python runtime: %w", err)
	}
	// 共享 Runtime 数据根：与插件数据目录平级，避免各插件 cwd 冲突。
	dataRoot := filepath.Join(m.dataDir, "shared_runtime_data")
	if abs, err := filepath.Abs(dataRoot); err == nil {
		dataRoot = abs
	}
	// #nosec G301 -- 运行时数据目录（用户态）
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create shared runtime data dir: %w", err)
	}

	cmd := exec.Command(env.PythonBin, "-m", "astrbot._bridge.shared_runtime") // #nosec G204 -- 启动共享 Runtime（插件系统核心）; nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd.Dir = dataRoot
	cmd.Env = env.Env(dataRoot, m.dataDir)
	cmd.Env = append(cmd.Env, sharedRuntimeEnvPlugins+"=[]")
	if caps := m.hostCapabilitiesSnapshot(); len(caps) > 0 {
		cmd.Env = append(cmd.Env, "ASTRBOT_HOST_CAPABILITIES="+strings.Join(caps, ","))
	}

	// connKey 用共享连接哨兵：SDK 据此刻的 SetCurrentHostPluginID 把该连接的
	// 身份标为共享连接，改按请求 plugin_name 解析（pluginsdk.SetSharedPluginResolver）。
	raw, pc, pid, minp, err := m.dispenseHandshake(ctx, pluginsdk.SharedRuntimeConnKey, dataRoot, "python", cmd, newAstrbotStartupParser())
	if err != nil {
		return nil, err
	}
	rt := &sharedRuntime{
		mgr:     m,
		raw:     raw,
		client:  pc,
		cmd:     cmd,
		pid:     pid,
		minp:    minp,
		abs:     dataRoot,
		plugins: map[string]bool{},
	}
	go m.watchSharedRuntime(rt)
	logger.I18nInfo("共享 Python Runtime 已启动 (pid=%d)", pid)
	return rt, nil
}

// ensureRuntime 返回可用的共享 Runtime，必要时启动。
func (pm *PythonRuntimeManager) ensureRuntime(ctx context.Context) (*sharedRuntime, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.rt != nil {
		pm.rt.mu.Lock()
		stopped := pm.rt.stopped
		exited := pm.rt.raw == nil || pm.rt.raw.Exited()
		pm.rt.mu.Unlock()
		if !stopped && !exited {
			return pm.rt, nil
		}
		// 已崩溃：清理后重建（watchSharedRuntime 通常已处理，这里兜底）。
		pm.rt = nil
	}
	rt, err := startSharedProcess(ctx, pm.mgr)
	if err != nil {
		return nil, err
	}
	pm.rt = rt
	return rt, nil
}

// EnsurePlugin 在共享 Runtime 中加载/激活一个插件，返回 per-plugin 客户端视图
// 与 Register 元数据。宿主随后以此为 Client 调用各 PluginService RPC（自动携带
// plugin_id）。
func (pm *PythonRuntimeManager) EnsurePlugin(ctx context.Context, id, pluginDir, pluginName, version string) (pluginsdk.PluginClient, *sdkv1.RegisterResponse, error) {
	rt, err := pm.ensureRuntime(ctx)
	if err != nil {
		return nil, nil, err
	}
	// ManagePlugin(load)：登记 + import（等价单插件 server.py 阶段 A）。
	pc := rt.client.ForPlugin(id)
	loadCtx, cancel := context.WithTimeout(ctx, registerTimeout)
	defer cancel()
	resp, err := pc.ManagePlugin(loadCtx, &sdkv1.ManagePluginRequest{
		Action:     "load",
		PluginId:   id,
		PluginDir:  pluginDir,
		PluginName: pluginName,
		Version:    version,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("共享 Runtime 加载插件 %s: %w", id, err)
	}
	if !resp.GetOk() {
		return nil, nil, fmt.Errorf("共享 Runtime 加载插件 %s 失败: %s", id, resp.GetError())
	}
	// Register：触发该插件的注册表构建 + 实例化（MultiTenant.Register 会在
	// 返回后异步 instantiate）。
	regCtx, cancel2 := context.WithTimeout(ctx, registerTimeout)
	defer cancel2()
	meta, err := pc.Register(regCtx)
	if err != nil {
		return nil, nil, fmt.Errorf("共享 Runtime 注册插件 %s: %w", id, err)
	}
	rt.mu.Lock()
	rt.plugins[id] = true
	rt.mu.Unlock()
	return pc, meta, nil
}

// UnloadPlugin 从共享 Runtime 卸载单个插件（不杀 Runtime 进程）。
func (pm *PythonRuntimeManager) UnloadPlugin(ctx context.Context, id string) error {
	pm.mu.Lock()
	rt := pm.rt
	pm.mu.Unlock()
	if rt == nil {
		return nil
	}
	rt.mu.Lock()
	if !rt.plugins[id] {
		rt.mu.Unlock()
		return nil
	}
	delete(rt.plugins, id)
	rt.mu.Unlock()
	pc := rt.client.ForPlugin(id)
	ctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	_, err := pc.ManagePlugin(ctx, &sdkv1.ManagePluginRequest{Action: "unload", PluginId: id})
	if err != nil {
		return fmt.Errorf("共享 Runtime 卸载插件 %s: %w", id, err)
	}
	return nil
}

// watchSharedRuntime 监控共享 Runtime 进程退出（方案第 6 节：Runtime 整体崩溃
// 由 Go Runtime Manager 检测 → 重启 → 重载全部插件 → 重新注册）。崩溃不猜测
// 罪魁祸首（方案第 7 节）：重启后按持久化的 ISOLATION_PENDING 决定隔离。
func (m *SubprocessManager) watchSharedRuntime(rt *sharedRuntime) {
	ticker := time.NewTicker(m.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			rt.mu.Lock()
			stopped := rt.stopped
			rt.mu.Unlock()
			if stopped {
				return
			}
			if rt.raw != nil && rt.raw.Exited() {
				m.handleSharedExit(rt)
				return
			}
		}
	}
}

// handleSharedExit 处理共享 Runtime 崩溃：标记所有共享插件失败并触发恢复
// （重启 Runtime + 重载）。隔离迁移本身待 PythonRuntimeManager 完整版；此处
// 只做进程级恢复与状态标记（用户确认的第 1 层边界）。
func (m *SubprocessManager) handleSharedExit(rt *sharedRuntime) {
	rt.mu.Lock()
	if rt.stopped {
		rt.mu.Unlock()
		return
	}
	rt.stopped = true
	ids := make([]string, 0, len(rt.plugins))
	for id := range rt.plugins {
		ids = append(ids, id)
	}
	rt.mu.Unlock()

	logger.I18nWarn("共享 Python Runtime 进程退出，受影响插件: %v", ids)
	// 不猜测罪魁祸首（方案第 7 节）：仅将崩溃时仍挂载的插件标记为恢复待定，
	// 由后续加载按持久化状态决定是否隔离。
	for _, id := range ids {
		if err := m.MarkRecoveryPending(id, ""); err != nil {
			logger.I18nWarn("标记插件 %s 恢复待定失败: %v", id, err)
		}
	}
}

// Shutdown 停止共享 Runtime（宿主退出时调用）。
func (pm *PythonRuntimeManager) Shutdown() {
	pm.mu.Lock()
	rt := pm.rt
	pm.rt = nil
	pm.mu.Unlock()
	if rt == nil {
		return
	}
	rt.mu.Lock()
	rt.stopped = true
	rt.mu.Unlock()
	if rt.raw != nil {
		rt.raw.Kill()
	}
	if rt.client != nil {
		_ = rt.client.Close()
	}
}

// RuntimePID 返回当前共享 Runtime 进程 pid（0 = 未运行）；供测试/诊断。
func (pm *PythonRuntimeManager) RuntimePID() int {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.rt == nil {
		return 0
	}
	return pm.rt.pid
}

// pluginWantsSharedRuntime 判断插件是否应接入共享 Runtime（python-shared）。
// 依据 manifest 的 Runtime（安装时选择）或 RuntimePreferred（恢复偏好）字段
// （方案第 2 节三值之一）。缺省/""/"grpc" 走既有 python-grpc 一插件一进程路径。
//
// 隔离迁移（方案第 6-7 节）：被标记 ISOLATION_PENDING / ISOLATED 的插件**不再
// 进入共享 Runtime**，改用 python-grpc 独立进程边界（复用既有进程隔离，不再
// 实现第二套 Worker）；RECOVERY_PENDING（更新后）重新尝试共享。
// pluginPreferredRuntime 规范化插件的**首选运行方式**为 "shared"/"grpc"
// （方案：preferred_runtime = 用户/插件声明的希望怎么运行；只有两个用户可见
// 选项——共享进程 / 独立进程）。旧值 python-shared → shared；
// python-isolated / python-grpc / isolated → grpc；空 → shared。
// 注意：**isolated 不是用户选项**，它是 Watchdog 在故障时把 shared 插件
// 迁移到独立进程后派生出的**状态**（current=python-grpc + health=ISOLATED），
// 用户选 shared 不代表禁止系统自动隔离。
func pluginPreferredRuntime(e *ManifestEntry) string {
	if e == nil {
		return "shared"
	}
	switch e.RuntimePreferred {
	case "shared":
		return "shared"
	case "grpc", "isolated", "python-isolated", "python-grpc":
		return "grpc"
	}
	switch e.Runtime {
	case "python-shared":
		return "shared"
	case "python-isolated", "python-grpc":
		return "grpc"
	}
	return "shared"
}

func (m *SubprocessManager) pluginWantsSharedRuntime(id string) bool {
	e := m.cachedManifest().Get(id)
	if e == nil {
		return false
	}
	// 系统自动故障隔离优先于用户偏好：被标记 ISOLATION_PENDING / ISOLATED /
	// isolation_required 的插件，即使 preferred=shared 也不接入共享 Runtime，
	// 改用独立 python-grpc 进程（方案「用户选 shared 不是禁止系统在故障时隔离」）。
	if e.IsolationRequired || e.HealthState == HealthIsolationPending || e.HealthState == HealthIsolated {
		return false
	}
	// preferred=grpc（独立进程）：天生不适合共享解释器的插件（C 扩展 /
	// sys.modules 污染 / 依赖版本冲突）直接独立进程。
	return pluginPreferredRuntime(e) == "shared"
}

// migrateToIsolated 把被判定 UNHEALTHY（已持久化 ISOLATION_PENDING）的插件从
// 共享 Runtime 摘除，改用独立 python-grpc 进程运行（方案第 4/6/9 节）：
//
//	ISOLATION_PENDING → 从 Shared Runtime 摘除 → 启动独立 python-grpc
//	→ ISOLATED（current=python-grpc，preferred 保持不变）
//
// 复用既有 python-grpc 进程隔离，不再实现第二套 Worker。异步执行；失败仅告警
// （下轮 sweep 会重试——插件仍非 shared，会被按 python-grpc 加载）。
func (m *SubprocessManager) migrateToIsolated(id string) {
	// 从共享 Runtime 卸载该插件（不杀共享进程）。
	if mgr := m.sharedRuntimeManagerIfAny(); mgr != nil {
		ctx, cancel := context.WithTimeout(m.ctx, cleanupTimeout)
		if err := mgr.UnloadPlugin(ctx, id); err != nil {
			logger.I18nWarn("隔离迁移：从共享 Runtime 摘除插件 %s 失败: %v", id, err)
		}
		cancel()
	}
	// 记录当前运行方式为 python-grpc（preferred 不变），状态 ISOLATED。
	if err := m.SetPluginRuntime(id, m.runtimePreferredOf(id), "python-grpc"); err != nil {
		logger.I18nWarn("隔离迁移：持久化插件 %s 运行方式失败: %v", id, err)
	}
	if err := m.SetPluginHealthState(id, HealthIsolated, "plugin_unhealthy"); err != nil {
		logger.I18nWarn("隔离迁移：持久化插件 %s ISOLATED 失败: %v", id, err)
	}
	// 重新加载为独立 python-grpc 进程（pluginWantsSharedRuntime 已因
	// ISOLATION_PENDING 返回 false）。
	if err := m.Reload(context.Background(), id); err != nil {
		logger.I18nWarn("隔离迁移：以 python-grpc 重载插件 %s 失败: %v", id, err)
		return
	}
	logger.I18nWarn("插件 %s 已隔离到独立 python-grpc 进程（ISOLATED）", id)
}

// isolationRecoveryWindow 是隔离插件恢复正常前的连续健康观察窗口（方案第 7
// 节「不能启动即 NORMAL」：插件可能启动成功 5 秒后又崩）。
const isolationRecoveryWindow = 5 * time.Minute

// migrateBackToShared 把已隔离/待恢复、且稳定正常的插件迁回共享 Runtime
// （方案第 7 节「恢复 → NORMAL」）：清除隔离标记 → 重载（pluginWantsShared
// Runtime 重新返回 true）→ 回到共享 Runtime，preferred/current 归位。
func (m *SubprocessManager) migrateBackToShared(id string) {
	// 仅迁回「首选=shared」的插件；用户明确选独立进程（grpc）的插件不因
	// 恢复而改回共享（隔离状态由 Watchdog 派生，不覆盖用户偏好）。
	if pluginPreferredRuntime(m.cachedManifest().Get(id)) != "shared" {
		return
	}
	// 清除隔离：恢复 NORMAL、isolation_required=false、FailureCount 归零。
	if err := m.ClearPluginIsolation(id); err != nil {
		logger.I18nWarn("恢复迁移：清除插件 %s 隔离标记失败: %v", id, err)
		return
	}
	// preferred 保持 "shared"；current 归位为 python-shared。
	if err := m.SetPluginRuntime(id, "shared", "python-shared"); err != nil {
		logger.I18nWarn("恢复迁移：持久化插件 %s 运行方式失败: %v", id, err)
	}
	if err := m.Reload(context.Background(), id); err != nil {
		logger.I18nWarn("恢复迁移：把插件 %s 迁回共享 Runtime 失败: %v", id, err)
		return
	}
	logger.I18nInfo("插件 %s 已恢复正常，迁回共享 Python Runtime", id)
}

// runtimePreferredOf 读取插件首选运行方式（规范化 "shared"/"isolated"）。
func (m *SubprocessManager) runtimePreferredOf(id string) string {
	return pluginPreferredRuntime(m.cachedManifest().Get(id))
}

// SetPluginRuntimePreference 设置插件的运行方式偏好（前端"运行方式"列）：
// runtime ∈ {"python-grpc","python-shared","python-isolated"}（Python）或
// {"grpc","native"}（Go）。写入 manifest.Runtime；对 Python 同时写
// RuntimePreferred/Current。需重载插件生效。
func (m *SubprocessManager) SetPluginRuntimePreference(id, runtime string) error {
	// Go 插件：grpc/native；Python 插件：shared/grpc（含旧值映射）。
	// Python 无 "isolated" 用户选项（isolated 是 Watchdog 的派生状态）。
	isNative := runtime == "native"
	var pref string // Python 首选运行方式
	switch runtime {
	case "shared", "python-shared":
		pref = "shared"
	case "grpc", "python-grpc", "python-isolated", "isolated":
		pref = "grpc"
	case "native":
	default:
		return fmt.Errorf("无效的运行方式 %q", runtime)
	}
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
	if e.Language == "python" {
		e.Runtime = ""
		e.RuntimePreferred = pref
		// 切换运行方式即视为重新评估：清除隔离/恢复标记，允许按新方式加载
		//（方案「ISOLATED 不是永久处罚」）。isolated 状态由 Watchdog 派生，
		// 不在此处写入。
		e.HealthState = ""
		e.IsolationRequired = false
		if pref == "grpc" {
			e.RuntimeCurrent = "python-grpc"
		} else {
			e.RuntimeCurrent = "python-shared"
		}
	} else {
		e.Runtime = "grpc"
		if isNative {
			e.Runtime = "native"
		}
	}
	return m.saveManifest(man)
}

// startSharedInstance 将插件加载进共享 Runtime，返回其 per-plugin 实例视图
// （Client 绑定 plugin_id，raw 为 nil，shared=true）。
func (m *SubprocessManager) startSharedInstance(ctx context.Context, id, abs string) (*PluginInstance, error) {
	entry := m.cachedManifest().Get(id)
	name, version := "", ""
	if entry != nil {
		name = entry.DisplayName
		version = entry.Version
	}
	pc, meta, err := m.sharedManager().EnsurePlugin(ctx, id, abs, name, version)
	if err != nil {
		return nil, err
	}
	inst := &PluginInstance{
		ID:        id,
		Binary:    abs,
		Language:  "python",
		Runtime:   "python-shared",
		shared:    true,
		Client:    pc,
		Meta:      meta,
		StartedAt: time.Now(),
		owner:     m,
	}
	if meta != nil {
		inst.Name = meta.Name
		inst.Version = normalizePluginVersion(meta.Version)
	}
	m.setHandlerMeta(id, meta)
	inst.Touch()
	if meta != nil && len(meta.Tools) > 0 {
		m.setPluginTools(id, meta.Tools)
	}
	// 持久化运行时偏好与当前值（方案第 5 节）：preferred 保持用户选择
	//（shared/grpc），current=python-shared（当前实际运行方式）。隔离迁移后
	// current 会变为 python-grpc，preferred 保持不变。
	_ = m.SetPluginRuntime(id, pluginPreferredRuntime(entry), "python-shared")
	logger.I18nInfo("插件 %s 已加载到共享 Python Runtime (plugin_id=%s)", id, id)
	return inst, nil
}

// marshalSharedPlugins 序列化初始插件清单到 JSON（供环境变量注入，预留）。
func marshalSharedPlugins(entries []map[string]string) string {
	if entries == nil {
		entries = []map[string]string{}
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// sharedInstanceTeardown 处理共享实例的卸载：Cleanup + ManagePlugin(unload)，
// 不 close 共享连接、不 kill 共享进程（方案第 8 节「通知 Runtime 卸载单插件」）。
func (m *SubprocessManager) sharedInstanceTeardown(inst *PluginInstance) {
	if inst == nil || inst.Client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	_ = inst.Client.Cleanup(ctx, &sdkv1.PluginRef{PluginId: inst.ID})
	cancel()
	if mgr := m.sharedRuntimeManagerIfAny(); mgr != nil {
		uctx, ucancel := context.WithTimeout(context.Background(), cleanupTimeout)
		_ = mgr.UnloadPlugin(uctx, inst.ID)
		ucancel()
	}
}

// sharedRuntimeManagerIfAny 返回已初始化的共享 Runtime 管理器（不触发创建）。
func (m *SubprocessManager) sharedRuntimeManagerIfAny() *PythonRuntimeManager {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sharedRTMgr
}
