// Package dashboard implements the WebUI API server.
// Ported from astrbot/dashboard/server.py
package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/config"
	"github.com/WaterGodFurina/Astrbot-golang/internal/conversation"
	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/cron"
	"github.com/WaterGodFurina/Astrbot-golang/internal/db"
	"github.com/WaterGodFurina/Astrbot-golang/internal/i18n"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/internal/plugin"
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	"github.com/WaterGodFurina/Astrbot-golang/internal/skills"
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
	dataDir            string
	configMgr          interface{} // *config.ConfigManager
	providerMgr        interface{} // *provider.ProviderManager
	platformMgr        interface{} // *platform.PlatformManager
	eventBus           interface{} // *core.EventBus
	chatAdapter        *chatStreamAdapter
	chatBus            *core.EventBus
	conversationMgr    interface{} // *conversation.Manager
	cronMgr            interface{} // *cron.CronJobManager
	subPluginMgr       *plugin.SubprocessManager
	kbMgr              interface{}              // *knowledgebase.Manager
	kbTasks            map[string]*kbUploadTask // knowledge base upload task states
	skillMgr           interface{}              // *skills.SkillManager
	personaMgr         interface{}              // *persona.PersonaManager
	personas           *personaStore
	chat               *chatStore
	mcp                *mcpStore
	starMgr            interface{} // *star.Manager
	database           *db.Database
	startTime          time.Time
	onPlatformsChanged func()
	onPluginsChanged   func()
	onConfigChanged    func()
	// installProgress tracks plugin install progress (keyed by install_id) so
	// the WebUI can poll it while an install request is in flight.
	installProgressMu sync.Mutex
	installProgress   map[string]*installStatus
	// marketCache caches fetched plugin market registry data (keyed by registry
	// URL) so repeated WebUI requests don't hammer the remote.
	marketMu     sync.Mutex
	marketCache  map[string]*marketCacheEntry
	loginLimiter *loginRateLimiter
	// restartFunc 由 lifecycle 注入，供 WebUI"重启"按钮触发进程自重启
	//（spawn 新进程 → 优雅停机 → 退出）。
	restartFunc func()
}

// defaultPluginMarketURL is the default plugin marketplace registry served by
// the AstrBot-Go community market (GitHub Pages).
const defaultPluginMarketURL = "https://astrbotgomarket.350430.xyz/package.json"

// marketCacheEntry is one cached registry snapshot.
type marketCacheEntry struct {
	data      interface{}
	fetchedAt time.Time
}

// installStatus is the live progress state of one plugin install.
type installStatus struct {
	Status     string `json:"status"` // "downloading" | "installing" | "done" | "error"
	Percent    int    `json:"percent"`
	Text       string `json:"text"`
	Downloaded int64  `json:"downloaded"`
	Total      int64  `json:"total"`
}

