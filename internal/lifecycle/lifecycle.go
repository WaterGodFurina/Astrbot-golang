// Package lifecycle implements AstrBot's core lifecycle management.
// Ported from astrbot/core/core_lifecycle.py and initial_loader.py
package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/backup"
	"github.com/WaterGodFurina/Astrbot-golang/internal/config"
	"github.com/WaterGodFurina/Astrbot-golang/internal/conversation"
	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/cron"
	"github.com/WaterGodFurina/Astrbot-golang/internal/dashboard"
	"github.com/WaterGodFurina/Astrbot-golang/internal/db"
	"github.com/WaterGodFurina/Astrbot-golang/internal/i18n"
	"github.com/WaterGodFurina/Astrbot-golang/internal/knowledgebase"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/pipeline"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	_ "github.com/WaterGodFurina/Astrbot-golang/internal/platform/sources"
	"github.com/WaterGodFurina/Astrbot-golang/internal/plugin"
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	_ "github.com/WaterGodFurina/Astrbot-golang/internal/provider/sources"
	"github.com/WaterGodFurina/Astrbot-golang/internal/sandbox"
	"github.com/WaterGodFurina/Astrbot-golang/internal/skills"
	"github.com/WaterGodFurina/Astrbot-golang/internal/star"
	"github.com/WaterGodFurina/Astrbot-golang/internal/star/builtin"
	"github.com/WaterGodFurina/Astrbot-golang/internal/toolchain"
	"github.com/WaterGodFurina/Astrbot-golang/internal/utils"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

var logger = log.GetDefault().WithComponent("Core")

// Lifecycle orchestrates all AstrBot components.
type Lifecycle struct {
	mu              sync.Mutex
	configMgr       *config.ConfigManager
	database        *db.Database
	providerMgr     *provider.ProviderManager
	starMgr         *star.Manager
	kbMgr           *knowledgebase.Manager
	platformMgr     *platform.PlatformManager
	cronMgr         *cron.CronJobManager
	dashboard       *dashboard.Server
	backupExporter  *backup.Exporter
	subPluginMgr    *plugin.SubprocessManager
	toolchain       *toolchain.Toolchain
	skillMgr        *skills.SkillManager
	sandboxMgr      *sandbox.Manager
	sandboxSig      string // last booter-selection signature (avoids needless rebuilds)
	eventBus        *core.EventBus
	conversationMgr *conversation.Manager
	pipelineMapping map[string]*core.PipelineScheduler
	webuiDir        string
	cancel          context.CancelFunc
	startedAt       time.Time
}

// New creates a Lifecycle.
func New() *Lifecycle {
	return &Lifecycle{
		configMgr:       config.NewConfigManager(),
		providerMgr:     provider.NewProviderManager(),
		pipelineMapping: make(map[string]*core.PipelineScheduler),
	}
}

// SetWebUIDir sets the WebUI static files directory.
func (l *Lifecycle) SetWebUIDir(dir string) {
	l.webuiDir = dir
}

