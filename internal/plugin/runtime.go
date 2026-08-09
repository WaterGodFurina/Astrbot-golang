package plugin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
	sdkv1 "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/gen/sdkv1"
	goplugin "github.com/hashicorp/go-plugin"
	"github.com/AstrBotDevs/AstrBot/internal/toolchain"
)

// startTimeout bounds the go-plugin handshake + first Register call. go-plugin
// itself does not time out the handshake, so Load enforces one.
const startTimeout = 15 * time.Second

// cleanupTimeout bounds the graceful Cleanup RPC before force-killing.
const cleanupTimeout = 5 * time.Second

// PluginInstance is a running plugin subprocess.
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
	failed   error            // set when the plugin is marked failed
}

// SubprocessManager manages plugins running as isolated child processes
// (go-plugin, gRPC). This is the NEW plugin runtime that replaces the legacy
// .so loader (manager.go, kept behind the `legacy_plugin_mode` config flag).
//
// Unlike .so plugins, child processes can be fully terminated (memory, file
// handles and goroutines are reclaimed by the OS) and a crash cannot take the
// host down; crashed plugins are automatically restarted with backoff.
type SubprocessManager struct {
	mu          sync.RWMutex
	instances   map[string]*PluginInstance
	failures    map[string]error
	toolchain   *toolchain.Toolchain
	compiler    *Compiler
	dataDir     string
	ctx         context.Context
	cancel      context.CancelFunc

	// AutoRestart enables automatic restart of crashed plugins.
	AutoRestart bool
	// MaxRestarts caps consecutive automatic restarts before the plugin is
	// marked failed.
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

// InstallOptions configures InstallFromSource.
type InstallOptions struct {
	// IgnoreRisk skips the static-scan risk gate and installs the plugin even
	// when risky imports are found (user explicitly confirmed on the WebUI).
	IgnoreRisk bool
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
// is installed.
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

	findings, err := StaticScan(srcDir)
	if err != nil {
		return nil, fmt.Errorf("static scan: %w", err)
	}
	if len(findings) > 0 && !opts.IgnoreRisk {
		return nil, &RiskError{Findings: findings}
	}
	if err := m.compiler.Prepare(srcDir, moduleNameFromID(id)); err != nil {
		return nil, fmt.Errorf("prepare module: %w", err)
	}
	if err := m.compiler.Vet(ctx, srcDir); err != nil {
		return nil, fmt.Errorf("go vet: %w", err)
	}

	artifact := filepath.Join(m.dataDir, "plugins-bin", sanitizeID(id), artifactName(id))
	if err := m.compiler.Build(ctx, srcDir, artifact); err != nil {
		return nil, fmt.Errorf("build plugin %s: %w", id, err)
	}

	inst, err := m.Load(ctx, id, artifact)
	if err != nil {
		return nil, err
	}
	if err := m.recordInstall(inst, source, artifact); err != nil {
		logger.Warn("Plugin %s installed but manifest persist failed: %v", id, err)
	}
	return inst, nil
}

// recordInstall upserts the plugin into the persisted install manifest.
func (m *SubprocessManager) recordInstall(inst *PluginInstance, source, artifact string) error {
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		return err
	}
	man.Upsert(ManifestEntry{
		ID:      inst.ID,
		Name:    inst.Name,
		Version: inst.Version,
		Source:  source,
		Binary:  artifact,
		Enabled: true,
	})
	return man.Save(m.manifestPath())
}

// manifestPath returns the persisted install manifest location.
func (m *SubprocessManager) manifestPath() string {
	return filepath.Join(m.dataDir, "plugins-manifest.json")
}

// Load launches a compiled plugin binary as a child process and registers it
// under id. Already-loaded ids return the existing instance.
func (m *SubprocessManager) Load(ctx context.Context, id, binary string) (*PluginInstance, error) {
	if id == "" {
		return nil, fmt.Errorf("plugin id cannot be empty")
	}

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
	logger.Info("Plugin %s loaded from %s (v%s)", id, inst.Binary, inst.Version)
	return inst, nil
}

// Reload restarts a plugin with zero downtime: start the new process first,
// swap it in, then stop the old one.
func (m *SubprocessManager) Reload(ctx context.Context, id string) error {
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

	m.mu.Lock()
	m.instances[id] = newInst
	delete(m.failures, id)
	m.mu.Unlock()

	old.mu.Lock()
	old.stopped = true
	old.mu.Unlock()
	go m.teardownInstance(old)

	m.startWatch(newInst)
	logger.Info("Plugin %s reloaded (v%s)", id, newInst.Version)
	return nil
}

