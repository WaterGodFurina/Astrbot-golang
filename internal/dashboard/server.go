// Package dashboard implements the WebUI API server.
// Ported from astrbot/dashboard/server.py
package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/config"
	"github.com/AstrBotDevs/AstrBot/internal/log"
	"github.com/AstrBotDevs/AstrBot/internal/plugin"
	"github.com/AstrBotDevs/AstrBot/internal/skills"
)

//go:embed web/dist/*
var webFS embed.FS

var logger = log.GetDefault().WithComponent("Dashboard")

// Server is the WebUI API server.
type Server struct {
	mux                *http.ServeMux
	srv                *http.Server
	mu                 sync.RWMutex
	handlers           map[string]http.HandlerFunc
	auth               *PasswordManager
	port               int
	webuiDir           string
	configMgr          interface{} // *config.ConfigManager
	providerMgr        interface{} // *provider.ProviderManager
	platformMgr        interface{} // *platform.PlatformManager
	conversationMgr    interface{} // *conversation.Manager
	cronMgr            interface{} // *cron.CronJobManager
	pluginMgr          interface{} // *plugin.Manager
	kbMgr              interface{} // *knowledgebase.Manager
	skillMgr           interface{} // *skills.SkillManager
	personaMgr         interface{} // *persona.PersonaManager
	personas           *personaStore
	chat               *chatStore
	mcp                *mcpStore
	starMgr            interface{} // *star.Manager
	onPlatformsChanged func()
	onPluginsChanged   func()
}

// SetOnPlatformsChanged registers a callback invoked after platform config changes
// (create/update/delete bots) so the runtime can reload adapters.
func (s *Server) SetOnPlatformsChanged(fn func()) {
	s.onPlatformsChanged = fn
}

// SetOnPluginsChanged registers a callback invoked after plugin changes
// (enable/disable/reload) so the runtime can re-bridge plugin commands.
func (s *Server) SetOnPluginsChanged(fn func()) {
	s.onPluginsChanged = fn
}

// notifyPluginsChanged triggers the plugin reload callback if registered.
func (s *Server) notifyPluginsChanged() {
	if s.onPluginsChanged != nil {
		s.onPluginsChanged()
	}
}

// NewServer creates a dashboard server with password management.
func NewServer(port int, configPath string) *Server {
	s := &Server{
		mux:      http.NewServeMux(),
		handlers: make(map[string]http.HandlerFunc),
		port:     port,
	}
	s.auth = NewPasswordManager(configPath)
	s.personas = newPersonaStore(filepath.Dir(configPath))
	s.chat = newChatStore(filepath.Dir(configPath))
	s.mcp = newMCPStore(filepath.Dir(configPath))
	s.setupRoutes()
	s.srv = &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      s.mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}
	return s
}

// NewServerWithManagers creates a dashboard server with all managers wired up.
func NewServerWithManagers(port int, configPath string, managers map[string]interface{}) *Server {
	s := NewServer(port, configPath)
	if managers != nil {
		if v, ok := managers["config"]; ok {
			s.configMgr = v
		}
		if v, ok := managers["provider"]; ok {
			s.providerMgr = v
		}
		if v, ok := managers["platform"]; ok {
			s.platformMgr = v
		}
		if v, ok := managers["conversation"]; ok {
			s.conversationMgr = v
		}
		if v, ok := managers["cron"]; ok {
			s.cronMgr = v
		}
		if v, ok := managers["plugin"]; ok {
			s.pluginMgr = v
		}
		if v, ok := managers["star"]; ok {
			s.starMgr = v
		}
		if v, ok := managers["knowledgebase"]; ok {
			s.kbMgr = v
		}
		if v, ok := managers["skills"]; ok {
			s.skillMgr = v
		}
		if v, ok := managers["persona"]; ok {
			s.personaMgr = v
		}
	}
	return s
}

// setupRoutes registers API endpoints.
func (s *Server) setupRoutes() {
	s.mux.HandleFunc("/api/", s.apiHandler)
	s.mux.HandleFunc("/health", s.healthHandler)
	// Serve embedded WebUI
	s.mux.HandleFunc("/", s.serveWebUI)
}

// healthHandler returns service health.
func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"version": "0.1.0-go",
	})
}