// Start initializes all components and begins event processing.
func (l *Lifecycle) Start(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	// 启动前清扫历史孤儿插件子进程：主进程被 SIGKILL/崩溃时 go-plugin 的
	// autoKill 未生效，插件进程会以 PPID=1 继续存活。先清理再启动，避免
	// 新实例与旧孤儿并存。
	cleanupOrphanPlugins()

	logger.I18nInfo("AstrBot Go - 正在初始化")
	l.startedAt = time.Now()

	// 1. Open database
	database, err := db.New("data/astrbot.db")
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	l.database = database
	logger.I18nInfo("数据库已打开（连接池 5，WAL 模式）")

	// 2. Load config
	cfg := config.NewConfig("data/cmd_config.json", config.DefaultConfig())
	if err := cfg.Load(); err != nil {
		logger.I18nWarn("加载配置失败，使用默认值: %v", err)
	}
	l.configMgr.Register("default", cfg)
	logger.I18nInfo("配置已加载（完整性校验通过）")

	// 加载内置 i18n locale 并应用 config 指定的语言（默认 zh_CN）。
	// 之后所有 i18n.Get(...) 的日志/指令文案按当前语言输出。
	_ = i18n.LoadEmbeddedLocales()
	if lang := cfg.GetString("language"); lang != "" {
		i18n.SetLocale(lang)
	} else {
		i18n.SetLocale("zh_CN")
	}

	// 应用全局 HTTP 代理（http_proxy / no_proxy）：provider、平台、工具等所有
	// 使用 `&http.Client{}`（Transport=nil 走 http.DefaultTransport）的请求
	// 都会走配置的代理。
	{
		noProxy := []string{}
		if np, ok := cfg.Get("no_proxy").([]interface{}); ok {
			for _, s := range np {
				noProxy = append(noProxy, fmt.Sprint(s))
			}
		}
		utils.ConfigureGlobalProxy(cfg.GetString("http_proxy"), noProxy)
		if cfg.GetString("http_proxy") != "" {
			logger.Debug("Global HTTP proxy enabled: %s", cfg.GetString("http_proxy"))
		}
	}

	// 插件方案：全面采用子进程插件运行时（go-plugin + gRPC），
	// 已舍弃 legacy .so 方案（不再加载/桥接 .so 插件）。

	// Apply the log level from config unless ASTRBOT_LOG_LEVEL is set
	// explicitly in the environment (env takes precedence).
	if os.Getenv("ASTRBOT_LOG_LEVEL") == "" {
		if lvl := cfg.GetString("log_level"); lvl != "" {
			log.GetDefault().SetLevel(log.ParseLevel(lvl))
			logger.I18nInfo("已按配置设置日志级别: %s", lvl)
		}
	}

	// 日志保存：log_file_enable 开启时写分段文件（每段 log_file_max_mb MB，保留最近 3 段）。
	// 未开启则只在内存保留 ~1MB 环形日志（前端 console 页/运存都不会无限增长）。
	if cfg.GetBool("log_file_enable") {
		path := cfg.GetString("log_file_path")
		if path == "" {
			path = "logs/astrbot.log"
		}
		maxMB := cfg.GetInt("log_file_max_mb")
		if maxMB <= 0 {
			maxMB = 5
		}
		if err := log.GetDefault().EnableFileLog(path, int64(maxMB)<<20, 3); err != nil {
			logger.I18nWarn("启用分段日志文件失败: %v", err)
		} else {
			logger.Debug("分段日志已启用: %s (每段 %d MB)", path, maxMB)
		}
	}

	// 3. Initialize conversation manager
	l.conversationMgr = conversation.NewManager(database)
	logger.I18nInfo("会话管理器已初始化")

	// 4. Initialize knowledge base manager
	l.kbMgr = knowledgebase.NewManager()
	logger.I18nInfo("知识库管理器已初始化")

	// 5. Initialize star/plugin manager
	l.starMgr = star.NewManager(database)
	logger.I18nInfo("插件管理器已初始化")

	// 5.5. Register built-in commands
	builtin.RegisterBuiltin(builtin.Deps{
		StarMgr:         l.starMgr,
		ConfigMgr:       l.configMgr,
		ConversationMgr: l.conversationMgr,
	})

	// 6. Initialize event bus
	l.eventBus = core.NewEventBus(1000)
	logger.I18nInfo("事件总线已初始化（缓冲 1000）")

	// 7. Initialize platform manager (must precede pipeline build so
	// RespondStage gets a valid reference).
	l.platformMgr = platform.NewPlatformManager()
	logger.I18nInfo("平台管理器已初始化")

	// 7.2. Initialize skill manager + sandbox manager (must precede pipeline
	// build so ProcessStage can inject skills into the LLM system prompt).
	l.skillMgr = skills.NewSkillManager("data/skills", "data/plugins", "data")
	logger.I18nInfo("技能管理器已初始化（%d 个技能）", len(l.skillMgr.ListSkills(false, "local")))
	l.sandboxMgr = sandbox.NewManager(l.skillMgr)
	l.syncSandboxBooter()
	logger.I18nInfo("沙盒管理器已初始化")

	// 7.5. Build pipeline schedulers
	for _, confID := range l.configMgr.IDs() {
		if err := l.buildPipelineScheduler(confID); err != nil {
			logger.Error("Failed to build pipeline for %s: %v", confID, err)
		}
	}
	logger.I18nInfo("管线调度器已构建（%d 个配置）", len(l.pipelineMapping))

	// 8. Cron manager
	l.cronMgr = cron.NewCronJobManager(database)
	// Fire handler for active_agent jobs: the task note is fed through the
	// normal chat pipeline (like a user message), so the LLM generates the
	// reply and RespondStage delivers it to the job's target session.
	l.cronMgr.RegisterHandler("active_agent", func(ctx context.Context, job *cron.Job) error {
		session, _ := job.Payload["session"].(string)
		note, _ := job.Payload["note"].(string)
		if session == "" {
			return fmt.Errorf("active_agent job %s has no session", job.ID)
		}
		platformID, convID := splitUMO(session)
		senderID, _ := job.Payload["sender_id"].(string)
		if senderID == "" {
			senderID = convID
		}
		chain := &message.MessageChain{Chain: []message.Component{&message.Plain{Text: note}}}
		evt := &core.Event{
			Type:              core.EventMessage,
			Source:            core.EventSource{Platform: platformID, ConvID: convID, SenderID: senderID, SenderName: senderID},
			Message:           chain,
			MessageStr:        note,
			PlainText:         note,
			Timestamp:         time.Now(),
			Metadata:          map[string]interface{}{"proactive": true},
			IsAtOrWakeCommand: true,
			CallLLM:           true,
		}
		if l.eventBus != nil {
			return l.eventBus.Publish(evt)
		}
		return fmt.Errorf("event bus not available")
	})
	l.cronMgr.SetNextRunFn(cronNextRun)
	l.cronMgr.Load()
	go l.cronMgr.Start(ctx)
	logger.I18nInfo("计划任务管理器已启动")

	// 10. Initialize backup exporter
	l.backupExporter = backup.NewExporter("data")
	logger.I18nInfo("备份导出器已初始化")

	// 9.5. Resolve the plugin build toolchain (bundled Go). This only locates
	// an existing toolchain — no network download happens at startup; the
	// compiler provisions it lazily on first plugin build.
	l.toolchain = toolchain.New()
	if bin, err := l.toolchain.GoBin(); err != nil {
		logger.I18nWarn("Go 构建工具链不可用（插件编译已禁用）: %v", err)
	} else {
		logger.I18nInfo("插件构建工具链: %s (GOROOT=%s, GOPATH=%s)", bin, l.toolchain.GOROOT(), l.toolchain.GOPATH())
	}

	// 9.6. Subprocess plugin runtime (go-plugin child processes). Loads
	// installed plugins from the manifest, bridges their handlers into the
	// star pipeline, and re-bridges after a crash-restart swaps an instance.
	l.subPluginMgr = plugin.NewSubprocessManager(l.toolchain, "data")
	l.subPluginMgr.OnInstancesChanged = func() { l.RebridgePlugins() }
	l.subPluginMgr.SetGitHubProxy(cfg.GetString("github_proxy"))
	l.subPluginMgr.SetGoConfig(cfg.GetString("goproxy"), cfg.GetString("goflags"))
	// Install reverse-call hooks (CallAction/SendMessage/RecallMessage/
	// GetConfig/SetConfig/ChatLLM) before plugins load, so handlers can call
	// back into the host.
	plugin.SetHostService(l.platformMgr, l.subPluginMgr, l.chatLLMForPlugins)
	l.subPluginMgr.LoadInstalled(ctx)
	star.RegisterSubprocessPlugins(l.starMgr, l.subPluginMgr.List())

	// (已舍弃 legacy .so 方案：不再加载/桥接 .so 插件)

	// 11. Load platform adapters from config
	if err := l.loadPlatforms(ctx); err != nil {
		logger.Error("Failed to load platforms: %v", err)
	}

	// 12. Start dashboard API server
	managers := map[string]interface{}{
		"config":            l.configMgr,
		"provider":          l.providerMgr,
		"platform":          l.platformMgr,
		"event_bus":         l.eventBus,
		"conversation":      l.conversationMgr,
		"cron":              l.cronMgr,
		"plugin_subprocess": l.subPluginMgr,
		"star":              l.starMgr,
		"knowledgebase":     l.kbMgr,
		"skills":            l.skillMgr,
		"database":          l.database,
	}
	l.dashboard = dashboard.NewServerWithManagers(6185, "data/cmd_config.json", managers)
	if l.webuiDir != "" {
		l.dashboard.SetWebUIDir(l.webuiDir)
	}
	// WebUI"重启"按钮：spawn 新实例 → 优雅停机 → 退出当前进程。
	l.dashboard.SetRestartFunc(l.Restart)
	l.dashboard.SetOnPlatformsChanged(func() {
		go l.ReloadPlatforms(ctx)
	})
	l.dashboard.SetOnPluginsChanged(func() {
		l.RebridgePlugins()
	})
	l.dashboard.SetOnConfigChanged(func() {
		// Rebuild the pipeline so provider/platform settings changes (e.g. the
		// default chat model) take effect immediately instead of on restart.
		if err := l.ReloadPipelineScheduler("default"); err != nil {
			logger.Error("Failed to reload pipeline after config change: %v", err)
		}
	})
	go func() {
		if err := l.dashboard.Start(ctx); err != nil {
			logger.Error("Dashboard server error: %v", err)
		}
	}()

	// 12. Start event bus
	ctx, cancel := context.WithCancel(ctx)
	l.cancel = cancel
	go func() {
		if err := l.eventBus.Start(ctx); err != nil {
			logger.Error("Event bus stopped: %v", err)
		}
	}()

	// 周期归还 Go 堆给 OS：长时间运行下 Go GC 不会把内存还给系统，RSS 只涨不降
	// （用户感知"保存配置/禁用插件后运存不释放"）。每 5 分钟强制回收一次。
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				runtime.GC()
				debug.FreeOSMemory()
			case <-ctx.Done():
				return
			}
		}
	}()

	// Trigger startup hooks
	l.subPluginMgr.TriggerHook(ctx, "startup")

	logger.I18nInfo("AstrBot Go 已启动 - API 端口 :6185")
	return nil
}

