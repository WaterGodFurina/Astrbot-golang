package plugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
	sdkv1 "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/gen/sdkv1"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/pysdk"
	"github.com/WaterGodFurina/Astrbot-golang/internal/toolchain"
	goplugin "github.com/hashicorp/go-plugin"
)

// logger 供插件运行时与编译相关路径记录日志。
var logger = log.GetDefault().WithComponent("Plugin")

// startTimeout bounds the go-plugin handshake + first Register call. go-plugin
// itself does not time out the handshake, so Load enforces one.
const startTimeout = 15 * time.Second

// startInstanceMu 串行化 startInstance 的 Set/Dispense 窗口：SDK 侧
// hostPluginID 是进程级全局变量，并发装载不同插件时 A 在握手 accept 前设置
// 的身份会被 B 覆盖，导致 A 的 HostService 连接被绑定为 B 的身份（身份隔离
// 可被破坏）。全局互斥保证同一时刻只有一个 startInstance 在跑。
var startInstanceMu sync.Mutex

// cleanupTimeout bounds the graceful Cleanup RPC before force-killing.
const cleanupTimeout = 5 * time.Second

// pluginHookRPCTimeout bounds each lifecycle-hook RPC so a hung plugin (dead
// loop/deadlock) cannot block Unload/SetEnabled forever and cascade-freeze all
// manifest operations.
const pluginHookRPCTimeout = 30 * time.Second

// restartBudgetResetWindow: 超过该间隔没有崩溃，则 restarts 预算清零，
// 使低频偶发崩溃不会被永久停用（预算只惩罚"连续/近期"崩溃）。
const restartBudgetResetWindow = 10 * time.Minute

type PluginInstance struct {
	ID        string
	Name      string
	Version   string
	Binary    string
	StartedAt time.Time

	// Language is "go" (compiled binary) or "python" (source tree).
	Language string
	// DisplayName / ShortDesc are the plugin's display metadata (from the
	// packaged metadata), surfaced to the WebUI.
	DisplayName string
	ShortDesc   string

	// Client is the typed gRPC client used by the star bridge to invoke
	// commands/filters/hooks.
	Client *pluginsdk.Client
	// Meta is the plugin's Register() metadata (handlers + config schema).
	Meta *sdkv1.RegisterResponse

	// toolsMu 保护 toolsCache/toolsLoaded：LLM 函数工具的"实时快照"。
	// 插件工具在实例化阶段（Context.add_llm_tools）注册，晚于 Register 的
	// Meta.Tools 快照——宿主经 ListTools RPC 拉取最新列表并缓存（插件
	// reload 后新实例缓存为空，重新拉取）。
	toolsMu     sync.Mutex
	toolsCache  []*sdkv1.ToolDesc
	toolsLoaded bool

	mu  sync.Mutex
	raw *goplugin.Client // go-plugin process client
	// pgid 是插件子进程的进程组 id（Setpgid 后 = 直接子进程 pid）。>0 时
	// teardown/失败路径按组回收整棵进程树（含 Python 桥再拉起的子进程），
	// 防宿主退出后插件孤儿化；0 = 未记录（如 exec 未成功启动）。
	pgid     int
	stopped  bool // set before intentional kill (suppresses restart)
	restarts int  // consecutive crash-restart count for this instance
	// lastRestartAt 记录上一次崩溃重启的时间，用于 restart 预算的基于时间衰减。
	lastRestartAt time.Time
	failed        error // set when the plugin is marked failed

	// lastActiveNano 是插件最后一次被调用（命令/过滤器/钩子/工具 RPC）的
	// UnixNano 时间戳，供闲置自动卸载（idle unload）判定使用。
	lastActiveNano atomic.Int64

	// owner 是插件所属的 SubprocessManager（RefreshTools 成功后回写工具
	// 注册表用；由 startInstance 赋值）。
	owner *SubprocessManager
}

// SubprocessManager manages plugins running as isolated child processes
// (go-plugin, gRPC). This is the NEW plugin runtime that replaces the legacy
// .so loader (fully removed; only this subprocess runtime remains).
//
// Unlike .so plugins, child processes can be fully terminated (memory, file
// handles and goroutines are reclaimed by the OS) and a crash cannot take the
// host down; crashed plugins are automatically restarted with backoff.
type SubprocessManager struct {
	mu        sync.RWMutex
	instances map[string]*PluginInstance
	failures  map[string]error
	// opMu 是每个插件的生命周期互斥（Reload/Unload/崩溃 restart 串行化），
	// 防止并发 reload/unload 导致孤儿进程或"禁用后复活"。用 sync.Map 常驻
	// 条目（数量 = 曾加载过的插件数，几十个级别），避免引用计数复杂度；
	// 条目创建后不再删除，插件卸载后的空互斥占几个字节，可接受。
	opMu sync.Map // map[string]*sync.Mutex

	toolchain *toolchain.Toolchain
	compiler  *Compiler
	dataDir   string
	// logLevels 是 per-plugin 日志级别覆盖存储（data/plugin_log_levels.json）。
	logLevels *logLevels
	ctx       context.Context
	cancel    context.CancelFunc
	// gen 是"实例表代际"标记：Shutdown 换新表时自增。restart 在 startInstance
	// 成功后写回 map 前对比 gen，代际不一致说明表已被换掉，需丢弃新实例并回收。
	gen uint64
	// manifestMu 串行化 manifest 的"读→改→写"整段（recordInstall/SetEnabled/
	// BindSource/ReinstallSource/Uninstall），防止并发修改丢条目。与 m.mu 职责
	// 分离：m.mu 保护内存 map，manifestMu 保护磁盘文件的一致性。
	manifestMu sync.Mutex
	// docMu 保护 docFetchCache（README/CHANGELOG 的远程拉取结果缓存，
	// 成功与失败（负面）都记录，TTL docFetchCacheTTL，避免 GitHub 不通时
	// 每次打开详情页都重试，同时不永久阻挡后续（配置加速后）的重新拉取）。
	docMu         sync.Mutex
	docFetchCache map[string]docCacheEntry

	// githubProxy 是插件 git clone 的 GitHub 加速地址（如 https://ghfast.top/），
	// 配置后克隆 https://github.com/... 仓库时在 URL 前加该前缀。
	githubProxy string

	// pipIndex / pipArgs 是 Python 插件依赖安装的 PyPI 镜像与额外 pip 参数
	//（config pypi_index_url / pip_install_arg）。pipIndex 空时回退
	// pysdk.PyPIIndex()（env/默认镜像）。
	pipIndex string
	pipArgs  []string

	// toolRegMu 保护 toolRegistry：LLM 工具名 → 所属插件 id + 工具描述。
	// 插件闲置休眠（UnloadIdle）时实例被移出 instances 表，但其工具仍留在
	// 注册表——LLM 调用该工具时宿主按名查注册表并 EnsureLoaded 唤醒插件；
	// 仅真实卸载/禁用（unloadLocked notify=true）时清除该插件的条目。
	toolRegMu    sync.RWMutex
	toolRegistry map[string]toolRegEntry

	// handlerMetaMu 保护 handlerMeta：插件 id → Register 元数据（handler 表
	// 快照）。插件闲置休眠时实例被移出 instances 表，但元数据仍保留，供
	// RebridgePlugins 重建休眠插件的 star handler（命令/过滤器/钩子），保证
	// 休眠插件指令在 Dashboard 可见、Rebridge 后依然注册、调用时自动唤醒；
	// 仅真实卸载/禁用（unloadLocked notify=true）时清除该插件的条目。
	handlerMetaMu sync.RWMutex
	handlerMeta   map[string]*sdkv1.RegisterResponse

	// pythonEnv 是 Python 插件子进程环境（解释器 + SDK 目录），首次启动
	// Python 插件时惰性解析（可能创建 venv 安装依赖）。nil 表示尚未解析。
	pythonEnv *pysdk.RuntimeEnv

	// AutoRestart enables automatic restart of crashed plugins.
	AutoRestart bool
	// MaxRestarts caps the total number of start chances: the plugin gets 1
	// initial start plus at most MaxRestarts-1 automatic restarts before it is
	// marked failed (handleExit trips count >= MaxRestarts on the
	// MaxRestarts-th crash).
	MaxRestarts int
	// RestartBaseDelay is the base backoff before the first restart (scaled
	// linearly per consecutive crash).
	RestartBaseDelay time.Duration
	// PollInterval is the process-exit polling interval for crash detection.
	PollInterval time.Duration
	// OnInstancesChanged is invoked after a plugin instance is replaced
	// (e.g. crash-restart) so the host can re-bridge handlers.
	OnInstancesChanged func()
	// MinPort / MaxPort bound the go-plugin handshake listener port range
	// (default 10000-25000). Tests set an isolated range so a concurrently
	// running real host (whose plugin subprocesses listen in the default
	// range) cannot interfere with the test subprocess handshake.
	MinPort int
	MaxPort int

	// idleUnload 是闲置自动卸载阈值（0 = 关闭）。启用后，超过该时长没有
	// 任何 RPC 活动的插件进程会被自动卸载（OS 回收内存），下次被触发时
	// 懒加载唤醒。嵌入式/低内存设备的"进程池"语义：进程按需创建、闲置回收。
	idleUnload   time.Duration
	scanInterval time.Duration // idle 清扫间隔（默认 1 分钟；测试可注入）

	// hostCapabilities 是宿主向 Python 插件公开的能力集合（平台适配器 ID +
	// 固定能力 llm/send_message/recall_message/react/t2i/config/web），经
	// ASTRBOT_HOST_CAPABILITIES 环境变量注入 Python 插件子进程（插件侧
	// HostBridge.has() 查询）。由宿主生命周期在启动与重载平台后设置。
	hostCapabilities []string

	// sessionWaitMu 保护 sessionWaitReg：跨进程会话等待注册表（waitID →
	// 插件/umo），由 HostService.RegisterSessionWait hook 写入、管线
	// SessionWaitStage 查询消费（见 session_wait.go）。
	sessionWaitMu  sync.Mutex
	sessionWaitReg map[string]*sessionWaitEntry

	// bridgeHooksMu 保护 bridgeHooks：实例 ID → 桥接钩子名集合。桥接钩子
	// 由 Python SDK 的 botpy/telegram 兼容层经 HostService.RegisterBridgeHook
	// 注册，宿主管线每收到入站消息时向注册过的插件推送序列化事件（见
	// pipeline.dispatchBridgeHooks）。注册表为空是常见路径，管线快速返回，
	// 不产生任何额外 RPC 开销。
	bridgeHooksMu sync.RWMutex
	bridgeHooks   map[string]map[string]struct{}
}

// Touch marks the plugin as active (called before/after every RPC into the
// plugin). 供闲置卸载判定使用。
func (inst *PluginInstance) Touch() {
	inst.lastActiveNano.Store(time.Now().UnixNano())
}

// LastActive returns the last activity time (zero if never touched).
func (inst *PluginInstance) LastActive() time.Time {
	return time.Unix(0, inst.lastActiveNano.Load())
}

// IsIdle reports whether the plugin has been inactive for longer than idle.
func (inst *PluginInstance) IsIdle(now time.Time, idle time.Duration) bool {
	last := inst.LastActive()
	return !last.IsZero() && now.Sub(last) > idle
}

// RefreshTools 经 ListTools RPC 拉取插件当前的 LLM 函数工具列表并缓存。
// 插件工具在实例化阶段注册（Context.add_llm_tools），晚于 Register 的
// Meta.Tools 快照——宿主在首次 collectPluginTools 时调用本方法刷新。
// RPC 失败时保留旧缓存（nil 则回退 Meta.Tools）。成功后同步更新管理器的
// 工具注册表（工具名 → 插件），供休眠插件按名唤醒分发。
func (inst *PluginInstance) RefreshTools(ctx context.Context) {
	if inst.Client == nil {
		return
	}
	rpcCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	tools, err := inst.Client.ListTools(rpcCtx)
	cancel()
	if err != nil {
		logger.I18nWarn("插件 %s ListTools 失败: %v", inst.ID, err)
		return
	}
	inst.toolsMu.Lock()
	inst.toolsCache = tools
	inst.toolsLoaded = true
	inst.toolsMu.Unlock()
	if m := inst.owner; m != nil {
		m.setPluginTools(inst.ID, tools)
	}
}

