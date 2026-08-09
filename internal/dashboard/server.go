// Package dashboard implements the WebUI API server.
// Ported from astrbot/dashboard/server.py
package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/config"
	"github.com/AstrBotDevs/AstrBot/internal/conversation"
	"github.com/AstrBotDevs/AstrBot/internal/cron"
	"github.com/AstrBotDevs/AstrBot/internal/db"
	"github.com/AstrBotDevs/AstrBot/internal/log"
	"github.com/AstrBotDevs/AstrBot/internal/plugin"
	"github.com/AstrBotDevs/AstrBot/internal/provider"
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
	pluginMgr          interface{} // *plugin.Manager (legacy .so)
	subPluginMgr       *plugin.SubprocessManager
	kbMgr              interface{} // *knowledgebase.Manager
	skillMgr           interface{} // *skills.SkillManager
	personaMgr         interface{} // *persona.PersonaManager
	personas           *personaStore
	chat               *chatStore
	mcp                *mcpStore
	starMgr            interface{} // *star.Manager
	database           *db.Database
	startTime          time.Time
	onPlatformsChanged func()
	onPluginsChanged   func()
	onConfigChanged    func()
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

// SetOnConfigChanged registers a callback invoked after config is persisted,
// so the runtime can rebuild the pipeline and pick up the new settings.
func (s *Server) SetOnConfigChanged(fn func()) {
	s.onConfigChanged = fn
}

// notifyConfigChanged triggers the config reload callback if registered.
func (s *Server) notifyConfigChanged() {
	if s.onConfigChanged != nil {
		s.onConfigChanged()
	}
}

// NewServer creates a dashboard server with password management.
func NewServer(port int, configPath string) *Server {
	s := &Server{
		mux:       http.NewServeMux(),
		handlers:  make(map[string]http.HandlerFunc),
		port:      port,
		startTime: time.Now(),
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
		if v, ok := managers["plugin_subprocess"]; ok {
			if pm, ok := v.(*plugin.SubprocessManager); ok {
				s.subPluginMgr = pm
			}
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
		if v, ok := managers["database"]; ok {
			if dbm, ok := v.(*db.Database); ok {
				s.database = dbm
			}
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
	started := s.startTime
	running := time.Since(started)
	messageCount := 0
	todayCalls := 0
	if s.database != nil {
		messageCount = s.database.TotalMessageCount()
		todayCalls = s.database.TodayProviderCalls()
	}
	return map[string]interface{}{
		"message_count":       messageCount,
		"platform_count":      len(s.getBotList()),
		"platform":            []interface{}{},
		"message_time_series": [][]int{},
		"memory": map[string]interface{}{
			"process": processMemoryMB(),
			"system":  systemMemoryMB(),
		},
		"cpu_percent":  processCPUPercent(),
		"today_calls":  todayCalls,
		"running":      runningComponents(running),
		"thread_count": runtime.NumGoroutine(),
		"start_time":   started.Unix(),
	}
}

// runningComponents splits a duration into hours/minutes/seconds.
func runningComponents(d time.Duration) map[string]interface{} {
	total := int(d.Seconds())
	return map[string]interface{}{
		"hours":   total / 3600,
		"minutes": (total % 3600) / 60,
		"seconds": total % 60,
	}
}

// processMemoryMB returns the process resident set size in MB (Linux /proc).
func processMemoryMB() int {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return int(pages * 4096 >> 20)
}

// systemMemoryMB returns total system RAM in MB (Linux /proc/meminfo).
func systemMemoryMB() int {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					return int(kb >> 10)
				}
			}
		}
	}
	return 0
}