// ReloadPipelineScheduler rebuilds the pipeline for a config ID.
func (l *Lifecycle) ReloadPipelineScheduler(confID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Sandbox booter depends on provider_settings.computer_use_runtime and
	// provider_settings.sandbox, which may have changed since startup (e.g. the
	// user configured the sandbox via the WebUI). Re-select it so sandbox tools
	// no longer fail with "no booter configured".
	l.syncSandboxBooter()
	return l.buildPipelineScheduler(confID)
}

// syncSandboxBooter selects the sandbox backend from the current config,
// mirroring Python's computer_client.get_booter. It is a no-op when the
// selection inputs are unchanged. Caller must hold l.mu.
func (l *Lifecycle) syncSandboxBooter() {
	if l.sandboxMgr == nil || l.configMgr == nil {
		return
	}
	cfg := l.configMgr.Get("default")
	if cfg == nil {
		return
	}
	all := cfg.All()
	ps, _ := all["provider_settings"].(map[string]interface{})
	runtime, _ := ps["computer_use_runtime"].(string)
	sandboxCfg, _ := ps["sandbox"].(map[string]interface{})
	if sandboxCfg == nil {
		sandboxCfg = map[string]interface{}{}
	}

	sig := runtime + "|" + string(mustJSON(sandboxCfg))
	if sig == l.sandboxSig {
		return
	}
	l.sandboxSig = sig

	booterType, _ := sandboxCfg["booter"].(string)
	if runtime != "sandbox" {
		l.sandboxMgr.SetBooter(nil)
		logger.I18nInfo("沙盒启动器已清除（computer_use_runtime=%q）", runtime)
		return
	}

	switch booterType {
	case "shipyard_neo", "":
		// URL-based Bay sandbox (Shipyard Neo), the default backend.
		ep, _ := sandboxCfg["shipyard_neo_endpoint"].(string)
		token, _ := sandboxCfg["shipyard_neo_access_token"].(string)
		profile, _ := sandboxCfg["shipyard_neo_profile"].(string)
		ttl := 3600
		if v := floatValue(sandboxCfg["shipyard_neo_ttl"]); v > 0 {
			ttl = int(v)
		}
		l.sandboxMgr.SetBooter(sandbox.NewShipyardNeoBooter(ep, token, profile, ttl))
		logger.I18nInfo("沙盒启动器已设置（shipyard_neo endpoint=%q profile=%q ttl=%d）", ep, profile, ttl)
	case "boxlite":
		// Boxlite = Docker-backed containers.
		l.setDockerOrLocalBooter(sandboxCfg)
	default:
		// shipyard (legacy), cua, and unknown types are not implemented in Go;
		// fall back to a docker/local backend.
		logger.I18nWarn("沙盒启动器类型 %q 未在 Go 实现；回退到 docker/local", booterType)
		l.setDockerOrLocalBooter(sandboxCfg)
	}
}