// ToolsSnapshot 返回插件当前的 LLM 工具列表：优先使用 ListTools 缓存
// （RefreshTools 拉取）；未拉取过则回退 Register 元数据快照（Meta.Tools）。
// 返回的切片不可修改。
func (inst *PluginInstance) ToolsSnapshot() []*sdkv1.ToolDesc {
	inst.toolsMu.Lock()
	defer inst.toolsMu.Unlock()
	if inst.toolsLoaded && inst.toolsCache != nil {
		return inst.toolsCache
	}
	if inst.Meta != nil {
		return inst.Meta.Tools
	}
	return nil
}

// docFetchCacheTTL 是远程文档拉取结果（含负面）的缓存时长。
const docFetchCacheTTL = 5 * time.Minute

// toolRegEntry 是工具注册表条目：工具名 → 所属插件 + 最新描述。
type toolRegEntry struct {
	PluginID string
	Desc     *sdkv1.ToolDesc
}

// setPluginTools 用插件的最新工具列表整体替换该插件在注册表中的条目
// （RefreshTools 成功后调用；同一插件旧工具名被清除）。
func (m *SubprocessManager) setPluginTools(id string, tools []*sdkv1.ToolDesc) {
	m.toolRegMu.Lock()
	defer m.toolRegMu.Unlock()
	if m.toolRegistry == nil {
		m.toolRegistry = make(map[string]toolRegEntry)
	}
	// 先清掉该插件旧条目（工具可能被插件动态增删）
	for name, e := range m.toolRegistry {
		if e.PluginID == id {
			delete(m.toolRegistry, name)
		}
	}
	for _, t := range tools {
		if t == nil || t.Name == "" {
			continue
		}
		m.toolRegistry[t.Name] = toolRegEntry{PluginID: id, Desc: t}
	}
}

// removePluginTools 清除某插件的全部注册表条目（真实卸载/禁用时调用；
// 闲置休眠保留，保证工具调用能按名唤醒）。
func (m *SubprocessManager) removePluginTools(id string) {
	m.toolRegMu.Lock()
	defer m.toolRegMu.Unlock()
	for name, e := range m.toolRegistry {
		if e.PluginID == id {
			delete(m.toolRegistry, name)
		}
	}
}

// RegisterBridgeHook 幂等注册实例 instID 的一个桥接钩子（botpy/telegram
// 兼容层经 HostService.RegisterBridgeHook 调用）。注册后宿主管线每收到入站
// 消息即向该钩子推送序列化事件。
func (m *SubprocessManager) RegisterBridgeHook(instID, hookName string) {
	if m == nil || instID == "" || hookName == "" {
		return
	}
	m.bridgeHooksMu.Lock()
	set := m.bridgeHooks[instID]
	if set == nil {
		set = make(map[string]struct{})
		m.bridgeHooks[instID] = set
	}
	set[hookName] = struct{}{}
	m.bridgeHooksMu.Unlock()
}

// UnregisterBridgeHook 幂等注销实例 instID 的一个桥接钩子；该实例无剩余
// 钩子时移除其键。
func (m *SubprocessManager) UnregisterBridgeHook(instID, hookName string) {
	if m == nil || instID == "" || hookName == "" {
		return
	}
	m.bridgeHooksMu.Lock()
	if set := m.bridgeHooks[instID]; set != nil {
		delete(set, hookName)
		if len(set) == 0 {
			delete(m.bridgeHooks, instID)
		}
	}
	m.bridgeHooksMu.Unlock()
}

// BridgeHookSnapshot 返回桥接钩子注册表的快照（实例 ID → hook 名切片）。
// 注册表为空时返回 nil，调用方可据此快速返回，零额外开销。
func (m *SubprocessManager) BridgeHookSnapshot() map[string][]string {
	if m == nil {
		return nil
	}
	m.bridgeHooksMu.RLock()
	defer m.bridgeHooksMu.RUnlock()
	if len(m.bridgeHooks) == 0 {
		return nil
	}
	out := make(map[string][]string, len(m.bridgeHooks))
	for instID, set := range m.bridgeHooks {
		names := make([]string, 0, len(set))
		for name := range set {
			names = append(names, name)
		}
		out[instID] = names
	}
	return out
}

// removePluginBridgeHooks 清除某实例的全部桥接钩子条目（真实卸载/禁用时
// 调用，防止向已终止进程推送）。
func (m *SubprocessManager) removePluginBridgeHooks(id string) {
	if m == nil || id == "" {
		return
	}
	m.bridgeHooksMu.Lock()
	delete(m.bridgeHooks, id)
	m.bridgeHooksMu.Unlock()
}

// setHandlerMeta 记录插件 id → Register 元数据（startInstance 注册成功时
// 调用；reload/唤醒/崩溃重启的新实例会覆盖旧条目）。meta 为 nil 时删除。
func (m *SubprocessManager) setHandlerMeta(id string, meta *sdkv1.RegisterResponse) {
	m.handlerMetaMu.Lock()
	defer m.handlerMetaMu.Unlock()
	if meta == nil {
		delete(m.handlerMeta, id)
		return
	}
	m.handlerMeta[id] = meta
}

// removeHandlerMeta 清除某插件的 handler 元数据（真实卸载/禁用时调用；
// 闲置休眠保留，供 RebridgePlugins 重建休眠插件 handler）。
func (m *SubprocessManager) removeHandlerMeta(id string) {
	m.handlerMetaMu.Lock()
	defer m.handlerMetaMu.Unlock()
	delete(m.handlerMeta, id)
}

// HandlerMetaByID 返回插件 id 的 Register 元数据（含休眠插件），未加载过
// 或已真实卸载返回 nil。
func (m *SubprocessManager) HandlerMetaByID(id string) *sdkv1.RegisterResponse {
	m.handlerMetaMu.RLock()
	defer m.handlerMetaMu.RUnlock()
	return m.handlerMeta[id]
}

// ToolOwner 返回注册了工具 name 的插件 id（running 或休眠中），未注册返回
// ("", false)。
func (m *SubprocessManager) ToolOwner(name string) (string, bool) {
	m.toolRegMu.RLock()
	defer m.toolRegMu.RUnlock()
	e, ok := m.toolRegistry[name]
	if !ok {
		return "", false
	}
	return e.PluginID, true
}

// AllPluginTools 返回全部已注册插件工具（含休眠插件；running 插件的条目
// 由 RefreshTools 保持最新）。供 collectPluginTools 注入 LLM 工具列表。
func (m *SubprocessManager) AllPluginTools() []toolRegEntry {
	m.toolRegMu.RLock()
	defer m.toolRegMu.RUnlock()
	out := make([]toolRegEntry, 0, len(m.toolRegistry))
	for _, e := range m.toolRegistry {
		out = append(out, e)
	}
	return out
}

// docCacheEntry 是一次远程文档拉取的结果（成功为内容，失败为空串）。
type docCacheEntry struct {
	content string
	ts      time.Time
}

// NewSubprocessManager creates the subprocess plugin manager.
func NewSubprocessManager(tc *toolchain.Toolchain, dataDir string) *SubprocessManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &SubprocessManager{
		instances:        make(map[string]*PluginInstance),
		failures:         make(map[string]error),
		docFetchCache:    make(map[string]docCacheEntry),
		toolRegistry:     make(map[string]toolRegEntry),
		handlerMeta:      make(map[string]*sdkv1.RegisterResponse),
		sessionWaitReg:   make(map[string]*sessionWaitEntry),
		bridgeHooks:      make(map[string]map[string]struct{}),
		logLevels:        newLogLevels(dataDir),
		toolchain:        tc,
		compiler:         NewCompiler(tc),
		dataDir:          dataDir,
		ctx:              ctx,
		cancel:           cancel,
		AutoRestart:      true,
		MaxRestarts:      5,
		RestartBaseDelay: time.Second,
		PollInterval:     500 * time.Millisecond,
	}
}

// lockOp acquires the per-plugin lifecycle lock for id and returns a release
// function. Callers must release via defer. 同 id 的 Reload/Unload/崩溃重启
// 通过该互斥串行化；不同 id 之间互不阻塞。
func (m *SubprocessManager) lockOp(id string) func() {
	l, _ := m.opMu.LoadOrStore(id, &sync.Mutex{})
	mu := l.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// SetGitHubProxy 设置插件 git clone 的 GitHub 加速地址（config github_proxy）。
func (m *SubprocessManager) SetGitHubProxy(url string) {
	m.githubProxy = url
}

// SetPipConfig 注入 Python 插件依赖安装的 PyPI 镜像（pypi_index_url）与
// 额外 pip 参数（pip_install_arg）。空 index 回退 pysdk.PyPIIndex()（env/
// 默认镜像）；args 按空白拆分为多个参数。
func (m *SubprocessManager) SetPipConfig(indexURL, extraArgs string) {
	m.pipIndex = strings.TrimSpace(indexURL)
	if t := strings.TrimSpace(extraArgs); t != "" {
		m.pipArgs = strings.Fields(t)
	} else {
		m.pipArgs = nil
	}
}

// SetHostCapabilities 设置宿主向 Python 插件公开的能力集合（平台适配器 ID
// + 固定能力）。传入的能力在 Python 插件子进程启动时经
// ASTRBOT_HOST_CAPABILITIES 环境变量注入，插件侧用 HostBridge.has() 查询。
// 启动与平台重载后由宿主调用；对已运行中的插件进程，能力在其下次重启
// （reload/崩溃重启/闲置唤醒）时生效。
func (m *SubprocessManager) SetHostCapabilities(caps []string) {
	m.mu.Lock()
	m.hostCapabilities = append([]string(nil), caps...)
	m.mu.Unlock()
}

// hostCapabilitiesSnapshot 返回当前宿主能力集合的副本（空 slice = 未设置）。
func (m *SubprocessManager) hostCapabilitiesSnapshot() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.hostCapabilities...)
}

// SetGoConfig 注入插件编译的 Go 包仓库地址（goproxy）与额外构建参数（goflags），
// 转发给内部的 Compiler。
func (m *SubprocessManager) SetGoConfig(goproxy, goflags string) {
	if m.compiler != nil {
		m.compiler.SetGoConfig(goproxy, goflags)
	}
}

