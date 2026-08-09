// Package lifecycle implements AstrBot's core lifecycle management.
// Ported from astrbot/core/core_lifecycle.py and initial_loader.py
package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/backup"
	"github.com/AstrBotDevs/AstrBot/internal/config"
	"github.com/AstrBotDevs/AstrBot/internal/conversation"
	"github.com/AstrBotDevs/AstrBot/internal/core"
	"github.com/AstrBotDevs/AstrBot/internal/cron"
	"github.com/AstrBotDevs/AstrBot/internal/dashboard"
	"github.com/AstrBotDevs/AstrBot/internal/db"
	"github.com/AstrBotDevs/AstrBot/internal/knowledgebase"
	"github.com/AstrBotDevs/AstrBot/internal/log"
	"github.com/AstrBotDevs/AstrBot/internal/pipeline"
	"github.com/AstrBotDevs/AstrBot/internal/platform"
	_ "github.com/AstrBotDevs/AstrBot/internal/platform/sources"
	"github.com/AstrBotDevs/AstrBot/internal/plugin"
	"github.com/AstrBotDevs/AstrBot/internal/provider"
	_ "github.com/AstrBotDevs/AstrBot/internal/provider/sources"
	"github.com/AstrBotDevs/AstrBot/internal/sandbox"
	"github.com/AstrBotDevs/AstrBot/internal/skills"
	"github.com/AstrBotDevs/AstrBot/internal/star"
	"github.com/AstrBotDevs/AstrBot/internal/star/builtin"
	"github.com/AstrBotDevs/AstrBot/pkg/message"
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
	pluginMgr       *plugin.Manager
	pluginCtx       *plugin.Context
	skillMgr        *skills.SkillManager
	sandboxMgr      *sandbox.Manager
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

	logger.Info("AstrBot Go - starting initialization")
	l.startedAt = time.Now()

	// 1. Open database
	database, err := db.New("data/astrbot.db")
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	l.database = database
	logger.Info("Database opened (pool: 5 conns, WAL mode)")

	// 2. Load config
	cfg := config.NewConfig("data/cmd_config.json", config.DefaultConfig())
	if err := cfg.Load(); err != nil {
		logger.Warn("Failed to load config, using defaults: %v", err)
	}
	l.configMgr.Register("default", cfg)
	logger.Info("Config loaded (integrity check passed)")

	// 3. Initialize conversation manager
	l.conversationMgr = conversation.NewManager(database)
	logger.Info("Conversation manager initialized")

	// 4. Initialize knowledge base manager
	l.kbMgr = knowledgebase.NewManager()
	logger.Info("Knowledge base manager initialized")

	// 5. Initialize star/plugin manager
	l.starMgr = star.NewManager(database)
	logger.Info("Plugin manager initialized")

	// 5.5. Register built-in commands
	builtin.RegisterBuiltin(builtin.Deps{
		StarMgr:         l.starMgr,
		ConfigMgr:       l.configMgr,
		ConversationMgr: l.conversationMgr,
	})

	// 6. Initialize event bus
	l.eventBus = core.NewEventBus(1000)
	logger.Info("Event bus initialized (buffer: 1000)")

	// 7. Initialize platform manager (must precede pipeline build so
	// RespondStage gets a valid reference).
	l.platformMgr = platform.NewPlatformManager()
	logger.Info("Platform manager initialized")

	// 7.2. Initialize skill manager + sandbox manager (must precede pipeline
	// build so ProcessStage can inject skills into the LLM system prompt).
	l.skillMgr = skills.NewSkillManager("data/skills", "data/plugins", "data")
	logger.Info("Skill manager initialized (%d skills)", len(l.skillMgr.ListSkills(false, "local")))
	l.sandboxMgr = sandbox.NewManager(l.skillMgr)
	if cfg := l.configMgr.Get("default"); cfg != nil {
		if all := cfg.All(); all != nil {
			if ps, ok := all["provider_settings"].(map[string]interface{}); ok {
				if runtime, _ := ps["computer_use_runtime"].(string); runtime == "sandbox" {
					image := ""
					if sb, ok := ps["sandbox"].(map[string]interface{}); ok {
						if ci, ok := sb["cua_image"].(map[string]interface{}); ok {
							if model, _ := ci["model"].(string); model != "" {
								image = model
							}
						}
					}
					l.sandboxMgr.SetBooter(sandbox.NewDockerBooter(image))
					logger.Info("Sandbox manager booter set (docker, image=%s)", image)
				}
			}
		}
	}
	logger.Info("Sandbox manager initialized")

	// 7.5. Build pipeline schedulers
	for _, confID := range l.configMgr.IDs() {
		if err := l.buildPipelineScheduler(confID); err != nil {
			logger.Error("Failed to build pipeline for %s: %v", confID, err)
		}
	}
	logger.Info("Pipeline schedulers built (%d configs)", len(l.pipelineMapping))

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
	logger.Info("Cron job manager started")

	// 10. Initialize backup exporter
	l.backupExporter = backup.NewExporter("data")
	logger.Info("Backup exporter initialized")

	// 10. Load .so plugins
	pluginCtx := &plugin.Context{
		DataDir:   "data",
		ConfigDir: "data",
		PluginDir: "data/plugins",
		Logger:    log.GetDefault().WithComponent("Plugin"),
	}
	l.pluginCtx = pluginCtx
	l.pluginMgr = plugin.NewManager(pluginCtx)
	if err := os.MkdirAll("data/plugins", 0755); err != nil {
		logger.Warn("Failed to create plugins dir: %v", err)
	}
	if loaded, errs := l.pluginMgr.LoadDir("data/plugins"); len(errs) > 0 {
		for _, e := range errs {
			logger.Warn("Plugin load error: %v", e)
		}
	} else {
		logger.Info("Plugins loaded: %d", len(loaded))
	}
	// Bridge .so plugin commands into the pipeline's star handler system.
	star.RegisterPluginCommands(l.starMgr, pluginCtx, l.pluginMgr.AllCommands())
	star.RegisterPluginFilters(l.starMgr, l.pluginMgr.AllFilters())
	star.RegisterPluginHooks(l.starMgr, l.pluginMgr.AllHooks())
	// Apply persisted command configs (enabled/renames/permissions).
	if cfg := l.configMgr.Get("default"); cfg != nil {
		if all := cfg.All(); all != nil {
			if records, ok := all["command_configs"].(map[string]interface{}); ok {
				star.ApplyCommandConfigs(l.starMgr.Handlers(), records)
			}
		}
	}

	// 11. Load platform adapters from config
	if err := l.loadPlatforms(ctx); err != nil {
		logger.Error("Failed to load platforms: %v", err)
	}

	// 12. Start dashboard API server
	managers := map[string]interface{}{
		"config":        l.configMgr,
		"provider":      l.providerMgr,
		"platform":      l.platformMgr,
		"conversation":  l.conversationMgr,
		"cron":          l.cronMgr,
		"plugin":        l.pluginMgr,
		"star":          l.starMgr,
		"knowledgebase": l.kbMgr,
		"skills":        l.skillMgr,
		"database":      l.database,
	}
	l.dashboard = dashboard.NewServerWithManagers(6185, "data/cmd_config.json", managers)
	if l.webuiDir != "" {
		l.dashboard.SetWebUIDir(l.webuiDir)
	}
	l.dashboard.SetOnPlatformsChanged(func() {
		go l.ReloadPlatforms(ctx)
	})
	l.dashboard.SetOnPluginsChanged(func() {
		l.RebridgePlugins()
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

	// Trigger startup hooks
	l.pluginMgr.TriggerHook(ctx, "startup")

	logger.Info("AstrBot Go started - API on :6185")
	return nil
}

// ReloadPipelineScheduler rebuilds the pipeline for a config ID.
func (l *Lifecycle) ReloadPipelineScheduler(confID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buildPipelineScheduler(confID)
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
		SkillManager:          l.skillMgr,
		SandboxManager:        l.sandboxMgr,
		CronManager:           l.cronMgr,
		Database:              l.database,
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
	logger.Info("Pipeline scheduler built for %s (9 stages)", confID)
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
	if l.pluginMgr != nil {
		l.pluginMgr.UnloadAll()
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
	logger.Info("AstrBot Go stopped")
}

// Uptime returns time since start.
func (l *Lifecycle) Uptime() time.Duration {
	return time.Since(l.startedAt)
}

// ReloadPlatforms stops all platform adapters and re-instantiates them from config.
// The PlatformManager instance is kept so existing references (pipeline, dashboard) stay valid.
func (l *Lifecycle) ReloadPlatforms(ctx context.Context) {
	if l.platformMgr == nil {
		return
	}
	logger.Info("Reloading platform adapters...")
	l.platformMgr.StopAll()
	l.platformMgr.Clear()
	if err := l.loadPlatforms(ctx); err != nil {
		logger.Error("Failed to reload platforms: %v", err)
	}
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

// personaResolver resolves a persona's system prompt from data/personas.json
// (persisted by the dashboard persona store). Falls back to provider_settings.persona.
func personaResolver(umo, personaID string) string {
	if personaID == "" {
		return ""
	}
	data, err := os.ReadFile("data/personas.json")
	if err != nil {
		return ""
	}
	var store struct {
		Personas []map[string]interface{} `json:"personas"`
	}
	if json.Unmarshal(data, &store) != nil {
		return ""
	}
	for _, p := range store.Personas {
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
	data, err := os.ReadFile("data/personas.json")
	if err != nil {
		return nil
	}
	var store struct {
		Personas []map[string]interface{} `json:"personas"`
	}
	if json.Unmarshal(data, &store) != nil {
		return nil
	}
	for _, p := range store.Personas {
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

// RebridgePlugins re-registers .so plugin commands/filters/hooks after plugin
// changes (enable/disable/reload) so the pipeline picks up the latest set.
func (l *Lifecycle) RebridgePlugins() {
	if l.starMgr == nil || l.pluginMgr == nil {
		return
	}
	star.RemovePluginCommands(l.starMgr)
	star.RemovePluginFilters(l.starMgr)
	star.RemovePluginHooks(l.starMgr)
	star.RegisterPluginCommands(l.starMgr, l.pluginCtx, l.pluginMgr.AllCommands())
	star.RegisterPluginFilters(l.starMgr, l.pluginMgr.AllFilters())
	star.RegisterPluginHooks(l.starMgr, l.pluginMgr.AllHooks())
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
		logger.Info("Platform %s (%s) started", adapter.ID(), adapter.Type())
	}
	return nil
}