func (l *Lifecycle) setDockerOrLocalBooter(sandboxCfg map[string]interface{}) {
	image := ""
	if ci, ok := sandboxCfg["cua_image"].(map[string]interface{}); ok {
		if model, _ := ci["model"].(string); model != "" {
			image = model
		}
	}
	if dockerAvailable() {
		l.sandboxMgr.SetBooter(sandbox.NewDockerBooter(image))
		logger.I18nInfo("沙盒启动器已设置（docker, image=%s）", image)
		return
	}
	l.sandboxMgr.SetBooter(sandbox.NewLocalBooter())
	logger.I18nInfo("沙盒启动器已设置（local，docker 不可用）")
}

// floatValue converts a JSON numeric (float64/int) to float64, returning 0 on
// non-numeric values.
func floatValue(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

// mustJSON serializes v deterministically (json sorts map keys), ignoring errors.
func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

// chatLLMForPlugins backs sdk.Host.ChatLLM: calls the host's default chat LLM
// provider with the given prompt + system prompt.
func (l *Lifecycle) chatLLMForPlugins(prompt, systemPrompt string) (string, error) {
	cfg := l.configMgr.Get("default")
	cfgMap := map[string]interface{}{}
	if cfg != nil {
		cfgMap = cfg.All()
	}
	return pipeline.ChatLLMFromConfig(cfgMap, prompt, systemPrompt)
}

// buildPipelineScheduler assembles the full 9-stage pipeline for a config ID
// and registers it with the event bus.
func (l *Lifecycle) buildPipelineScheduler(confID string) error {
	scheduler := core.NewPipelineScheduler(confID)

	cfg := l.configMgr.Get(confID)
	cfgMap := map[string]interface{}{}
	if cfg != nil {
		cfgMap = cfg.All()
	}
	ctx := &pipeline.PipelineContext{
		AstrbotConfig:         cfgMap,
		ConvManager:           l.conversationMgr,
		PluginManager:         l.starMgr,
		PlatformMgr:           l.platformMgr,
		PersonaResolver:       personaResolver,
		PersonaSkillsResolver: personaSkillsResolver,
		UmoAliasResolver:      l.umoAliasResolver,
		SkillManager:          l.skillMgr,
		SandboxManager:        l.sandboxMgr,
		CronManager:           l.cronMgr,
		Database:              l.database,
		EventBus:              l.eventBus,
		SubPlugins:            l.subPluginMgr,
	}

	stageFactory := []func() core.PipelineStage{
		func() core.PipelineStage { return pipeline.NewWakingCheckStage() },
		func() core.PipelineStage { return pipeline.NewWhitelistCheckStage() },
		func() core.PipelineStage { return pipeline.NewSessionStatusCheckStage() },
		func() core.PipelineStage { return pipeline.NewRateLimitStage() },
		func() core.PipelineStage { return pipeline.NewContentSafetyCheckStage() },
		func() core.PipelineStage { return pipeline.NewPreProcessStage() },
		func() core.PipelineStage { return pipeline.NewProcessStage() },
		func() core.PipelineStage { return pipeline.NewResultDecorateStage() },
		func() core.PipelineStage { return pipeline.NewRespondStage() },
	}
	for _, factory := range stageFactory {
		stage := factory()
		if initializer, ok := stage.(interface {
			Initialize(*pipeline.PipelineContext) error
		}); ok {
			if err := initializer.Initialize(ctx); err != nil {
				logger.Error("Pipeline stage %s init failed: %v", stage.Name(), err)
			}
		}
		scheduler.AddStage(stage)
	}

	l.pipelineMapping[confID] = scheduler
	l.eventBus.RegisterScheduler(confID, scheduler)
	logger.I18nInfo("已为 %s 构建管线调度器（9 个阶段）", confID)
	return nil
}

// Stop shuts down all components.
func (l *Lifecycle) Stop() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.cancel != nil {
		l.cancel()
	}
	if l.sandboxMgr != nil {
		l.sandboxMgr.Stop()
	}
	if l.subPluginMgr != nil {
		l.subPluginMgr.Shutdown()
	}
	if l.dashboard != nil {
		l.dashboard.Stop()
	}
	if l.cronMgr != nil {
		l.cronMgr.Stop()
	}
	if l.platformMgr != nil {
		l.platformMgr.StopAll()
	}
	if l.eventBus != nil {
		l.eventBus.Stop()
	}
	if l.database != nil {
		_ = l.database.Close()
	}
	logger.I18nInfo("AstrBot Go 已停止")
}