// GoInstall installs a Go module/binary into the toolchain's GOPATH/bin using
// the bundled (or system) go toolchain — the Go equivalent of "pip install".
// pkg may include a version suffix (e.g. "github.com/x/y@latest"); an explicit
// "@latest" is appended when absent. It returns the combined command output.
func (m *SubprocessManager) GoInstall(ctx context.Context, pkg, goproxy string) (string, error) {
	if m.toolchain == nil {
		return "", fmt.Errorf("go 工具链不可用")
	}
	bin, err := m.toolchain.GoBin()
	if err != nil {
		return "", err
	}
	if pkg = strings.TrimSpace(pkg); pkg == "" {
		return "", fmt.Errorf("模块名不能为空")
	}
	if !strings.Contains(pkg, "@") {
		pkg += "@latest"
	}
	extra := map[string]string{}
	if strings.TrimSpace(goproxy) != "" {
		extra["GOPROXY"] = strings.TrimSpace(goproxy)
	}
	cmd := exec.CommandContext(ctx, bin, "install", pkg) // #nosec G204 -- 运行 go install 安装用户指定模块（插件安装核心）; nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd.Env = m.toolchain.BuildEnv(extra)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// applyGitHubProxy 对 github.com 的 git 源 URL 应用加速前缀（如 ghfast.top）。
func (m *SubprocessManager) applyGitHubProxy(source string) string {
	if m.githubProxy == "" {
		return source
	}
	if strings.HasPrefix(source, "https://github.com/") {
		return strings.TrimRight(m.githubProxy, "/") + "/" + source
	}
	return source
}

// InstallOptions configures InstallFromSource.
type InstallOptions struct {
	// IgnoreRisk skips the static-scan risk gate and installs the plugin even
	// when risky imports are found (user explicitly confirmed on the WebUI).
	IgnoreRisk bool
	// Progress receives toolchain download progress (bytes) during the first
	// plugin build, when the bundled Go has to be downloaded (~150-200MB).
	Progress func(downloaded, total int64)

	// Stage receives human-readable phase changes during install (e.g. "下载
	// C 编译器 (Clang)…" / "下载 Go 工具链…" / "编译插件…") so the WebUI can
	// show what the install is doing while progress bytes are 0.
	Stage func(text string)

	// Install source metadata persisted into the manifest so the WebUI can
	// offer reinstall / change-source. installMethod is one of "market",
	// "repository", "url" or "upload"; registryURL/marketPluginID describe a
	// marketplace binding, repo/downloadURL the actual fetch targets.
	InstallMethod  string
	RegistryURL    string
	RegistryName   string
	MarketPluginID string
	Repo           string
	DownloadURL    string

	// CCChoice carries the user's answer to a cgo C-compiler prompt (one of
	// "gcc" / "clang" / "download" / "cancel"). It is only meaningful when the
	// plugin declares cgo and the host needs to pick a compiler; empty means no
	// decision has been made yet (→ a CCompilerPromptError is returned).
	CCChoice string

	// GoChoice carries the user's answer to a Go toolchain/SDK prompt (one of
	// "download" / "cancel"). Empty means no decision has been made yet (→ a
	// RuntimePromptError with Kind RuntimePromptGoSDK is returned when the Go
	// toolchain or the plugin SDK is not resolvable).
	GoChoice string

	// GoMirror carries the user's chosen download mirror base URL for the Go
	// toolchain (one of the mirrors in the prompt response data). Empty means
	// the default/env resolution is used. Applied via toolchain.SetGoMirror
	// before the download.
	GoMirror string

	// PythonChoice carries the user's answer to a Python runtime prompt (one of
	// "download" / "cancel"). Empty means no decision has been made yet (→ a
	// RuntimePromptError with Kind RuntimePromptPython is returned when no
	// usable CPython runtime can be prepared during install).
	PythonChoice string

	// PythonMirror carries the user's chosen download mirror prefix for CPython
	// (one of the mirrors in the prompt response data). Empty means the
	// default/env resolution is used. Applied via pysdk.SetPythonMirror before
	// the download.
	PythonMirror string
}

// RiskError is returned by InstallFromSource when the static scan found risky
// imports and IgnoreRisk was not set. The dashboard surfaces it to the WebUI
// so the user can review the offending code lines.
type RiskError struct {
	Findings []ScanFinding
}

func (e *RiskError) Error() string {
	return fmt.Sprintf("plugin source contains %d risky import(s)", len(e.Findings))
}

// InstallFromSource downloads a plugin's Go source, statically scans it,
// compiles it with the bundled toolchain and loads it. The compiled artifact
// and install record are persisted so a restart can reload from cache.
//
// source may be a git URL, an archive URL (.zip/.tar.gz/.tgz), or a local
// directory. When the static scan finds risky imports and IgnoreRisk is not
// set, a *RiskError with the offending code locations is returned and nothing
// is installed. When the plugin declares cgo and the host must pick a C
// compiler, a *CCompilerPromptError is returned so the caller can ask the user
// and retry with opts.CCChoice set.
func (m *SubprocessManager) InstallFromSource(ctx context.Context, id, source string, opts InstallOptions) (*PluginInstance, error) {
	if id == "" {
		return nil, fmt.Errorf("plugin id cannot be empty")
	}
	if m.Get(id) != nil {
		return nil, fmt.Errorf("plugin %s already installed (reload or uninstall first)", id)
	}

	// 优先使用市场提供的 download_url（zip 直链）下载；否则回退 source
	// （git 仓库 URL 走 git clone）。对齐 Python updater：有 download_url 时
	// 直接下载安装包，避免在无 git 环境（如 Termux/Android）下克隆失败。
	fetchSource := source
	if url := strings.TrimSpace(opts.DownloadURL); url != "" && isArchiveURL(url) {
		fetchSource = url
	}
	srcDir, err := m.fetchSource(ctx, id, fetchSource)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(srcDir)

	// Plugin packages must ship metadata.json or metadata.yaml (identity) at
	// their root. Go plugins additionally need main.go; Python plugins need
	// main.py or a package __init__.py. 语言按入口文件判断（main.py/
	// __init__.py → python，否则 go），不依赖 metadata 声明。
	meta, err := ReadPluginMetadata(srcDir)
	if err != nil {
		return nil, err
	}
	lang := ResolveLanguage(srcDir)

	// 稳定 id：插件名 + language（PluginIDFromMeta）。来源推导 id（带版本/
	// commit，如 astrbot-plugin-xxx-4.11.2-<commit>）在更新后变化，导致重装
	// （不勾清除配置/数据）时数据目录变成全新的。稳定 id 让重装后配置
	// （按 name）与数据目录（按 id）都能保留。
	if stableID := PluginIDFromMeta(meta, lang); stableID != "" && stableID != id {
		if m.Get(stableID) != nil {
			return nil, fmt.Errorf("plugin %s already installed (reload or uninstall first)", stableID)
		}
		// 旧来源 id 的数据目录迁移到稳定 id：重装已装插件时保留运行时数据。
		m.migratePluginData(id, stableID)
		id = stableID
	}

	if lang == "python" {
		return m.installPythonSource(ctx, id, srcDir, source, meta, opts)
	}
	if err := ensureMainGo(srcDir); err != nil {
		return nil, err
	}

	findings, err := StaticScan(srcDir)
	if err != nil {
		return nil, fmt.Errorf("static scan: %w", err)
	}
	if len(findings) > 0 && !opts.IgnoreRisk {
		return nil, &RiskError{Findings: findings}
	}

	// cgo plugin → resolve the C compiler first (may surface a user prompt).
	var cc, cxx string
	if meta.RequiresCgo() {
		cc, cxx, err = ensureCCompiler(ctx, opts)
		if err != nil {
			var promptErr *CCompilerPromptError
			if errors.As(err, &promptErr) {
				return nil, promptErr
			}
			return nil, fmt.Errorf("cgo 插件需要 C 编译器: %w", err)
		}
	}

	// Go 工具链/SDK 探测：不可用且用户未决定时返回 RuntimePromptError（前端
	// 弹窗询问是否下载），"download" 自动下载工具链并把 SDK 拉进模块缓存。
	if err := m.ensureGoInstallReady(ctx, opts); err != nil {
		return nil, err
	}

	if opts.Stage != nil {
		opts.Stage("准备编译插件…")
	}
	// 插件本体（源码树）持久化到 data/plugins/<id>：与 Python 插件同布局
	// （本体/文档/logo 统一位置，按 id = name_language 隔离），后续编译与
	// 文档缓存都从这里取。
	srcDest := filepath.Join(m.dataDir, "plugins", sanitizeID(id))
	if err := os.RemoveAll(srcDest); err != nil {
		return nil, err
	}
	if err := copyDir(srcDir, srcDest); err != nil {
		return nil, fmt.Errorf("拷贝插件源码: %w", err)
	}
	if err := m.compiler.Prepare(srcDest, goModuleNameOf(srcDest, meta)); err != nil {
		return nil, fmt.Errorf("prepare module: %w", err)
	}
	if err := m.compiler.Vet(ctx, srcDest); err != nil {
		return nil, fmt.Errorf("go vet: %w", err)
	}

	if opts.Stage != nil {
		if cc != "" {
			opts.Stage("使用 C 编译器 (cgo) 编译插件…")
		} else {
			opts.Stage("编译插件…")
		}
	}
	artifact := filepath.Join(m.dataDir, "plugins-bin", sanitizeID(id), artifactName(id))
	lineCb := func(line string) {
		line = strings.TrimSpace(line)
		if line == "" {
			return
		}
		if opts.Stage != nil {
			opts.Stage(line)
		}
	}
	if err := m.compiler.BuildWithProgressOut(ctx, srcDest, artifact, opts.Progress, cc, cxx, lineCb); err != nil {
		return nil, fmt.Errorf("build plugin %s: %w", id, err)
	}

	// 尾段持 per-plugin 生命周期锁：与 Uninstall 互斥，防止并发"安装+卸载"
	// 时 Uninstall 清理完成后这里又重建 manifest 条目（插件"复活"）。
	unlock := m.lockOp(id)
	defer unlock()

	inst, err := m.loadLocked(ctx, id, artifact, "go")
	if err != nil {
		return nil, err
	}
	// metadata.json is the canonical identity: override the runtime-reported
	// name/version so the WebUI shows the packaged metadata.
	if meta.Name != "" {
		inst.Name = meta.Name
	}
	if meta.Version != "" {
		inst.Version = meta.Version
	}
	inst.DisplayName = meta.DisplayName
	inst.ShortDesc = meta.ShortDesc
	// repo 回退：本地/URL 安装时 opts.Repo 为空，用 metadata 声明的 repo
	//（Python 插件 metadata.yaml 必带 repo），避免 WebUI "在GitHub中查看仓库"
	// 按钮指向本地安装路径。
	if opts.Repo == "" && meta.Repo != "" {
		opts.Repo = meta.Repo
	}
	if err := m.recordInstall(inst, source, artifact, opts); err != nil {
		logger.I18nWarn("插件 %s 已安装但 manifest 持久化失败: %v", id, err)
	}
	m.cachePluginDocs(inst.ID, srcDest, meta)
	m.writeMetadataConfig(inst.ID, meta)
	return inst, nil
}

// installPythonSource installs a Python plugin: copies the source tree into
// data/plugins/<id> (the "binary" the runtime launches), optionally
// installs requirements.txt into the Python venv, then loads it.
func (m *SubprocessManager) installPythonSource(ctx context.Context, id, srcDir, source string, meta *PluginMetadata, opts InstallOptions) (*PluginInstance, error) {
	if err := ensurePythonEntry(srcDir); err != nil {
		return nil, err
	}
	if opts.Stage != nil {
		opts.Stage("准备 Python 插件…")
	}
	// 安装路径先解析 Python 运行时（可能触发下载/venv），运行时无法准备且
	// 用户未决定时返回 RuntimePromptError（前端弹窗询问是否下载 CPython）。
	// 运行期加载（startInstance）保持 pythonRuntime 原行为，不弹窗。
	env, err := m.pythonRuntimeForInstall(opts)
	if err != nil {
		return nil, err
	}
	dest := filepath.Join(m.dataDir, "plugins", sanitizeID(id))
	if err := os.RemoveAll(dest); err != nil {
		return nil, err
	}
	if err := copyDir(srcDir, dest); err != nil {
		return nil, fmt.Errorf("拷贝 Python 插件源码: %w", err)
	}

	// requirements.txt → pip install（使用 Python venv；失败仅告警，插件仍可加载）
	req := filepath.Join(dest, "requirements.txt")
	if _, err := os.Stat(req); err == nil {
		if opts.Stage != nil {
			opts.Stage("安装 Python 依赖 (requirements.txt)…")
		}
		if err := m.pipInstall(env, dest, req); err != nil {
			logger.I18nWarn("插件 %s 依赖安装失败: %v（插件可能缺少依赖）", id, err)
		}
	}

	unlock := m.lockOp(id)
	defer unlock()

	inst, err := m.loadLocked(ctx, id, dest, "python")
	if err != nil {
		return nil, err
	}
	if meta.Name != "" {
		inst.Name = meta.Name
	}
	if meta.Version != "" {
		inst.Version = meta.Version
	}
	inst.DisplayName = meta.DisplayName
	inst.ShortDesc = meta.ShortDesc
	// repo 回退：本地/URL 安装时 opts.Repo 为空，用 metadata 声明的 repo
	//（Python 插件 metadata.yaml 必带 repo），避免 WebUI "在GitHub中查看仓库"
	// 按钮指向本地安装路径。
	if opts.Repo == "" && meta.Repo != "" {
		opts.Repo = meta.Repo
	}
	if err := m.recordInstall(inst, source, dest, opts); err != nil {
		logger.I18nWarn("插件 %s 已安装但 manifest 持久化失败: %v", id, err)
	}
	m.cachePluginDocs(inst.ID, srcDir, meta)
	m.writeMetadataConfig(inst.ID, meta)
	return inst, nil
}

// pipInstall runs `pip install -r requirements.txt` inside the plugin's source
// directory so relative dependencies resolve; the pip index honors
// ASTRBOT_PYPI_INDEX (or PIP_INDEX_URL). All paths are made absolute because
// the subprocess cwd differs from the host's.
func (m *SubprocessManager) pipInstall(env *pysdk.RuntimeEnv, pluginDir, req string) error {
	if abs, err := filepath.Abs(req); err == nil {
		req = abs
	}
	if abs, err := filepath.Abs(pluginDir); err == nil {
		pluginDir = abs
	}
	args := []string{"-m", "pip", "install", "--disable-pip-version-check", "-q", "-r", req}
	// config pip_install_arg（额外参数）→ 追加；config pypi_index_url 或默认镜像。
	if len(m.pipArgs) > 0 {
		args = append(args, m.pipArgs...)
	}
	index := m.pipIndex
	if index == "" {
		index = pysdk.PyPIIndex()
	}
	args = append(args, "-i", index)
	cmd := exec.Command(env.PythonBin, args...) // #nosec G204 -- pip 安装插件依赖（参数来自插件配置）; nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd.Dir = pluginDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensurePythonEntry guards the requirement that Python plugin packages have a
// loadable entry (main.py or a package __init__.py).
func ensurePythonEntry(srcDir string) error {
	if _, err := os.Stat(filepath.Join(srcDir, "main.py")); err == nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(srcDir, "__init__.py")); err == nil {
		return nil
	}
	return fmt.Errorf("python 插件源码缺少 main.py 或 __init__.py 入口")
}

// cachePluginDocs copies the plugin's README.md, CHANGELOG.md and logo image
// from the fetched source into its 本体目录（data/plugins/<id>，与源码同目录）
// so the WebUI readme/changelog endpoints and the plugin logo endpoint can
// serve them (mirrors Python's plugin_dir/README.md lookup).
func (m *SubprocessManager) cachePluginDocs(id, srcDir string, meta *PluginMetadata) {
	dir := filepath.Join(m.dataDir, "plugins", sanitizeID(id))
	_ = os.MkdirAll(dir, 0o755) // #nosec G301 -- 插件文档缓存目录（WebUI 需读取）
	for _, src := range []string{"README.md", "readme.md", "CHANGELOG.md", "changelog.md"} {
		content, err := os.ReadFile(filepath.Join(srcDir, src)) // #nosec G304 -- 读取插件源码内固定文件名文档
		if err != nil {
			continue
		}
		dst := filepath.Join(dir, src)
		// #nosec G306 -- 文档缓存非常规敏感信息
		if err := os.WriteFile(dst, content, 0o644); err != nil {
			logger.I18nWarn("缓存插件 %s 的文档 %s 失败: %v", id, src, err)
		}
	}
	// Logo：metadata 声明的 logo_path 优先，其次根目录常见文件名
	// （Python AstrBot 插件惯例 logo.png）。文件拷入 plugins/<name>/ 目录，
	// 由 dashboard /api/v1/plugins/logo 端点提供。
	candidates := []string{}
	if meta != nil && strings.TrimSpace(meta.LogoPath) != "" {
		candidates = append(candidates, strings.TrimSpace(meta.LogoPath))
	}
	candidates = append(candidates, []string{"logo.png", "logo.jpg", "logo.jpeg", "logo.gif", "icon.png"}...)
	for _, rel := range candidates {
		if rel == "" || strings.Contains(rel, "..") || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "\\") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(srcDir, filepath.FromSlash(rel))) // #nosec G304 -- rel 已过滤 "../" 与绝对路径
		if err != nil {
			continue
		}
		base := filepath.Base(rel)
		// #nosec G306 -- Logo 缓存非常规敏感信息
		if err := os.WriteFile(filepath.Join(dir, base), content, 0o644); err != nil {
			logger.I18nWarn("缓存插件 %s 的 Logo %s 失败: %v", id, rel, err)
			continue
		}
		break
	}
}

