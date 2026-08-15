package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
	sdkv1 "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/gen/sdkv1"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
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

	// Client is the typed gRPC client used by the star bridge to invoke
	// commands/filters/hooks.
	Client *pluginsdk.Client
	// Meta is the plugin's Register() metadata (handlers + config schema).
	Meta *sdkv1.RegisterResponse

	mu       sync.Mutex
	raw      *goplugin.Client // go-plugin process client
	stopped  bool             // set before intentional kill (suppresses restart)
	restarts int              // consecutive crash-restart count for this instance
	// lastRestartAt 记录上一次崩溃重启的时间，用于 restart 预算的基于时间衰减。
	lastRestartAt time.Time
	failed        error // set when the plugin is marked failed
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
	ctx       context.Context
	cancel    context.CancelFunc
	// gen 是"实例表代际"标记：Shutdown 换新表时自增。restart 在 startInstance
	// 成功后写回 map 前对比 gen，代际不一致说明表已被换掉，需丢弃新实例并回收。
	gen uint64
	// manifestMu 串行化 manifest 的"读→改→写"整段（recordInstall/SetEnabled/
	// BindSource/ReinstallSource/Uninstall），防止并发修改丢条目。与 m.mu 职责
	// 分离：m.mu 保护内存 map，manifestMu 保护磁盘文件的一致性。
	manifestMu sync.Mutex
	// githubProxy 是插件 git clone 的 GitHub 加速地址（如 https://ghfast.top/），
	// 配置后克隆 https://github.com/... 仓库时在 URL 前加该前缀。
	githubProxy string

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
}

// NewSubprocessManager creates the subprocess plugin manager.
func NewSubprocessManager(tc *toolchain.Toolchain, dataDir string) *SubprocessManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &SubprocessManager{
		instances:        make(map[string]*PluginInstance),
		failures:         make(map[string]error),
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
		return "", fmt.Errorf("Go 工具链不可用")
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
	cmd := exec.CommandContext(ctx, bin, "install", pkg)
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

	srcDir, err := m.fetchSource(ctx, id, source)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(srcDir)

	// Plugin packages must ship metadata.json (identity/source/cgo) and main.go
	// (entrypoint) at their root.
	meta, err := ReadPluginMetadata(srcDir)
	if err != nil {
		return nil, err
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

	if opts.Stage != nil {
		opts.Stage("准备编译插件…")
	}
	if err := m.compiler.Prepare(srcDir, goModuleNameOf(srcDir, meta)); err != nil {
		return nil, fmt.Errorf("prepare module: %w", err)
	}
	if err := m.compiler.Vet(ctx, srcDir); err != nil {
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
	if err := m.compiler.BuildWithProgressOut(ctx, srcDir, artifact, opts.Progress, cc, cxx, lineCb); err != nil {
		return nil, fmt.Errorf("build plugin %s: %w", id, err)
	}

	// 尾段持 per-plugin 生命周期锁：与 Uninstall 互斥，防止并发"安装+卸载"
	// 时 Uninstall 清理完成后这里又重建 manifest 条目（插件"复活"）。
	unlock := m.lockOp(id)
	defer unlock()

	inst, err := m.loadLocked(ctx, id, artifact)
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
	if err := m.recordInstall(inst, source, artifact, opts); err != nil {
		logger.I18nWarn("插件 %s 已安装但 manifest 持久化失败: %v", id, err)
	}
	m.cachePluginDocs(inst.Name, srcDir)
	m.writeMetadataConfig(inst.Name, meta)
	return inst, nil
}

// cachePluginDocs copies the plugin's README.md and CHANGELOG.md from the
// fetched source into its config/data directory so the WebUI readme/changelog
// endpoints can serve them without re-fetching (mirrors Python's
// plugin_dir/README.md lookup).
func (m *SubprocessManager) cachePluginDocs(name, srcDir string) {
	dir := filepath.Join(m.dataDir, "plugins", sanitizePluginName(name))
	_ = os.MkdirAll(dir, 0o755)
	for _, src := range []string{"README.md", "readme.md", "CHANGELOG.md", "changelog.md"} {
		content, err := os.ReadFile(filepath.Join(srcDir, src))
		if err != nil {
			continue
		}
		dst := filepath.Join(dir, src)
		if err := os.WriteFile(dst, content, 0o644); err != nil {
			logger.I18nWarn("缓存插件 %s 的文档 %s 失败: %v", name, src, err)
		}
	}
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
		InstallMethod:  opts.InstallMethod,
		RegistryURL:    opts.RegistryURL,
		RegistryName:   opts.RegistryName,
		MarketPluginID: opts.MarketPluginID,
		Repo:           opts.Repo,
		DownloadURL:    opts.DownloadURL,
		// 记录插件在 data 下创建的目录，供卸载时精确清理。
		ConfigDir: filepath.Join("plugins_config", sanitizePluginName(inst.Name)),
		DataDir:   filepath.Join("plugins_data", sanitizeID(inst.ID)),
		DocsDir:   filepath.Join("plugins", sanitizePluginName(inst.Name)),
	})
	return man.Save(m.manifestPath())
}