// Uptime returns time since start.
func (l *Lifecycle) Uptime() time.Duration {
	return time.Since(l.startedAt)
}

// Restart 实现 WebUI"重启"：spawn 当前可执行文件的新实例（独立会话、继承
// stdout/stderr 到原日志文件），随后优雅停机（Stop 会 kill 插件、关闭 DB）
// 并退出当前进程。新实例启动时 cleanupOrphanPlugins 会清理本进程遗留的
// 孤儿插件子进程。在 dashboard handler 的 goroutine 中调用（不持有 l.mu）。
func (l *Lifecycle) Restart() {
	exe, err := os.Executable()
	if err != nil {
		logger.Error("Restart: resolve executable: %v", err)
		return
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logger.Error("Restart: spawn new instance failed: %v", err)
		return
	}
	logger.I18nInfo("重启：已启动新实例（pid %d），正在关闭当前进程", cmd.Process.Pid)
	l.Stop()
	os.Exit(0)
}

// ReloadPlatforms stops all platform adapters and re-instantiates them from config.
// The PlatformManager instance is kept so existing references (pipeline, dashboard) stay valid.
func (l *Lifecycle) ReloadPlatforms(ctx context.Context) {
	if l.platformMgr == nil {
		return
	}
	logger.I18nInfo("正在重载平台适配器...")
	l.platformMgr.StopAll()
	l.platformMgr.Clear()
	if err := l.loadPlatforms(ctx); err != nil {
		logger.Error("Failed to reload platforms: %v", err)
	}
	// Re-register the dashboard-chat reply sink that Clear() wiped.
	if l.dashboard != nil && l.dashboard.ChatAdapter() != nil {
		l.platformMgr.Register(l.dashboard.ChatAdapter())
	}
}