// PluginLogoFile returns the cached logo file path for a plugin id ("" when
// the plugin has no cached logo). The dashboard logo endpoint uses it.
func (m *SubprocessManager) PluginLogoFile(id string) string {
	dir := filepath.Join(m.dataDir, "plugins", sanitizeID(id))
	for _, n := range []string{"logo.png", "logo.jpg", "logo.jpeg", "logo.gif", "icon.png"} {
		p := filepath.Join(dir, n)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

// recordInstall upserts the plugin into the persisted install manifest.
func (m *SubprocessManager) recordInstall(inst *PluginInstance, source, artifact string, opts InstallOptions) error {
	// 串行化 manifest 的读→改→写，防止并发 Install/SetEnabled 互相覆盖丢条目。
	m.manifestMu.Lock()
	defer m.manifestMu.Unlock()
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		return err
	}
	man.Upsert(ManifestEntry{
		ID:             inst.ID,
		Name:           inst.Name,
		Version:        inst.Version,
		Source:         source,
		Binary:         artifact,
		Enabled:        true,
		Language:       inst.Language,
		DisplayName:    inst.DisplayName,
		ShortDesc:      inst.ShortDesc,
		InstallMethod:  opts.InstallMethod,
		RegistryURL:    opts.RegistryURL,
		RegistryName:   opts.RegistryName,
		MarketPluginID: opts.MarketPluginID,
		Repo:           opts.Repo,
		DownloadURL:    opts.DownloadURL,
		// 记录插件在 data 下创建的目录，供卸载时精确清理。目录一律按插件
		// 实例 id（name_language）分键：同名 Go/Python 插件的本体（plugins/）、
		// 配置（plugins_config/）、数据（plugins_data/）完全隔离。
		ConfigDir: filepath.Join("plugins_config", sanitizeID(inst.ID)),
		DataDir:   filepath.Join("plugins_data", sanitizeID(inst.ID)),
		DocsDir:   filepath.Join("plugins", sanitizeID(inst.ID)),
	})
	return man.Save(m.manifestPath())
}

// manifestPath returns the persisted install manifest location.
func (m *SubprocessManager) manifestPath() string {
	return filepath.Join(m.dataDir, "plugins-manifest.json")
}

// Load launches a compiled plugin binary (or Python source tree) as a child
// process and registers it under id. Already-loaded ids return the existing
// instance. It holds the per-plugin lifecycle lock so a concurrent Uninstall
// cannot unload/remove the plugin in the middle of its registration window.
func (m *SubprocessManager) Load(ctx context.Context, id, binary string) (*PluginInstance, error) {
	return m.LoadLang(ctx, id, binary, "")
}

// LoadLang is Load with an explicit language ("go" / "python"; empty means
// "go").
func (m *SubprocessManager) LoadLang(ctx context.Context, id, binary, language string) (*PluginInstance, error) {
	if id == "" {
		return nil, fmt.Errorf("plugin id cannot be empty")
	}
	if language == "" {
		language = "go"
	}
	unlock := m.lockOp(id)
	defer unlock()
	return m.loadLocked(ctx, id, binary, language)
}

// loadLocked is Load's body; the caller must hold the per-plugin lifecycle lock
// for id (m.lockOp).
func (m *SubprocessManager) loadLocked(ctx context.Context, id, binary, language string) (*PluginInstance, error) {
	m.mu.RLock()
	if inst, ok := m.instances[id]; ok {
		m.mu.RUnlock()
		return inst, nil
	}
	m.mu.RUnlock()

	inst, err := m.startInstance(ctx, id, binary, language)
	if err != nil {
		return nil, err
	}
	// 落盘 config schema 缓存，供插件禁用后仍能渲染配置对话框。
	if inst.Meta != nil {
		m.cacheConfigSchema(inst.ID, inst.Meta)
	}

	m.mu.Lock()
	if existing, ok := m.instances[id]; ok {
		m.mu.Unlock()
		go m.teardownInstance(inst)
		return existing, nil
	}
	delete(m.failures, id)
	m.instances[id] = inst
	m.mu.Unlock()

	m.startWatch(inst)
	logger.I18nInfo("插件 %s 已从 %s 加载 (v%s)", id, inst.Binary, inst.Version)
	// 通知所有已加载插件：新插件加载完成（on_plugin_loaded）。
	m.TriggerHookPayload(ctx, pluginsdk.EventOnPluginLoaded, map[string]string{"plugin_name": inst.Name})
	return inst, nil
}

// Reload restarts a plugin with zero downtime: start the new process first,
// swap it in, then stop the old one. The per-plugin lifecycle lock serializes
// it against concurrent crash-restarts and unloads so no instance is orphaned.
func (m *SubprocessManager) Reload(ctx context.Context, id string) error {
	unlock := m.lockOp(id)
	defer unlock()

	m.mu.RLock()
	old, ok := m.instances[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("plugin %s not loaded", id)
	}

	newInst, err := m.startInstance(ctx, id, old.Binary, old.Language)
	if err != nil {
		return fmt.Errorf("reload plugin %s: %w", id, err)
	}

	// 防御性身份比对：锁内理论上不会被并发覆盖，但防止未来改动遗漏。
	m.mu.Lock()
	if cur, ok := m.instances[id]; !ok || cur != old {
		m.mu.Unlock()
		go m.teardownInstance(newInst)
		return fmt.Errorf("plugin %s changed while reloading", id)
	}
	m.instances[id] = newInst
	delete(m.failures, id)
	m.mu.Unlock()

	old.mu.Lock()
	old.stopped = true
	old.mu.Unlock()
	go m.teardownInstance(old)

	m.startWatch(newInst)
	logger.I18nInfo("插件 %s 已重载 (v%s)", id, newInst.Version)
	return nil
}

// Unload stops a plugin process; the OS fully reclaims its resources. The
// per-plugin lifecycle lock blocks a concurrent crash-restart in its start
// window, so the plugin cannot "come back" after being disabled.
func (m *SubprocessManager) Unload(id string) error {
	unlock := m.lockOp(id)
	defer unlock()
	return m.unloadLocked(id, true)
}

// UnloadIdle stops an idle-sleeping plugin process but KEEPS its star-pipeline
// handlers registered: the handlers lazily re-load the plugin on the next
// triggered call (resolveActive → EnsureLoaded), so a sleeping plugin always
// wakes up. 手动卸载/禁用走 Unload（notifyChanged 移除 handler）。
func (m *SubprocessManager) UnloadIdle(id string) error {
	unlock := m.lockOp(id)
	defer unlock()
	return m.unloadLocked(id, false)
}

// unloadLocked is Unload/UnloadIdle's body; the caller must hold the
// per-plugin lifecycle lock for id (m.lockOp). notify=true fires
// OnInstancesChanged so the host re-bridges (removes) the handlers; false
// keeps them for lazy reload.
func (m *SubprocessManager) unloadLocked(id string, notify bool) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("plugin %s not loaded", id)
	}
	delete(m.instances, id)
	m.mu.Unlock()

	// 通知其余已加载插件：某插件被卸载（on_plugin_unloaded）。已从
	// instances 中删除，被卸载插件自身不会收到。
	m.TriggerHookPayload(context.Background(), pluginsdk.EventOnPluginUnloaded, map[string]string{"plugin_name": inst.Name})

	inst.mu.Lock()
	inst.stopped = true
	inst.mu.Unlock()
	m.teardownInstance(inst)
	// 子进程已终止，其 Python 侧 SessionWaiter 状态随之丢失：注销该插件
	// 的全部会话等待（休眠卸载与真实卸载都适用，防止向死进程反复推送）。
	m.unregisterPluginWaits(inst.Name)
	if notify {
		// 真实卸载/禁用：工具注册表条目与 handler 元数据一并清除（休眠则
		// 保留，前者供按名唤醒、后者供 Rebridge 重建休眠插件 handler）。
		m.removePluginTools(id)
		m.removeHandlerMeta(id)
		m.removePluginBridgeHooks(id)
		logger.I18nInfo("插件 %s 已卸载", id)
		m.notifyChanged()
	} else {
		logger.I18nInfo("插件 %s 已休眠（闲置卸载，触发时自动唤醒）", id)
	}
	return nil
}

