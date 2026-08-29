// Package dashboard implements the WebUI API server.
// Ported from astrbot/dashboard/server.py
package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/WaterGodFurina/Astrbot-golang/internal/knowledgebase"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/internal/plugin"
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	"github.com/WaterGodFurina/Astrbot-golang/internal/skills"
	"github.com/WaterGodFurina/Astrbot-golang/internal/t2i"
	"github.com/WaterGodFurina/Astrbot-golang/internal/version"
)

//go:embed web/dist/*
var webFS embed.FS

var logger = log.GetDefault().WithComponent("Dashboard")

// Server is the WebUI API server.
type Server struct {
	mux *http.ServeMux
	srv *http.Server
	mu  sync.RWMutex
	// configMu 串行化"快照→修改→整键回写"的配置保存组合（upsertProvider 等），
	// 防止并发保存请求基于同一旧快照互相覆盖导致配置丢失。
	configMu        sync.Mutex
	handlers        map[string]http.HandlerFunc
	auth            *PasswordManager
	port            int
	webuiDir        string
	dataDir         string
	configMgr       interface{} // *config.ConfigManager
	providerMgr     interface{} // *provider.ProviderManager
	platformMgr     interface{} // *platform.PlatformManager
	eventBus        interface{} // *core.EventBus
	chatAdapter     *chatStreamAdapter
	chatBus         *core.EventBus
	conversationMgr interface{} // *conversation.Manager
	cronMgr         interface{} // *cron.CronJobManager
	subPluginMgr    *plugin.SubprocessManager
	kbMgr           interface{}              // *knowledgebase.Manager
	kbTasks         map[string]*kbUploadTask // knowledge base upload task states
	skillMgr        interface{}              // *skills.SkillManager
	personaMgr      interface{}              // *persona.PersonaManager
	personas        *personaStore
	chat            *chatStore
	threads         *threadStore
	projects        *projectStore
	apiKeys         *apiKeyStore
	mcp             *mcpStore
	// backupTaskMu/backupTasks 跟踪后台备份任务（导出/导入）进度。
	backupTaskMu sync.Mutex
	backupTasks  map[string]*backupTaskState
	// uploadMu/uploadSessions 跟踪备份分片上传会话。
	uploadMu       sync.Mutex
	uploadSessions map[string]*uploadSession
	starMgr        interface{} // *star.Manager
	database       *db.Database
	// fileTokens 是宿主文件令牌注册表（plugin.RegisterFileToken 反调用写入，
	// GET /api/file/{token} 公开路由消费），由 lifecycle 经 managers 注入。
	fileTokens         *plugin.FileTokenRegistry
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
	// webhookHandlers maps webhook_uuid -> platform callback for the unified
	// webhook entry (/api/v1/webhooks/platforms/{uuid}).
	webhookMu       sync.RWMutex
	webhookHandlers map[string]func(http.ResponseWriter, *http.Request)
	// updateProgress 跟踪"切换版本"升级进度（keyed by progress_id），供前端轮询。
	updateProgressMu sync.Mutex
	updateProgress   map[string]*updateProgress
	// chatRuns 跟踪每个 chat session 正在进行的 run（sessionID -> runID ->
	// cancel），POST /chat/sessions/{id}/stop 与 WebSocket interrupt 复用，
	// 对齐 Python active_event_registry.request_agent_stop_all。
	chatRunMu sync.Mutex
	chatRuns  map[string]map[string]context.CancelFunc
	// ticketMu/wsTickets：一次性 ws-ticket 表（见 auth.go issueWSTicket /
	// consumeWSTicket），用于 WebSocket 连接与下载 URL 鉴权，避免长效 JWT
	// 进 query。惰性清理过期/已用条目。
	ticketMu  sync.Mutex
	wsTickets map[string]*wsTicket
}

// RegisterWebhook registers a unified-webhook callback by uuid.
func (s *Server) RegisterWebhook(uuid string, handler func(http.ResponseWriter, *http.Request)) {
	if uuid == "" {
		return
	}
	s.webhookMu.Lock()
	defer s.webhookMu.Unlock()
	if s.webhookHandlers == nil {
		s.webhookHandlers = make(map[string]func(http.ResponseWriter, *http.Request))
	}
	s.webhookHandlers[uuid] = handler
}

// ClearWebhooks removes all unified-webhook callbacks (used when reloading
// platform adapters so stale uuids do not linger).
func (s *Server) ClearWebhooks() {
	s.webhookMu.Lock()
	defer s.webhookMu.Unlock()
	s.webhookHandlers = make(map[string]func(http.ResponseWriter, *http.Request))
}

// UnregisterWebhook removes a unified-webhook callback by uuid.
func (s *Server) UnregisterWebhook(uuid string) {
	if uuid == "" {
		return
	}
	s.webhookMu.Lock()
	defer s.webhookMu.Unlock()
	delete(s.webhookHandlers, uuid)
}