// umoAliasResolver returns the display name a user set for a session (config
// `umo_alias`, set via the /name command), or "".
func (l *Lifecycle) umoAliasResolver(umo string) string {
	if l.configMgr == nil {
		return ""
	}
	cfg := l.configMgr.Get("default")
	if cfg == nil {
		return ""
	}
	all := cfg.All()
	aliases, _ := all["umo_alias"].(map[string]interface{})
	if aliases == nil {
		return ""
	}
	alias, _ := aliases[umo].(string)
	return alias
}

// cronNextRun computes the next run time for a cron job (cron expression or
// one-time run_at), honoring the job's timezone.
func cronNextRun(job *cron.Job) (time.Time, error) {
	loc := time.Local
	if job.Timezone != "" {
		if tz, err := time.LoadLocation(job.Timezone); err == nil {
			loc = tz
		}
	}
	now := time.Now().In(loc)
	if job.RunOnce {
		if job.RunAt.IsZero() {
			return time.Time{}, fmt.Errorf("run_once job has no run_at")
		}
		return job.RunAt.In(loc), nil
	}
	if job.CronExpression == "" {
		return time.Time{}, fmt.Errorf("job %s has no cron expression", job.ID)
	}
	sched, err := cron.ParseCron(job.CronExpression)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(now), nil
}