// Get returns the running instance for id, or nil.
func (m *SubprocessManager) Get(id string) *PluginInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.instances[id]
}

// SetIdleUnload enables the idle-unload sweep: plugin processes with no RPC
// activity for longer than idle are unloaded so the OS reclaims their memory
// (lazy reload happens on the next triggered call via EnsureLoaded).
// idle <= 0 disables the sweep. Cross-platform: works with any plugin process
// (Go binary / Python interpreter) since it only manages process lifecycles.
func (m *SubprocessManager) SetIdleUnload(idle time.Duration) {
	m.mu.Lock()
	prev := m.idleUnload
	m.idleUnload = idle
	start := prev <= 0 && idle > 0
	m.mu.Unlock()
	if start {
		go m.idleSweepLoop()
		logger.I18nInfo("插件闲置自动卸载已启用（闲置 %v 后回收进程内存）", idle)
	}
}

// IdleUnloadEnabled reports whether the idle-unload sweep is enabled (global
// switch), consumed by the WebUI behavior page.
func (m *SubprocessManager) IdleUnloadEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.idleUnload > 0
}

// IdleUnloadMinutes returns the configured idle threshold in minutes (0 =
// disabled), consumed by the WebUI behavior page.
func (m *SubprocessManager) IdleUnloadMinutes() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return int(m.idleUnload / time.Minute)
}

// SetIdleUnloadBlocked marks a plugin as exempt from the idle sweep (blocked =
// true keeps it resident; false re-allows idle sleep). Persisted in the
// manifest so the setting survives restarts.
func (m *SubprocessManager) SetIdleUnloadBlocked(id string, blocked bool) error {
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
	e.IdleUnloadBlocked = blocked
	if err := man.Save(m.manifestPath()); err != nil {
		return err
	}
	if blocked {
		logger.I18nInfo("插件 %s 已设置为常驻（不参与闲置自动休眠）", id)
	} else {
		logger.I18nInfo("插件 %s 已允许闲置自动休眠", id)
	}
	return nil
}

// IdleUnloadBlocked reports whether the plugin is exempt from the idle sweep.
func (m *SubprocessManager) IdleUnloadBlocked(id string) bool {
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		return false
	}
	if e := man.Get(id); e != nil {
		return e.IdleUnloadBlocked
	}
	return false
}

// idleSweepLoop periodically unloads idle plugin processes.
func (m *SubprocessManager) idleSweepLoop() {
	interval := m.scanInterval
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.sweepIdlePlugins()
		}
	}
}

// SweepIdle immediately runs the idle-unload sweep once (also invoked by the
// background loop). Exported for tests and dashboard-triggered sweeps.
func (m *SubprocessManager) SweepIdle() {
	m.sweepIdlePlugins()
}

// sweepIdlePlugins unloads every loaded plugin that has been idle longer than
// the configured threshold. Unload marks the instance stopped (no crash
// restart) and triggers OnInstancesChanged so the host re-bridges handlers.
func (m *SubprocessManager) sweepIdlePlugins() {
	m.mu.RLock()
	idle := m.idleUnload
	insts := make([]*PluginInstance, 0, len(m.instances))
	for _, inst := range m.instances {
		insts = append(insts, inst)
	}
	m.mu.RUnlock()
	if idle <= 0 {
		return
	}
	now := time.Now()
	for _, inst := range insts {
		if m.IdleUnloadBlocked(inst.ID) {
			continue // 常驻插件（WebUI 行为页勾选"不允许休眠"）不参与清扫
		}
		if inst.IsIdle(now, idle) {
			logger.I18nInfo("插件 %s 已闲置 %v，自动休眠（内存已回收，下次触发时自动唤醒）",
				inst.ID, now.Sub(inst.LastActive()).Round(time.Second))
			if err := m.UnloadIdle(inst.ID); err != nil && err.Error() != fmt.Sprintf("plugin %s not loaded", inst.ID) {
				logger.I18nWarn("闲置休眠插件 %s 失败: %v", inst.ID, err)
			}
		}
	}
}

// EnsureLoaded returns the running instance for id, lazily re-loading it from
// the manifest when it was previously unloaded (idle sweep or manual unload).
// This is the lazy-load half of the process-pool lifecycle: a triggered plugin
// that is not running is brought back on demand. On success it fires
// OnInstancesChanged so the host re-bridges the handlers.
func (m *SubprocessManager) EnsureLoaded(ctx context.Context, id string) (*PluginInstance, error) {
	if inst := m.Get(id); inst != nil {
		return inst, nil
	}
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		return nil, fmt.Errorf("读取插件清单: %w", err)
	}
	e := man.Get(id)
	if e == nil || !e.Enabled {
		return nil, fmt.Errorf("插件 %s 未安装或已被禁用", id)
	}
	lang := e.Language
	if lang == "" {
		lang = "go"
	}
	inst, err := m.LoadLang(ctx, id, e.Binary, lang)
	if err != nil {
		return nil, fmt.Errorf("唤醒插件 %s: %w", id, err)
	}
	// 注意：不触发 OnInstancesChanged——闲置休眠保留着 star handler，
	// 调用方（resolveActive）直接用返回的新实例 RPC，handler 无需重建；
	// 避免在 handler 执行中并发重建 handler 表。
	return inst, nil
}

// List returns all running plugin instances.
func (m *SubprocessManager) List() []*PluginInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*PluginInstance, 0, len(m.instances))
	for _, inst := range m.instances {
		out = append(out, inst)
	}
	return out
}

// RegisteredPlugins 返回所有已加载过（含闲置休眠）的插件：运行中的返回
// 真实实例；休眠插件返回仅含 ID+Meta 的占位实例（Client 为 nil，由 star
// handler 经 resolveActive 懒加载唤醒，不依赖 inst.Client）。供
// RebridgePlugins 一次性重建全部插件 handler，保证休眠插件指令/过滤器/钩子
// 在任意一次 Rebridge（启用/卸载/重载其它插件）后依然注册。
func (m *SubprocessManager) RegisteredPlugins() []*PluginInstance {
	m.mu.RLock()
	running := make(map[string]*PluginInstance, len(m.instances))
	for id, inst := range m.instances {
		running[id] = inst
	}
	m.mu.RUnlock()

	m.handlerMetaMu.RLock()
	ids := make([]string, 0, len(m.handlerMeta))
	for id := range m.handlerMeta {
		ids = append(ids, id)
	}
	m.handlerMetaMu.RUnlock()

	out := make([]*PluginInstance, 0, len(ids))
	for _, id := range ids {
		if inst, ok := running[id]; ok {
			out = append(out, inst)
			continue
		}
		meta := m.HandlerMetaByID(id)
		if meta == nil {
			continue
		}
		out = append(out, &PluginInstance{
			ID:   id,
			Name: meta.Name,
			Meta: meta,
		})
	}
	return out
}

// Clients returns the RPC client of every running plugin (for the star bridge).
func (m *SubprocessManager) Clients() map[string]*pluginsdk.Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]*pluginsdk.Client, len(m.instances))
	for id, inst := range m.instances {
		out[id] = inst.Client
	}
	return out
}

// Failed returns plugins that crashed too many times and were taken down.
func (m *SubprocessManager) Failed() map[string]error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]error, len(m.failures))
	for id, err := range m.failures {
		out[id] = err
	}
	return out
}

// Shutdown stops the manager and all plugin processes. Safe to call once at
// application exit.
func (m *SubprocessManager) Shutdown() {
	m.cancel()
	m.mu.Lock()
	insts := make([]*PluginInstance, 0, len(m.instances))
	for _, inst := range m.instances {
		insts = append(insts, inst)
	}
	m.instances = make(map[string]*PluginInstance)
	// 代际自增：在途 restart 写回 map 前会对比 gen，发现表已被换掉则丢弃
	// 新实例并 teardown，避免实例落入无人回收的新表。
	m.gen++
	m.mu.Unlock()
	m.toolRegMu.Lock()
	m.toolRegistry = make(map[string]toolRegEntry)
	m.toolRegMu.Unlock()
	m.bridgeHooksMu.Lock()
	m.bridgeHooks = make(map[string]map[string]struct{})
	m.bridgeHooksMu.Unlock()

	for _, inst := range insts {
		inst.mu.Lock()
		inst.stopped = true
		inst.mu.Unlock()
		m.teardownInstance(inst)
	}
	logger.I18nInfo("子进程插件管理器已关闭 (%d 个插件已停止)", len(insts))
}

// SetAutoRestart enables/disables automatic crash restarts.
// 保持导出字段为普通 bool 以便现有调用方直接赋值（如测试），读写统一走
// m.mu 加锁同步，避免与 handleExit 的读取产生数据竞争。
func (m *SubprocessManager) SetAutoRestart(enabled bool) {
	m.mu.Lock()
	m.AutoRestart = enabled
	m.mu.Unlock()
}

// startInstance launches one plugin subprocess (compiled Go binary or Python
// source tree) and performs the handshake + first Register call. On any
// failure the process is killed and resources released.
// It holds startInstanceMu for its whole lifetime so the SDK-side process-global
// go-plugin 握手端口全局分配器：为每个插件实例分配独占单端口。
// 背景：go-plugin 默认端口范围（10000-25000）下多个插件进程会尝试绑定同一
// 起始端口，而 gRPC 的 SO_REUSEPORT 允许双绑 → 宿主连接可能被内核路由到
// 错误的插件进程（Register 元数据串台，如 box 返回 meme_manager）。分配器
// 必须是进程级全局（不能 per-manager），否则并发实例/测试管理器会重复分配。
var (
	globalPortMu   sync.Mutex
	globalPortUsed = map[int]struct{}{}
)

// allocPluginPort 分配一个全局唯一的握手端口（min=max=单端口），从 base
// 起向上扫描第一个未使用端口。base<=0 时用 go-plugin 默认起始值 10000。
// 除进程内已分配记录外，还会检测端口当前是否被监听（孤儿插件进程、其他
// 服务的残留监听），占用则跳过——否则 SO_REUSEPORT 双绑会让宿主连接被
// 内核路由到错误的进程（Register 元数据串台）。
func allocPluginPort(base int) (uint, uint) {
	globalPortMu.Lock()
	defer globalPortMu.Unlock()
	if base <= 0 {
		base = 10000
	}
	p := base
	for {
		if _, used := globalPortUsed[p]; !used && !portInUse(p) {
			globalPortUsed[p] = struct{}{}
			return uint(p), uint(p) // #nosec G115 -- 端口从 base(≥1) 起向上扫描，int→uint 不溢出
		}
		p++
	}
}

