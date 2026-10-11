package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/v2"
	goplugin "github.com/hashicorp/go-plugin"
)

// errSharedDepsConflict 表示插件因 requirements.txt 版本冲突不能加入共享
// Runtime，应回退独立 python-grpc 进程（方案第 9 节）。shared 与 grpc 共用
// 同一 venv，本错误用于避免把互斥版本约束的插件塞进同一个共享进程。
var errSharedDepsConflict = errors.New("共享 Runtime 依赖冲突")

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
	// reloadFunc 是崩溃恢复时逐插件重载的回调：生产绑定 SubprocessManager.Reload
	//（Reload 内部走 startInstance → pluginWantsSharedRuntime 按持久化状态分流：
	// RECOVERY_PENDING 重新加入新共享 Runtime，ISOLATION_PENDING/ISOLATED 起
	// 独立 python-grpc）；测试可注入桩避免真起子进程。nil 时回退 mgr.Reload。
	reloadFunc func(ctx context.Context, id string) error
	// recoveryMu 串行化崩溃恢复：恢复过程中 Runtime 再次崩溃会经
	// handleSharedExit 再次触发，下一轮恢复在此排队（新一轮清理+重载），
	// 避免两轮恢复并发清理/重载交错。恢复全程不持 pm.mu（重载路径
	// startInstance → EnsurePlugin → ensureRuntime 会取 pm.mu），无锁序反转。
	recoveryMu sync.Mutex

	// shutdownOnce 保证 Shutdown 幂等：宿主关闭路径（SubprocessManager.Shutdown）
	// 与测试 cleanup 可能多次触发，重复 Kill/Close 必须安全。
	shutdownOnce sync.Once

	mu sync.Mutex
	rt *sharedRuntime
	// gen 在每次 Runtime 重建（崩溃重启）时自增，供恢复流程区分新旧实例。
	gen uint64
	// pluginReqs 记录当前已加入共享 Runtime 的每个插件的 requirements.txt
	//（规范化包名 → 版本约束），用于新插件加入前的依赖冲突检测。Runtime 重建
	// （崩溃重启）时清空，随插件重新加载重建；卸载插件时移除其贡献。
	pluginReqs map[string]map[string]string
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
	return &PythonRuntimeManager{
		mgr:        m,
		reloadFunc: m.Reload,
	}
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

// disposeRuntime 清理一个（疑似/确认已死或被替换的）共享 Runtime：kill 进程
// 兜底收尾（进程已死也无妨，go-plugin 需完成簿记 reap）、关闭共享连接、归还
// 握手端口段。崩溃恢复（recoverSharedRuntime）与 ensureRuntime 兜底重建共用，
// 保证两条路径的资源回收语义一致。幂等（重复 Kill/Close/release 安全）。
func disposeRuntime(rt *sharedRuntime) {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	rt.stopped = true
	minp := rt.minp
	rt.mu.Unlock()
	// 归还握手端口段：不归还会永久泄漏 [minp, minp+15]（全局分配器耗尽）。
	releasePluginPort(minp)
	if rt.raw != nil {
		rt.raw.Kill()
	}
	if rt.client != nil {
		_ = rt.client.Close()
	}
}

