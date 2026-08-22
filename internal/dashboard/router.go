// Package dashboard - API route dispatcher.
// Ported from astrbot/dashboard/api/ (all route modules)
package dashboard

import (
	"net/http"
	"strings"
)

// apiHandler dispatches API requests.
// Supports both /api/xxx and /api/v1/xxx prefixes.
func (s *Server) apiHandler(w http.ResponseWriter, r *http.Request) {
	// JSON 请求体统一大小上限（16 MiB），防超大请求体耗尽内存。
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	}
	// Global auth gate: every endpoint except the public whitelist below
	// requires a valid session token (JWT or legacy in-memory token). The
	// WebUI attaches "Authorization: Bearer <token>" to all requests via its
	// axios request interceptor; extractToken 也接受 HttpOnly Cookie
	// astrbot_token（前端迁移中，见 server.go setSessionCookie）。
	if !s.apiAuthAllowed(r) {
		writeJSON(w, http.StatusUnauthorized, apiError("未认证"))
		return
	}
	path := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/"), "v1/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		return
	}

	category := parts[0]
	rest := parts[1:]

	switch category {

	// ── Auth ──────────────────────────────────────────────
	case "auth":
		s.handleAuth(w, r, rest)

	// ── System config ─────────────────────────────────────
	case "system-config":
		s.handleSystemConfig(w, r, rest)
	case "config":
		s.handleConfig(w, r, rest)
	case "config-profiles":
		s.handleConfigProfiles(w, r, rest)
	case "config-routes":
		s.handleConfigRoutes(w, r, rest)

	// ── Stats / Version ──────────────────────────────────
	case "stat", "stats":
		s.handleStat(w, r, rest)

	// ── Providers ─────────────────────────────────────────
	case "provider", "providers":
		s.handleProviders(w, r, rest)
	case "provider-sources":
		s.handleProviderSources(w, r, rest)

	// ── Platforms / Bots ──────────────────────────────────
	case "platform", "bots":
		s.handleBots(w, r, rest)
	case "bot-types":
		s.handleBotTypes(w, r, rest)

	// ── Plugins ──────────────────────────────────────────
	case "plugin", "plugins":
		s.handlePlugins(w, r, rest)
	case "plugin-sources":
		s.handlePluginSources(w, r, rest)
	case "plug":
		// Python-AstrBot-compatible plugin Web API proxy:
		// /api/plug/<plugin_path> → plugin process (register_web_api).
		s.handlePluginWebProxy(w, r, strings.Join(rest, "/"))

	// ── Knowledge base ───────────────────────────────────
	case "knowledge_base", "knowledge-bases":
		s.handleKB(w, r, rest)

	// ── Sessions / Conversations / Session Groups ────────
	case "sessions":
		s.handleSessions(w, r, rest)
	case "session-groups":
		// Needs the full parts (parts[1] is the group id) rather than rest,
		// which apiHandler strips the leading category from.
		s.handleSessionGroups(w, r, parts)
	case "conversations":
		s.handleConversations(w, r, rest)

	// ── Personas ─────────────────────────────────────────
	case "personas":
		s.handlePersonas(w, r, rest)
	case "persona-folders":
		s.handlePersonaFolders(w, r)

	// ── Tools / Skills ───────────────────────────────────
	case "tools":
		s.handleTools(w, r, rest)
	case "skills":
		s.handleSkills(w, r, rest)

	// ── MCP ──────────────────────────────────────────────
	case "mcp":
		s.handleMCP(w, r, rest)

	// ── Logs ──────────────────────────────────────────────
	case "logs":
		s.handleLogs(w, r, rest)

	// ── Backups ──────────────────────────────────────────
	case "backups":
		s.handleBackups(w, r, rest)

	// ── Cron ──────────────────────────────────────────────
	case "cron":
		s.handleCron(w, r, rest)

	// ── Extensions / Changelogs ──────────────────────────
	case "extensions":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	case "changelogs":
		s.handleChangelogs(w, r, rest)

	// ── Chat ──────────────────────────────────────────────
	case "chat":
		s.handleChat(w, r, rest)
	case "unified-chat":
		s.handleUnifiedChatWS(w, r)
	case "live-chat":
		s.handleUnifiedChatWS(w, r)

	// ── Updates ──────────────────────────────────────────
	case "update", "updates":
		s.handleUpdate(w, r, rest)

	// ── API keys / Subagents ─────────────────────────────
	case "api-keys":
		s.handleAPIKeys(w, r, rest)
	case "subagents":
		s.handleSubagents(w, r, rest)

	// ── Files ─────────────────────────────────────────────
	case "files":
		s.handleFiles(w, r, rest)

	// ── T2I (text-to-image) ──────────────────────────────
	case "t2i":
		s.handleT2I(w, r, rest)

	// ── Commands ──────────────────────────────────────────
	case "commands":
		s.handleCommands(w, r, rest)

	// ── Webhooks ──────────────────────────────────────────
	case "webhooks":
		s.handleWebhooks(w, r, rest)

	// ── Trace ─────────────────────────────────────────────
	case "trace":
		s.handleTrace(w, r, rest)

	// ── System ────────────────────────────────────────────
	case "system":
		s.handleSystem(w, r, rest)

	// ── Tokens (file tokens etc) ─────────────────────────
	case "t":
		// Short token redirect endpoint
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))

	// ── Go package install (Go 版的 pip install) ─────────
	case "pip":
		s.installGoPackage(w, r)

	default:
		writeJSON(w, http.StatusNotFound, apiError("unknown endpoint: /api/"+category))
	}
}

