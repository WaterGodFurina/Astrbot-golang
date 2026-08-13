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
	// Global auth gate: every endpoint except the public whitelist below
	// requires a valid session token (JWT or legacy in-memory token). The
	// WebUI attaches "Authorization: Bearer <token>" to all requests via its
	// axios request interceptor.
	if !s.apiAuthAllowed(r) {
		writeJSON(w, http.StatusUnauthorized, apiError("未认证"))
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/")
	if strings.HasPrefix(path, "v1/") {
		path = strings.TrimPrefix(path, "v1/")
	}
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

	// ── Knowledge base ───────────────────────────────────
	case "knowledge_base", "knowledge-bases":
		s.handleKB(w, r, rest)

	// ── Sessions / Conversations / Session Groups ────────
	case "sessions", "session-groups":
		s.handleSessions(w, r, rest)
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
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"versions": []interface{}{},
		}))

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
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"platforms": []interface{}{},
		}))

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

	// ── Pip install (stub) ───────────────────────────────
	case "pip":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"status": "not_supported",
		}))

	default:
		writeJSON(w, http.StatusNotFound, apiError("unknown endpoint: /api/"+category))
	}
}

// apiAuthAllowed reports whether the request may pass without a session token.
// Public endpoints: auth login/check/setup-status, first-install auth/setup
// (password change still required), and the WebSocket chat transports, which
// authenticate themselves via their own ?token= query check (browsers cannot
// set Authorization headers on WebSocket upgrades).
func (s *Server) apiAuthAllowed(r *http.Request) bool {
	if s.auth == nil {
		return true
	}
	p := strings.TrimPrefix(r.URL.Path, "/api/")
	if strings.HasPrefix(p, "v1/") {
		p = strings.TrimPrefix(p, "v1/")
	}
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
			if s.auth.PasswordChangeRequired() {
				return true
			}
			return s.auth.IsAuthenticated(extractToken(r))
		}
		// totp / account 等子端点：需已认证会话。
		return s.auth.IsAuthenticated(extractToken(r))
	case "unified-chat", "live-chat":
		// WebSocket transport validates its own token query parameter.
		return true
	}
	return s.auth.IsAuthenticated(extractToken(r))
}