// Unload stops a plugin process; the OS fully reclaims its resources.
func (m *SubprocessManager) Unload(id string) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("plugin %s not loaded", id)
	}
	delete(m.instances, id)
	m.mu.Unlock()

	inst.mu.Lock()
	inst.stopped = true
	inst.mu.Unlock()
	m.teardownInstance(inst)
	logger.Info("Plugin %s unloaded", id)
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
	m.mu.Unlock()

	for _, inst := range insts {
		inst.mu.Lock()
		inst.stopped = true
		inst.mu.Unlock()
		m.teardownInstance(inst)
	}
	logger.Info("Subprocess plugin manager shut down (%d plugin(s) stopped)", len(insts))
}

// SetAutoRestart enables/disables automatic crash restarts.
func (m *SubprocessManager) SetAutoRestart(enabled bool) {
	m.AutoRestart = enabled
}

// startInstance launches one plugin binary and performs the handshake + first
// Register call. On any failure the process is killed and resources released.
func (m *SubprocessManager) startInstance(ctx context.Context, id, binary string) (*PluginInstance, error) {
	abs, err := filepath.Abs(binary)
	if err != nil {
		return nil, fmt.Errorf("resolve binary: %w", err)
	}
	if info, err := os.Stat(abs); err != nil || info.IsDir() {
		return nil, fmt.Errorf("plugin binary not found: %s", abs)
	}

	cmd := exec.Command(abs)
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
		raw.Kill()
		return nil, fmt.Errorf("start plugin %s: handshake timed out after %v", id, startTimeout)
	case <-m.ctx.Done():
		raw.Kill()
		return nil, fmt.Errorf("start plugin %s: manager shutting down", id)
	}

	regCtx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	meta, err := pc.Register(regCtx)
	if err != nil {
		raw.Kill()
		return nil, fmt.Errorf("plugin %s Register: %w", id, err)
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
				raw := inst.raw
				inst.mu.Unlock()
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
	inst.restarts++
	count := inst.restarts
	inst.mu.Unlock()

	m.mu.RLock()
	_, loaded := m.instances[inst.ID]
	m.mu.RUnlock()
	if !loaded {
		return
	}

	if !m.AutoRestart {
		m.markFailed(inst, fmt.Errorf("plugin %s exited unexpectedly (auto-restart disabled)", inst.ID))
		return
	}
	if count > m.MaxRestarts {
		m.markFailed(inst, fmt.Errorf("plugin %s exited unexpectedly %d time(s)", inst.ID, count))
		return
	}

	delay := m.RestartBaseDelay * time.Duration(count)
	logger.Warn("Plugin %s exited unexpectedly, restarting in %v (attempt %d/%d)",
		inst.ID, delay, count, m.MaxRestarts)
	select {
	case <-time.After(delay):
	case <-m.ctx.Done():
		return
	}
	m.restart(inst)
}

// restart starts a fresh instance for the same id/binary, seeding the restart
// budget so a permanently-crashing plugin eventually trips MaxRestarts.
func (m *SubprocessManager) restart(inst *PluginInstance) {
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()

	newInst, err := m.startInstance(ctx, inst.ID, inst.Binary)
	if err != nil {
		m.markFailed(inst, fmt.Errorf("restart plugin %s: %w", inst.ID, err))
		return
	}
	inst.mu.Lock()
	newInst.restarts = inst.restarts
	inst.mu.Unlock()

	m.mu.Lock()
	m.instances[inst.ID] = newInst
	m.mu.Unlock()

	inst.mu.Lock()
	inst.stopped = true
	inst.mu.Unlock()
	go m.teardownInstance(inst)

	m.startWatch(newInst)
	logger.Info("Plugin %s restarted (v%s)", inst.ID, newInst.Version)
	m.notifyChanged()
}

// LoadInstalled loads all enabled plugins from the persisted install manifest
// (their cached compiled binaries). Called at startup.
func (m *SubprocessManager) LoadInstalled(ctx context.Context) {
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		logger.Warn("LoadInstalled: %v", err)
		return
	}
	loaded := 0
	for _, e := range man.Plugins {
		if !e.Enabled {
			continue
		}
		if _, err := m.Load(ctx, e.ID, e.Binary); err != nil {
			logger.Warn("Failed to load installed plugin %s: %v", e.ID, err)
			continue
		}
		loaded++
	}
	if len(man.Plugins) > 0 {
		logger.Info("Loaded %d of %d installed subprocess plugin(s) from manifest", loaded, len(man.Plugins))
	}
}

// TriggerHook fires lifecycle hooks (e.g. "startup"/"shutdown") on all running
// plugins via RPC.
func (m *SubprocessManager) TriggerHook(ctx context.Context, event string) {
	for _, inst := range m.List() {
		if inst.Client == nil || inst.Meta == nil {
			continue
		}
		for _, h := range inst.Meta.Hooks {
			if h.Event != event {
				continue
			}
			if err := inst.Client.HandleHook(ctx, h.Name, &pluginsdk.Event{}); err != nil {
				logger.Warn("Hook %s (%s) on plugin %s failed: %v", h.Name, h.Event, inst.ID, err)
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
}

// teardownInstance gracefully asks the plugin to clean up, then kills the
// process. Safe to call multiple times.
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
}
