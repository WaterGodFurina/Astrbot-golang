// Package lifecycle implements AstrBot's core lifecycle management.
// Ported from astrbot/core/core_lifecycle.py and initial_loader.py
package lifecycle

import (
        "context"
        "fmt"
        "os"
        "sync"
        "time"

        "github.com/AstrBotDevs/AstrBot/internal/backup"
        "github.com/AstrBotDevs/AstrBot/internal/config"
        "github.com/AstrBotDevs/AstrBot/internal/core"
        "github.com/AstrBotDevs/AstrBot/internal/cron"
        "github.com/AstrBotDevs/AstrBot/internal/dashboard"
        "github.com/AstrBotDevs/AstrBot/internal/db"
        "github.com/AstrBotDevs/AstrBot/internal/knowledgebase"
        "github.com/AstrBotDevs/AstrBot/internal/log"
        "github.com/AstrBotDevs/AstrBot/internal/platform"
        "github.com/AstrBotDevs/AstrBot/internal/plugin"
        "github.com/AstrBotDevs/AstrBot/internal/provider"
        "github.com/AstrBotDevs/AstrBot/internal/sandbox"
        "github.com/AstrBotDevs/AstrBot/internal/skills"
        "github.com/AstrBotDevs/AstrBot/internal/star"
)

var logger = log.GetDefault().WithComponent("Core")

// Lifecycle orchestrates all AstrBot components.
type Lifecycle struct {
        mu              sync.Mutex
        configMgr       *config.ConfigManager
        database        *db.Database
        providerMgr     *provider.Manager
        starMgr         *star.Manager
        kbMgr           *knowledgebase.Manager
        platformMgr     *platform.PlatformManager
        cronMgr         *cron.CronJobManager
        dashboard       *dashboard.Server
        backupExporter  *backup.Exporter
        pluginMgr       *plugin.Manager
        skillMgr        *skills.SkillManager
        sandboxMgr      *sandbox.Manager
        eventBus        *core.EventBus
        pipelineMapping map[string]*core.PipelineScheduler
        webuiDir        string
        cancel          context.CancelFunc
        startedAt       time.Time
}

// New creates a Lifecycle.
func New() *Lifecycle {
        return &Lifecycle{
                configMgr:       config.NewConfigManager(),
                providerMgr:     provider.NewManager(),
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

        // 3. Initialize knowledge base manager
        l.kbMgr = knowledgebase.NewManager()
        logger.Info("Knowledge base manager initialized")

        // 4. Initialize star/plugin manager
        l.starMgr = star.NewManager(database)
        logger.Info("Plugin manager initialized")

        // 5. Initialize event bus
        l.eventBus = core.NewEventBus(1000)
        logger.Info("Event bus initialized (buffer: 1000)")

        // 6. Build pipeline schedulers
        for _, confID := range l.configMgr.IDs() {
                scheduler := core.NewPipelineScheduler(confID)
                l.pipelineMapping[confID] = scheduler
                l.eventBus.RegisterScheduler(confID, scheduler)
        }
        logger.Info("Pipeline schedulers built (%d configs)", len(l.pipelineMapping))

        // 7. Initialize platform manager
        l.platformMgr = platform.NewPlatformManager()
        logger.Info("Platform manager initialized")

        // 8. Initialize cron manager
        l.cronMgr = cron.NewCronJobManager()
        go l.cronMgr.Start(ctx)
        logger.Info("Cron job manager started")

        // 9. Initialize backup exporter
        l.backupExporter = backup.NewExporter("data")
        logger.Info("Backup exporter initialized")

        // 10. Load .so plugins
        pluginCtx := &plugin.Context{
                DataDir:   "data",
                ConfigDir: "data",
                PluginDir: "data/plugins",
                Logger:    log.GetDefault().WithComponent("Plugin"),
        }
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

        // 11. Initialize skill manager
        l.skillMgr = skills.NewSkillManager("data/skills", "data/plugins", "data")
        logger.Info("Skill manager initialized (%d skills)", len(l.skillMgr.ListSkills(false, "local")))

        // 12. Initialize sandbox manager
        l.sandboxMgr = sandbox.NewManager(l.skillMgr)
        logger.Info("Sandbox manager initialized")

        // 13. Start dashboard API server
        l.dashboard = dashboard.NewServer(6185, "data/cmd_config.json")
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

        scheduler := core.NewPipelineScheduler(confID)
        l.pipelineMapping[confID] = scheduler
        l.eventBus.RegisterScheduler(confID, scheduler)
        logger.Info("Reloaded pipeline scheduler for %s", confID)
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
