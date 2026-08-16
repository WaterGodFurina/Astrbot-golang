package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

// pluginLogLevelsFile 是 per-plugin 日志级别覆盖的持久化文件（对齐 Python
// 的 plugin_log_levels.json），键为插件实例 id（name_language 稳定 id，
// 同名 Go/Python 插件不互相遮蔽）。
const pluginLogLevelsFile = "plugin_log_levels.json"

// validPluginLogLevels 与 Python PLUGIN_LOG_LEVELS 保持一致。
var validPluginLogLevels = map[string]bool{
	"DEBUG":    true,
	"INFO":     true,
	"WARNING":  true,
	"ERROR":    true,
	"CRITICAL": true,
}

// logLevels 是 SubprocessManager 上的 per-plugin 日志级别覆盖存储。
// 惰性加载 + 互斥保护；SetPluginLogLevel 读→改→写整段持锁（与
// manifestMu 一样防止并发修改丢条目）。
type logLevels struct {
	mu    sync.Mutex
	path  string
	cache map[string]string
}

func newLogLevels(dataDir string) *logLevels {
	return &logLevels{path: filepath.Join(dataDir, pluginLogLevelsFile)}
}

func (l *logLevels) loadLocked() map[string]string {
	if l.cache != nil {
		return l.cache
	}
	l.cache = map[string]string{}
	data, err := os.ReadFile(l.path)
	if err != nil {
		return l.cache
	}
	var m map[string]string
	if json.Unmarshal(data, &m) == nil {
		for id, lvl := range m {
			if validPluginLogLevels[strings.ToUpper(strings.TrimSpace(lvl))] {
				l.cache[id] = strings.ToUpper(strings.TrimSpace(lvl))
			}
		}
	}
	return l.cache
}

// GetPluginLogLevel returns the persisted per-plugin log level override for
// id, or "" when the plugin follows the host's global level.
func (l *logLevels) GetPluginLogLevel(id string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.loadLocked()[id]
}

// SetPluginLogLevel persists a per-plugin log level override. An empty level
// removes the override (follow global); a non-empty level must be one of
// DEBUG/INFO/WARNING/ERROR/CRITICAL. Returns false for an invalid level.
func (l *logLevels) SetPluginLogLevel(id, level string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	level = strings.ToUpper(strings.TrimSpace(level))
	m := l.loadLocked()
	if level == "" {
		if _, ok := m[id]; !ok {
			return true
		}
		delete(m, id)
	} else {
		if !validPluginLogLevels[level] {
			return false
		}
		m[id] = level
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return false
	}
	if err := os.WriteFile(l.path, out, 0o644); err != nil {
		return false
	}
	return true
}

// EffectivePluginLogLevel returns the level a plugin should actually log at:
// its override when set, else the host's current global level.
func (l *logLevels) EffectivePluginLogLevel(id string) string {
	if lvl := l.GetPluginLogLevel(id); lvl != "" {
		return lvl
	}
	return log.LevelString(log.GetDefault().GetLevel())
}

// GetPluginLogLevel returns the persisted per-plugin log level override for
// the instance id, or "" when the plugin follows the global level.
func (m *SubprocessManager) GetPluginLogLevel(id string) string {
	if m == nil || m.logLevels == nil {
		return ""
	}
	return m.logLevels.GetPluginLogLevel(id)
}

// SetPluginLogLevel persists a per-plugin log level override (empty removes
// it). Returns false for an invalid level.
func (m *SubprocessManager) SetPluginLogLevel(id, level string) bool {
	if m == nil || m.logLevels == nil {
		return false
	}
	return m.logLevels.SetPluginLogLevel(id, level)
}

// EffectivePluginLogLevel returns the level a plugin should log at: its
// override when set, else the host's current global level.
func (m *SubprocessManager) EffectivePluginLogLevel(id string) string {
	if m == nil || m.logLevels == nil {
		return log.LevelString(log.GetDefault().GetLevel())
	}
	return m.logLevels.EffectivePluginLogLevel(id)
}