// ensureRuntime 返回可用的共享 Runtime，必要时启动。
func (pm *PythonRuntimeManager) ensureRuntime(ctx context.Context) (*sharedRuntime, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.rt != nil {
		pm.rt.mu.Lock()
		stopped := pm.rt.stopped
		// raw == nil（未挂接 go-plugin 句柄的 rt，如测试桩）不视为已退出：
		// 仅 stopped 或真实句柄报告退出才触发重建。
		exited := pm.rt.raw != nil && pm.rt.raw.Exited()
		pm.rt.mu.Unlock()
		if !stopped && !exited {
			return pm.rt, nil
		}
		// 已崩溃：清理死句柄后重建（watchSharedRuntime → handleSharedExit 通常
		// 已异步处理，这里兜底）。不清理会让死 client 的失败 RPC 反复重试、
		// 端口段泄漏。
		disposeRuntime(pm.rt)
		pm.rt = nil
		// Runtime 重建后所有插件都会重新加载：依赖跟踪随之失效，必须清空
		// 以免用旧集合做冲突误判。
		pm.pluginReqs = nil
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
func (pm *PythonRuntimeManager) EnsurePlugin(ctx context.Context, id, pluginDir, pluginName, version string) (pluginsdk.PluginClient, *pluginsdk.PluginInfo, error) {
	// 依赖冲突检测（方案第 9 节）：shared 与 grpc 共用同一 venv，若本插件与
	// 已加入共享 Runtime 的其他插件对同一包要求互斥版本，则拒绝加入 shared，
	// 由调用方回退独立 python-grpc 进程。绝不假装 shared 有环境隔离。
	reqs := parseRequirements(filepath.Join(pluginDir, "requirements.txt"))
	if other, pkg, specSelf, specOther, ok := pm.conflictFor(id, reqs); ok {
		return nil, nil, fmt.Errorf("%w: 插件 %s 需要 %s%s，但共享 Runtime 中的 %s 需要 %s%s", errSharedDepsConflict, id, pkg, specSelf, other, pkg, specOther)
	}
	rt, err := pm.ensureRuntime(ctx)
	if err != nil {
		return nil, nil, err
	}
	// 依赖准备（与 python-grpc 启动路径语义一致）：把插件 requirements.txt
	// 安装进共享宿主 venv。装过时 pip 命中缓存秒回；失败仅告警不阻断。
	pm.mgr.ensurePluginRequirements(ctx, pluginDir)
	// ManagePlugin(load)：登记 + import（等价单插件 server.py 阶段 A）。
	pc := rt.client.ForPlugin(id)
	loadCtx, cancel := context.WithTimeout(ctx, registerTimeout)
	defer cancel()
	resp, err := pc.ManagePlugin(loadCtx, pluginsdk.ManagePluginRequest{
		Action:     "load",
		PluginID:   id,
		PluginDir:  pluginDir,
		PluginName: pluginName,
		Version:    version,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("共享 Runtime 加载插件 %s: %w", id, err)
	}
	if !resp.OK {
		return nil, nil, fmt.Errorf("共享 Runtime 加载插件 %s 失败: %s", id, resp.Error)
	}
	// Register：触发该插件的注册表构建 + 实例化（MultiTenant.Register 会在
	// 返回后异步 instantiate）。
	regCtx, cancel2 := context.WithTimeout(ctx, registerTimeout)
	defer cancel2()
	meta, err := pc.Register(regCtx)
	if err != nil {
		// 回滚：ManagePlugin(load) 已成功，插件模块已 import 进共享进程；
		// Register 失败后不回滚会留下僵尸模块（占 sys.modules、可能在后续
		// 同名加载时报错）。尽力 unload，失败仅告警不掩盖原始错误。
		ulCtx, ulCancel := context.WithTimeout(ctx, cleanupTimeout)
		_, ulErr := pc.ManagePlugin(ulCtx, pluginsdk.ManagePluginRequest{Action: "unload", PluginID: id})
		ulCancel()
		if ulErr != nil {
			logger.I18nWarn("共享 Runtime 回滚插件 %s 失败（可能残留僵尸模块）: %v", id, ulErr)
		}
		return nil, nil, fmt.Errorf("共享 Runtime 注册插件 %s: %w", id, err)
	}
	rt.mu.Lock()
	rt.plugins[id] = true
	rt.mu.Unlock()
	pm.trackSharedReqs(id, reqs)
	return pc, &meta, nil
}

// conflictFor 返回 candidate(id, reqs) 与已加入共享 Runtime 的其他插件的依赖
// 冲突：返回 (另一插件 id, 冲突包名, 本插件约束, 另一插件约束, 是否冲突)。
func (pm *PythonRuntimeManager) conflictFor(id string, reqs map[string]string) (string, string, string, string, bool) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for otherID, otherReqs := range pm.pluginReqs {
		if otherID == id {
			continue
		}
		if pkg, specSelf, specOther, ok := requirementsConflict(reqs, otherReqs); ok {
			return otherID, pkg, specSelf, specOther, true
		}
	}
	return "", "", "", "", false
}

// trackSharedReqs 记录已成功加入共享 Runtime 的插件的依赖声明（供后续插件做
// 冲突检测）。
func (pm *PythonRuntimeManager) trackSharedReqs(id string, reqs map[string]string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.pluginReqs == nil {
		pm.pluginReqs = map[string]map[string]string{}
	}
	pm.pluginReqs[id] = reqs
}

// UnloadPlugin 从共享 Runtime 卸载单个插件（不杀 Runtime 进程）。
// rt.plugins[id] 条目在 ManagePlugin(unload) RPC **成功后**才删除：失败时保留
// 条目以便重试（否则条目已删、重试直接 short-circuit，共享进程内残留已卸载
// 一半的插件模块，状态永久不一致）。
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
	rt.mu.Unlock()
	pc := rt.client.ForPlugin(id)
	ctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	_, err := pc.ManagePlugin(ctx, pluginsdk.ManagePluginRequest{Action: "unload", PluginID: id})
	if err != nil {
		// RPC 失败：保留 rt.plugins 条目与依赖贡献，供后续重试。
		return fmt.Errorf("共享 Runtime 卸载插件 %s: %w", id, err)
	}
	// 成功后才移除跟踪条目（先 rt.plugins 再依赖贡献，二者均为纯内存操作）。
	rt.mu.Lock()
	delete(rt.plugins, id)
	rt.mu.Unlock()
	pm.mu.Lock()
	delete(pm.pluginReqs, id)
	pm.mu.Unlock()
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

// handleSharedExit 处理共享 Runtime 崩溃：标记恢复状态并触发完整恢复闭环
// （重启 Runtime + 重载全部插件，方案第 6 节 Watchdog 崩溃流程）。
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
	// 不猜测罪魁祸首（方案第 7 节）：shared process 崩溃只说明 Runtime 失败，
	// 不能证明任何插件有问题。**已判定需隔离的插件（ISOLATION_PENDING/ISOLATED）
	// 必须保留隔离要求**——不能被统一重置清掉 IsolationRequired。仅非隔离插件
	// 标记 RECOVERY_PENDING，重启后重新尝试 shared。单次原子 Load→改→Save。
	if err := m.markCrashRecovery(ids); err != nil {
		logger.I18nWarn("标记共享 Runtime 崩溃恢复状态失败: %v", err)
	}
	// 异步触发恢复闭环（重启 Runtime → 逐插件 Reload → 重新注册）：不阻塞
	// watch goroutine（它已随 handleSharedExit 返回退出）。rt.stopped=true
	// 保证本 rt 不会被重复触发；恢复全程由 pm.recoveryMu 串行化，恢复过程中
	// Runtime 再次崩溃则新一轮恢复在此排队。
	if mgr := m.sharedRuntimeManagerIfAny(); mgr != nil {
		go mgr.recoverSharedRuntime(rt, ids)
	} else {
		logger.I18nWarn("共享 Runtime 崩溃恢复：管理器未初始化，跳过自动重载")
	}
}

// recoverSharedRuntime 执行共享 Runtime 崩溃恢复闭环（方案第 6 节）：
//
//	清理死 Runtime（Kill/Close/释放端口）→ 重置管理器状态 → 逐插件 Reload
//	→ Reload 内部按 manifest 持久化状态分流：RECOVERY_PENDING 的重新加入
//	新共享 Runtime，ISOLATION_PENDING/ISOLATED 的起独立 python-grpc。
//
// 可重入：恢复过程中 Runtime 再次崩溃时，handleSharedExit 会再触发一轮
// recoverSharedRuntime——recoveryMu 保证上一轮清理完成后新一轮才开始
// （新一轮的旧 rt 句柄 dispose 幂等，重复 dispose 无副作用）。
// 逐插件串行执行（Reload 内部持 per-id 生命周期锁），单插件失败仅告警，
// 不中断其余插件恢复。
func (pm *PythonRuntimeManager) recoverSharedRuntime(rt *sharedRuntime, ids []string) {
	pm.recoveryMu.Lock()
	defer pm.recoveryMu.Unlock()

	logger.I18nInfo("共享 Runtime 崩溃恢复开始：清理旧进程并重载 %d 个插件", len(ids))
	// 1. 清理死 Runtime：client 收尾 + 端口段归还（幂等，进程已死无妨）。
	disposeRuntime(rt)
	// 2. 重置管理器状态：死 runtime 句柄、依赖跟踪一并清空（重载会重建），
	//    gen++ 使旧代际的在途加载作废。
	pm.mu.Lock()
	if pm.rt == rt {
		pm.rt = nil
	}
	pm.pluginReqs = nil
	pm.gen++
	pm.mu.Unlock()

	// 3. 逐插件串行重载。reloadFunc 绑定 SubprocessManager.Reload：
	//    startInstance → pluginWantsSharedRuntime 按 manifest 健康状态分流
	//   （RECOVERY_PENDING → 新 shared；ISOLATION_PENDING/ISOLATED → grpc）。
	reload := pm.reloadFunc
	if reload == nil && pm.mgr != nil {
		reload = pm.mgr.Reload
	}
	if reload == nil {
		logger.I18nWarn("共享 Runtime 崩溃恢复：无可用重载回调，跳过 %d 个插件", len(ids))
		return
	}
	for _, id := range ids {
		if pm.mgr != nil && pm.mgr.ctx.Err() != nil {
			logger.I18nWarn("共享 Runtime 崩溃恢复：宿主已关闭，中止重载剩余插件")
			return
		}
		ctx, cancel := context.WithTimeout(pm.mgrCtx(), 2*registerTimeout)
		err := reload(ctx, id)
		cancel()
		if err != nil {
			// Reload 失败（含 ErrPluginNotLoaded：实例表已被并发更新）：
			// 不中断其余插件恢复，失败状态由 Reload/markFailed 内部记录。
			logger.I18nWarn("共享 Runtime 崩溃恢复：重载插件 %s 失败: %v", id, err)
			continue
		}
		logger.I18nInfo("共享 Runtime 崩溃恢复：插件 %s 已重载", id)
	}
	logger.I18nInfo("共享 Runtime 崩溃恢复完成")
}

// mgrCtx 返回所属管理器的生命周期 ctx（nil 安全；测试注入桩可能无 mgr）。
func (pm *PythonRuntimeManager) mgrCtx() context.Context {
	if pm.mgr == nil || pm.mgr.ctx == nil {
		return context.Background()
	}
	return pm.mgr.ctx
}

// Shutdown 停止共享 Runtime（宿主退出时调用）。幂等：sync.Once 保证重复调用
// 只执行一次停止序列；无运行中的 Runtime 时为空操作。
func (pm *PythonRuntimeManager) Shutdown() {
	pm.shutdownOnce.Do(func() { pm.shutdown() })
}

// shutdown 是 Shutdown 的实际执行体（经 Shutdown 的 sync.Once 保证单次执行）。
func (pm *PythonRuntimeManager) shutdown() {
	pm.mu.Lock()
	rt := pm.rt
	pm.rt = nil
	// Runtime 重建后依赖跟踪失效，一并清空，避免残留旧集合做冲突误判。
	pm.pluginReqs = nil
	pm.mu.Unlock()
	// 复用 disposeRuntime：标记 stopped + Kill + Close + 归还握手端口段。
	disposeRuntime(rt)
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

// updateNormalSince 推进「连续 NORMAL」观察窗口起点（纯函数，便于单测）：
//   - health != NORMAL（DEGRADED/UNHEALTHY/...）→ 清零（窗口中断，重新计时）；
//   - health == NORMAL 且尚未计时 → 记为 now（开始计时）；
//   - health == NORMAL 且已计时 → 保持原起点（连续计时）。
//
// 零值表示当前不在连续 NORMAL 窗口内。绝不能用 StartedAt 代替：插件可能
// 启动成功后反复异常/恢复，StartedAt 计满窗口不代表这段一直健康。
func updateNormalSince(prev time.Time, health string, now time.Time) time.Time {
	if health != HealthNormal {
		return time.Time{}
	}
	if prev.IsZero() {
		return now
	}
	return prev
}

// recoveryWindowElapsed 报告自 normalSince 起是否已连续 NORMAL 满观察窗口。
func recoveryWindowElapsed(normalSince, now time.Time) bool {
	return !normalSince.IsZero() && now.Sub(normalSince) >= isolationRecoveryWindow
}

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
	_ = inst.Client.Cleanup(ctx)
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