// handleAuth handles authentication endpoints.
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing action"})
		return
	}
	action := parts[0]
	switch action {
	case "login":
		s.handleLogin(w, r)
	case "logout":
		s.handleLogout(w, r)
	case "check":
		s.handleCheck(w, r)
	case "setup-status":
		s.handleSetupStatus(w, r)
	case "setup":
		s.handleSetup(w, r)
	case "totp":
		if len(parts) > 1 {
			switch parts[1] {
			case "setup":
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"enable": false,
				}))
			case "recovery":
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"recovery_codes": []string{},
				}))
			default:
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown totp action"})
			}
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{"enable": false}))
		}
	case "account":
		if len(parts) > 1 && parts[1] == "edit" {
			s.handleAccountEdit(w, r)
		} else if r.Method == http.MethodPatch {
			s.handleAccountEdit(w, r)
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"username": s.auth.Username(),
			}))
		}
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown auth action"})
	}
}

// handleLogin handles POST /api/auth/login.
// Frontend expects: { status: "ok", data: { token, username, password_upgrade_required, md5_pwd_hint, change_pwd_hint } }
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var creds struct {
		Username        string `json:"username"`
		Password        string `json:"password"`
		Code            string `json:"code"`
		TrustDeviceFlag bool   `json:"trust_device_flag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if s.auth == nil {
		// No auth configured — return a guest session
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"token":                     generateRandomToken(32),
			"username":                  "astrbot",
			"password_upgrade_required": false,
			"md5_pwd_hint":              false,
			"change_pwd_hint":           false,
		}))
		return
	}
	if creds.Username == s.auth.Username() && s.auth.VerifyPassword(creds.Password) {
		token := generateRandomToken(32)
		s.auth.RegisterToken(token)
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"token":                     token,
			"username":                  s.auth.Username(),
			"password_upgrade_required": false,
			"md5_pwd_hint":              false,
			"change_pwd_hint":           s.auth.PasswordChangeRequired(),
		}))
		return
	}
	writeJSON(w, http.StatusUnauthorized, apiError("密码错误"))
}

// handleLogout handles POST /api/auth/logout.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := extractToken(r)
	if s.auth != nil && token != "" {
		s.auth.Logout(token)
	}
	writeJSON(w, http.StatusOK, apiOK(nil))
}

// handleCheck handles GET /api/auth/check.
func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	token := extractToken(r)
	loggedIn := false
	if s.auth != nil {
		loggedIn = s.auth.IsAuthenticated(token)
	}
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"loggedin": loggedIn,
		"username": s.auth.Username(),
	}))
}

// handleSetupStatus handles GET /api/auth/setup-status.
func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	setupRequired := false
	if s.auth != nil {
		setupRequired = s.auth.PasswordChangeRequired()
	}
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"setup_required":             setupRequired,
		"skip_default_password_auth": false,
		"password_upgrade_required":  false,
	}))
}

// handleSetup handles POST /api/auth/setup.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var body struct {
		Username        string `json:"username"`
		Password        string `json:"password"`
		ConfirmPassword string `json:"confirm_password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if s.auth == nil {
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"token":    generateRandomToken(32),
			"username": body.Username,
		}))
		return
	}
	if body.Password != body.ConfirmPassword {
		writeJSON(w, http.StatusOK, apiError("密码不匹配"))
		return
	}
	// Set credentials
	if body.Username != "" {
		s.auth.SetUsername(body.Username)
	}
	if body.Password != "" {
		s.auth.SetPassword(body.Password)
	}
	token := generateRandomToken(32)
	s.auth.RegisterToken(token)
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"token":                     token,
		"username":                  s.auth.Username(),
		"password_upgrade_required": false,
		"md5_pwd_hint":              false,
		"change_pwd_hint":           false,
	}))
}