// processCPUPercent returns the process CPU usage percentage (Linux /proc).
// It samples the process utime+stime vs total CPU over a 300ms window.
func processCPUPercent() float64 {
	pid := os.Getpid()
	read := func(path string) (int64, int64, bool) {
		data, err := os.ReadFile(path)
		if err != nil {
			return 0, 0, false
		}
		if path == "/proc/stat" {
			// first line: cpu  user nice system idle ...
			line := strings.SplitN(string(data), "\n", 2)[0]
			fields := strings.Fields(line)
			if len(fields) < 5 {
				return 0, 0, false
			}
			var total int64
			for _, f := range fields[1:] {
				v, err := strconv.ParseInt(f, 10, 64)
				if err == nil {
					total += v
				}
			}
			return total, 0, true
		}
		// /proc/<pid>/stat: field 14 = utime, 15 = stime (1-indexed after pid)
		str := string(data)
		idx := strings.LastIndex(str, ")")
		if idx < 0 {
			return 0, 0, false
		}
		rest := strings.Fields(str[idx+1:])
		if len(rest) < 13 {
			return 0, 0, false
		}
		utime, e1 := strconv.ParseInt(rest[11], 10, 64)
		stime, e2 := strconv.ParseInt(rest[12], 10, 64)
		if e1 != nil || e2 != nil {
			return 0, 0, false
		}
		return utime + stime, 0, true
	}

	proc1, _, ok1 := read(fmt.Sprintf("/proc/%d/stat", pid))
	total1, _, ok2 := read("/proc/stat")
	if !ok1 || !ok2 {
		return 0
	}
	time.Sleep(300 * time.Millisecond)
	proc2, _, _ := read(fmt.Sprintf("/proc/%d/stat", pid))
	total2, _, _ := read("/proc/stat")

	dProc := proc2 - proc1
	dTotal := total2 - total1
	if dTotal <= 0 {
		return 0
	}
	cpus := float64(runtime.NumCPU())
	percent := float64(dProc) / float64(dTotal) * 100 * cpus
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	return round1(percent)
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

// getProviderTokenStats returns provider token statistics for StatsPage.vue.
func (s *Server) getProviderTokenStats() map[string]interface{} {
	todayCalls := 0
	todayTokens := 0
	if s.database != nil {
		todayCalls = s.database.TodayProviderCalls()
		todayTokens = s.database.TodayProviderTokens()
	}
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
		"today_total_tokens":    todayTokens,
		"today_total_calls":     todayCalls,
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
	if key == "dashboard" {
		if m, ok := value.(map[string]interface{}); ok {
			s.injectAuthFields(m)
		}
	}
	if err := cfg.Set(key, value); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	s.notifyConfigChanged()
	return nil
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
	if dash, ok := updates["dashboard"].(map[string]interface{}); ok {
		s.injectAuthFields(dash)
	}
	if err := cfg.Update(updates); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	s.notifyConfigChanged()
	return nil
}

// injectAuthFields re-asserts the dashboard auth fields from the password
// manager so config saves never wipe them out.
func (s *Server) injectAuthFields(dash map[string]interface{}) {
	if s.auth == nil {
		return
	}
	dash["username"] = s.auth.Username()
	if h := s.auth.HashedPassword(); h != "" {
		dash["pbkdf2_password"] = h
	}
	if sec := s.auth.JWTSecret(); sec != "" {
		dash["jwt_secret"] = sec
	}
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

// getActiveUMOs returns active session UMOs and their info, mirroring
// Python's session_management_service.list_active_umos.
func (s *Server) getActiveUMOs() map[string]interface{} {
	umos := []string{}
	if s.conversationMgr != nil {
		if cm, ok := s.conversationMgr.(interface{ ActiveUMOs() []string }); ok {
			umos = cm.ActiveUMOs()
		}
	}
	infos := make([]interface{}, 0, len(umos))
	for _, umo := range umos {
		infos = append(infos, conversation.BuildUMOInfo(umo))
	}
	return map[string]interface{}{
		"umos":      umos,
		"umo_infos": infos,
	}
}

// getConversationDetail returns a single conversation by cid.
func (s *Server) getConversationDetail(cid string) map[string]interface{} {
	if s.conversationMgr == nil {
		return nil
	}
	cm, ok := s.conversationMgr.(interface {
		GetConversationByCID(cid string) map[string]interface{}
	})
	if !ok {
		return nil
	}
	return cm.GetConversationByCID(cid)
}

// conversationDeleteByCID removes a conversation by cid.
func (s *Server) conversationDeleteByCID(cid string) bool {
	if s.conversationMgr == nil {
		return false
	}
	cm, ok := s.conversationMgr.(interface {
		DeleteConversationByCID(cid string) bool
	})
	if !ok {
		return false
	}
	return cm.DeleteConversationByCID(cid)
}

// conversationUpdateByCID patches a conversation (title/persona/history).
func (s *Server) conversationUpdateByCID(cid string, patch map[string]interface{}) bool {
	if s.conversationMgr == nil {
		return false
	}
	if title, ok := patch["title"].(string); ok {
		if cm, ok := s.conversationMgr.(interface {
			SetTitleByCID(cid, title string) bool
		}); ok {
			cm.SetTitleByCID(cid, title)
		}
	}
	if persona, ok := patch["persona_id"].(string); ok {
		if cm, ok := s.conversationMgr.(interface {
			SetPersonaByCID(cid, personaID string) bool
		}); ok {
			cm.SetPersonaByCID(cid, persona)
		}
	}
	if raw, ok := patch["history"].([]interface{}); ok {
		history := make([]map[string]interface{}, 0, len(raw))
		for _, item := range raw {
			if m, ok := item.(map[string]interface{}); ok {
				history = append(history, m)
			}
		}
		if cm, ok := s.conversationMgr.(interface {
			ReplaceHistoryByCID(cid string, history []map[string]interface{}) bool
		}); ok {
			return cm.ReplaceHistoryByCID(cid, history)
		}
	}
	return true
}

// getCronJobs returns all cron jobs.
func (s *Server) getCronJobs() []interface{} {
	if s.cronMgr == nil {
		return []interface{}{}
	}
	cm, ok := s.cronMgr.(interface {
		ListInfo() []map[string]interface{}
	})
	if !ok {
		return []interface{}{}
	}
	jobs := cm.ListInfo()
	result := make([]interface{}, len(jobs))
	for i, j := range jobs {
		result[i] = j
	}
	return result
}

// cronCreateJob creates a cron job from a WebUI payload.
func (s *Server) cronCreateJob(body map[string]interface{}) (map[string]interface{}, string) {
	cm, ok := s.cronMgr.(interface {
		AddActiveJob(name, cronExpr string, payload map[string]interface{}, description, timezone string, runOnce bool, runAt time.Time) (*cron.Job, error)
	})
	if !ok || cm == nil {
		return nil, "cron manager not available"
	}
	name, _ := body["name"].(string)
	if name == "" {
		name = "active_agent_task"
	}
	cronExpr, _ := body["cron_expression"].(string)
	note, _ := body["note"].(string)
	if note == "" {
		note, _ = body["description"].(string)
	}
	if note == "" {
		note = name
	}
	timezone, _ := body["timezone"].(string)
	runOnce, _ := body["run_once"].(bool)
	var runAt time.Time
	runAtRaw := ""
	if r, ok := body["run_at"].(string); ok && strings.TrimSpace(r) != "" {
		runAtRaw = r
		if t, err := cron.ParseRunAt(r); err == nil {
			runAt = t
			// A concrete execution time implies a one-time task unless a cron
			// expression is explicitly provided.
			runOnce = true
		}
	}
	if cronExpr != "" {
		runOnce = false
	}
	payload := map[string]interface{}{"note": note, "origin": "api"}
	if runAtRaw != "" {
		payload["run_at"] = runAtRaw
	}
	if session, ok := body["session"].(string); ok {
		payload["session"] = session
	}
	job, err := cm.AddActiveJob(name, cronExpr, payload, note, timezone, runOnce, runAt)
	if err != nil {
		return nil, err.Error()
	}
	return cron.SerializeJob(job), ""
}

// cronDeleteJob removes a cron job by id.
func (s *Server) cronDeleteJob(jobID string) bool {
	cm, ok := s.cronMgr.(interface {
		Remove(id string)
	})
	if !ok || cm == nil {
		return false
	}
	cm.Remove(jobID)
	return true
}

// cronRunJob immediately executes a job's handler in a background goroutine.
func (s *Server) cronRunJob(jobID string) error {
	cm, ok := s.cronMgr.(interface {
		RunNow(id string) error
	})
	if !ok || cm == nil {
		return fmt.Errorf("cron manager not available")
	}
	return cm.RunNow(jobID)
}

// cronUpdateJob patches an existing cron job in place (enabled toggle and/or// schedule fields), keeping it in the list even when disabled.
func (s *Server) cronUpdateJob(jobID string, body map[string]interface{}) (map[string]interface{}, string) {
	cm, ok := s.cronMgr.(interface {
		Get(id string) *cron.Job
		SetEnabled(id string, enabled bool) bool
		UpdateJob(job *cron.Job)
	})
	if !ok || cm == nil {
		return nil, "cron manager not available"
	}
	job := cm.Get(jobID)
	if job == nil {
		return nil, "job not found"
	}
	changed := false
	if v, ok := body["enabled"].(bool); ok {
		cm.SetEnabled(jobID, v)
	}
	if v, ok := body["name"].(string); ok && v != "" {
		job.Name = v
		changed = true
	}
	if v, ok := body["note"].(string); ok && v != "" {
		job.Payload["note"] = v
		job.Description = v
		changed = true
	} else if v, ok := body["description"].(string); ok && v != "" {
		job.Description = v
		changed = true
	}
	if v, ok := body["cron_expression"].(string); ok {
		job.CronExpression = v
		job.RunOnce = false
		changed = true
	}
	if v, ok := body["run_at"].(string); ok && strings.TrimSpace(v) != "" {
		if t, err := cron.ParseRunAt(v); err == nil {
			job.RunAt = t
			job.RunOnce = true
			job.Payload["run_at"] = v
			changed = true
		}
	}
	if v, ok := body["timezone"].(string); ok {
		job.Timezone = v
		changed = true
	}
	if v, ok := body["session"].(string); ok && v != "" {
		job.Payload["session"] = v
		changed = true
	}
	if changed {
		cm.UpdateJob(job)
	}
	return cron.SerializeJob(job), ""
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

// getPluginList returns all plugins: legacy .so plugins plus subprocess
// plugins. Entries are keyed by name and the subprocess runtime wins on
// collisions (a plugin id belongs to one runtime only).
func (s *Server) getPluginList() []interface{} {
	byName := make(map[string]interface{})
	if pm := s.pluginManager(); pm != nil {
		for _, p := range pm.ListPluginsInfo() {
			if name, _ := p["name"].(string); name != "" {
				byName[name] = p
			}
		}
	}
	if s.subPluginMgr != nil {
		for _, p := range s.subPluginMgr.ListInfo() {
			if name, _ := p["name"].(string); name != "" {
				byName[name] = p
			}
		}
	}
	result := make([]interface{}, 0, len(byName))
	for _, p := range byName {
		result = append(result, p)
	}
	return result
}

// pluginManagerIface returns the plugin manager (*plugin.Manager).
func (s *Server) pluginManager() *plugin.Manager {
	pm, _ := s.pluginMgr.(*plugin.Manager)
	return pm
}

// providerManager returns the runtime provider manager, or nil.
func (s *Server) providerManager() *provider.ProviderManager {
	pm, _ := s.providerMgr.(*provider.ProviderManager)
	return pm
}

// unregisterProvider removes a provider instance from the runtime manager so a
// deleted provider stops being usable immediately.
func (s *Server) unregisterProvider(id string) {
	if pm := s.providerManager(); pm != nil {
		pm.Unregister(id)
	}
}

func (s *Server) pluginByID(id string) map[string]interface{} {
	for _, p := range s.getPluginList() {
		if m, ok := p.(map[string]interface{}); ok {
			if name, _ := m["name"].(string); name == id {
				return m
			}
		}
	}
	return map[string]interface{}{}
}

func (s *Server) pluginFailed() map[string]interface{} {
	result := map[string]interface{}{}
	if pm := s.pluginManager(); pm != nil {
		for k, v := range pm.ListFailedPlugins() {
			result[k] = v
		}
	}
	if s.subPluginMgr != nil {
		for k, v := range s.subPluginMgr.ListFailedPlugins() {
			result[k] = v
		}
	}
	return result
}

// resolveSubprocessPlugin maps an identifier (id or name) onto the subprocess
// runtime, returning the manifest ID, the plugin name and whether the plugin
// belongs to the subprocess runtime at all.
func (s *Server) resolveSubprocessPlugin(id string) (string, string, bool) {
	if s.subPluginMgr == nil {
		return "", "", false
	}
	if inst := s.subPluginMgr.Get(id); inst != nil {
		return inst.ID, inst.Name, true
	}
	for _, p := range s.subPluginMgr.ListInfo() {
		pid, _ := p["id"].(string)
		name, _ := p["name"].(string)
		if pid == id || name == id {
			return pid, name, true
		}
	}
	return "", "", false
}

func (s *Server) pluginSetEnabled(id string, enabled bool) {
	if pid, _, ok := s.resolveSubprocessPlugin(id); ok {
		if err := s.subPluginMgr.SetEnabled(pid, enabled); err != nil {
			logger.Warn("SetEnabled(%s): %v", id, err)
		}
		s.notifyPluginsChanged()
		return
	}
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
	if id == "" {
		if pm := s.pluginManager(); pm != nil {
			pm.ReloadAll()
		}
		if s.subPluginMgr != nil {
			s.subPluginMgr.LoadInstalled(context.Background())
		}
		s.notifyPluginsChanged()
		return
	}
	if pid, _, ok := s.resolveSubprocessPlugin(id); ok {
		if err := s.subPluginMgr.Reload(context.Background(), pid); err != nil {
			logger.Warn("Reload(%s): %v", id, err)
		}
		s.notifyPluginsChanged()
		return
	}
	pm := s.pluginManager()
	if pm == nil {
		return
	}
	_ = pm.Reload(id)
	s.notifyPluginsChanged()
}

func (s *Server) pluginUninstall(id string, deleteConfig bool) {
	if pid, _, ok := s.resolveSubprocessPlugin(id); ok {
		if err := s.subPluginMgr.Uninstall(pid, deleteConfig); err != nil {
			logger.Warn("Uninstall(%s): %v", id, err)
		}
		s.notifyPluginsChanged()
		return
	}
	pm := s.pluginManager()
	if pm == nil {
		return
	}
	_ = pm.Uninstall(id, deleteConfig)
	s.notifyPluginsChanged()
}

func (s *Server) pluginConfigSchema(id string) map[string]interface{} {
	if _, name, ok := s.resolveSubprocessPlugin(id); ok {
		return s.subPluginMgr.ConfigSchema(name)
	}
	pm := s.pluginManager()
	if pm == nil {
		return map[string]interface{}{}
	}
	return pm.ConfigSchema(id)
}

func (s *Server) pluginLoadConfig(id string) map[string]interface{} {
	if _, name, ok := s.resolveSubprocessPlugin(id); ok {
		return s.subPluginMgr.LoadConfig(name)
	}
	pm := s.pluginManager()
	if pm == nil {
		return map[string]interface{}{}
	}
	return pm.LoadConfig(id)
}

func (s *Server) pluginSaveConfig(id string, cfg map[string]interface{}) {
	if _, name, ok := s.resolveSubprocessPlugin(id); ok {
		if cfg != nil {
			if err := s.subPluginMgr.SaveConfig(name, cfg); err != nil {
				logger.Warn("SaveConfig(%s): %v", id, err)
			}
		}
		return
	}
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