// ChatAdapter returns the dashboard-chat reply sink adapter (nil when not
// created). Used by the lifecycle to re-register it after platform reloads.
func (s *Server) ChatAdapter() *chatStreamAdapter {
	return s.chatAdapter
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
		dataDir:   filepath.Dir(configPath),
	}
	s.auth = NewPasswordManager(configPath)
	s.personas = newPersonaStore(filepath.Dir(configPath))
	s.chat = newChatStore(filepath.Dir(configPath))
	s.chatAdapter = newChatStreamAdapter()
	s.mcp = newMCPStore(filepath.Dir(configPath))
	s.installProgress = make(map[string]*installStatus)
	s.marketCache = make(map[string]*marketCacheEntry)
	s.kbTasks = make(map[string]*kbUploadTask)
	s.loginLimiter = newLoginRateLimiter()
	s.setupRoutes()
	s.srv = &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           s.mux,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		// 注意：不设置 WriteTimeout —— 它会在请求开始时设定绝对截止，
		// 掐断最长 300s 的 SSE 聊天流（/chat 与 /unified-chat）。慢连接/长流
		// 由各 handler 自带的 deadline 与 r.Context().Done() 兜底。
		IdleTimeout: 5 * time.Minute,
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
			if pm, ok := v.(*platform.PlatformManager); ok {
				// Register the dashboard-chat reply sink so the pipeline's
				// RespondStage / streamSender routes webchat replies here.
				pm.Register(s.chatAdapter)
			}
		}
		if v, ok := managers["event_bus"]; ok {
			s.eventBus = v
			if bus, ok := v.(*core.EventBus); ok {
				s.chatBus = bus
			}
		}
		if v, ok := managers["conversation"]; ok {
			s.conversationMgr = v
		}
		if v, ok := managers["cron"]; ok {
			s.cronMgr = v
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

// loginRateLimitConfig reads dashboard.auth_rate_limit from the config and
// returns (average_interval, max_burst, enabled). Defaults match the config
// template (enable=true, 1.0s interval, burst 3).
func (s *Server) loginRateLimitConfig() (float64, float64, bool) {
	interval := 1.0
	burst := 3.0
	enabled := true
	cfg := s.getConfigData("default")
	if dash, ok := cfg["dashboard"].(map[string]interface{}); ok {
		if arl, ok := dash["auth_rate_limit"].(map[string]interface{}); ok {
			if v, ok := arl["enable"].(bool); ok {
				enabled = v
			}
			if v, ok := arl["average_interval"].(float64); ok && v > 0 {
				interval = v
			}
			if v, ok := arl["max_burst"].(float64); ok && v > 0 {
				burst = v
			}
		}
	}
	return interval, burst, enabled
}

// trustProxyEnabled reports whether X-Forwarded-For should be honored when
// deriving the client IP (dashboard.trust_proxy_headers).
func (s *Server) trustProxyEnabled() bool {
	cfg := s.getConfigData("default")
	if dash, ok := cfg["dashboard"].(map[string]interface{}); ok {
		if v, ok := dash["trust_proxy_headers"].(bool); ok {
			return v
		}
	}
	return false
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
		s.handleTOTP(w, r, parts[1:])
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
	// Brute-force throttle: dashboard.auth_rate_limit.enable (default true)
	// with average_interval / max_burst from config, keyed by client IP.
	if s.loginLimiter != nil {
		interval, burst, enabled := s.loginRateLimitConfig()
		if enabled && !s.loginLimiter.Allow(clientIP(r, s.trustProxyEnabled()), interval, burst) {
			writeJSON(w, http.StatusTooManyRequests, apiError("登录尝试过于频繁，请稍后再试"))
			return
		}
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
		// TOTP 双因素：启用后登录必须携带验证码（或恢复码）。
		// 使用恢复码登录会一次性禁用双因素（对齐 Python 语义）。
		if s.auth.TOTPEnabled() {
			ok, usedRecovery := s.auth.VerifyTOTPEx(creds.Code)
			if !ok {
				writeJSON(w, http.StatusUnauthorized, apiError("TOTP 验证码错误"))
				return
			}
			if usedRecovery {
				logger.I18nWarn("TOTP 恢复码登录，双因素认证已禁用")
				s.auth.DisableTOTP()
			}
		}
		// Sign a persistent JWT (survives restarts) so the WebSocket chat
		// transport keeps authenticating until expiry.
		token, err := s.auth.IssueToken(s.auth.Username())
		if err != nil {
			writeJSON(w, http.StatusOK, apiError("签发会话令牌失败: "+err.Error()))
			return
		}
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

// handleTOTP 处理 TOTP 双因素：setup（两步：生成密钥→验证码启用）、recovery
// （重新生成恢复码并返回明文）、disable、status。
func (s *Server) handleTOTP(w http.ResponseWriter, r *http.Request, parts []string) {
	action := "status"
	if len(parts) > 0 {
		action = parts[0]
	}
	switch action {
	case "setup":
		if s.auth == nil {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{"enable": false}))
			return
		}
		var body struct {
			Secret string `json:"secret"`
			Code   string `json:"code"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Code == "" {
			// 第一步：生成密钥与恢复码（不启用），前端扫码
			secret, otpauth, codes, err := s.auth.GenerateTOTP()
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, apiError(err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"secret":         secret,
				"otpauth_url":    otpauth,
				"recovery_codes": codes,
			}))
			return
		}
		// 第二步：验证码启用
		if !s.auth.EnableTOTP(body.Code) {
			writeJSON(w, http.StatusOK, apiError("验证码错误"))
			return
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{"enable": true}))
	case "recovery":
		if s.auth == nil || !s.auth.TOTPEnabled() {
			writeJSON(w, http.StatusOK, apiError("TOTP 未启用"))
			return
		}
		// 恢复码以哈希存储无法回显，重新生成一批并返回明文（旧恢复码作废）
		_, _, codes, err := s.auth.GenerateTOTP()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, apiError(err.Error()))
			return
		}
		if !s.auth.TOTPEnabled() {
			// GenerateTOTP 会把 enabled 置 false，这里恢复已启用状态
			s.auth.EnableTOTPNoop()
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{"recovery_codes": codes}))
	case "disable":
		if s.auth != nil {
			s.auth.DisableTOTP()
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{"enable": false}))
	case "status", "":
		enabled := s.auth != nil && s.auth.TOTPEnabled()
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{"enable": enabled}))
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown totp action"})
	}
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
		OldPassword     string `json:"old_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("无效的请求体"))
		return
	}
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
	// Outside the first-install onboarding flow (a random password was set and
	// a change is required), the caller must prove knowledge of the current
	// password so this endpoint cannot be used to hijack the account.
	if !s.auth.PasswordChangeRequired() && !s.auth.VerifyPassword(body.OldPassword) {
		writeJSON(w, http.StatusUnauthorized, apiError("旧密码错误"))
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
		// offset_sec is the lookback window for the message trend / platform
		// ranking (the WebUI sends it; default to the last 24h).
		offset := 24 * 60 * 60
		if v := r.URL.Query().Get("offset_sec"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				offset = n
			}
		}
		writeJSON(w, http.StatusOK, apiOK(s.getBaseStats(offset)))
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
		days := 1
		if v := r.URL.Query().Get("days"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && (n == 1 || n == 3 || n == 7) {
				days = n
			}
		}
		writeJSON(w, http.StatusOK, apiOK(s.getProviderTokenStats(days)))
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// getBaseStats returns the dashboard statistics consumed by StatsPage.vue.
func (s *Server) getBaseStats(offsetSec int) map[string]interface{} {
	started := s.startTime
	running := time.Since(started)
	messageCount := 0
	todayCalls := 0
	var platformRank []map[string]interface{}
	var timeSeries [][]int
	if s.database != nil {
		messageCount = s.database.TotalMessageCount()
		todayCalls = s.database.TodayProviderCalls()
		platformRank = s.database.PlatformMessageRank(offsetSec)
		// 1-hour buckets for the message trend chart.
		timeSeries = s.database.MessageTimeSeries(offsetSec, 3600)
	}
	if platformRank == nil {
		platformRank = []map[string]interface{}{}
	}
	if timeSeries == nil {
		timeSeries = [][]int{}
	}
	return map[string]interface{}{
		"message_count":       messageCount,
		"platform_count":      len(s.getBotList()),
		"platform":            platformRank,
		"message_time_series": timeSeries,
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
// It samples the process utime+stime vs total CPU over a 300ms window and
// caches the result for cpuPercentCacheTTL so concurrent /stats polls do not
// each block for 300ms.
func processCPUPercent() float64 {
	cpuCacheMu.Lock()
	if time.Since(cpuCacheAt) < cpuPercentCacheTTL {
		v := cpuCacheVal
		cpuCacheMu.Unlock()
		return v
	}
	cpuCacheMu.Unlock()

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
	result := round1(percent)
	cpuCacheMu.Lock()
	cpuCacheVal = result
	cpuCacheAt = time.Now()
	cpuCacheMu.Unlock()
	return result
}

// cpuPercentCacheTTL bounds how often processCPUPercent re-samples /proc.
const cpuPercentCacheTTL = 5 * time.Second

var (
	cpuCacheMu  sync.Mutex
	cpuCacheAt  time.Time
	cpuCacheVal float64
)

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

// getProviderTokenStats returns provider token statistics for StatsPage.vue.
// It aggregates provider_stats over the given lookback window (1/3/7 days):
// hourly token trend per provider, total tokens/calls/success rate, TTFT and
// duration averages, provider ranking and per-session (umo) token ranking, plus
// today's totals and per-model/provider breakdowns.
func (s *Server) getProviderTokenStats(days int) map[string]interface{} {
	empty := func() map[string]interface{} {
		return map[string]interface{}{
			"days":                  days,
			"trend":                 map[string]interface{}{"series": []interface{}{}, "total_series": [][]int{}},
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
			"today_by_provider":     []interface{}{},
			"today_by_model":        []interface{}{},
		}
	}
	if s.database == nil {
		return empty()
	}

	now := time.Now()
	loc := now.Location()
	// rangeStart is `days` days ago (same hour), floored to the hour — this
	// mirrors Python's range_start_local. todayStart is local midnight.
	rangeStart := now.AddDate(0, 0, -days)
	rangeStart = time.Date(rangeStart.Year(), rangeStart.Month(), rangeStart.Day(), rangeStart.Hour(), 0, 0, 0, loc)
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	queryStart := rangeStart
	if todayStart.Before(rangeStart) {
		queryStart = todayStart
	}

	records, err := s.database.ProviderStatsSince(queryStart.Add(-24 * time.Hour).UTC())
	if err != nil {
		logger.I18nWarn("ProviderStatsSince: %v", err)
		return empty()
	}

	// Bucket timestamps (hourly) in local time, as unix ms.
	var bucketTimestamps []int64
	for t := rangeStart; !t.After(now); t = t.Add(time.Hour) {
		bucketTimestamps = append(bucketTimestamps, t.UnixMilli())
	}

	trendByProvider := map[string]map[int64]int{}
	totalByProvider := map[string]int{}
	totalByUmo := map[string]int{}
	totalByBucket := map[int64]int{}
	todayByModel := map[string]int{}
	todayByProvider := map[string]int{}
	var rangeTotalTokens, rangeTotalCalls, rangeSuccessCalls int

	for _, rec := range records {
		createdLocal := rec.CreatedAt.In(loc)
		tokenTotal := rec.InputOther + rec.InputCached + rec.Output
		if createdLocal.Year() < 2020 {
			continue
		}
		providerID := rec.ProviderID
		if providerID == "" {
			providerID = "unknown"
		}
		model := rec.Model
		if model == "" {
			model = "Unknown"
		}

		if createdLocal.After(rangeStart) || createdLocal.Equal(rangeStart) {
			bucket := time.Date(createdLocal.Year(), createdLocal.Month(), createdLocal.Day(), createdLocal.Hour(), 0, 0, 0, loc).UnixMilli()
			if trendByProvider[providerID] == nil {
				trendByProvider[providerID] = map[int64]int{}
			}
			trendByProvider[providerID][bucket] += tokenTotal
			totalByProvider[providerID] += tokenTotal
			totalByUmo[rec.UMO] += tokenTotal
			totalByBucket[bucket] += tokenTotal
			rangeTotalTokens += tokenTotal
			rangeTotalCalls++
			rangeSuccessCalls++
		}

		if createdLocal.After(todayStart) || createdLocal.Equal(todayStart) {
			todayByModel[model] += tokenTotal
			todayByProvider[providerID] += tokenTotal
		}
	}

	// Today's totals are computed directly (created_at is UTC in SQLite).
	todayCalls := 0
	todayTokens := 0
	if s.database != nil {
		todayCalls = s.database.TodayProviderCalls()
		todayTokens = s.database.TodayProviderTokens()
	}

	// Sort providers by total tokens (desc).
	sortedProviders := sortedKeysByValue(totalByProvider)

	var series []map[string]interface{}
	for _, pid := range sortedProviders {
		pts := trendByProvider[pid]
		data := make([][]int, 0, len(bucketTimestamps))
		for _, bt := range bucketTimestamps {
			data = append(data, []int{int(bt), pts[bt]})
		}
		series = append(series, map[string]interface{}{
			"name":         pid,
			"data":         data,
			"total_tokens": totalByProvider[pid],
		})
	}
	if series == nil {
		series = []map[string]interface{}{}
	}

	totalSeries := make([][]int, 0, len(bucketTimestamps))
	for _, bt := range bucketTimestamps {
		totalSeries = append(totalSeries, []int{int(bt), totalByBucket[bt]})
	}
	if totalSeries == nil {
		totalSeries = [][]int{}
	}

	rangeByProvider := make([]map[string]interface{}, 0, len(sortedProviders))
	for _, pid := range sortedProviders {
		rangeByProvider = append(rangeByProvider, map[string]interface{}{
			"provider_id": pid,
			"tokens":      totalByProvider[pid],
		})
	}

	sortedUmo := sortedKeysByValue(totalByUmo)
	rangeByUmo := make([]map[string]interface{}, 0, len(sortedUmo))
	for _, umo := range sortedUmo {
		rangeByUmo = append(rangeByUmo, map[string]interface{}{
			"umo":    umo,
			"tokens": totalByUmo[umo],
		})
	}

	todayByProviderData := make([]map[string]interface{}, 0, len(todayByProvider))
	for _, pid := range sortedKeysByValue(todayByProvider) {
		todayByProviderData = append(todayByProviderData, map[string]interface{}{
			"provider_id": pid,
			"tokens":      todayByProvider[pid],
		})
	}
	todayByModelData := make([]map[string]interface{}, 0, len(todayByModel))
	for _, m := range sortedKeysByValue(todayByModel) {
		todayByModelData = append(todayByModelData, map[string]interface{}{
			"provider_model": m,
			"tokens":         todayByModel[m],
		})
	}

	successRate := 0.0
	if rangeTotalCalls > 0 {
		successRate = float64(rangeSuccessCalls) / float64(rangeTotalCalls)
	}

	return map[string]interface{}{
		"days":                  days,
		"trend":                 map[string]interface{}{"series": series, "total_series": totalSeries},
		"range_total_tokens":    rangeTotalTokens,
		"range_total_calls":     rangeTotalCalls,
		"range_avg_ttft_ms":     0,
		"range_avg_duration_ms": 0,
		"range_avg_tpm":         0,
		"range_success_rate":    successRate,
		"range_by_provider":     rangeByProvider,
		"range_by_umo":          rangeByUmo,
		"today_total_tokens":    todayTokens,
		"today_total_calls":     todayCalls,
		"today_by_provider":     todayByProviderData,
		"today_by_model":        todayByModelData,
	}
}

// sortedKeysByValue returns map keys sorted by their int values descending.
func sortedKeysByValue(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return m[keys[i]] > m[keys[j]]
	})
	return keys
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
	// Honor dashboard.disable_access_log (default true): keep the API response
	// log line off unless explicitly enabled.
	if cm, ok := s.configMgr.(*config.ConfigManager); ok {
		if cfg := cm.Get("default"); cfg != nil {
			if dash, ok := cfg.Get("dashboard").(map[string]interface{}); ok {
				if v, ok := dash["disable_access_log"].(bool); ok {
					setAPILogDisabled(v)
				}
			}
		}
	}
	// Print startup banner with URLs
	if s.auth != nil {
		s.auth.PrintStartupBanner(s.port, getLocalIPs())
	}
	logger.I18nInfo("仪表盘 API 服务器正在 :%d 端口启动", s.port)
	go func() {
		<-ctx.Done()
		s.Stop()
	}()

	// Bind with retry: after a WebUI "restart" the new instance is spawned while
	// the old process is still shutting down, so the port may briefly be in use.
	// Retry binding (bounded by ctx) instead of dying with EADDRINUSE.
	addr := fmt.Sprintf(":%d", s.port)
	ln, bindErr := listenWithRetry(ctx, addr, s.port)
	if bindErr != nil {
		return bindErr
	}
	defer ln.Close()
	if enable, cert, key := s.sslConfig(); enable && cert != "" && key != "" {
		logger.I18nInfo("仪表盘 HTTPS 已启用（证书=%s）", cert)
		if err := s.srv.ServeTLS(ln, cert, key); err != nil && err != http.ErrServerClosed {
			return err
		}
	} else if enable {
		logger.I18nWarn("dashboard.ssl.enable=true 但未配置 cert_file/key_file，回退到 HTTP")
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			return err
		}
	} else if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// listenWithRetry binds addr, retrying when the port is briefly still held by
// a shutting-down predecessor (EADDRINUSE). Bounded by ctx so cancellation
// (shutdown) aborts the wait immediately.
func listenWithRetry(ctx context.Context, addr string, port int) (net.Listener, error) {
	const (
		maxAttempts  = 40
		retryBackoff = 500 * time.Millisecond
	)
	for attempt := 1; ; attempt++ {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
		if !isAddrInUse(err) {
			return nil, fmt.Errorf("bind dashboard %s: %w", addr, err)
		}
		if attempt >= maxAttempts {
			return nil, fmt.Errorf("bind dashboard %s: port still in use after %d attempts: %w", addr, maxAttempts, err)
		}
		logger.I18nWarn("仪表盘端口 :%d 仍被占用，等待释放后重试（%d/%d）", port, attempt, maxAttempts)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryBackoff):
		}
	}
}

func isAddrInUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return errors.Is(opErr.Err, syscall.EADDRINUSE) ||
			errors.Is(opErr.Err, syscall.EADDRNOTAVAIL)
	}
	return false
}