// handleAccountEdit handles POST /api/auth/account/edit.
func (s *Server) handleAccountEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var body struct {
		Username    string `json:"new_username"`
		Password    string `json:"new_password"`
		OldPassword string `json:"password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if s.auth == nil {
		writeJSON(w, http.StatusOK, apiOK(nil))
		return
	}
	if !s.auth.VerifyPassword(body.OldPassword) {
		writeJSON(w, http.StatusOK, apiError("原密码错误"))
		return
	}
	if body.Username != "" {
		s.auth.SetUsername(body.Username)
	}
	if body.Password != "" {
		s.auth.SetPassword(body.Password)
	}
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"username": s.auth.Username(),
	}))
}

// handleStat handles stat endpoints.
func (s *Server) handleStat(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) == 0 {
		writeJSON(w, http.StatusOK, apiOK(s.getBaseStats()))
		return
	}
	switch parts[0] {
	case "version":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"version":           "4.27.2-go",
			"dashboard_version": "0.1.0-go",
			"python_version":    "go1.23",
		}))
	case "versions":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"versions": []interface{}{},
		}))
	case "start-time":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"start_time": time.Now().Unix(),
		}))
	case "restart-core":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"message": "restart not supported",
		}))
	case "first-notice":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"notice": "",
		}))
	case "test-ghproxy-connection":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"ok": false,
		}))
	case "storage":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"total": 0,
		}))
	case "storage-cleanup", "cleanup":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"cleaned": 0,
		}))
	case "provider-tokens":
		writeJSON(w, http.StatusOK, apiOK(s.getProviderTokenStats()))
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// getBaseStats returns the dashboard statistics consumed by StatsPage.vue.
func (s *Server) getBaseStats() map[string]interface{} {
	start := time.Now()
	return map[string]interface{}{
		"message_count":       0,
		"platform_count":      len(s.getBotList()),
		"platform":            []interface{}{},
		"message_time_series": [][]int{},
		"memory": map[string]interface{}{
			"process": 0,
			"system":  0,
		},
		"cpu_percent": 0,
		"running": map[string]interface{}{
			"hours":   0,
			"minutes": 0,
			"seconds": 0,
		},
		"thread_count": 0,
		"start_time":   start.Unix(),
	}
}

// getProviderTokenStats returns provider token statistics for StatsPage.vue.
func (s *Server) getProviderTokenStats() map[string]interface{} {
	return map[string]interface{}{
		"days": 7,
		"trend": map[string]interface{}{
			"series":       []interface{}{},
			"total_series": [][]int{},
		},
		"range_total_tokens":    0,
		"range_total_calls":     0,
		"range_avg_ttft_ms":     0,
		"range_avg_duration_ms": 0,
		"range_avg_tpm":         0,
		"range_success_rate":    0,
		"range_by_provider":     []interface{}{},
		"range_by_umo":          []interface{}{},
		"today_total_tokens":    0,
		"today_total_calls":     0,
	}
}

// apiOK builds a standard success response with data wrapper.
func apiOK(data interface{}) map[string]interface{} {
	if data == nil {
		data = map[string]interface{}{}
	}
	return map[string]interface{}{
		"status":  "ok",
		"message": nil,
		"data":    data,
	}
}

// apiOKMsg builds a success response with a user-facing message
// (the WebUI shows res.data.message in toasts).
func apiOKMsg(message string, data interface{}) map[string]interface{} {
	if data == nil {
		data = map[string]interface{}{}
	}
	return map[string]interface{}{
		"status":  "ok",
		"message": message,
		"data":    data,
	}
}

// apiError builds a standard error response.
func apiError(message string) map[string]interface{} {
	return map[string]interface{}{
		"status":  "error",
		"message": message,
	}
}

// extractToken gets the Bearer token from the Authorization header.
func extractToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return auth
}

// Start begins serving.
func (s *Server) Start(ctx context.Context) error {
	// Print startup banner with URLs
	if s.auth != nil {
		s.auth.PrintStartupBanner(s.port, getLocalIPs())
	}
	logger.Info("Dashboard API server starting on :%d", s.port)
	go func() {
		<-ctx.Done()
		s.Stop()
	}()
	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Stop shuts down the server.
func (s *Server) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
	logger.Info("Dashboard API server stopped")
}

func mustMarshal(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func writeJSON(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
	logger.Info("API response: %s", mustMarshal(data))
}

// SetWebUIDir sets the external WebUI static files directory.
// When set, serveWebUI reads from this directory first, falling back to the embedded dist.
func (s *Server) SetWebUIDir(dir string) {
	s.webuiDir = dir
}

// serveWebUI serves the Vue dashboard (AstrBot original WebUI).
func (s *Server) serveWebUI(w http.ResponseWriter, r *http.Request) {
	// Strip leading slash for fs lookup
	cleanPath := strings.TrimPrefix(r.URL.Path, "/")
	if cleanPath == "" {
		cleanPath = "index.html"
	}

	// Prefer external WebUI directory if configured
	if s.webuiDir != "" {
		fsPath := filepath.Join(s.webuiDir, cleanPath)
		if data, err := os.ReadFile(fsPath); err == nil {
			w.Header().Set("Content-Type", contentTypeFor(cleanPath))
			if strings.HasPrefix(cleanPath, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			w.Write(data)
			return
		}
	}

	// Fall back to embedded dist directory
	fsPath := "web/dist/" + cleanPath
	data, err := webFS.ReadFile(fsPath)
	if err != nil {
		// SPA fallback: serve index.html for unknown non-file paths
		// (enables Vue Router history mode)
		data, err = webFS.ReadFile("web/dist/index.html")
		if err != nil {
			http.Error(w, "WebUI not available", http.StatusInternalServerError)
			return
		}
	}

	// Set content type based on file extension
	w.Header().Set("Content-Type", contentTypeFor(cleanPath))

	// Cache static assets aggressively (they have content hashes in filenames)
	if strings.HasPrefix(cleanPath, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	w.Write(data)
}

// contentTypeFor returns the MIME type for a file path.
func contentTypeFor(path string) string {
	switch {
	case strings.HasSuffix(path, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(path, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(path, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(path, ".json"):
		return "application/json; charset=utf-8"
	case strings.HasSuffix(path, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(path, ".png"):
		return "image/png"
	case strings.HasSuffix(path, ".jpg"), strings.HasSuffix(path, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(path, ".woff2"):
		return "font/woff2"
	case strings.HasSuffix(path, ".woff"):
		return "font/woff"
	case strings.HasSuffix(path, ".ttf"):
		return "font/ttf"
	case strings.HasSuffix(path, ".ico"):
		return "image/x-icon"
	default:
		return "application/octet-stream"
	}
}

// getLocalIPs returns non-loopback IPv4 addresses for the banner.
func getLocalIPs() []string {
	return []string{}
}

// getConfigData returns the config data for the given config ID.
func (s *Server) getConfigData(configID string) map[string]interface{} {
	if s.configMgr == nil {
		return map[string]interface{}{}
	}
	cm, ok := s.configMgr.(*config.ConfigManager)
	if !ok {
		return map[string]interface{}{}
	}
	cfg := cm.Get(configID)
	if cfg == nil {
		return map[string]interface{}{}
	}
	return cfg.All()
}

// setConfigData updates and persists a key in the default config.
func (s *Server) setConfigData(key string, value interface{}) error {
	if s.configMgr == nil {
		return nil
	}
	cm, ok := s.configMgr.(*config.ConfigManager)
	if !ok {
		return fmt.Errorf("config manager does not support updates")
	}
	cfg := cm.Get("default")
	if cfg == nil {
		return fmt.Errorf("default config not found")
	}
	if err := cfg.Set(key, value); err != nil {
		return err
	}
	return cfg.Save()
}

// setConfigDataAll merges multiple keys into the default config and persists it.
func (s *Server) setConfigDataAll(updates map[string]interface{}) error {
	if s.configMgr == nil {
		return nil
	}
	cm, ok := s.configMgr.(*config.ConfigManager)
	if !ok {
		return fmt.Errorf("config manager does not support updates")
	}
	cfg := cm.Get("default")
	if cfg == nil {
		return fmt.Errorf("default config not found")
	}
	if err := cfg.Update(updates); err != nil {
		return err
	}
	return cfg.Save()
}

// getProviderList returns the provider list from config.
func (s *Server) getProviderList() []interface{} {
	cfg := s.getConfigData("default")
	providers, ok := cfg["provider"].([]interface{})
	if !ok {
		return []interface{}{}
	}
	return providers
}

// getBotList returns the bot/platform list from config.
func (s *Server) getBotList() []interface{} {
	cfg := s.getConfigData("default")
	platforms, ok := cfg["platform"].([]interface{})
	if !ok {
		return []interface{}{}
	}
	return platforms
}

// getConversationList returns all conversations.
func (s *Server) getConversationList() []interface{} {
	if s.conversationMgr == nil {
		return []interface{}{}
	}
	cm, ok := s.conversationMgr.(interface {
		GetAllConversations() []interface{}
	})
	if !ok {
		return []interface{}{}
	}
	return cm.GetAllConversations()
}

// getCronJobs returns all cron jobs.
func (s *Server) getCronJobs() []interface{} {
	if s.cronMgr == nil {
		return []interface{}{}
	}
	cm, ok := s.cronMgr.(interface {
		ListJobsInfo() []map[string]interface{}
	})
	if !ok {
		return []interface{}{}
	}
	jobs := cm.ListJobsInfo()
	result := make([]interface{}, len(jobs))
	for i, j := range jobs {
		result[i] = j
	}
	return result
}

// skillSetActive toggles a skill's active state via the skill manager.
func (s *Server) skillSetActive(name string, active bool) error {
	if s.skillMgr == nil {
		return fmt.Errorf("技能管理器不可用")
	}
	sm, ok := s.skillMgr.(*skills.SkillManager)
	if !ok || sm == nil {
		return fmt.Errorf("技能管理器不可用")
	}
	return sm.SetSkillActive(name, active)
}

// skillDelete removes a skill via the skill manager.
func (s *Server) skillDelete(name string) error {
	if s.skillMgr == nil {
		return fmt.Errorf("技能管理器不可用")
	}
	sm, ok := s.skillMgr.(*skills.SkillManager)
	if !ok || sm == nil {
		return fmt.Errorf("技能管理器不可用")
	}
	return sm.DeleteSkill(name)
}

// getPluginList returns all plugins.
func (s *Server) getPluginList() []interface{} {
	pm, ok := s.pluginMgr.(interface {
		ListPluginsInfo() []map[string]interface{}
	})
	if !ok {
		return []interface{}{}
	}
	plugins := pm.ListPluginsInfo()
	result := make([]interface{}, len(plugins))
	for i, p := range plugins {
		result[i] = p
	}
	return result
}

// pluginManagerIface returns the plugin manager (*plugin.Manager).
func (s *Server) pluginManager() *plugin.Manager {
	pm, _ := s.pluginMgr.(*plugin.Manager)
	return pm
}

func (s *Server) pluginByID(id string) map[string]interface{} {
	pm := s.pluginManager()
	if pm == nil {
		return map[string]interface{}{}
	}
	for _, p := range pm.ListPluginsInfo() {
		if name, _ := p["name"].(string); name == id {
			return p
		}
	}
	return map[string]interface{}{}
}

func (s *Server) pluginFailed() map[string]interface{} {
	pm := s.pluginManager()
	if pm == nil {
		return map[string]interface{}{}
	}
	return pm.ListFailedPlugins()
}

func (s *Server) pluginSetEnabled(id string, enabled bool) {
	pm := s.pluginManager()
	if pm == nil {
		return
	}
	if enabled {
		if err := pm.EnablePlugin(id); err != nil {
			logger.Warn("EnablePlugin(%s): %v", id, err)
		}
	} else {
		if err := pm.DisablePlugin(id); err != nil {
			logger.Warn("DisablePlugin(%s): %v", id, err)
		}
	}
	s.notifyPluginsChanged()
}

func (s *Server) pluginReload(id string) {
	pm := s.pluginManager()
	if pm == nil {
		return
	}
	if id == "" {
		pm.ReloadAll()
	} else {
		_ = pm.Reload(id)
	}
	s.notifyPluginsChanged()
}

func (s *Server) pluginUninstall(id string, deleteConfig bool) {
	pm := s.pluginManager()
	if pm == nil {
		return
	}
	_ = pm.Uninstall(id, deleteConfig)
}

func (s *Server) pluginConfigSchema(id string) map[string]interface{} {
	pm := s.pluginManager()
	if pm == nil {
		return map[string]interface{}{}
	}
	return pm.ConfigSchema(id)
}

func (s *Server) pluginLoadConfig(id string) map[string]interface{} {
	pm := s.pluginManager()
	if pm == nil {
		return map[string]interface{}{}
	}
	return pm.LoadConfig(id)
}

func (s *Server) pluginSaveConfig(id string, cfg map[string]interface{}) {
	pm := s.pluginManager()
	if pm == nil || cfg == nil {
		return
	}
	_ = pm.SaveConfig(id, cfg)
}

// getKBList returns all knowledge bases.
func (s *Server) getKBList() []interface{} {
	if s.kbMgr == nil {
		return []interface{}{}
	}
	km, ok := s.kbMgr.(interface {
		ListKBsInfo() []map[string]interface{}
	})
	if !ok {
		return []interface{}{}
	}
	kbs := km.ListKBsInfo()
	result := make([]interface{}, len(kbs))
	for i, kb := range kbs {
		result[i] = kb
	}
	return result
}

// getSkillList returns all skills.
func (s *Server) getSkillList() []interface{} {
	if s.skillMgr == nil {
		return []interface{}{}
	}
	sm, ok := s.skillMgr.(interface {
		ListSkillsInfo() []map[string]interface{}
	})
	if !ok {
		return []interface{}{}
	}
	skills := sm.ListSkillsInfo()
	result := make([]interface{}, len(skills))
	for i, sk := range skills {
		result[i] = sk
	}
	return result
}

// getPersonaList returns all personas.
func (s *Server) getPersonaList() []interface{} {
	if s.personaMgr == nil {
		return []interface{}{}
	}
	pm, ok := s.personaMgr.(interface {
		All() []interface{}
	})
	if !ok {
		return []interface{}{}
	}
	return pm.All()
}