// apiAuthAllowed reports whether the request may pass without a session token.
// Public endpoints: auth login/check/setup-status, first-install auth/setup
// (password change still required), and the WebSocket chat transports, which
// authenticate themselves via their own ?token= query check (browsers cannot
// set Authorization headers on WebSocket upgrades); ws-ticket 换取端点
// auth/ws-ticket 需已认证会话（JWT/Cookie，API key 拒绝）。
//
// 其余端点双层鉴权（对齐 Python require_scope / require_system_scope）：
//   - 携带 API key（ApiKey 头 / X-API-Key / ?api_key= / ?key=）→ 按 key 的
//     scope 校验（见 endpointScopeRules 映射表），systemOnly 端点一律拒绝；
//   - 否则按 JWT（Bearer 头）鉴权，JWT 恒全权。
func (s *Server) apiAuthAllowed(r *http.Request) bool {
	// fail-closed：认证服务缺失时拒绝一切 /api 请求（修复 fail-open）。
	if s.auth == nil {
		return false
	}
	p := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/"), "v1/")
	parts := strings.Split(p, "/")
	if len(parts) == 0 || parts[0] == "" {
		return false
	}
	switch parts[0] {
	case "auth":
		if len(parts) < 2 {
			return false
		}
		switch parts[1] {
		case "login", "check", "setup-status", "logout":
			return true
		case "setup":
			// The first-install onboarding flow may set credentials without a
			// session token; after onboarding, an authenticated session is
			// required and handleSetup still verifies the old password.
			// setup 属敏感凭据操作（Python optional_system_auth 语义）：
			// 引导窗口之外仅接受 JWT，API key 一律拒绝。
			if s.auth.PasswordChangeRequired() {
				return true
			}
			return extractAPIKey(r) == "" && s.auth.IsAuthenticated(extractToken(r))
		}
		// totp / account 等子端点：system 级，仅 JWT。
		return s.auth.IsAuthenticated(extractToken(r))
	case "unified-chat", "live-chat":
		// WebSocket transport validates its own ?token= (一次性 ws-ticket
		// 或遗留 JWT，见 chat_stream.go handleUnifiedChatWS）。
		return true
	case "plugins":
		// 插件 logo（GET /api/v1/plugins/logo）供 WebUI <img src> 直接加载：
		// 浏览器 img 无法携带 Authorization header，而 logo 只是公开无害的
		// 图片（与 Python 侧 file-token 公开提供 logo 语义一致），放行。
		if len(parts) >= 2 && parts[1] == "logo" {
			return true
		}
		if len(parts) >= 4 && parts[1] == "page-content" {
			// 插件页面资产：iframe 与其子资源（css/js/img）无法携带
			// Authorization 头，凭入口端点签发的短期 asset_token（query）
			// 或专用 Cookie 放行（对齐 Python 的 asset_token 机制）。
			return s.pluginPageAssetAllowed(r, parts[2:])
		}
	case "webhooks":
		// 统一 webhook 回调入口 /api/v1/webhooks/platforms/{uuid}：外部平台
		// 服务器回调不携带 dashboard token。uuid 本身不可猜测且各平台回调
		// 内部另有签名校验，因此放行带具体 uuid 的路径；仅 webhooks 段的
		// uuid 列表枚举（GET /api/v1/webhooks）仍要求认证。
		if len(parts) >= 3 && parts[1] == "platforms" && parts[2] != "" {
			return true
		}
	}

	// API key 优先于 JWT（Python require_scope 语义：携带 API key 时按 key
	// 校验，不回落 JWT；Authorization 以 "Bearer " 开头才算 JWT）。
	if raw := extractAPIKey(r); raw != "" {
		return s.apiKeyAuthorized(raw, s.endpointScopeFor(parts, r.Method), r.Method)
	}
	return s.auth.IsAuthenticated(extractToken(r))
}