// manifestPath returns the persisted install manifest location.
func (m *SubprocessManager) manifestPath() string {
	return filepath.Join(m.dataDir, "plugins-manifest.json")
}

// Load launches a compiled plugin binary as a child process and registers it
// under id. Already-loaded ids return the existing instance. It holds the
// per-plugin lifecycle lock so a concurrent Uninstall cannot unload/remove the
// plugin in the middle of its registration window.
func (m *SubprocessManager) Load(ctx context.Context, id, binary string) (*PluginInstance, error) {
	if id == "" {
		return nil, fmt.Errorf("plugin id cannot be empty")
	}
	unlock := m.lockOp(id)
	defer unlock()
	return m.loadLocked(ctx, id, binary)
}

// loadLocked is Load's body; the caller must hold the per-plugin lifecycle lock
// for id (m.lockOp).
func (m *SubprocessManager) loadLocked(ctx context.Context, id, binary string) (*PluginInstance, error) {
	m.mu.RLock()
	if inst, ok := m.instances[id]; ok {
		m.mu.RUnlock()
		return inst, nil
	}
	m.mu.RUnlock()

	inst, err := m.startInstance(ctx, id, binary)
	if err != nil {
		return nil, err
	}
	// 落盘 config schema 缓存，供插件禁用后仍能渲染配置对话框。
	if inst.Meta != nil {
		m.cacheConfigSchema(inst.Name, inst.Meta)
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

	newInst, err := m.startInstance(ctx, id, old.Binary)
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
	return m.unloadLocked(id)
}

// unloadLocked is Unload's body; the caller must hold the per-plugin lifecycle
// lock for id (m.lockOp).
func (m *SubprocessManager) unloadLocked(id string) error {
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
	logger.I18nInfo("插件 %s 已卸载", id)
	m.notifyChanged()
	return nil
}

// Get returns the running instance for id, or nil.
func (m *SubprocessManager) Get(id string) *PluginInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.instances[id]
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

// startInstance launches one plugin binary and performs the handshake + first
// Register call. On any failure the process is killed and resources released.
// It holds startInstanceMu for its whole lifetime so the SDK-side process-global
// hostPluginID cannot be clobbered by a concurrently loading plugin.
func (m *SubprocessManager) startInstance(ctx context.Context, id, binary string) (*PluginInstance, error) {
	startInstanceMu.Lock()
	defer startInstanceMu.Unlock()

	abs, err := filepath.Abs(binary)
	if err != nil {
		return nil, fmt.Errorf("resolve binary: %w", err)
	}
	if info, err := os.Stat(abs); err != nil || info.IsDir() {
		return nil, fmt.Errorf("plugin binary not found: %s", abs)
	}

	// 插件子进程工作目录设为统一数据根目录 data/plugins_data/<id>，插件写相对
	// 路径的运行时数据（修仙存档、表情库等）自动落盘于此，便于管理/备份/卸载。
	pluginDataRoot := m.pluginDataRoot(id)
	if err := os.MkdirAll(pluginDataRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create plugin data dir: %w", err)
	}
	cmd := exec.Command(abs)
	cmd.Dir = pluginDataRoot
	raw := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig:  pluginsdk.Handshake,
		Plugins:          pluginsdk.PluginMap,
		Cmd:              cmd,
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		Managed:          true,
	})

	// go-plugin's handshake has no built-in timeout; enforce one.
	type dispenseResult struct {
		pc  *pluginsdk.Client
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
		proto, err := raw.Client()
		if err != nil {
			resCh <- dispenseResult{err: err}
			return
		}
		rpcClient, err := proto.Dispense("plugin_service")
		if err != nil {
			resCh <- dispenseResult{err: err}
			return
		}
		pc, ok := rpcClient.(*pluginsdk.Client)
		if !ok {
			resCh <- dispenseResult{err: fmt.Errorf("unexpected plugin client type %T", rpcClient)}
			return
		}
		resCh <- dispenseResult{pc: pc}
	}()

	var pc *pluginsdk.Client
	select {
	case res := <-resCh:
		if res.err != nil {
			raw.Kill()
			return nil, fmt.Errorf("start plugin %s: %w", id, res.err)
		}
		pc = res.pc
	case <-time.After(startTimeout):
		// 杀进程先让 dispense goroutine 结束，再取回可能已创建的
		// *pluginsdk.Client（持有 gRPC conn + HostService server）并关闭，
		// 避免反复 Load 泄漏连接与 goroutine。
		raw.Kill()
		if res := <-resCh; res.pc != nil {
			_ = res.pc.Close()
		}
		return nil, fmt.Errorf("start plugin %s: handshake timed out after %v", id, startTimeout)
	case <-m.ctx.Done():
		raw.Kill()
		if res := <-resCh; res.pc != nil {
			_ = res.pc.Close()
		}
		return nil, fmt.Errorf("start plugin %s: manager shutting down", id)
	}

	regCtx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	meta, err := pc.Register(regCtx)
	if err != nil {
		_ = pc.Close()
		raw.Kill()
		return nil, fmt.Errorf("plugin %s Register: %w", id, err)
	}
	// 用 Register 返回的注册名更新 HostService 连接身份（accept 时只绑定
	// manifest id，name 与 id 可能不同）。之后插件 GetConfig/SetConfig 传的
	// name 与连接身份一致，身份隔离校验才能通过。
	if meta != nil && meta.Name != "" {
		pluginsdk.BindHostServiceName(id, meta.Name)
	}

	return &PluginInstance{
		ID:        id,
		Name:      meta.Name,
		Version:   meta.Version,
		Binary:    abs,
		Client:    pc,
		Meta:      meta,
		raw:       raw,
		StartedAt: time.Now(),
	}, nil
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

	newInst, err := m.startInstance(ctx, inst.ID, inst.Binary)
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
		if _, err := m.Load(ctx, e.ID, e.Binary); err != nil {
			logger.I18nWarn("加载已安装插件 %s 失败: %v", e.ID, err)
			continue
		}
		loaded++
	}
	if len(man.Plugins) > 0 {
		logger.I18nInfo("已从 manifest 加载 %d/%d 个已安装子进程插件", loaded, len(man.Plugins))
	}
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
			_, _, err := inst.Client.HandleHookWithPayload(rpcCtx, h.Name, &pluginsdk.Event{}, nil, payload)
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

// teardownInstance gracefully asks the plugin to clean up, kills the process,
// then releases the RPC client (gRPC conn + HostService server) so repeated
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