// registerChatRun tracks a run's cancel func for a chat session.
func (s *Server) registerChatRun(sessionID, runID string, cancel context.CancelFunc) {
	s.chatRunMu.Lock()
	defer s.chatRunMu.Unlock()
	if s.chatRuns[sessionID] == nil {
		s.chatRuns[sessionID] = make(map[string]context.CancelFunc)
	}
	s.chatRuns[sessionID][runID] = cancel
}

// unregisterChatRun removes a finished run from the registry.
func (s *Server) unregisterChatRun(sessionID, runID string) {
	s.chatRunMu.Lock()
	defer s.chatRunMu.Unlock()
	if runs, ok := s.chatRuns[sessionID]; ok {
		delete(runs, runID)
		if len(runs) == 0 {
			delete(s.chatRuns, sessionID)
		}
	}
}

// cancelChatRuns cancels every in-flight run of a session (POST stop /
// session delete) and returns the number of runs stopped. Mirrors Python
// active_event_registry.request_agent_stop_all.
func (s *Server) cancelChatRuns(sessionID string) int {
	s.chatRunMu.Lock()
	runs := s.chatRuns[sessionID]
	stopped := len(runs)
	for _, cancel := range runs {
		cancel()
	}
	delete(s.chatRuns, sessionID)
	s.chatRunMu.Unlock()
	return stopped
}

// handleWebhooks dispatches GET/POST /api/v1/webhooks/platforms/{webhook_uuid}.
func (s *Server) handleWebhooks(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) < 2 || parts[0] != "platforms" || parts[1] == "" {
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"platforms": s.webhookUUIDs(),
		}))
		return
	}
	uuid := parts[1]
	s.webhookMu.RLock()
	handler := s.webhookHandlers[uuid]
	s.webhookMu.RUnlock()
	if handler == nil {
		logger.Warn("未找到 webhook_uuid 为 %s 的平台", uuid)
		writeJSON(w, http.StatusNotFound, apiError("平台未找到"))
		return
	}
	handler(w, r)
}

// webhookUUIDs returns the list of registered webhook uuids.
func (s *Server) webhookUUIDs() []string {
	s.webhookMu.RLock()
	defer s.webhookMu.RUnlock()
	uuids := make([]string, 0, len(s.webhookHandlers))
	for uuid := range s.webhookHandlers {
		uuids = append(uuids, uuid)
	}
	return uuids
}

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

// updateProgress 是切换版本进度的富结构（对齐 Python UpdateService
// update_progress：分 dashboard/core 两个下载阶段，各带字节/速度/百分比，
// 外层 overall_percent 汇总），供 WebUI 更新对话框渲染逐阶段进度条。
type updateProgress struct {
	ID             string                        `json:"id"`
	Status         string                        `json:"status"` // running | success | error
	Stage          string                        `json:"stage"`  // preparing | core | dependencies | restart | done
	Version        string                        `json:"version"`
	Message        string                        `json:"message"`
	OverallPercent int                           `json:"overall_percent"`
	Stages         map[string]*downloadStageInfo `json:"stages"`
}

// downloadStageInfo 单个阶段的下载进度。
type downloadStageInfo struct {
	Status     string `json:"status"` // pending | downloading | done
	Downloaded int64  `json:"downloaded"`
	Total      int64  `json:"total"`
	Percent    int    `json:"percent"`
	Speed      int64  `json:"speed"` // KiB/s
}

// newUpdateProgress 构造 Python 形状的初始进度（两阶段均 pending）。
func newUpdateProgress(id, version string) *updateProgress {
	return &updateProgress{
		ID:      id,
		Status:  "running",
		Stage:   "preparing",
		Version: version,
		Message: "正在准备更新...",
		Stages: map[string]*downloadStageInfo{
			"dashboard": {Status: "pending"},
			"core":      {Status: "pending"},
		},
	}
}