// endpointScope 描述一个 API 端点对 API key 的 scope 要求。systemOnly 端点
// 仅允许 JWT 全权访问（对齐 Python require_system_scope：system 不在
// ALL_OPEN_API_SCOPES，API key 带 system 必 403）；readScope / writeScope
// 为空表示默认规则（满足任一默认 OPEN API scope 即可）。
type endpointScope struct {
	readScope  string
	writeScope string
	systemOnly bool
}

// endpointScopeRules 端点 → scope 映射表（对齐 Python 各 router 的
// ScopeDependency）。键为去掉 /api[/v1] 前缀后的路径段 parts[0] 或
// parts[0]/parts[1]；未列出的端点按默认规则。
var endpointScopeRules = map[string]endpointScope{
	// ── system 级管理端点：仅 JWT（Python require_system_scope）──
	"api-keys":              {systemOnly: true},
	"auth/account":          {systemOnly: true},
	"auth/totp":             {systemOnly: true},
	"system":                {systemOnly: true},
	"pip":                   {systemOnly: true},
	"update":                {systemOnly: true},
	"updates":               {systemOnly: true},
	"cron":                  {systemOnly: true},
	"logs":                  {systemOnly: true},
	"stat/restart-core":     {systemOnly: true},
	"stats/restart-core":    {systemOnly: true},
	"stat/storage-cleanup":  {systemOnly: true},
	"stats/storage-cleanup": {systemOnly: true},
	"stat/cleanup":          {systemOnly: true},
	"stats/cleanup":         {systemOnly: true},

	// ── provider / bot / config ─────────────────────────────
	"provider":         {readScope: "provider", writeScope: "provider"},
	"providers":        {readScope: "provider", writeScope: "provider"},
	"provider-sources": {readScope: "provider", writeScope: "provider"},
	"platform":         {readScope: "bot", writeScope: "bot"},
	"bots":             {readScope: "bot", writeScope: "bot"},
	"bot-types":        {readScope: "bot", writeScope: "bot"},
	// config 类：写操作需 config:edit_admin（Python 敏感 scope 语义）。
	"system-config":   {readScope: "config", writeScope: "config:edit_admin"},
	"config":          {readScope: "config", writeScope: "config:edit_admin"},
	"config-profiles": {readScope: "config", writeScope: "config:edit_admin"},
	"config-routes":   {readScope: "config", writeScope: "config:edit_admin"},
	"subagents":       {readScope: "config", writeScope: "config:edit_admin"},
	"t2i":             {readScope: "config", writeScope: "config:edit_admin"},

	// ── persona / data / chat / skill / mcp / plugin ─────────
	"personas":        {readScope: "persona", writeScope: "persona"},
	"persona-folders": {readScope: "persona", writeScope: "persona"},
	"knowledge_base":  {readScope: "data", writeScope: "data"},
	"knowledge-bases": {readScope: "data", writeScope: "data"},
	"sessions":        {readScope: "data", writeScope: "data"},
	"session-groups":  {readScope: "data", writeScope: "data"},
	"conversations":   {readScope: "data", writeScope: "data"},
	"chat":            {readScope: "chat", writeScope: "chat"},
	"tools":           {readScope: "skill", writeScope: "skill"},
	"skills":          {readScope: "skill", writeScope: "skill"},
	"mcp":             {readScope: "mcp", writeScope: "mcp"},
	"plugin-sources":  {readScope: "plugin", writeScope: "plugin"},
	"plug":            {readScope: "plugin", writeScope: "plugin"},

	// ── 默认规则端点（任一默认 scope 即可）──────────────────
	"stat":       {},
	"stats":      {},
	"changelogs": {},
	"extensions": {},
	"commands":   {},
	"trace":      {},
	"webhooks":   {},
	"t":          {},
}