// portInUse 探测 127.0.0.1:p 是否已被监听（绑定成功=空闲；失败=被占）。
func portInUse(p int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	if err != nil {
		return true
	}
	_ = l.Close()
	return false
}

// allocPortRange 从配置的 MinPort 起分配（测试/显式端口范围场景）。
func (m *SubprocessManager) allocPortRange() (uint, uint) {
	return allocPluginPort(m.MinPort)
}

// allocPortRangeDefault 从默认起始端口（10000）起分配（生产默认）。
func (m *SubprocessManager) allocPortRangeDefault() (uint, uint) {
	return allocPluginPort(0)
}

// hostPluginID cannot be clobbered by a concurrently loading plugin.
func (m *SubprocessManager) startInstance(ctx context.Context, id, binary, language string) (*PluginInstance, error) {
	startInstanceMu.Lock()
	defer startInstanceMu.Unlock()

	abs, err := filepath.Abs(binary)
	if err != nil {
		return nil, fmt.Errorf("resolve binary: %w", err)
	}
	if info, err := os.Stat(abs); err != nil || (info.IsDir() && language != "python") {
		return nil, fmt.Errorf("plugin binary not found: %s", abs)
	}

	// 插件子进程工作目录设为统一数据根目录 data/plugins_data/<id>，插件写相对
	// 路径的运行时数据（修仙存档、表情库等）自动落盘于此，便于管理/备份/卸载。
	pluginDataRoot := m.pluginDataRoot(id)
	// #nosec G301 -- 插件数据目录（用户态）
	if err := os.MkdirAll(pluginDataRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create plugin data dir: %w", err)
	}

	// Python 插件：python3 -m astrbot._bridge.server <源码目录>
	if language == "python" {
		env, err := m.pythonRuntime()
		if err != nil {
			return nil, fmt.Errorf("python runtime: %w", err)
		}
		// venv 可能刚被重建（缓存失效/外部清理）：宿主基础依赖重装后，插件
		// 自身 requirements.txt 不会自动重装。每次启动前若源码目录有
		// requirements.txt 则尝试安装——装过时 pip 命中缓存秒回，开销可忽略；
		// 缺依赖插件也能加载（加载失败会清晰报 ModuleNotFoundError 而非
		// 挂死）。失败仅告警，不阻止启动（与安装路径 installPythonSource 一致）。
		if req := filepath.Join(abs, "requirements.txt"); func() bool {
			_, err := os.Stat(req)
			return err == nil
		}() {
			if err := m.pipInstall(env, abs, req); err != nil {
				logger.I18nWarn("插件 %s 依赖安装失败: %v（插件可能缺少依赖）", id, err)
			}
		}
		cmd := exec.Command(env.PythonBin, "-m", "astrbot._bridge.server", abs) // #nosec G204 -- 启动插件进程（插件系统核心）; nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
		cmd.Dir = pluginDataRoot
		cmd.Env = env.Env(abs, m.dataDir)
		// 宿主能力注入：Python 插件经 HostBridge.has() 查询宿主公开了哪些
		// 平台/能力（ASTRBOT_HOST_CAPABILITIES）。未设置能力（空集）时不注入
		// 变量，插件侧容错为空集。
		if caps := m.hostCapabilitiesSnapshot(); len(caps) > 0 {
			cmd.Env = append(cmd.Env, "ASTRBOT_HOST_CAPABILITIES="+strings.Join(caps, ","))
		}
		// per-plugin 日志级别：Python 桥启动时经 ASTRBOT_PLUGIN_LOG_LEVEL
		// 设置 root logger 过滤级别（对齐 Python effective = 覆盖 || 全局）。
		if lvl := m.logLevels.EffectivePluginLogLevel(id); lvl != "" {
			cmd.Env = append(cmd.Env, "ASTRBOT_PLUGIN_LOG_LEVEL="+lvl)
		}
		// Python 插件的 stderr 走 [ASTRBOT] 协议解析器（go-plugin 逐行转发
		// 到 cfg.Stderr）：启动失败时能把 go-plugin 笼统的握手错误提升为
		// phase 化的清晰错误。Go 插件保持 go-plugin 默认行为（parser=nil）。
		return m.dispensePlugin(ctx, id, abs, language, cmd, newAstrbotStartupParser())
	}

	cmd := exec.Command(abs) // #nosec G204 -- 启动 Go 插件可执行文件（插件系统核心）; nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd.Dir = pluginDataRoot
	return m.dispensePlugin(ctx, id, abs, language, cmd, nil)
}

// pythonRuntime resolves (once) the Python subprocess environment: SDK
// extraction + venv/grpcio preparation + (optionally) downloading a bundled
// Python when the system has none. The first Python plugin load may take a
// while (download / venv creation + pip install).
func (m *SubprocessManager) pythonRuntime() (*pysdk.RuntimeEnv, error) {
	return m.pythonRuntimeWithStage(nil)
}

// pythonRuntimeWithStage is pythonRuntime with a stage callback surfaced to
// the WebUI install dialog (e.g. "下载 Python 解释器…").
func (m *SubprocessManager) pythonRuntimeWithStage(stage func(string)) (*pysdk.RuntimeEnv, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pythonEnv != nil {
		// 缓存校验：venv/解释器可能被外部清理（如 ~/.cache 被系统回收、
		// 用户手动删除），缓存命中但解释器/SDK 目录已不存在时丢弃缓存
		// 重新准备——否则插件闲置休眠后唤醒（EnsureLoaded → startInstance）
		// 会拿一个不存在的解释器启动子进程（"python-venv-xxx/bin/python
		// 路径不存在"），LLM 工具调用（executePluginTool）随之失败。
		if pythonEnvUsable(m.pythonEnv) {
			return m.pythonEnv, nil
		}
		logger.I18nWarn("Python 运行时缓存失效（解释器/SDK 目录不存在），重新准备…")
		m.pythonEnv = nil
	}
	// 宿主 venv 基础依赖安装（pysdk 内部 pip）也用 config 的 PyPI 镜像。
	pysdk.SetPyPIIndex(m.pipIndex)
	env, err := pysdk.PrepareRuntimeWithStage(m.dataDir, stage)
	if err != nil {
		return nil, err
	}
	m.pythonEnv = env
	return env, nil
}

// pythonEnvUsable 校验缓存的 Python 运行时仍可用：解释器文件与 SDK 目录
// 必须存在。只做轻量 stat 检查（不跑 import 探测），覆盖 venv 缓存被外部
// 清理的场景；不通过时调用方丢弃缓存走 PrepareRuntimeWithStage 全量重建
// （EnsureVenv 会重建 venv 并重装宿主依赖）。
func pythonEnvUsable(env *pysdk.RuntimeEnv) bool {
	if env == nil {
		return false
	}
	info, err := os.Stat(env.PythonBin)
	if err != nil || info.IsDir() {
		return false
	}
	if sdk, err := os.Stat(env.SDKDir); err != nil || !sdk.IsDir() {
		return false
	}
	return true
}

// dispensePlugin runs the go-plugin handshake against a prepared command.
// stderrParser is non-nil for Python plugins: their stderr lines are routed
// through the [ASTRBOT] protocol parser and phase-aware startup errors.
func (m *SubprocessManager) dispensePlugin(ctx context.Context, id, abs, language string, cmd *exec.Cmd, stderrParser *astrbotStartupParser) (*PluginInstance, error) {
	// 进程组隔离（Linux 附加 Pdeathsig）：宿主死亡/退出时内核自动回收插件，
	// teardown 时按组杀整棵进程树（见 process_*.go）。
	setupChildProcess(cmd)
	cfg := &goplugin.ClientConfig{
		HandshakeConfig:  pluginsdk.Handshake,
		Plugins:          pluginsdk.PluginMap,
		Cmd:              cmd,
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		Managed:          true,
	}
	if stderrParser != nil {
		cfg.Stderr = stderrParser
	}
	// 每个插件分配独占握手端口（见 portAllocMu 注释）：避免 SO_REUSEPORT
	// 同端口双绑导致宿主连接路由到错误的插件进程。
	if m.MaxPort > 0 && m.MinPort > 0 {
		minp, maxp := m.allocPortRange()
		cfg.MinPort, cfg.MaxPort = minp, maxp
	} else {
		minp, maxp := m.allocPortRangeDefault()
		cfg.MinPort, cfg.MaxPort = minp, maxp
	}
	raw := goplugin.NewClient(cfg)

	// go-plugin's handshake has no built-in timeout; enforce one.
	type dispenseResult struct {
		pc  *pluginsdk.Client
		pid int // direct child pid once the process has started (0 = not started)
		err error
	}
	resCh := make(chan dispenseResult, 1)
	go func() {
		// 绑定当前插件身份：go-plugin Dispense 时宿主 accept HostService，
		// SDK 据此刻的当前 id 给 per-connection hostServiceServer 绑定插件名，
		// 用于 HostService 反向调用（GetConfig/SetConfig）的身份隔离。这里以
		// manifest id 为 key（与 acceptHostService 的 hostServers 记录、以及
		// Register 后的 BindHostServiceName(id, name) 查找 key 一致）；插件
		// GetConfig/SetConfig 传的是注册名（name），Register 成功后由
		// BindHostServiceName 把身份更新为注册名，二者对齐后隔离校验才能通过。
		pluginsdk.SetCurrentHostPluginID(id)
		defer pluginsdk.SetCurrentHostPluginID("")
		var pid int
		proto, err := raw.Client()
		if cmd.Process != nil {
			pid = cmd.Process.Pid
		}
		if err != nil {
			resCh <- dispenseResult{pid: pid, err: err}
			return
		}
		rpcClient, err := proto.Dispense("plugin_service")
		if err != nil {
			resCh <- dispenseResult{pid: pid, err: err}
			return
		}
		pc, ok := rpcClient.(*pluginsdk.Client)
		if !ok {
			resCh <- dispenseResult{pid: pid, err: fmt.Errorf("unexpected plugin client type %T", rpcClient)}
			return
		}
		resCh <- dispenseResult{pc: pc, pid: pid}
	}()

	var pc *pluginsdk.Client
	var pid int
	select {
	case res := <-resCh:
		if res.err != nil {
			raw.Kill()
			// 握手失败时直接子进程可能已退出（killProcessGroup 对 ESRCH 视为
			// 完成），但 Python 桥可能已拉起子进程：按组回收兜底。
			killProcessGroup(&PluginInstance{pgid: res.pid})
			return nil, m.wrapStartError(stderrParser, fmt.Errorf("start plugin %s: %w", id, res.err))
		}
		pc = res.pc
		pid = res.pid
	case <-time.After(startTimeout):
		// 杀进程先让 dispense goroutine 结束，再取回可能已创建的
		// *pluginsdk.Client（持有 gRPC conn + HostService server）并关闭，
		// 避免反复 Load 泄漏连接与 goroutine。
		raw.Kill()
		res := <-resCh
		killProcessGroup(&PluginInstance{pgid: res.pid})
		if res.pc != nil {
			_ = res.pc.Close()
		}
		return nil, m.wrapStartError(stderrParser, fmt.Errorf("start plugin %s: handshake timed out after %v", id, startTimeout))
	case <-m.ctx.Done():
		raw.Kill()
		res := <-resCh
		killProcessGroup(&PluginInstance{pgid: res.pid})
		if res.pc != nil {
			_ = res.pc.Close()
		}
		return nil, m.wrapStartError(stderrParser, fmt.Errorf("start plugin %s: manager shutting down", id))
	}

	regCtx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	meta, err := pc.Register(regCtx)
	logger.I18nInfo("startInstance %s: Register meta name=%q version=%q (pid=%d)", id,
		meta.GetName(), meta.GetVersion(), cmd.Process.Pid)
	if err != nil {
		_ = pc.Close()
		raw.Kill()
		killProcessGroup(&PluginInstance{pgid: pid})
		return nil, m.wrapStartError(stderrParser, fmt.Errorf("plugin %s Register: %w", id, err))
	}
	// 用 Register 返回的注册名更新 HostService 连接身份（accept 时只绑定
	// manifest id，name 与 id 可能不同）。之后插件 GetConfig/SetConfig 传的
	// name 与连接身份一致，身份隔离校验才能通过。
	if meta != nil && meta.Name != "" {
		pluginsdk.BindHostServiceName(id, meta.Name)
	}

	inst := &PluginInstance{
		ID:        id,
		Name:      meta.Name,
		Version:   meta.Version,
		Binary:    abs,
		Language:  language,
		Client:    pc,
		Meta:      meta,
		raw:       raw,
		pgid:      pid, // Setpgid 后进程组 id = 直接子进程 pid
		StartedAt: time.Now(),
		owner:     m,
	}
	// 登记 handler 元数据：休眠后实例被移出 instances 表，但元数据保留，
	// 供 RebridgePlugins 重建休眠插件的 star handler（命令/过滤器/钩子）。
	m.setHandlerMeta(id, meta)
	inst.Touch() // 新加载实例视为活跃，避免被闲置清扫立刻回收
	// Register 快照里的工具（Go 插件在 Register 元数据中声明）先入注册表；
	// Python 插件工具晚于 Register 注册，由首次 RefreshTools 回写。
	if len(meta.Tools) > 0 {
		m.setPluginTools(id, meta.Tools)
	}
	return inst, nil
}