// 状态 map（kbTasks / installProgress）的终态条目在 TTL 后自动清除，防止只增不删。
const (
	kbTaskCleanupTTL = 10 * time.Minute
	installStatusTTL = 10 * time.Minute
)

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
	// 注入 t2i 远程渲染的模板来源（<data>/t2i_templates + 内置 base 模板），
	// 供 t2i.RenderRemote 对齐 Python 原版按模板名取内容后走 /text2img/generate。
	t2i.T2ITemplateDir = filepath.Join(filepath.Dir(configPath), "t2i_templates")
	t2i.T2IDefaultTemplate = t2iDefaultTemplate
	t2i.T2IVersion = version.Version
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
	s.threads = newThreadStore(filepath.Dir(configPath))
	s.projects = newProjectStore(filepath.Dir(configPath))
	s.apiKeys = newAPIKeyStore(filepath.Dir(configPath))
	s.chatAdapter = newChatStreamAdapter()
	s.mcp = newMCPStore(filepath.Dir(configPath))
	s.updateProgress = make(map[string]*updateProgress)
	s.installProgress = make(map[string]*installStatus)
	s.marketCache = make(map[string]*marketCacheEntry)
	s.kbTasks = make(map[string]*kbUploadTask)
	s.backupTasks = make(map[string]*backupTaskState)
	s.uploadSessions = make(map[string]*uploadSession)
	s.chatRuns = make(map[string]map[string]context.CancelFunc)
	s.wsTickets = make(map[string]*wsTicket)
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
			s.auth.SetConfigManager(v)
			// NewPasswordManager 在 CM 加载之后运行，可能因空值规则重置过
			// 凭据（直写磁盘）：把最新 auth 状态回填进 CM 快照并落盘一次，
			// 消除"auth 直写磁盘 vs CM 旧快照（username/change_required 为
			// 重置前的值）"的不一致窗口——否则首次配置保存就会把重置状态
			// 冲掉。
			s.syncAuthToConfig()
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
			// 接线 URL 上传后端：复用 URL 导入 handler 的分块→嵌入→双写路径。
			// 类型断言失败时跳过，绝不阻断启动。
			if km, ok := v.(*knowledgebase.Manager); ok {
				km.SetUploadFunc(func(h *knowledgebase.KBHelper, url string, content []byte, chunkSize, chunkOverlap int) error {
					if h == nil || h.KB == nil {
						return fmt.Errorf("knowledge base helper is nil")
					}
					kbID := h.KB.KBID
					if kbID == "" {
						return fmt.Errorf("knowledge base id is empty")
					}
					name := filepath.Base(strings.TrimRight(url, "/"))
					if name == "" || name == "." || name == "/" {
						name = "url_import"
					}
					docID := fmt.Sprintf("doc_url_%d_%s", time.Now().UnixNano(), name)
					_, err := s.indexKBFile(kbID, docID, name, content, chunkSize, chunkOverlap)
					return err
				})
			}
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
		if v, ok := managers["file_tokens"]; ok {
			if reg, ok := v.(*plugin.FileTokenRegistry); ok {
				s.fileTokens = reg
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
		"version": version.Version,
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
	case "ws-ticket":
		s.handleWSTicket(w, r)
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
		writeJSON(w, http.StatusInternalServerError, apiError("认证服务未初始化"))
		return
	}
	if creds.Username == s.auth.Username() && s.auth.VerifyPassword(creds.Password) {
		// 旧版无盐 MD5 哈希登录成功后立即升级为 PBKDF2 重新落盘（不注销会话）。
		upgradeRequired := false
		if isMD5Hash(s.auth.HashedPassword()) {
			s.auth.SetPasswordKeepUsername(creds.Password)
			upgradeRequired = true
		}
		// TOTP 双因素：启用后登录必须携带验证码（或恢复码）。
		// 使用恢复码登录会一次性禁用双因素（对齐 Python 语义）。
		if s.auth.TOTPEnabled() {
			if creds.Code == "" {
				// 密码正确但未提供验证码：告知前端进入 TOTP 输入阶段
				// （对齐 Python：data={"totp_required": true}，401）。
				writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
					"status":  "error",
					"message": "需要 TOTP 验证",
					"data":    map[string]interface{}{"totp_required": true},
				})
				return
			}
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
		// 除 JSON 返回外，同步种下 HttpOnly Cookie（Path=/、SameSite=Lax，
		// HTTPS 时 Secure），供前端迁移后无需再在 sessionStorage 保存 token；
		// 现有前端经 Authorization 头鉴权的路径继续可用（Cookie 是新增途径）。
		s.setSessionCookie(w, token)
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"token":                     token,
			"username":                  s.auth.Username(),
			"password_upgrade_required": upgradeRequired,
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
	// 无论 token 来自 Cookie 还是 Authorization 头，登出都清除 Cookie。
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, apiOK(nil))
}

// sessionCookieName 是登录成功后种下的会话 Cookie 名（复核开放项 10-1）：
// HttpOnly 防 JS 读取（前端 token 从 sessionStorage 迁向 Cookie 的长期方案），
// 与 Authorization Bearer / ?token= 并存，互为回退。
const sessionCookieName = "astrbot_token"

// setSessionCookie 写入会话 Cookie：Path=/、HttpOnly、SameSite=Lax；仅在
// dashboard.ssl 实际启用 HTTPS（enable + cert_file + key_file 齐备，与
// Start 的 ServeTLS 分支条件一致）时附加 Secure。HTTP 部署下不加 Secure，
// 避免本地 HTTP 调试时浏览器静默丢弃 Cookie。
func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	c := &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(tokenTTL),
	}
	if enable, cert, key := s.sslConfig(); enable && cert != "" && key != "" {
		c.Secure = true
	}
	http.SetCookie(w, c)
}