// endpointScopeFor 查表得出端点的 scope 规则：优先 parts[0]+parts[1] 精确
// 匹配，其次 parts[0]；plugins / files / backups 有各自子路径规则；无映射
// 时返回默认规则（任一默认 scope 即可）。
func (s *Server) endpointScopeFor(parts []string, method string) endpointScope {
	first := parts[0]
	if len(parts) > 1 {
		if rule, ok := endpointScopeRules[first+"/"+parts[1]]; ok {
			return rule
		}
	}
	switch first {
	case "plugins":
		return pluginEndpointRule(parts, method)
	case "files":
		return fileEndpointRule(parts, method)
	case "backups":
		return backupEndpointRule(parts, method)
	case "stat", "stats":
		// /stat/storage/cleanup（三段 POST 清理）→ 仅 JWT。
		if len(parts) >= 3 && parts[1] == "storage" && parts[2] == "cleanup" {
			return endpointScope{systemOnly: true}
		}
	}
	if rule, ok := endpointScopeRules[first]; ok {
		return rule
	}
	return endpointScope{}
}

// pluginEndpointRule 判定 plugins 段的权限：读取（列表/详情/市场/配置读取/
// 文档/扩展代理等）→ plugin scope；管理写操作（安装/更新/卸载/配置保存/
// 重载/启停/日志级别/绑定源）→ 仅 JWT（对齐任务要求的 systemOnly）。
func pluginEndpointRule(parts []string, method string) endpointScope {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	if len(parts) > 1 {
		switch parts[1] {
		case "install", "update", "reload", "enabled", "idle-unload",
			"log-level", "source":
			return endpointScope{systemOnly: true}
		case "idle-unload-global":
			if method != http.MethodGet {
				return endpointScope{systemOnly: true}
			}
		}
	}
	switch sub {
	case "install", "update", "reload", "enabled", "idle-unload":
		// 写操作端点：handler 不接受 GET 的也会在方法校验处拒绝，但
		// 门级统一按 systemOnly 收紧（部分端点 GET 亦有写副作用）。
		return endpointScope{systemOnly: true}
	case "extensions":
		// 插件 Web API 代理（register_web_api，同 /api/plug 语义）：
		// 对齐 Python require_plugin_scope，全部方法按 plugin scope。
		return endpointScope{readScope: "plugin", writeScope: "plugin"}
	case "by-id":
		// GET 为详情读取；DELETE/POST 为卸载（管理写）。
		if method == http.MethodDelete || method == http.MethodPost {
			return endpointScope{systemOnly: true}
		}
		return endpointScope{readScope: "plugin", writeScope: "plugin"}
	case "config":
		// GET 读取配置（含 schema）；PUT/POST 保存配置（管理写）。
		if method == http.MethodPost || method == http.MethodPut {
			return endpointScope{systemOnly: true}
		}
		return endpointScope{readScope: "plugin", writeScope: "plugin"}
	default:
		// {plugin_id} 动态段：GET 详情为读；DELETE/POST 卸载与
		// {plugin_id}/config|source|update|reload|log-level 为管理写
		// （{plugin_id}/config 的 GET 为配置读取，plugin scope 放行）。
		if len(parts) > 1 {
			switch parts[1] {
			case "config":
				if method != http.MethodGet {
					return endpointScope{systemOnly: true}
				}
				return endpointScope{readScope: "plugin", writeScope: "plugin"}
			case "source", "update", "reload", "log-level":
				return endpointScope{systemOnly: true}
			}
		}
		if method == http.MethodDelete || method == http.MethodPost {
			return endpointScope{systemOnly: true}
		}
		return endpointScope{readScope: "plugin", writeScope: "plugin"}
	}
}

// fileEndpointRule 判定 files 段的权限：读取（列表/内容/附件）→ file scope；
// 上传（POST）与删除（DELETE）→ 仅 JWT。
func fileEndpointRule(parts []string, method string) endpointScope {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	if method == http.MethodPost || method == http.MethodDelete || sub == "upload" {
		return endpointScope{systemOnly: true}
	}
	return endpointScope{readScope: "file", writeScope: "file"}
}

// backupEndpointRule 判定 backups 段的权限：读取（列表/下载/任务进度）→
// data scope；管理写（创建/上传/导入/重命名/删除/校验）→ 仅 JWT。
func backupEndpointRule(parts []string, method string) endpointScope {
	if method == http.MethodGet {
		return endpointScope{readScope: "data", writeScope: "data"}
	}
	return endpointScope{systemOnly: true}
}