// wrapStartError 提升 Python 插件启动失败的错误：若 stderr 的 [ASTRBOT]
// 协议捕获了 STARTUP_ERROR 行，则把 go-plugin 笼统的握手错误替换为
// phase 化的清晰错误（原始 go-plugin 错误拼接在后）。没有捕获到
// STARTUP_ERROR（或 parser 为 nil，即 Go 插件）时原样返回。
func (m *SubprocessManager) wrapStartError(parser *astrbotStartupParser, err error) error {
	if parser == nil || err == nil {
		return err
	}
	// go-plugin 在 Kill 时已排空 stderr 管道，STARTUP_ERROR 行通常已落地；
	// 给 1s 兜底等 stderr 转发协程完成，避免竞态丢掉错误行。
	se := parser.WaitError(1 * time.Second)
	if se == nil {
		return err
	}
	return fmt.Errorf("Python 插件启动失败: phase=%s plugin=%s error=%s（go-plugin 原始错误: %v）",
		se.Phase, se.Plugin, se.Error, err)
}

// startWatch polls the child process for exit and triggers crash handling.
func (m *SubprocessManager) startWatch(inst *PluginInstance) {
	go func() {
		ticker := time.NewTicker(m.PollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-ticker.C:
				inst.mu.Lock()
				stopped := inst.stopped
				raw := inst.raw
				inst.mu.Unlock()
				// 已被主动卸载/禁用：进程退出前 watcher 直接结束，避免每个
				// 正常卸载的实例残留一个常驻 ticker goroutine。
				if stopped {
					return
				}
				if raw == nil {
					return
				}
				if raw.Exited() {
					m.handleExit(inst)
					return
				}
			}
		}
	}()
}

// handleExit processes an unexpected process exit, deciding whether to
// restart or mark the plugin failed.
func (m *SubprocessManager) handleExit(inst *PluginInstance) {
	inst.mu.Lock()
	if inst.stopped {
		inst.mu.Unlock()
		return
	}
	// 重启预算基于时间衰减：距上次崩溃超过 restartBudgetResetWindow 则清零
	// 预算，低频偶发崩溃不会被永久停用（只惩罚连续/近期崩溃）。
	if !inst.lastRestartAt.IsZero() && time.Since(inst.lastRestartAt) > restartBudgetResetWindow {
		inst.restarts = 0
	}
	inst.restarts++
	inst.lastRestartAt = time.Now()
	count := inst.restarts
	inst.mu.Unlock()

	m.mu.RLock()
	_, loaded := m.instances[inst.ID]
	autoRestart := m.AutoRestart
	maxRestarts := m.MaxRestarts
	m.mu.RUnlock()
	if !loaded {
		return
	}

	if !autoRestart {
		m.markFailed(inst, fmt.Errorf("plugin %s exited unexpectedly (auto-restart disabled)", inst.ID))
		return
	}
	// count >= maxRestarts: 第 maxRestarts 次崩溃即停用，插件总共获得
	// maxRestarts 次启动机会（1 次初始 + maxRestarts-1 次重启）。
	// restarts 预算跨实例传递（restart 里 newInst.restarts = inst.restarts），
	// 因此计数语义在重启后保持一致。
	if count >= maxRestarts {
		m.markFailed(inst, fmt.Errorf("plugin %s exited unexpectedly %d time(s)", inst.ID, count))
		return
	}

	delay := m.RestartBaseDelay * time.Duration(count)
	logger.I18nWarn("插件 %s 意外退出，将在 %v 后重启 (尝试 %d/%d)",
		inst.ID, delay, count, maxRestarts)
	select {
	case <-time.After(delay):
	case <-m.ctx.Done():
		return
	}

	// 退避窗口内用户可能 Unload/禁用/卸载：持 per-id 生命周期锁重查，
	// 插件已被停止、管理器已关闭或 map 里已不是本实例时不再拉起，
	// 避免"禁用后自己活了"与对已卸载插件做幽灵重启。
	unlock := m.lockOp(inst.ID)
	defer unlock()

	inst.mu.Lock()
	stopped := inst.stopped
	inst.mu.Unlock()
	if stopped || m.ctx.Err() != nil {
		return
	}
	m.mu.RLock()
	cur, ok := m.instances[inst.ID]
	m.mu.RUnlock()
	if !ok || cur != inst {
		return
	}
	m.restart(inst)
}

// restart starts a fresh instance for the same id/binary, seeding the restart
// budget so a permanently-crashing plugin eventually trips MaxRestarts. It must
// be called with the per-plugin lifecycle lock held (see handleExit).
func (m *SubprocessManager) restart(inst *PluginInstance) {
	// 防御性身份/上下文复查：与 handleExit 的复查一致，防止并发路径遗漏。
	m.mu.RLock()
	cur, ok := m.instances[inst.ID]
	gen := m.gen
	m.mu.RUnlock()
	if !ok || cur != inst {
		return
	}
	if m.ctx.Err() != nil {
		return
	}

	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()

	newInst, err := m.startInstance(ctx, inst.ID, inst.Binary, inst.Language)
	if err != nil {
		m.markFailed(inst, fmt.Errorf("restart plugin %s: %w", inst.ID, err))
		return
	}
	inst.mu.Lock()
	newInst.restarts = inst.restarts
	newInst.lastRestartAt = inst.lastRestartAt
	inst.mu.Unlock()

	m.mu.Lock()
	// 写回前再查一次管理器是否已关闭（Shutdown 竞态），关闭则丢弃新实例。
	// gen 对比兜底：若 Shutdown 已换过实例表（m.gen 自增），本实例属于旧代际，
	// 写入新表无人回收，直接丢弃并 teardown。
	if m.ctx.Err() != nil || gen != m.gen {
		m.mu.Unlock()
		go m.teardownInstance(newInst)
		return
	}
	m.instances[inst.ID] = newInst
	m.mu.Unlock()

	inst.mu.Lock()
	inst.stopped = true
	inst.mu.Unlock()
	go m.teardownInstance(inst)

	m.startWatch(newInst)
	logger.I18nInfo("插件 %s 已重启 (v%s)", inst.ID, newInst.Version)
	m.notifyChanged()
}

// LoadInstalled loads all enabled plugins from the persisted install manifest
// (their cached compiled binaries). Called at startup.
func (m *SubprocessManager) LoadInstalled(ctx context.Context) {
	// 旧布局一次性迁移：plugins-src/<id> → plugins/<id>（Python 源码本体）、
	// plugins_config/<name> → plugins_config/<id>（配置按实例 id 隔离）、
	// plugins/<name> 文档并入 plugins/<id>。
	m.migratePluginLayout()

	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		logger.I18nWarn("LoadInstalled: %v", err)
		return
	}
	loaded := 0
	for _, e := range man.Plugins {
		if !e.Enabled {
			continue
		}
		lang := e.Language
		if lang == "" {
			lang = "go"
		}
		if _, err := m.LoadLang(ctx, e.ID, e.Binary, lang); err != nil {
			logger.I18nWarn("加载已安装插件 %s 失败: %v", e.ID, err)
			continue
		}
		loaded++
	}
	if len(man.Plugins) > 0 {
		logger.I18nInfo("已从 manifest 加载 %d/%d 个已安装子进程插件", loaded, len(man.Plugins))
	}
	// 一次性迁移：旧版本会把打包元数据混入 config.json，启动时对全部已安装
	// 插件执行剥离（元数据归位独立文件、config 只留真实配置项）。
	m.migrateLegacyMetadataConfigs(man)
}

// migrateLegacyMetadataConfigs strips packaged-metadata keys from every
// installed plugin's config.json (moving them into the standalone
// metadata.json file) so the WebUI config dialog and the on-disk config only
// ever carry real config items.
func (m *SubprocessManager) migrateLegacyMetadataConfigs(man *Manifest) {
	if man == nil {
		return
	}
	for _, e := range man.Plugins {
		if e.ID == "" {
			continue
		}
		_ = m.LoadConfig(e.ID)
	}
}

