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
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"message": "endpoint not yet implemented: " + category,
		}))
	}
}