// clearSessionCookie 清除会话 Cookie（登出）。
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
	})
}

// handleWSTicket 处理 POST /api/v1/auth/ws-ticket：已认证会话换取一次性
// 短期票据（30s、单次使用），供 WebSocket 连接 / 下载 URL 携带，避免长效
// JWT 进 query（会进代理/服务端日志）。路由已由 apiAuthAllowed 保证需已
// 认证会话（JWT/Cookie；API key 拒绝——auth 子端点 systemOnly 语义）。
func (s *Server) handleWSTicket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	ticket := s.issueWSTicket()
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"ticket":     ticket,
		"expires_in": int(wsTicketTTL.Seconds()),
	}))
}

// handleCheck handles GET /api/auth/check.
func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	token := extractToken(r)
	loggedIn := false
	username := ""
	if s.auth != nil {
		loggedIn = s.auth.IsAuthenticated(token)
		if loggedIn {
			// 未认证请求不返回管理员用户名，避免泄露账户名
			username = s.auth.Username()
		}
	}
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"loggedin": loggedIn,
		"username": username,
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
		// 首次安装/重置窗口内 setup 必须提供启动时打印在控制台的初始密码
		// （防窗口期内未授权者抢注管理员账户），前端据此显示初始密码输入框。
		"require_initial_password": setupRequired && s.auth.InitialPassword() != "",
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
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, apiError("无效的 JSON: "+err.Error()))
			return
		}
		if body.Code == "" {
			if s.auth.TOTPEnabled() {
				// 已启用时拒绝重新生成，避免 GenerateTOTP 覆盖现有密钥
				// 导致用户验证器失配（M-07）；需先禁用再重新设置。
				writeJSON(w, http.StatusOK, apiError("TOTP 已启用，如需重新设置请先禁用"))
				return
			}
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
		// 恢复码以哈希存储无法回显，重新生成一批并返回明文（保留原密钥，
		// 旧恢复码作废，避免 GenerateTOTP 覆盖 secret 导致验证器失配）
		codes, err := s.auth.RegenerateRecoveryCodes()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, apiError(err.Error()))
			return
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
	if s.auth.PasswordChangeRequired() {
		// 首次安装/重置密码窗口内 setup 端点公开，但仍必须提供启动时生成并
		// 打印在控制台的初始密码，防止窗口期内任何人抢注管理员账户。
		if !s.auth.VerifyPassword(body.OldPassword) {
			writeJSON(w, http.StatusUnauthorized, apiError("请提供初始密码（见启动控制台）"))
			return
		}
	} else if !s.auth.VerifyPassword(body.OldPassword) {
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
	// 首次安装/重置流程完成即建立会话：与登录一致，同步种下 HttpOnly Cookie。
	s.setSessionCookie(w, token)
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("无效的 JSON: "+err.Error()))
		return
	}
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
			"version":           version.Version,
			"dashboard_version": version.Version,
			"python_version":    version.PythonVersion,
			"go_version":        runtime.Version(),
		}))
	case "versions":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"versions": []interface{}{},
		}))
	case "start-time":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"start_time": s.startTime.Unix(),
		}))
	case "restart-core":
		// 对齐 Python stat_service.restart_core：前端"重启"按钮经
		// /api/stat/restart-core 触发核心自重启。异步执行，先返回响应，
		// 前端 WaitingForRestart 轮询 start-time 检测重启完成并刷新。
		if s.restartFunc != nil {
			go s.restartFunc()
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "重启中...",
			}))
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "重启功能不可用",
			}))
		}
	case "first-notice":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"notice": "",
		}))
	case "test-ghproxy-connection", "ghproxy":
		// 前端 openapi 路径为 /api/v1/stats/ghproxy/test（parts=[ghproxy,test]）；
		// 旧路径 /api/v1/stat/test-ghproxy-connection 也兼容。
		if parts[0] == "ghproxy" && (len(parts) < 2 || parts[1] != "test") {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
			return
		}
		s.handleGhproxyTest(w, r)
	case "storage":
		if len(parts) > 1 && parts[1] == "cleanup" {
			if r.Method == http.MethodPost {
				s.cleanupStorage(w, r)
				return
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
			return
		}
		writeJSON(w, http.StatusOK, apiOK(s.getStorageStatus()))
	case "storage-cleanup", "cleanup":
		s.cleanupStorage(w, r)
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

// storageDirStat 统计一个目录下的文件大小/数量（对齐 Python
// StorageCleaner._summarize_files）。
func storageDirStat(dir string) (sizeBytes int64, fileCount int) {
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		fileCount++
		sizeBytes += info.Size()
		return nil
	})
	return sizeBytes, fileCount
}