// splitUMO splits a unified_msg_origin ("platform:convID") into platform id and
// conversation id.
func splitUMO(umo string) (string, string) {
	for i := 0; i < len(umo); i++ {
		if umo[i] == ':' {
			return umo[:i], umo[i+1:]
		}
	}
	return umo, umo
}

// personaCache 缓存 data/personas.json 的解析结果，避免每次 LLM 调用都读盘。
// 通过文件 mtime 判断内容是否变化，仅在有变更时重新读取。
type personaCache struct {
	mu      sync.Mutex
	content []byte
	modTime time.Time
}

var personaFileCache personaCache

// loadPersonas 返回 data/personas.json 解析后的 persona 列表。
// mtime 未变化时直接返回缓存内容，变化时才重读文件。
func loadPersonas() []map[string]interface{} {
	info, err := os.Stat("data/personas.json")
	if err != nil {
		return nil
	}
	personaFileCache.mu.Lock()
	defer personaFileCache.mu.Unlock()
	if personaFileCache.content != nil && personaFileCache.modTime.Equal(info.ModTime()) {
		return parsePersonas(personaFileCache.content)
	}
	data, err := os.ReadFile("data/personas.json")
	if err != nil {
		return nil
	}
	personaFileCache.content = data
	personaFileCache.modTime = info.ModTime()
	return parsePersonas(data)
}

func parsePersonas(data []byte) []map[string]interface{} {
	var store struct {
		Personas []map[string]interface{} `json:"personas"`
	}
	if json.Unmarshal(data, &store) != nil {
		return nil
	}
	return store.Personas
}

// personaResolver resolves a persona's system prompt from data/personas.json
// (persisted by the dashboard persona store). Falls back to provider_settings.persona.
func personaResolver(umo, personaID string) string {
	if personaID == "" {
		return ""
	}
	for _, p := range loadPersonas() {
		if id, _ := p["persona_id"].(string); id == personaID {
			if prompt, ok := p["system_prompt"].(string); ok {
				return prompt
			}
		}
	}
	return ""
}

// personaSkillsResolver returns the skill allow-list configured on a persona
// (data/personas.json). nil = unrestricted, empty slice = no skills allowed.
func personaSkillsResolver(personaID string) []string {
	if personaID == "" {
		return nil
	}
	for _, p := range loadPersonas() {
		if id, _ := p["persona_id"].(string); id != personaID {
			continue
		}
		skillsRaw, ok := p["skills"]
		if !ok || skillsRaw == nil {
			return nil
		}
		list, ok := skillsRaw.([]interface{})
		if !ok {
			return nil
		}
		result := make([]string, 0, len(list))
		for _, v := range list {
			if name, ok := v.(string); ok {
				result = append(result, name)
			}
		}
		return result
	}
	return nil
}

// RebridgePlugins re-registers plugin commands/filters/hooks after plugin
// changes (enable/disable/reload/install/unload) so the pipeline picks up the
// latest set. 全面采用子进程插件运行时（legacy .so 已舍弃）。
func (l *Lifecycle) RebridgePlugins() {
	if l.starMgr == nil {
		return
	}
	star.RemovePluginCommands(l.starMgr)
	star.RemovePluginFilters(l.starMgr)
	star.RemovePluginHooks(l.starMgr)
	if l.subPluginMgr != nil {
		star.RegisterSubprocessPlugins(l.starMgr, l.subPluginMgr.List())
	}
}