// sslConfig 读取 dashboard.ssl 配置（enable/cert_file/key_file）。
func (s *Server) sslConfig() (bool, string, string) {
	cfg := s.getConfigData("default")
	dash, ok := cfg["dashboard"].(map[string]interface{})
	if !ok {
		return false, "", ""
	}
	ssl, ok := dash["ssl"].(map[string]interface{})
	if !ok {
		return false, "", ""
	}
	enable, _ := ssl["enable"].(bool)
	cert, _ := ssl["cert_file"].(string)
	key, _ := ssl["key_file"].(string)
	return enable, cert, key
}

// Stop shuts down the server.
func (s *Server) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
	logger.I18nInfo("仪表盘 API 服务器已停止")
}

func mustMarshal(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func writeJSON(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
	if apiLogDisabled.Load() {
		return
	}
	s := mustMarshal(data)
	if len(s) > 300 {
		s = s[:300] + "...(truncated)"
	}
	logger.Debug("API response: %s", s)
}

// apiLogDisabled gates the per-request "API response" log line (default true,
// mirroring dashboard.disable_access_log). Logging every response floods the
// console page and inflates the log history, freezing the WebUI.
var apiLogDisabled atomic.Bool

func init() {
	apiLogDisabled.Store(true)
}

func setAPILogDisabled(v bool) { apiLogDisabled.Store(v) }

// SetWebUIDir sets the external WebUI static files directory.
// When set, serveWebUI reads from this directory first, falling back to the embedded dist.
func (s *Server) SetWebUIDir(dir string) {
	s.webuiDir = dir
}

// SetRestartFunc 注入进程自重启回调（由 lifecycle 提供），供 WebUI 重启按钮触发。
func (s *Server) SetRestartFunc(fn func()) {
	s.restartFunc = fn
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
		// 防路径穿越：拼接后校验仍在 webuiDir 内（嵌入式 webFS 自身会拒绝
		// `..`，但外部目录分支是 os.ReadFile，需显式防护）。
		fsPath := filepath.Join(s.webuiDir, cleanPath)
		if rel, err := filepath.Rel(s.webuiDir, fsPath); err != nil || strings.HasPrefix(rel, "..") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
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
	// 运行时语言切换钩子：单 key 保存 language 时立即生效
	if key == "language" {
		if lang, ok := value.(string); ok && lang != "" {
			i18n.SetLocale(lang)
		}
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
	// 运行时语言切换钩子：config 的 language 变化后立即生效（无需重启），
	// 之后的日志/指令文案按新语言输出（之前的日志不追溯）。
	if lang := cfg.GetString("language"); lang != "" {
		i18n.SetLocale(lang)
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
	// The plaintext password mirrors the hash so a config save round-trip
	// (which strips "password") does not drop the persisted credential.
	if p := s.auth.PlainPassword(); p != "" {
		dash["password"] = p
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

// getPluginList returns all subprocess plugins. Entries are keyed by name.
func (s *Server) getPluginList() []interface{} {
	byName := make(map[string]interface{})
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

// setInstallProgress updates the progress state for an install_id.
func (s *Server) setInstallProgress(id string, st *installStatus) {
	if id == "" {
		return
	}
	s.installProgressMu.Lock()
	defer s.installProgressMu.Unlock()
	s.installProgress[id] = st
}

// getInstallProgress returns the current progress state for an install_id.
func (s *Server) getInstallProgress(id string) *installStatus {
	s.installProgressMu.Lock()
	defer s.installProgressMu.Unlock()
	if st := s.installProgress[id]; st != nil {
		return st
	}
	return &installStatus{Status: "unknown"}
}

// installProgressCallback builds a progress callback that records toolchain
// download progress (the ~150-200MB first-time download) for the given install_id.
func (s *Server) installProgressCallback(installID string) func(downloaded, total int64) {
	return func(downloaded, total int64) {
		if installID == "" {
			logger.Debug("installStage: skip (empty id)")
			return
		}
		percent := 0
		if total > 0 {
			percent = int(downloaded * 100 / total)
		}
		s.setInstallProgress(installID, &installStatus{
			Status:     "downloading",
			Percent:    percent,
			Text:       "下载依赖中…",
			Downloaded: downloaded,
			Total:      total,
		})
	}
}

// installStageCallback builds a callback that records a human-readable phase
// text (e.g. "下载 C 编译器 (Clang)…", "编译插件…") for the given install_id,
// shown by the WebUI while no byte progress is available.
func (s *Server) installStageCallback(installID string) func(text string) {
	return func(text string) {
		if installID == "" {
			logger.Debug("installStage: skip (empty id)")
			return
		}
		logger.Debug("installStage[%s]: %s", installID, text)
		cur := s.getInstallProgress(installID)
		status := "installing"
		percent := 0
		if cur.Status == "downloading" {
			status = "downloading"
			percent = cur.Percent
		}
		// 编译阶段没有字节级进度，给一个中间进度值让 WebUI 进度条推进，
		// 否则停留在 0% 看起来像卡住（cgo 编译可达数分钟）。
		if strings.Contains(text, "编译") {
			percent = 60
		} else if cur.Percent >= 60 {
			// `go build -v` 的实时输出行（下载依赖/编译包名）继续更新 text，
			// 但保持编译阶段的中间进度不回落。
			percent = cur.Percent
		}
		s.setInstallProgress(installID, &installStatus{
			Status:     status,
			Percent:    percent,
			Text:       text,
			Downloaded: cur.Downloaded,
			Total:      cur.Total,
		})
	}
}

func (s *Server) pluginByID(id string) map[string]interface{} {
	for _, p := range s.getPluginList() {
		if m, ok := p.(map[string]interface{}); ok {
			if name, _ := m["name"].(string); name == id {
				// WebUI "行为" 详情页需要 commands/tools/hooks 组件；从插件
				// Register 元数据填充（否则显示"未知"）。
				if _, pname, ok2 := s.resolveSubprocessPlugin(id); ok2 && s.subPluginMgr != nil {
					if comps := s.subPluginMgr.Components(pname); comps != nil {
						m["components"] = comps
					}
				}
				return m
			}
		}
	}
	return map[string]interface{}{}
}

func (s *Server) pluginFailed() map[string]interface{} {
	result := map[string]interface{}{}
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
			logger.I18nWarn("设置插件 %s 启用状态失败: %v", id, err)
		}
		s.notifyPluginsChanged()
	}
}

func (s *Server) pluginReload(id string) {
	if id == "" {
		if s.subPluginMgr != nil {
			s.subPluginMgr.LoadInstalled(context.Background())
		}
		s.notifyPluginsChanged()
		return
	}
	if pid, _, ok := s.resolveSubprocessPlugin(id); ok {
		if err := s.subPluginMgr.Reload(context.Background(), pid); err != nil {
			logger.I18nWarn("重载插件 %s 失败: %v", id, err)
		}
		s.notifyPluginsChanged()
	}
}

func (s *Server) pluginUninstall(id string, deleteConfig, deleteData bool) {
	if pid, _, ok := s.resolveSubprocessPlugin(id); ok {
		if err := s.subPluginMgr.Uninstall(pid, deleteConfig, deleteData); err != nil {
			logger.I18nWarn("卸载插件 %s 失败: %v", id, err)
		}
		s.notifyPluginsChanged()
	}
}

func (s *Server) pluginConfigSchema(id string) map[string]interface{} {
	if _, name, ok := s.resolveSubprocessPlugin(id); ok {
		return s.subPluginMgr.ConfigSchema(name)
	}
	return map[string]interface{}{}
}

// pluginConfigPayload returns the plugin config dialog payload consumed by the
// WebUI's AstrBotConfig component: {plugin_name, log_level, metadata, config,
// i18n}. metadata is keyed by the plugin name and carries the flat schema
// ("items"), matching the Python reference (config_service.get_plugin_config).
func (s *Server) pluginConfigPayload(id string) map[string]interface{} {
	name := id
	sub := false
	if _, n, ok := s.resolveSubprocessPlugin(id); ok {
		name = n
		sub = true
	}

	items := map[string]interface{}{}
	if sub && s.subPluginMgr != nil {
		items = s.subPluginMgr.FlatSchema(name)
	}
	metadata := map[string]interface{}{}
	if len(items) > 0 {
		metadata[name] = map[string]interface{}{
			"description": name + " 配置",
			"type":        "object",
			"items":       items,
		}
	}

	cfg := s.pluginLoadConfig(id)
	if sub && s.subPluginMgr != nil {
		cfg = s.subPluginMgr.LoadConfigWithDefaults(name)
	}

	return map[string]interface{}{
		"plugin_name": id,
		"log_level":   nil,
		"metadata":    metadata,
		"config":      cfg,
		"i18n":        map[string]interface{}{},
	}
}

func (s *Server) pluginLoadConfig(id string) map[string]interface{} {
	if _, name, ok := s.resolveSubprocessPlugin(id); ok {
		return s.subPluginMgr.LoadConfig(name)
	}
	return map[string]interface{}{}
}

func (s *Server) pluginSaveConfig(id string, cfg map[string]interface{}) {
	if _, name, ok := s.resolveSubprocessPlugin(id); ok {
		if cfg != nil {
			if err := s.subPluginMgr.SaveConfig(name, cfg); err != nil {
				logger.I18nWarn("保存插件 %s 配置失败: %v", id, err)
			}
		}
	}
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