// storageDirEntries 收集目录下全部文件路径（对齐 Python _iter_files）。
func storageDirEntries(dir string) []string {
	var out []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		out = append(out, path)
		return nil
	})
	return out
}

// getStorageStatus 返回 logs/temp 两个清理目标的占用统计
// （对齐 Python StorageCleaner.get_status，frontend StorageCleanupPanel 消费）。
func (s *Server) getStorageStatus() map[string]interface{} {
	logsDir := filepath.Join(s.kbDataDir(), "logs")
	tempDir := filepath.Join(s.kbDataDir(), "temp")
	logsBytes, logsCount := storageDirStat(logsDir)
	cacheBytes, cacheCount := storageDirStat(tempDir)
	_, logsExists := os.Stat(logsDir)
	_, tempExists := os.Stat(tempDir)
	return map[string]interface{}{
		"logs": map[string]interface{}{
			"size_bytes": logsBytes,
			"file_count": logsCount,
			"path":       logsDir,
			"exists":     logsExists == nil,
		},
		"cache": map[string]interface{}{
			"size_bytes": cacheBytes,
			"file_count": cacheCount,
			"path":       tempDir,
			"exists":     tempExists == nil,
		},
		"total_bytes": logsBytes + cacheBytes,
	}
}

// cleanupStorage implements POST /stats/storage/cleanup：按 target
// （logs/cache/all）清理 data/logs 与 data/temp（对齐 Python
// StorageCleaner.cleanup）。启用的日志文件（log_file_enable/trace_log_enable）
// 只截断不删除。
func (s *Server) cleanupStorage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Target string `json:"target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("无效的 JSON: "+err.Error()))
		return
	}
	target := strings.ToLower(strings.TrimSpace(body.Target))
	if target == "" {
		target = "all"
	}
	if target != "logs" && target != "cache" && target != "all" {
		writeJSON(w, http.StatusBadRequest, apiError("Unsupported cleanup target: "+body.Target))
		return
	}
	cfg := s.getConfigData("default")
	logFileEnabled, _ := cfg["log_file_enable"].(bool)
	traceLogEnabled, _ := cfg["trace_log_enable"].(bool)
	resolveLogPath := func(key, def string) string {
		if v, ok := cfg[key].(string); ok && v != "" {
			if filepath.IsAbs(v) {
				return v
			}
			return filepath.Join(s.kbDataDir(), filepath.FromSlash(v))
		}
		return filepath.Join(s.kbDataDir(), filepath.FromSlash(def))
	}
	activeLogs := map[string]bool{}
	if logFileEnabled {
		activeLogs[resolveLogPath("log_file_path", "logs/astrbot.log")] = true
	}
	if traceLogEnabled {
		activeLogs[resolveLogPath("trace_log_path", "logs/astrbot.trace.log")] = true
	}

	cleanTarget := func(name string) map[string]interface{} {
		var files []string
		if name == "logs" {
			files = storageDirEntries(filepath.Join(s.kbDataDir(), "logs"))
		} else {
			files = storageDirEntries(filepath.Join(s.kbDataDir(), "temp"))
		}
		removedBytes, deletedFiles, truncatedFiles, failedFiles := int64(0), 0, 0, 0
		for _, f := range files {
			info, err := os.Lstat(f)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			size := info.Size()
			if activeLogs[f] {
				if err := os.WriteFile(f, nil, 0o644); err == nil {
					truncatedFiles++
					removedBytes += size
				} else {
					failedFiles++
				}
				continue
			}
			if err := os.Remove(f); err == nil {
				deletedFiles++
				removedBytes += size
			} else {
				failedFiles++
			}
		}
		return map[string]interface{}{
			"removed_bytes":   removedBytes,
			"processed_files": deletedFiles + truncatedFiles,
			"deleted_files":   deletedFiles,
			"truncated_files": truncatedFiles,
			"failed_files":    failedFiles,
		}
	}

	results := map[string]interface{}{}
	aggregates := map[string]interface{}{
		"removed_bytes":   int64(0),
		"processed_files": 0,
		"deleted_files":   0,
		"truncated_files": 0,
		"failed_files":    0,
	}
	names := []string{target}
	if target == "all" {
		names = []string{"logs", "cache"}
	}
	for _, name := range names {
		res := cleanTarget(name)
		results[name] = res
		for _, k := range []string{"removed_bytes", "processed_files", "deleted_files", "truncated_files", "failed_files"} {
			aggregates[k] = sumCleanupMetrics(aggregates[k], res[k])
		}
	}
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"target":          target,
		"results":         results,
		"status":          s.getStorageStatus(),
		"removed_bytes":   aggregates["removed_bytes"],
		"processed_files": aggregates["processed_files"],
		"deleted_files":   aggregates["deleted_files"],
		"truncated_files": aggregates["truncated_files"],
		"failed_files":    aggregates["failed_files"],
	}))
}

// sumCleanupMetrics 累加清理结果指标（int64/int 混合）。
func sumCleanupMetrics(a, b interface{}) interface{} {
	switch av := a.(type) {
	case int64:
		switch bv := b.(type) {
		case int64:
			return av + bv
		case int:
			return av + int64(bv)
		}
	case int:
		switch bv := b.(type) {
		case int64:
			return int64(av) + bv
		case int:
			return av + bv
		}
	}
	return a
}

// handleGhproxyTest 测 GitHub 加速地址连通性（对齐 Python
func (s *Server) handleGhproxyTest(w http.ResponseWriter, r *http.Request) {
	proxyURL := strings.TrimSpace(r.URL.Query().Get("proxy_url"))
	if proxyURL == "" {
		var body struct {
			ProxyURL string `json:"proxy_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, apiError("无效的 JSON: "+err.Error()))
			return
		}
		proxyURL = strings.TrimSpace(body.ProxyURL)
	}
	if proxyURL == "" {
		writeJSON(w, http.StatusBadRequest, apiError("proxy_url is required"))
		return
	}
	testURL := strings.TrimRight(proxyURL, "/") +
		"/https://github.com/AstrBotDevs/AstrBot/raw/refs/heads/master/.python-version"
	// 校验出站 URL 防 SSRF：仅 http/https，拒绝内网/回环/元数据地址与
	// localhost 主机名（对齐 market.go validateOutboundURL）。
	if err := validateOutboundURL(testURL); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("proxy_url 校验失败: "+err.Error()))
		return
	}
	start := time.Now()
	client := newOutboundClient(10 * time.Second)
	// #nosec tainted-url-host -- 测速端点需 dashboard 登录鉴权（apiAuthAllowed），仅管理员可调用；
	// 目标路径固定为 GitHub raw 文件（对齐 Python stat_service.test_ghproxy_connection），
	// 响应体被丢弃（只上报延迟/状态码），且 testURL 已通过 validateOutboundURL（防 SSRF）。
	resp, err := client.Get(testURL) // nosemgrep: go.lang.security.injection.tainted-url-host.tainted-url-host
	if err != nil {
		logger.I18nWarn("ghproxy 测速失败 %s: %v", proxyURL, err)
		writeJSON(w, http.StatusBadGateway, apiError("ghproxy 测速失败: "+err.Error()))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.I18nWarn("ghproxy 测速失败 %s: HTTP %d", proxyURL, resp.StatusCode)
		writeJSON(w, http.StatusBadGateway, apiError(fmt.Sprintf("ghproxy 测速失败: HTTP %d", resp.StatusCode)))
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	latency := float64(time.Since(start).Microseconds()) / 1000.0
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"ok":      true,
		"latency": math.Round(latency*100) / 100,
	}))
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
	// 总运存 = 主进程 RSS + 全部子进程（gRPC 插件子进程：Go 二进制与
	// Python 解释器）RSS。直接扫描 /proc 的父子关系，不依赖 go-plugin。
	processMB := processMemoryMB()
	pluginMB := childProcessMemoryMB()
	return map[string]interface{}{
		"message_count":       messageCount,
		"platform_count":      len(s.getBotList()),
		"platform":            platformRank,
		"message_time_series": timeSeries,
		"memory": map[string]interface{}{
			"process": processMB,
			"plugins": pluginMB,
			"total":   processMB + pluginMB,
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

// processMemoryMB / systemMemoryMB / childProcessMemoryMB 为平台相关实现，
// 见 proc_mem_linux.go（/proc）、proc_mem_darwin.go（sysctl + ps）、
// proc_mem_windows.go（Toolhelp32Snapshot + NtQueryInformationProcess）与
// proc_mem_other.go（不支持时返回 0）。

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

// extractToken 提取会话 token（复核开放项 10-1）：优先 HttpOnly Cookie
// astrbot_token（新增鉴权途径，前端迁移中），其次 Authorization Bearer，
// 最后回退 ?token= query（兼容旧 WebSocket / 备份下载客户端）。
// Authorization 以 "ApiKey " 开头时按 API key 处理（对齐 Python
// _extract_dashboard_jwt 语义），不视为 JWT / legacy token。
func extractToken(r *http.Request) string {
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	auth := r.Header.Get("Authorization")
	if auth != "" {
		if strings.HasPrefix(auth, "Bearer ") {
			return strings.TrimPrefix(auth, "Bearer ")
		}
		if strings.HasPrefix(auth, "ApiKey ") {
			return ""
		}
		return auth
	}
	if v := strings.TrimSpace(r.URL.Query().Get("token")); v != "" {
		return v
	}
	return ""
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
	// 敏感响应体（登录/重置返回的 token、一次性 ws-ticket、TOTP 密钥/otpauth
	// 链接与恢复码、provider 配置里的 API key）不写入访问日志；writeJSON 无
	// 请求上下文，按响应中的敏感键名就地脱敏。
	if strings.Contains(s, `"token":`) || strings.Contains(s, `"ticket":`) ||
		strings.Contains(s, `"secret":`) ||
		strings.Contains(s, `"recovery_codes":`) || strings.Contains(s, `"otpauth_url":`) ||
		strings.Contains(s, `"key":`) || strings.Contains(s, `"api_key":`) {
		s = `"<redacted>"`
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
			// #nosec no-direct-write-to-responsewriter -- 静态资源服务：内容为 WebUI 自身构建产物（embed/外部目录），
			// 路径经穿越校验，Content-Type 按扩展名设置，非用户输入内容。
			_, _ = w.Write(data) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter
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
	// #nosec no-direct-write-to-responsewriter -- 静态资源服务：内容为 WebUI 自身构建产物（embed/web/dist），
	// Content-Type 按扩展名设置，非用户输入内容。
	_, _ = w.Write(data) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter
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

// mutateConfig 在单一临界区内完成"快照→修改→整键回写"，使读-改-写组合
// （upsertProvider/setProviderEnabled/upsertBot 等）对并发保存请求串行化，
// 避免两个请求都基于同一旧快照、后写者覆盖前写者导致配置丢失。
func (s *Server) mutateConfig(fn func(cfg map[string]interface{}) error) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	cfg := s.getConfigSnapshot()
	if err := fn(cfg); err != nil {
		return err
	}
	return s.setConfigDataAll(cfg)
}

// syncAuthToConfig 把 PasswordManager 的当前凭据状态回填进 ConfigManager
// 快照并落盘（启动时 SetConfigManager 之后调用一次）。
func (s *Server) syncAuthToConfig() {
	if s.configMgr == nil || s.auth == nil {
		return
	}
	cm, ok := s.configMgr.(*config.ConfigManager)
	if !ok {
		return
	}
	cfg := cm.Get("default")
	if cfg == nil {
		return
	}
	dash, _ := cfg.Get("dashboard").(map[string]interface{})
	if dash == nil {
		dash = map[string]interface{}{}
	}
	s.injectAuthFields(dash)
	if err := cfg.Update(map[string]interface{}{"dashboard": dash}); err != nil {
		return
	}
	_ = cfg.Save()
}

// injectAuthFields re-asserts the dashboard auth fields from the password
// manager so config saves never wipe them out.
func (s *Server) injectAuthFields(dash map[string]interface{}) {
	if s.auth == nil {
		return
	}
	// 等待状态（初始密码已生成、用户尚未完成 setup）下不回填 username：
	// 重置只落密码哈希，username 键保持缺失，下次启动按"无键=等待状态"
	// 语义处理而不是误判为已设置账号。
	if !s.auth.PasswordChangeRequired() {
		dash["username"] = s.auth.Username()
	} else {
		delete(dash, "username")
	}
	if h := s.auth.HashedPassword(); h != "" {
		dash["password"] = h
	}
	// change_required 必须随保存回填：否则用户手改 password 为空触发重置后，
	// ConfigManager 用旧快照（false）覆盖掉重置流程写入的 true，等待状态丢失。
	dash["password_change_required"] = s.auth.PasswordChangeRequired()
	// 明文密码不再持久化：password 字段只存哈希，配置保存（会剔除 password
	// 键）不会丢失凭据。
	if sec := s.auth.JWTSecret(); sec != "" {
		dash["jwt_secret"] = sec
	}
	// 保留 TOTP 段：EnableTOTP/DisableTOTP 直写配置文件，ConfigManager 内存
	// 快照看不到 dashboard.totp；保存 dashboard 是整体替换，必须从
	// PasswordManager 回填，否则任意一次配置保存都会清掉双因素认证。
	dash["totp"] = s.auth.TOTPConfig()
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

// cronUpdateJob patches an existing cron job (enabled toggle and/or schedule
// fields), keeping it in the list even when disabled. All field mutations run
// inside UpdateJob so they are serialized with the cron tick goroutine.
func (s *Server) cronUpdateJob(jobID string, body map[string]interface{}) (map[string]interface{}, string) {
	cm, ok := s.cronMgr.(interface {
		Get(id string) *cron.Job
		SetEnabled(id string, enabled bool) bool
		UpdateJob(id string, mutate func(*cron.Job)) bool
	})
	if !ok || cm == nil {
		return nil, "cron manager not available"
	}
	if v, ok := body["enabled"].(bool); ok {
		cm.SetEnabled(jobID, v)
	}
	cm.UpdateJob(jobID, func(job *cron.Job) {
		if v, ok := body["name"].(string); ok && v != "" {
			job.Name = v
		}
		if v, ok := body["note"].(string); ok && v != "" {
			job.Payload["note"] = v
			job.Description = v
		} else if v, ok := body["description"].(string); ok && v != "" {
			job.Description = v
		}
		if v, ok := body["cron_expression"].(string); ok {
			job.CronExpression = v
			job.RunOnce = false
		}
		if v, ok := body["run_at"].(string); ok && strings.TrimSpace(v) != "" {
			if t, err := cron.ParseRunAt(v); err == nil {
				job.RunAt = t
				job.RunOnce = true
				job.Payload["run_at"] = v
			}
		}
		if v, ok := body["timezone"].(string); ok {
			job.Timezone = v
		}
		if v, ok := body["session"].(string); ok && v != "" {
			job.Payload["session"] = v
		}
	})
	job := cm.Get(jobID)
	if job == nil {
		return nil, "job not found"
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
	s.installProgress[id] = st
	s.installProgressMu.Unlock()
	if st != nil && (st.Status == "done" || st.Status == "error") {
		// 终态在 TTL 后自动清除（仅当仍是本条记录时），避免 map 只增不删。
		time.AfterFunc(installStatusTTL, func() {
			s.installProgressMu.Lock()
			if s.installProgress[id] == st {
				delete(s.installProgress, id)
			}
			s.installProgressMu.Unlock()
		})
	}
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
				if pid, _, ok2 := s.resolveSubprocessPlugin(id); ok2 && s.subPluginMgr != nil {
					if comps := s.subPluginMgr.Components(pid); comps != nil {
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

// levelOrGlobal renders a log level for human-readable logging: the level
// name, or "global" when following the host's global level.
func levelOrGlobal(level string) string {
	if level == "" {
		return "global"
	}
	return level
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
	if pid, _, ok := s.resolveSubprocessPlugin(id); ok {
		return s.subPluginMgr.ConfigSchema(pid)
	}
	return map[string]interface{}{}
}

// pluginConfigPayload returns the plugin config dialog payload consumed by the
// WebUI's AstrBotConfig component: {plugin_name, log_level, metadata, config,
// i18n}. metadata is keyed by the plugin name and carries the flat schema
// ("items"), matching the Python reference (config_service.get_plugin_config).
func (s *Server) pluginConfigPayload(id string) map[string]interface{} {
	name := id
	pid := ""
	sub := false
	if p, n, ok := s.resolveSubprocessPlugin(id); ok {
		pid, name = p, n
		sub = true
	}

	items := map[string]interface{}{}
	if sub && s.subPluginMgr != nil {
		// 优先按实例 id 取 schema：同名插件（如 Python 版与旧 Go 版同名）
		// 不会被互相遮蔽，配置对话框显示用户所点那个插件的完整
		// description/hint。回退到按 name（未加载/禁用时用磁盘缓存）。
		items = s.subPluginMgr.FlatSchemaByID(pid)
		if len(items) == 0 {
			items = s.subPluginMgr.FlatSchema(pid)
		}
	}
	// 无配置项时 metadata 必须为 null（而非 {}）——前端以
	// `v-if="extension_config.metadata"` 判断是否有配置，空对象是 JS
	// 真值，会渲染出空白配置面板；null 则走 noConfig 提示（对齐 Python
	// get_plugin_config 返回 metadata=None）。
	var metadata map[string]interface{}
	if len(items) > 0 {
		// metadata 键用请求 id（= 前端 metadataKey/curr_namespace），
		// 前端传 name 或 id 都能对应上。
		metadata = map[string]interface{}{
			id: map[string]interface{}{
				"description": name + " 配置",
				"type":        "object",
				"items":       items,
			},
		}
	}

	cfg := s.pluginLoadConfig(id)
	if sub && s.subPluginMgr != nil {
		cfg = s.subPluginMgr.ConfigResolver().ResolvePluginConfig(pid)
	}

	// per-plugin 日志级别覆盖（无覆盖 = null = 跟随全局），对齐 Python
	// get_plugin_config 返回 LogManager.get_plugin_log_level(plugin_id)。
	var logLevel interface{}
	if sub && s.subPluginMgr != nil {
		if lvl := s.subPluginMgr.GetPluginLogLevel(pid); lvl != "" {
			logLevel = lvl
		}
	}

	return map[string]interface{}{
		"plugin_name": id,
		"log_level":   logLevel,
		"metadata":    metadata,
		"config":      cfg,
		"i18n":        map[string]interface{}{},
	}
}

func (s *Server) pluginLoadConfig(id string) map[string]interface{} {
	if pid, _, ok := s.resolveSubprocessPlugin(id); ok {
		return s.subPluginMgr.LoadConfig(pid)
	}
	return map[string]interface{}{}
}

func (s *Server) pluginSaveConfig(id string, cfg map[string]interface{}) {
	if pid, _, ok := s.resolveSubprocessPlugin(id); ok {
		if cfg != nil {
			if err := s.subPluginMgr.SaveConfig(pid, cfg); err != nil {
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