// loadPlatforms reads the platform configs and instantiates adapters.
func (l *Lifecycle) loadPlatforms(ctx context.Context) error {
	if l.configMgr == nil || l.platformMgr == nil {
		return nil
	}
	cfg := l.configMgr.Get("default")
	if cfg == nil {
		return nil
	}
	all := cfg.All()
	platforms, _ := all["platform"].([]interface{})
	settings := map[string]interface{}{}
	if ps, ok := all["platform_settings"].(map[string]interface{}); ok {
		settings = ps
	}
	for _, p := range platforms {
		config, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		ptype, _ := config["type"].(string)
		if ptype == "" {
			continue
		}
		// Skip disabled platforms (config `enable: false`). Without this, a
		// disabled bot is still instantiated and its adapter keeps retrying
		// (e.g. QQ official access_token errors) even though the user turned
		// it off.
		if enabled, ok := config["enable"].(bool); ok && !enabled {
			logger.I18nInfo("平台 %s (%s) 在配置中已禁用，跳过", config["id"], ptype)
			continue
		}
		adapter, err := platform.CreatePlatform(ptype, config, settings)
		if err != nil {
			logger.Error("Failed to create platform %s: %v", ptype, err)
			continue
		}
		if bus, ok := adapter.(platform.EventBusSetter); ok {
			bus.SetEventBus(l.eventBus)
		}
		l.platformMgr.Register(adapter)
		if err := adapter.Start(ctx); err != nil {
			logger.Error("Failed to start platform %s: %v", ptype, err)
			continue
		}
		logger.I18nInfo("平台 %s (%s) 已启动", adapter.ID(), adapter.Type())
	}
	return nil
}

// dockerAvailable reports whether the docker CLI is present on this host
// (used to pick a sandbox backend). The result is cached after the first call.
var dockerOnce sync.Once
var dockerOK bool

func dockerAvailable() bool {
	dockerOnce.Do(func() {
		_, err := exec.LookPath(os.Getenv("ASTRBOT_DOCKER_BIN"))
		if err != nil {
			_, err = exec.LookPath("docker")
		}
		dockerOK = err == nil
	})
	return dockerOK
}

// orphanCleanupGrace is how long cleanupOrphanPlugins waits after SIGTERM
// before force-killing surviving orphans.
const orphanCleanupGrace = 1 * time.Second

// cleanupOrphanPlugins 清理上一轮异常退出遗留的孤儿插件子进程（PPID=1、
// 命令行指向 plugins-bin 目录的 go-plugin 子进程）。主进程被 SIGKILL/崩溃
// 时这些进程不会被回收，会一直占用资源；启动时清扫一次防止与后续新实例
// 并存。仅 Linux（依赖 /proc）；其他平台直接跳过。
func cleanupOrphanPlugins() {
	if runtime.GOOS != "linux" {
		return
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	var orphans []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// stat 格式: pid (comm) state ppid ...（comm 含空格时仍在括号内）
		s := string(stat)
		idx := strings.LastIndex(s, ")")
		if idx < 0 {
			continue
		}
		fields := strings.Fields(s[idx+1:])
		if len(fields) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil || ppid != 1 {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		// 插件子进程可执行文件位于 <dataDir>/plugins-bin/<id>/ 下
		if strings.Contains(string(cmdline), "plugins-bin") {
			orphans = append(orphans, pid)
		}
	}
	if len(orphans) == 0 {
		return
	}
	logger.I18nWarn("发现 %d 个上次异常退出遗留的孤儿插件进程，正在清理...", len(orphans))
	for _, pid := range orphans {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	time.Sleep(orphanCleanupGrace)
	for _, pid := range orphans {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	logger.I18nInfo("已清理 %d 个孤儿插件进程", len(orphans))
}