// migratePluginLayout 把旧版目录布局迁移到"统一 plugins/ + 按实例 id
// （name_language）分键"的新布局：
//
//	plugins-src/<id>      → plugins/<id>        （Python 源码本体）
//	plugins_config/<name> → plugins_config/<id> （配置/元数据/schema，Go/Python 隔离）
//	plugins/<name>        → 并入 plugins/<id>   （旧文档缓存）
//
// 同时更新 manifest 的 Binary（plugins-src 前缀）与 ConfigDir/DocsDir/DataDir
// 足迹字段。幂等：新旧路径相同或目标已存在时跳过。
func (m *SubprocessManager) migratePluginLayout() {
	man, err := LoadManifest(m.manifestPath())
	if err != nil || man == nil {
		return
	}
	changed := false
	for i, e := range man.Plugins {
		sid := sanitizeID(e.ID)
		if sid == "" {
			continue
		}
		// 1) Python 源码：plugins-src/<id> → plugins/<id>
		oldSrc := filepath.Join(m.dataDir, "plugins-src", sid)
		newSrc := filepath.Join(m.dataDir, "plugins", sid)
		if _, err := os.Stat(oldSrc); err == nil {
			_ = os.MkdirAll(filepath.Dir(newSrc), 0o755) // #nosec G301 -- 迁移建目录
			if _, err := os.Stat(newSrc); err == nil {
				// 目标已被旧文档缓存目录占位：源码合并进去（源码文件优先），
				// 再删旧源码目录——绝不能直接删源码。
				if err := copyDirMerge(oldSrc, newSrc); err != nil {
					logger.I18nWarn("迁移插件 %s 源码合并失败: %v", e.ID, err)
				}
				_ = os.RemoveAll(oldSrc)
			} else {
				if rerr := os.Rename(oldSrc, newSrc); rerr == nil {
					logger.I18nInfo("迁移插件 %s 源码: plugins-src → plugins", e.ID)
				}
			}
			changed = true
		}
		// 2) 配置：plugins_config/<name> → plugins_config/<id>
		oldCfg := filepath.Join(m.dataDir, "plugins_config", sanitizePluginName(e.Name))
		newCfg := filepath.Join(m.dataDir, "plugins_config", sid)
		if oldCfg != newCfg {
			if _, err := os.Stat(oldCfg); err == nil {
				if _, err := os.Stat(newCfg); err != nil {
					if rerr := os.Rename(oldCfg, newCfg); rerr == nil {
						logger.I18nInfo("迁移插件 %s 配置: %s → %s", e.ID, oldCfg, newCfg)
					}
				} else {
					_ = os.RemoveAll(oldCfg)
				}
				changed = true
			}
		}
		// 3) 旧文档缓存 plugins/<name>：并入 plugins/<id>（源码树自带 README）
		oldDocs := filepath.Join(m.dataDir, "plugins", sanitizePluginName(e.Name))
		if oldDocs != newSrc {
			if _, err := os.Stat(oldDocs); err == nil {
				if _, err := os.Stat(newSrc); err != nil {
					if rerr := os.Rename(oldDocs, newSrc); rerr == nil {
						logger.I18nInfo("迁移插件 %s 文档缓存: plugins/<name> → plugins/<id>", e.ID)
					} else {
						_ = os.RemoveAll(oldDocs)
					}
				} else {
					// 源码目录已存在（如 Python 源码刚迁移过来）：把缺失的
					// 文档文件拷入（不覆盖源码树内同名文件），再删旧目录。
					copyMissingDocs(oldDocs, newSrc)
					_ = os.RemoveAll(oldDocs)
				}
				changed = true
			}
		}
		// 4) legacy id 归一化：id → sanitizePluginName(name)_language。
		// 语言经插件入口文件推断（main.py/__init__.py → python，否则 go），
		// 不猜路径。稳定 id 让同名 Go/Python 插件的本体/配置/数据完全隔离；
		// 目标 id 已被占用（真冲突）时跳过，不合并两个插件。
		lang := e.Language
		if lang != "go" && lang != "python" {
			srcProbe := filepath.Join(m.dataDir, "plugins", sid)
			if _, err := os.Stat(filepath.Join(srcProbe, "main.py")); err == nil {
				lang = "python"
			} else if _, err := os.Stat(filepath.Join(srcProbe, "__init__.py")); err == nil {
				lang = "python"
			} else {
				lang = "go"
			}
		}
		name := e.Name
		if name == "" {
			name = e.ID
		}
		stableID := sanitizePluginName(name) + "_" + lang
		if stableID != e.ID {
			if man.Get(stableID) == nil {
				// 移动四个目录（本体/二进制/配置/数据），旧路径不存在则跳过。
				moved := m.movePluginDir(filepath.Join("plugins", sid), filepath.Join("plugins", sanitizeID(stableID)))
				m.movePluginDir(filepath.Join("plugins-bin", sid), filepath.Join("plugins-bin", sanitizeID(stableID)))
				m.movePluginDir(filepath.Join("plugins_config", sid), filepath.Join("plugins_config", sanitizeID(stableID)))
				m.movePluginDir(filepath.Join("plugins_data", sid), filepath.Join("plugins_data", sanitizeID(stableID)))
				if moved {
					logger.I18nInfo("插件 %s 归一化为稳定 id %s（语言 %s）", e.ID, stableID, lang)
				}
				oldID := e.ID
				e.ID = stableID
				e.Language = lang
				sid = sanitizeID(stableID)
				e.Binary = rewritePluginIDPath(e.Binary, oldID, stableID)
				changed = true
			} else {
				logger.I18nWarn("插件 %s 归一化 id 冲突（%s 已存在），保留原 id", e.ID, stableID)
			}
		} else if e.Language != lang {
			e.Language = lang
			changed = true
		}
		// 5) manifest 足迹与 Binary 路径更新（按最终 id）。
		newBinary := e.Binary
		newBinary = strings.Replace(newBinary, "plugins-src", "plugins", 1)
		if newBinary != e.Binary || e.ConfigDir != filepath.Join("plugins_config", sid) ||
			e.DocsDir != filepath.Join("plugins", sid) || e.DataDir != filepath.Join("plugins_data", sid) {
			e.Binary = newBinary
			e.ConfigDir = filepath.Join("plugins_config", sid)
			e.DocsDir = filepath.Join("plugins", sid)
			e.DataDir = filepath.Join("plugins_data", sid)
			man.Plugins[i] = e
			changed = true
		}
	}
	if changed {
		_ = man.Save(m.manifestPath())
	}
	// 清理已空的旧目录（Rename 后 plugins-src 应为空；若有残余仅删空目录）。
	_ = os.Remove(filepath.Join(m.dataDir, "plugins-src"))
}

// movePluginDir moves a dataDir-relative directory (old → new), creating the
// parent as needed. Reports whether anything moved.
func (m *SubprocessManager) movePluginDir(oldSub, newSub string) bool {
	old := filepath.Join(m.dataDir, filepath.FromSlash(oldSub))
	new := filepath.Join(m.dataDir, filepath.FromSlash(newSub))
	if old == new {
		return false
	}
	if _, err := os.Stat(old); err != nil {
		return false
	}
	if _, err := os.Stat(new); err == nil {
		_ = os.RemoveAll(old)
		return true
	}
	_ = os.MkdirAll(filepath.Dir(new), 0o755) // #nosec G301 -- 迁移建目录
	if err := os.Rename(old, new); err != nil {
		logger.I18nWarn("迁移目录 %s → %s 失败: %v", old, new, err)
		return false
	}
	return true
}

// rewritePluginIDPath rewrites a manifest-recorded path whose <oldID> path
// SEGMENT became <newID> (e.g. "data/plugins-bin/box/box-linux-amd64" →
// "data/plugins-bin/box_go/box-linux-amd64"). 只匹配完整路径段（前后为 /
// 或串首尾），避免把以 id 为前缀的产物文件名（box-linux-amd64）一并改写
// （磁盘上的文件并未改名，只是目录被移动）。
func rewritePluginIDPath(path, oldID, newID string) string {
	if path == "" {
		return path
	}
	out := path
	for _, seg := range []string{oldID, sanitizeID(oldID)} {
		if seg == "" {
			continue
		}
		re := regexp.MustCompile(`(^|/)` + regexp.QuoteMeta(seg) + `(/|$)`)
		out = re.ReplaceAllString(out, "${1}"+newID+"${2}")
	}
	return out
}

// copyMissingDocs copies doc files (README.md/logo 等) from src into dst,
// skipping files that already exist in dst. 用于旧文档缓存目录并入源码目录
// （源码树内同名文件优先保留）。
func copyMissingDocs(src, dst string) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return
	}
	_ = os.MkdirAll(dst, 0o755) // #nosec G301 -- 文档缓存目录
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !isDocFileName(name) {
			continue
		}
		target := filepath.Join(dst, name)
		if _, err := os.Stat(target); err == nil {
			continue
		}
		// #nosec G304 -- 读取旧文档缓存目录内固定文件名
		if data, err := os.ReadFile(filepath.Join(src, name)); err == nil {
			// #nosec G306 -- 文档缓存非常规敏感信息
			_ = os.WriteFile(target, data, 0o644)
		}
	}
}

// isDocFileName reports whether the file is a cached plugin doc/logo name
// （旧文档缓存目录只放这些文件）。
func isDocFileName(name string) bool {
	switch strings.ToLower(name) {
	case "readme.md", "changelog.md", "logo.png", "logo.jpg", "logo.jpeg", "logo.gif", "icon.png", "config_schema.json":
		return true
	}
	return false
}

// TriggerHook fires payload-less lifecycle hooks (e.g. "startup"/"shutdown") on
// all running plugins via RPC.
func (m *SubprocessManager) TriggerHook(ctx context.Context, event string) {
	m.TriggerHookPayload(ctx, event, nil)
}

// TriggerHookPayload fires lifecycle hooks (e.g. "startup"/"shutdown",
// "on_astrbot_loaded", "on_plugin_loaded", "on_plugin_unloaded",
// "on_platform_loaded") on all running plugins via RPC, attaching a JSON
// payload for payload-carrying events (nil for event-only hooks). Each RPC runs
// under a bounded timeout so one hung plugin cannot block the whole broadcast.
func (m *SubprocessManager) TriggerHookPayload(ctx context.Context, event string, payload any) {
	for _, inst := range m.List() {
		if inst.Client == nil || inst.Meta == nil {
			continue
		}
		for _, h := range inst.Meta.Hooks {
			if h.Event != event {
				continue
			}
			hookCtx := ctx
			if hookCtx == nil {
				hookCtx = context.Background()
			}
			rpcCtx, cancel := context.WithTimeout(hookCtx, pluginHookRPCTimeout)
			_, _, _, err := inst.Client.HandleHookWithPayload(rpcCtx, h.Name, &pluginsdk.Event{}, nil, payload)
			cancel()
			if err != nil {
				logger.I18nWarn("钩子 %s (%s) 在插件 %s 上执行失败: %v", h.Name, h.Event, inst.ID, err)
			}
		}
	}
}

// notifyChanged invokes the OnInstancesChanged callback, if set.
func (m *SubprocessManager) notifyChanged() {
	if m.OnInstancesChanged != nil {
		m.OnInstancesChanged()
	}
}

// markFailed removes a plugin from the running set and records the failure.
func (m *SubprocessManager) markFailed(inst *PluginInstance, err error) {
	logger.Error("Plugin %s marked failed: %v", inst.ID, err)
	m.mu.Lock()
	if cur, ok := m.instances[inst.ID]; ok && cur == inst {
		delete(m.instances, inst.ID)
	}
	m.failures[inst.ID] = err
	m.mu.Unlock()

	inst.mu.Lock()
	inst.stopped = true
	inst.failed = err
	inst.mu.Unlock()

	// Release the process and RPC client resources.
	go m.teardownInstance(inst)

	// 插件已永久失效：注销其全部会话等待（子进程已死，残留条目只会
	// 反复推送失败）。
	m.unregisterPluginWaits(inst.Name)

	// 通知宿主清理 star 注册表里的命令/过滤器/钩子闭包，否则残留 handler
	// 会继续对已关闭的 conn 发 RPC。
	m.notifyChanged()
}

// needMemoryReclaim throttles forced GC + OS memory return: at most once per
// 30s and only when the Go heap is non-trivial, so a plugin crash-restart loop
// cannot churn.
func needMemoryReclaim() bool {
	lastReclaimMu.Lock()
	defer lastReclaimMu.Unlock()
	if time.Since(lastReclaimAt) < 30*time.Second {
		return false
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	if ms.HeapAlloc < 8<<20 {
		return false
	}
	lastReclaimAt = time.Now()
	return true
}

var (
	lastReclaimMu sync.Mutex
	lastReclaimAt time.Time
)

// teardownInstance gracefully asks the plugin to clean up, kills the process
// (its whole process group, so anything the plugin spawned dies too), then
// releases the RPC client (gRPC conn + HostService server) so repeated
// reloads do not leak connections/goroutines. Safe to call multiple times.
func (m *SubprocessManager) teardownInstance(inst *PluginInstance) {
	if inst == nil || inst.raw == nil {
		return
	}
	if !inst.raw.Exited() {
		if inst.Client != nil {
			ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
			_ = inst.Client.Cleanup(ctx)
			cancel()
		}
	}
	// 先按进程组回收（SIGTERM → 宽限 → SIGKILL，含 Python 桥再拉起的
	// 子进程）；killProcessGroup 返回 false（未记录 pgid / 非 unix 平台）
	// 或进程已被组信号杀死后，raw.Kill() 兜底回收直接子进程并完成
	// go-plugin 的簿记（reap）。顺序不可反：raw.Kill() 只杀直接子进程，
	// 先组杀保证整棵进程树被回收。
	killProcessGroup(inst)
	inst.raw.Kill()
	if inst.Client != nil {
		_ = inst.Client.Close()
	}
	// 归还 Go 堆给 OS：插件子进程被杀后其内存已由 OS 回收，但宿主 Go 运行时
	// 默认不会把释放的对象还给系统（RSS 只涨不降）。这里强制 GC + 归还，
	// 解决"插件禁用/重载后运存不释放"。
	if needMemoryReclaim() {
		runtime.GC()
		debug.FreeOSMemory()
	}
}
