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
	"time"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
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
	"github.com/WaterGodFurina/Astrbot-golang/internal/pysdk"
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
	// 孤儿插件清理必须在获取 l.mu 之前执行（6.2）：cleanupOrphanPlugins
	// 在发现孤儿后会 SIGTERM 并休眠 orphanCleanupGrace(1s) 等待退出。若在
	// 持锁状态下休眠，会阻塞 Stop / ReloadPipelineScheduler 等所有其他锁
	// 使用者。放在加锁前执行仍满足顺序约定——清理在插件加载（后续
	// subPluginMgr.LoadInstalled）之前完成。cleanupOrphanPlugins 不触碰
	// 任何 l 的状态，因此锁外执行是安全的。
	cleanupOrphanPlugins()

	l.mu.Lock()
	defer l.mu.Unlock()

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
	cfg := config.NewConfig("data/cmd_config.json")
	if err := cfg.Load(); err != nil {
		logger.I18nWarn("加载配置失败（文件损坏？），已回退到默认配置继续运行: %v", err)
		cfg.ResetToDefaults()
	}
	l.configMgr.Register("default", cfg)
	logger.I18nInfo("配置已加载（完整性校验通过）")
	warnNoAvailableProvider(cfg)
	loadProvidersToManager(cfg, l.providerMgr)

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
		Database:        l.database,
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

	// 9.5. Resolve the plugin build toolchain (bundled Go). This only locates
	// an existing toolchain — no network download happens at startup; the
	// compiler provisions it lazily on first plugin build.
	l.toolchain = toolchain.New()
	if bin, err := l.toolchain.GoBin(); err != nil {
		logger.I18nWarn("Go 构建工具链不可用（插件编译已禁用）: %v", err)
	} else {
		logger.I18nInfo("插件构建工具链: %s (GOROOT=%s, GOPATH=%s)", bin, l.toolchain.GOROOT(), l.toolchain.GOPATH())
	}

	// 9.6a. Subprocess plugin runtime 的**对象**必须在管线构建前创建：
	// ProcessStage 经 PipelineContext.SubPlugins 收集插件 LLM 函数工具，
	// 若此时为 nil，collectPluginTools 直接返回空，插件工具不会注入 LLM
	// （只有后续 ReloadPipelineScheduler 重建管线才恢复）。实例的 LoadInstalled
	// 仍在 9.6 处（管线之后）执行。
	l.subPluginMgr = plugin.NewSubprocessManager(l.toolchain, "data")

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
		// 三段式 PythonUMO（platform_id:MessageType:session_id）：platformID
		// 段是适配器实例 ID、中段是消息类型、末段是投递目标会话 ID。
		parts := strings.SplitN(session, ":", 3)
		if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
			return fmt.Errorf("active_agent job %s 的 session 不是三段式 umo: %q", job.ID, session)
		}
		platformID := parts[0]
		convID := parts[2]
		senderID, _ := job.Payload["sender_id"].(string)
		if senderID == "" {
			senderID = convID
		}
		chain := &message.MessageChain{Chain: []message.Component{&message.Plain{Text: note}}}
		evt := &core.Event{
			Type:              core.EventMessage,
			Source:            core.EventSource{Platform: platformID, PlatformID: platformID, ConvID: convID, SenderID: senderID, SenderName: senderID},
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
	// 派生子上下文供所有后台组件使用（cron/dashboard/eventBus/内存回收等）。
	// 必须用新变量名 runCtx：不能对 ctx 再做 WithCancel 重赋值，否则会与已
	// 启动 goroutine 闭包捕获的 ctx 变量产生数据竞争。
	runCtx, cancel := context.WithCancel(ctx)
	l.cancel = cancel
	go l.cronMgr.Start(runCtx)
	logger.I18nInfo("计划任务管理器已启动")

	// 10. Initialize backup exporter
	l.backupExporter = backup.NewExporter("data")
	logger.I18nInfo("备份导出器已初始化")

	// 9.6. Subprocess plugin runtime (go-plugin child processes). Loads
	// installed plugins from the manifest, bridges their handlers into the
	// star pipeline, and re-bridges after a crash-restart swaps an instance.
	// 对象已在 9.6a（管线构建前）创建；此处只做配置接线与实例装载。
	l.subPluginMgr.OnInstancesChanged = func() { l.RebridgePlugins() }
	l.subPluginMgr.SetGitHubProxy(cfg.GetString("github_proxy"))
	l.subPluginMgr.SetGoConfig(cfg.GetString("goproxy"), cfg.GetString("goflags"))
	// Python 插件依赖安装的 PyPI 镜像与额外 pip 参数（config pypi_index_url /
	// pip_install_arg），供插件 requirements.txt 与宿主 venv 基础依赖安装使用。
	l.subPluginMgr.SetPipConfig(cfg.GetString("pypi_index_url"), cfg.GetString("pip_install_arg"))
	// Python SDK（非嵌入，从 astrbot-python-sdk 仓库下载）的 GitHub 加速前缀。
	pysdk.SetSDKGitHubProxy(cfg.GetString("github_proxy"))
	// pip/venv 安装代理：config http_proxy 优先于系统代理，为空时 pip 才回退
	// 系统 https_proxy（与通用请求"配置为空即直连"不同）。
	pysdk.SetPipProxy(cfg.GetString("http_proxy"))
	// 嵌入式/低内存设备：插件闲置自动卸载（进程内存回收），触发时懒加载唤醒。
	l.syncIdleUnload()
	// Install reverse-call hooks (CallAction/SendMessage/RecallMessage/
	// GetConfig/SetConfig/ChatLLM) before plugins load, so handlers can call
	// back into the host. 同时注入会话/人格/Provider/Star 管理器，供插件
	// 反向调用会话管理、人格解析、Provider 选择与插件安装等管理能力。
	// starMgr 以闭包（StarManagerLike）传入，规避 star→plugin 的导入环。
	// CommandDescriptors 用 star.CollectCommandDescriptors 聚合全局命令
	//（内置 + 子进程插件），供插件桥跨进程枚举指令。
	plugin.SetHostService(l.platformMgr, l.subPluginMgr, l.chatLLMForPlugins, l.conversationMgr, l.providerMgr, plugin.StarManagerLikeFunc{
		Fn: func() []any {
			if l.starMgr == nil {
				return nil
			}
			metas := l.starMgr.Registry().All()
			out := make([]any, 0, len(metas))
			for _, m := range metas {
				out = append(out, m)
			}
			return out
		},
		CmdFn: func() []map[string]any {
			if l.starMgr == nil {
				return nil
			}
			descs := star.CollectCommandDescriptors(l.starMgr.Handlers())
			out := make([]map[string]any, 0, len(descs))
			for _, d := range descs {
				out = append(out, map[string]any{
					"plugin_name":       d.PluginName,
					"handler_full_name": d.HandlerFullName,
					"handler_name":      d.HandlerName,
					"command":           d.EffectiveCommand,
					"aliases":           d.Aliases,
					"description":       d.Description,
					"permission":        d.Permission,
					"enabled":           d.Enabled,
					"parent_group":      d.ParentSignature,
					"is_sub_command":    d.IsSubCommand,
					"command_type":      d.CommandType,
				})
			}
			return out
		},
	}, l.configMgr)
	// 能力接线：先注入固定能力集（平台尚未加载，All() 为空），插件进程
	// 启动时即带 ASTRBOT_HOST_CAPABILITIES 环境变量；loadPlatforms 完成后
	// 再同步一次（含平台 ID），供后续懒加载/重载的插件使用。
	l.syncHostCapabilities()
	l.subPluginMgr.LoadInstalled(runCtx)
	star.RegisterSubprocessPlugins(l.starMgr, l.subPluginMgr, l.subPluginMgr.RegisteredPlugins())

	// (已舍弃 legacy .so 方案：不再加载/桥接 .so 插件)

	// 11. Load platform adapters from config
	if err := l.loadPlatforms(runCtx); err != nil {
		logger.Error("Failed to load platforms: %v", err)
	}
	// 平台就绪后刷新能力集（平台适配器 ID 加入），已运行插件的 env 不会
	// 被追溯更新，但后续 reload/闲置唤醒/新装插件会拿到完整能力集。
	l.syncHostCapabilities()
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
	dashboardPort := 6185
	if dm, ok := cfg.Get("dashboard").(map[string]interface{}); ok {
		if p, ok := dm["port"]; ok {
			switch v := p.(type) {
			case float64:
				dashboardPort = int(v)
			case int:
				dashboardPort = v
			}
		}
	}
	l.dashboard = dashboard.NewServerWithManagers(dashboardPort, "data/cmd_config.json", managers)
	if l.webuiDir != "" {
		l.dashboard.SetWebUIDir(l.webuiDir)
	}
	// WebUI"重启"按钮：spawn 新实例 → 优雅停机 → 退出当前进程。
	l.dashboard.SetRestartFunc(l.Restart)
	l.dashboard.SetOnPlatformsChanged(func() {
		go l.ReloadPlatforms(runCtx)
	})
	l.dashboard.SetOnPluginsChanged(func() {
		l.RebridgePlugins()
	})
	l.dashboard.SetOnConfigChanged(func() {
		// 同步插件休眠阈值：用户在系统配置页直接改 plugin_idle_unload_minutes
		// （非休眠策略 API）时，运行时清扫必须立即生效——否则配置已关（0）
		// 但 sweep 仍按旧阈值继续休眠插件。
		l.syncIdleUnload()
		// Rebuild the pipeline so provider/platform settings changes (e.g. the
		// default chat model) take effect immediately instead of on restart.
		if err := l.ReloadPipelineScheduler("default"); err != nil {
			logger.Error("Failed to reload pipeline after config change: %v", err)
		}
	})
	go func() {
		if err := l.dashboard.Start(runCtx); err != nil {
			logger.Error("Dashboard server error: %v", err)
		}
	}()

	// 12. Start event bus
	go func() {
		if err := l.eventBus.Start(runCtx); err != nil {
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
			case <-runCtx.Done():
				return
			}
		}
	}()

	// Trigger startup hooks
	l.subPluginMgr.TriggerHook(runCtx, "startup")
	// Python SDK 的 on_astrbot_loaded：宿主加载完成后通知所有插件。
	l.subPluginMgr.TriggerHook(runCtx, pluginsdk.EventOnAstrbotLoaded)

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
	computerUseRuntime, _ := ps["computer_use_runtime"].(string)
	sandboxCfg, _ := ps["sandbox"].(map[string]interface{})
	if sandboxCfg == nil {
		sandboxCfg = map[string]interface{}{}
	}

	sig := computerUseRuntime + "|" + string(mustJSON(sandboxCfg))
	if sig == l.sandboxSig {
		return
	}
	l.sandboxSig = sig

	booterType, _ := sandboxCfg["booter"].(string)
	if computerUseRuntime != "sandbox" {
		l.sandboxMgr.SetBooterFactory(nil)
		logger.I18nInfo("沙盒启动器已清除（computer_use_runtime=%q）", computerUseRuntime)
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
		// 每个会话（群/私聊）独立创建沙盒实例（对齐 Python get_booter 的
		// session_booter 模型），每会话最多一个沙盒；总数由 Bay 侧决定。
		l.sandboxMgr.SetBooterFactory(func() sandbox.Booter {
			return sandbox.NewShipyardNeoBooter(ep, token, profile, ttl)
		})
		logger.I18nInfo("沙盒启动器已设置（shipyard_neo endpoint=%q profile=%q ttl=%d）", ep, profile, ttl)
	case "boxlite", "cua":
		// boxlite = Docker-backed 容器；cua = computer-use 容器（镜像取自
		// sandbox.cua_image.model，等价 astrbot-py CuaBooter 的容器化语义：
		// 经 docker exec 提供 shell/文件能力）。共用同一 docker/local 后端。
		l.setDockerOrLocalBooter(sandboxCfg)
	default:
		// shipyard (legacy) 与未知类型未在 Go 实现；回退 docker/local。
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
	// 对齐 Python computer_client.py：用户显式选择容器沙箱（boxlite）时，
	// docker 不可用必须让沙箱启动失败并给出明确指引，绝不静默降级到无隔离
	// 的本地执行（LocalBooter 仅限开发/测试场景）。
	if !dockerAvailable() {
		l.sandboxMgr.SetBooterFactory(nil)
		logger.I18nError("boxlite 沙箱需要 Docker，但当前不可用；请安装 Docker 或改用 shipyard_neo 后端")
		return
	}
	l.sandboxMgr.SetBooterFactory(func() sandbox.Booter {
		return sandbox.NewDockerBooter(image)
	})
	logger.I18nInfo("沙盒启动器已设置（docker, image=%s）", image)
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
// provider with the given prompt + system prompt (+ optional image URLs).
func (l *Lifecycle) chatLLMForPlugins(cmd plugin.ChatLLMCmd) (string, error) {
	cfg := l.configMgr.Get("default")
	cfgMap := map[string]interface{}{}
	if cfg != nil {
		cfgMap = cfg.All()
	}
	return pipeline.ChatLLMFromConfig(cfgMap, cmd.Prompt, cmd.SystemPrompt, cmd.ImageURLs, cmd.AudioURLs, cmd.Tools, cmd.Contexts, cmd.ProviderID)
}

// buildPipelineScheduler assembles the full 10-stage pipeline for a config ID
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
		KBManager:             l.kbMgr,
		KBRetriever: func(umo, query string) (string, error) {
			if l.dashboard == nil {
				return "", nil
			}
			return l.dashboard.RetrieveKBContext(umo, query)
		},
		ProviderManager: l.providerMgr,
		SubPlugins:      l.subPluginMgr,
	}

	stageFactory := []func() core.PipelineStage{
		func() core.PipelineStage { return pipeline.NewSessionWaitStage() },
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
	logger.I18nInfo("已为 %s 构建管线调度器（10 个阶段）", confID)
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
		_ = l.sandboxMgr.Stop()
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

// Restart 实现 WebUI"重启"：先优雅停机（Stop 最先关闭 dashboard 释放监听
// 端口，随后 kill 插件、关闭 DB），再 spawn 当前可执行文件的新实例（独立
// 会话、继承 stdout/stderr 到原日志文件），最后退出当前进程。新实例启动时
// cleanupOrphanPlugins 会清理本进程遗留的孤儿插件子进程。在 dashboard
// handler 的 goroutine 中调用（不持有 l.mu）。
func (l *Lifecycle) Restart() {
	exe, err := os.Executable()
	if err != nil {
		logger.Error("Restart: resolve executable: %v", err)
		return
	}
	// 先优雅停机（Stop 最先关闭 dashboard 释放监听端口，随后 kill 插件、
	// 关闭 DB），再 spawn 新实例——避免新实例绑定仪表盘端口时旧进程仍持有
	// 端口，只能靠 listenWithRetry 反复等待。
	l.Stop()
	// #nosec G204 -- restart spawns this same executable with the original
	// command-line arguments passed at launch; not user-controlled input.
	cmd := exec.Command(exe, os.Args[1:]...) // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	utils.DetachProcess(cmd)
	if err := cmd.Start(); err != nil {
		logger.Error("Restart: spawn new instance failed: %v", err)
		// 本进程已停机（dashboard/DB/插件均已关闭），无法继续提供完整服务，
		// 只能以失败状态退出。
		os.Exit(1)
	}
	logger.I18nInfo("重启：已启动新实例（pid %d），正在退出当前进程", cmd.Process.Pid)
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
	if l.dashboard != nil {
		l.dashboard.ClearWebhooks()
	}
	if err := l.loadPlatforms(ctx); err != nil {
		logger.Error("Failed to reload platforms: %v", err)
	}
	// Re-register the dashboard-chat reply sink that Clear() wiped.
	if l.dashboard != nil && l.dashboard.ChatAdapter() != nil {
		l.platformMgr.Register(l.dashboard.ChatAdapter())
	}
	// 平台集合变化后刷新宿主能力集（已运行插件在下次重启时生效）。
	l.syncHostCapabilities()
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
	// 按路径取单个键，避免热路径（每条消息调用）全量深拷贝整个配置树。
	aliases, _ := cfg.GetNested("umo_alias").(map[string]interface{})
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

// personaCache 缓存 data/personas.json 的解析结果，避免每次 LLM 调用都读盘
// 并重复 JSON 解析。通过文件 mtime 判断内容是否变化，仅在有变更时重读。
type personaCache struct {
	mu      sync.Mutex
	parsed  []map[string]interface{}
	modTime time.Time
}

var personaFileCache personaCache

// loadPersonas 返回 data/personas.json 解析后的 persona 列表。
// mtime 未变化时直接返回缓存的解析结果，变化时才重读文件并重新解析。
func loadPersonas() []map[string]interface{} {
	info, err := os.Stat("data/personas.json")
	if err != nil {
		return nil
	}
	personaFileCache.mu.Lock()
	defer personaFileCache.mu.Unlock()
	if personaFileCache.parsed != nil && personaFileCache.modTime.Equal(info.ModTime()) {
		return personaFileCache.parsed
	}
	data, err := os.ReadFile("data/personas.json")
	if err != nil {
		return nil
	}
	personaFileCache.parsed = parsePersonas(data)
	personaFileCache.modTime = info.ModTime()
	return personaFileCache.parsed
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
			// 类型错误时按最严格语义处理：技能白名单失效等价于禁止全部技能，
			// 而不是 fail-open 变成全量放行。
			logger.I18nWarn("persona %s 的 skills 字段类型错误（应为数组），已按禁止全部技能处理", personaID)
			return []string{}
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

// syncIdleUnload reads plugin_idle_unload_minutes from the default config and
// applies it to the subprocess runtime (0 = 全局休眠关闭，所有插件常驻）。
// 启动与配置热更新共用，保证配置与运行时清扫行为一致。
func (l *Lifecycle) syncIdleUnload() {
	if l.subPluginMgr == nil || l.configMgr == nil {
		return
	}
	cfg := l.configMgr.Get("default")
	if cfg == nil {
		return
	}
	idleMin := cfg.GetInt("plugin_idle_unload_minutes")
	if idleMin < 0 {
		idleMin = 0
	}
	l.subPluginMgr.SetIdleUnload(time.Duration(idleMin) * time.Minute)
}

// fixedHostCapabilities 是宿主无条件公开的固定能力（与 Python AstrBot 的
// 宿主能力一致）：llm（ChatLLM 反向调用）、send_message（SendMessage）、
// recall_message（RecallMessage）、react（React）、t2i（TextToImage）、
// config（GetConfig/SetConfig）、web（Web API 网关）。
var fixedHostCapabilities = []string{
	"llm", "send_message", "recall_message", "react", "t2i", "config", "web",
}

// syncHostCapabilities 计算宿主向 Python 插件公开的能力集合（已注册平台
// 适配器 ID + 固定能力），并推给子进程插件管理器（经 ASTRBOT_HOST_CAPABILITIES
// 环境变量注入插件进程）。启动与平台重载后调用；平台 ID 来自
// platformMgr.All()（如 aiocqhttp/qq_official/telegram/slack…）。
func (l *Lifecycle) syncHostCapabilities() {
	if l.subPluginMgr == nil {
		return
	}
	caps := append([]string(nil), fixedHostCapabilities...)
	seen := make(map[string]struct{}, len(caps))
	for _, c := range caps {
		seen[c] = struct{}{}
	}
	if l.platformMgr != nil {
		for _, a := range l.platformMgr.All() {
			id := a.ID()
			if id == "" {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			caps = append(caps, id)
		}
	}
	l.subPluginMgr.SetHostCapabilities(caps)
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
	star.RemovePluginMetadata(l.starMgr)
	if l.subPluginMgr != nil {
		// RegisteredPlugins 含休眠插件（占位实例），Rebridge 后休眠插件的
		// 命令/过滤器/钩子 handler 依然注册，触发时由 resolveActive 唤醒。
		star.RegisterSubprocessPlugins(l.starMgr, l.subPluginMgr, l.subPluginMgr.RegisteredPlugins())
	}
}

// loadPlatforms reads the platform configs and instantiates adapters.
func (l *Lifecycle) loadPlatforms(ctx context.Context) error {
	if l.configMgr == nil || l.platformMgr == nil {
		return fmt.Errorf("loadPlatforms: config manager or platform manager not initialized")
	}
	cfg := l.configMgr.Get("default")
	if cfg == nil {
		return fmt.Errorf("loadPlatforms: default config missing")
	}
	all := cfg.All()
	platforms, _ := all["platform"].([]interface{})
	settings := map[string]interface{}{}
	if ps, ok := all["platform_settings"].(map[string]interface{}); ok {
		settings = ps
	}
	// Attach provider_settings so adapters that implement quoted-message
	// parsing (quoted_message_parser.*) can read their limits.
	if ps, ok := all["provider_settings"].(map[string]interface{}); ok {
		settings["provider_settings"] = ps
	}
	loaded, skipped, failed := 0, 0, 0
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
			skipped++
			continue
		}
		adapter, err := platform.CreatePlatform(ptype, config, settings)
		if err != nil {
			logger.Error("Failed to create platform %s: %v", ptype, err)
			failed++
			continue
		}
		if bus, ok := adapter.(platform.EventBusSetter); ok {
			bus.SetEventBus(l.eventBus)
		}
		// Star registry injection: platform adapters that register commands
		// (e.g. Discord slash commands) read the active handler registry.
		if sm, ok := adapter.(platform.StarManagerSetter); ok {
			sm.SetStarManager(l.starMgr)
		}
		l.platformMgr.Register(adapter)
		// Unified webhook registration (lark webhook mode).
		if wh, ok := adapter.(platform.WebhookPlatform); ok && l.dashboard != nil {
			l.dashboard.RegisterWebhook(wh.WebhookUUID(), wh.WebhookCallback)
		}
		if err := adapter.Start(ctx); err != nil {
			logger.Error("Failed to start platform %s: %v", ptype, err)
			failed++
			continue
		}
		logger.I18nInfo("平台 %s (%s) 已启动", adapter.ID(), adapter.Type())
		// Python SDK 的 on_platform_loaded：通知所有插件该平台适配器已加载。
		l.subPluginMgr.TriggerHookPayload(ctx, pluginsdk.EventOnPlatformLoaded, map[string]string{"platform": adapter.ID()})
		loaded++
	}
	if loaded == 0 && failed > 0 {
		return fmt.Errorf("loadPlatforms: no platform started (created %d, failed %d, skipped %d)", loaded, failed, skipped)
	}
	if loaded == 0 {
		// 配置里没有平台（或全部禁用）也允许启动：dashboard 等仍可用，
		// 后续真正收发消息时 PlatformManager 会因找不到适配器报错。
		logger.I18nWarn("当前未启动任何平台适配器（配置中平台为空或全部禁用），机器人将以无平台模式运行")
	}
	return nil
}

// dockerCheckTTL is how long dockerAvailable caches its result before re-running
// the LookPath check, so a Docker install after the process started is detected
// within one TTL. Var (not const) so tests can shorten it.
var dockerCheckTTL = 30 * time.Second

// dockerCache memoizes the dockerAvailable result together with its expiry.
var dockerCache struct {
	mu      sync.Mutex
	checked time.Time
	ok      bool
}

// dockerAvailable reports whether the docker CLI is present on this host
// (used to pick a sandbox backend). Unlike a permanent sync.Once cache, the
// result expires after dockerCheckTTL and the LookPath check re-runs, so a
// Docker installed after the first call is eventually detected (6.1).
func dockerAvailable() bool {
	dockerCache.mu.Lock()
	defer dockerCache.mu.Unlock()
	if !dockerCache.checked.IsZero() && time.Since(dockerCache.checked) < dockerCheckTTL {
		return dockerCache.ok
	}
	_, err := exec.LookPath(os.Getenv("ASTRBOT_DOCKER_BIN"))
	if err != nil {
		_, err = exec.LookPath("docker")
	}
	dockerCache.checked = time.Now()
	dockerCache.ok = err == nil
	return dockerCache.ok
}

// orphanCleanupGrace is how long cleanupOrphanPlugins waits after SIGTERM
// before force-killing surviving orphans.
const orphanCleanupGrace = 1 * time.Second

// cleanupOrphanPlugins 清理上一轮异常退出遗留的孤儿插件子进程（PPID=1、
// 可执行路径位于 <dataDir>/plugins-bin/ 下的 go-plugin 子进程）。主进程被
// SIGKILL/崩溃时这些进程不会被回收，会一直占用资源；启动时清扫一次防止与
// 后续新实例并存。仅 Linux（依赖 /proc）；其他平台直接跳过。
func cleanupOrphanPlugins() {
	if runtime.GOOS != "linux" {
		return
	}
	pluginBinPrefix := pluginBinaryPrefix()
	if pluginBinPrefix == "" {
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
		if isOrphanPluginCmdline(cmdline, pluginBinPrefix) {
			orphans = append(orphans, pid)
		}
	}
	if len(orphans) == 0 {
		return
	}
	logger.I18nWarn("发现 %d 个上次异常退出遗留的孤儿插件进程，正在清理...", len(orphans))
	for _, pid := range orphans {
		_ = utils.KillProcess(pid)
	}
	time.Sleep(orphanCleanupGrace)
	for _, pid := range orphans {
		_ = utils.ForceKillProcess(pid)
	}
	logger.I18nInfo("已清理 %d 个孤儿插件进程", len(orphans))
}

// pluginBinaryPrefix returns the resolved absolute prefix of the plugins-bin
// directory under the data dir (e.g. /abs/path/data/plugins-bin/), or "" when
// the data dir cannot be resolved to an absolute path.
func pluginBinaryPrefix() string {
	abs, err := filepath.Abs("data")
	if err != nil {
		return ""
	}
	return filepath.Join(abs, "plugins-bin") + string(os.PathSeparator)
}

// isOrphanPluginCmdline reports whether a /proc/<pid>/cmdline blob belongs to
// a plugin child process: its executable path (argv[0]) must fall under the
// plugins-bin directory. Matching the resolved absolute path prefix (instead
// of a bare "plugins-bin" substring) avoids killing unrelated processes whose
// command line merely mentions plugins-bin.
func isOrphanPluginCmdline(cmdline []byte, pluginBinPrefix string) bool {
	if pluginBinPrefix == "" {
		return false
	}
	args := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
	if len(args) == 0 || args[0] == "" {
		return false
	}
	return strings.HasPrefix(args[0], pluginBinPrefix)
}

// warnNoAvailableProvider 检查配置中是否至少有一个启用的模型提供商。
// 没有时在启动时打印 warn（对齐 Python 原版静默 + 日志提示）；聊天侧不再
// 向平台发送"未找到可用的模型提供商"提示。
func warnNoAvailableProvider(cfg *config.AstrBotConfig) {
	providers, _ := cfg.Get("provider").([]interface{})
	for _, p := range providers {
		pc, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		if enable, _ := pc["enable"].(bool); enable {
			return
		}
	}
	logger.I18nWarn("未找到可用的模型提供商，请先配置")
}

// loadProvidersToManager 将配置中启用的 provider 实例化并填充 ProviderManager。
//
// 宿主 pipeline 在请求时经 CreateProvider 现场实例化 provider（瞬态实例），
// 而插件桥（host_service 的 ListProviders/GetUsingProvider）读取的是
// ProviderManager——若启动时不填充，插件侧（如 livingmemory）永远看到空
// provider 列表。此处按 cmd_config.json 的 provider 数组逐条注册，并按
// provider_settings 设置默认 chat/embedding provider ID。
func loadProvidersToManager(cfg *config.AstrBotConfig, pm *provider.ProviderManager) {
	if pm == nil {
		return
	}
	providers, _ := cfg.Get("provider").([]interface{})
	loaded := 0
	for _, p := range providers {
		pc, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		if enable, _ := pc["enable"].(bool); !enable {
			continue
		}
		id, _ := pc["id"].(string)
		typeName, _ := pc["type"].(string)
		if id == "" || typeName == "" {
			continue
		}
		inst, err := provider.CreateProvider(typeName, pc, map[string]interface{}{})
		if err != nil {
			logger.I18nWarn("Provider %s (%s) 实例化失败，已跳过: %v", id, typeName, err)
			continue
		}
		pm.Register(id, inst)
		loaded++
	}
	if settings, ok := cfg.Get("provider_settings").(map[string]interface{}); ok {
		if v, _ := settings["default_provider_id"].(string); v != "" {
			pm.SetDefaultChatProvider(v)
		}
		if v, _ := settings["default_embedding_provider_id"].(string); v != "" {
			pm.SetDefaultEmbeddingProvider(v)
		}
	}
	if loaded > 0 {
		logger.I18nInfo("ProviderManager 已加载 %d 个 provider（插件桥数据源就绪）", loaded)
	}
}
