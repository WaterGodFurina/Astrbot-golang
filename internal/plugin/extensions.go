// Package plugin - .so plugin management extensions: failure tracking,
// disable/enable (.so.disable), hot reload and config storage.
package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FailedPlugin records a plugin that failed to load.
type FailedPlugin struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Reason  string `json:"reason"`
	Enabled bool   `json:"enabled"`
}

// ListPluginsInfo returns plugin info compatible with the dashboard API.
// Includes both loaded plugins and disabled (.so.disable) plugins so the
// dashboard still shows them (mirrors Python's plugin list behavior).
func (m *Manager) ListPluginsInfo() []map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]map[string]interface{}, 0, len(m.plugins))
	loadedNames := make(map[string]bool, len(m.plugins))
	for _, p := range m.plugins {
		loadedNames[p.Name] = true
		result = append(result, map[string]interface{}{
			"name":        p.Name,
			"version":     p.Version,
			"description": p.Description,
			"path":        p.Path,
			"id":          p.Name,
			"loaded":      true,
			"enabled":     true,
			"activated":   true,
			"reserved":    false,
			"author":      "",
		})
	}

	// Disabled plugins (.so.disable) are listed with activated=false.
	dir := m.ctx.PluginDir
	if dir == "" {
		dir = "data/plugins"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return result
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".so.disable") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".so.disable")
		if loadedNames[name] {
			continue
		}
		result = append(result, map[string]interface{}{
			"name":        name,
			"version":     "",
			"description": "插件已禁用",
			"path":        filepath.Join(dir, e.Name()),
			"id":          name,
			"loaded":      false,
			"enabled":     false,
			"activated":   false,
			"reserved":    false,
			"author":      "",
		})
	}
	return result
}

// ListFailedPlugins returns failed plugins as a map (name -> error detail),
// matching the dashboard's /plugins/failed contract.
func (m *Manager) ListFailedPlugins() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := map[string]interface{}{}
	for _, fp := range m.failedPlugins {
		result[fp.Name] = map[string]interface{}{
			"name":    fp.Name,
			"path":    fp.Path,
			"reason":  fp.Reason,
			"enabled": fp.Enabled,
		}
	}
	return result
}

// DisablePlugin disables a loaded plugin by renaming its .so to .so.disable.
// Returns the disabled plugin name.
func (m *Manager) DisablePlugin(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for path, p := range m.plugins {
		if p.Name != name {
			continue
		}
		if p.cleanup != nil {
			_ = p.cleanup()
		}
		disabled := path + ".disable"
		if err := os.Rename(path, disabled); err != nil {
			return fmt.Errorf("rename %s: %w", path, err)
		}
		delete(m.plugins, path)
		logger.Info("Plugin %s disabled (%s)", name, disabled)
		return nil
	}
	return fmt.Errorf("plugin %s not loaded (loaded: %v)", name, m.pluginNamesLocked())
}

func (m *Manager) pluginNamesLocked() []string {
	names := make([]string, 0, len(m.plugins))
	for _, p := range m.plugins {
		names = append(names, p.Name)
	}
	return names
}

// EnablePlugin re-enables a disabled plugin by renaming .so.disable back to .so
// and loading it. Hot reload is emulated by copying to a fresh temp file first
// (Go's plugin package caches by path).
func (m *Manager) EnablePlugin(name string) error {
	dir := m.ctx.PluginDir
	if dir == "" {
		dir = "data/plugins"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".so.disable") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".so.disable")
		// match by plugin name in the file name
		if base != name && !strings.Contains(base, name) {
			continue
		}
		src := filepath.Join(dir, e.Name())
		dst := filepath.Join(dir, base+".so")
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("rename %s: %w", src, err)
		}
		if _, err := m.LoadPlugin(dst); err != nil {
			return fmt.Errorf("reload %s: %w", dst, err)
		}
		logger.Info("Plugin %s enabled (%s)", base, dst)
		return nil
	}
	return fmt.Errorf("disabled plugin %s not found", name)
}

// ReloadAll re-scans the plugin directory. Go's plugin package can only open
// a given set of exported symbols once ("plugin already loaded"), so plugins
// that are already loaded are kept as-is; only newly added .so files are
// loaded. Editing a plugin's code requires a process restart to take effect.
func (m *Manager) ReloadAll() ([]string, []error) {
	var reloaded []string
	var errs []error
	m.failedPlugins = nil

	m.mu.RLock()
	loadedNames := make(map[string]bool, len(m.plugins))
	for _, p := range m.plugins {
		loadedNames[p.Name] = true
	}
	m.mu.RUnlock()

	dir := m.ctx.PluginDir
	if dir == "" {
		dir = "data/plugins"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []error{err}
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".so") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".so")
		if loadedNames[name] {
			logger.Info("Reload: plugin %s already loaded (Go plugin cache), skipping", name)
			continue
		}
		src := filepath.Join(dir, e.Name())
		tmp := filepath.Join(dir, fmt.Sprintf(".reload_%d_%s", time.Now().UnixNano(), e.Name()))
		data, err := os.ReadFile(src)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.WriteFile(tmp, data, 0755); err != nil {
			errs = append(errs, err)
			continue
		}
		p, err := m.LoadPlugin(tmp)
		_ = os.Remove(tmp)
		if err != nil {
			m.failedPlugins = append(m.failedPlugins, FailedPlugin{
				Name:    name,
				Path:    src,
				Reason:  err.Error(),
				Enabled: false,
			})
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		reloaded = append(reloaded, p.Name)
	}
	logger.Info("Hot reload scan done: %d new plugin(s), %d failed", len(reloaded), len(errs))
	return reloaded, errs
}

// PluginDataDir returns the per-plugin data directory (plugins/<name>/data),
// creating it if needed.
func (m *Manager) PluginDataDir(name string) string {
	base := m.ctx.PluginDir
	if base == "" {
		base = "data/plugins"
	}
	dir := filepath.Join(base, name, "data")
	_ = os.MkdirAll(dir, 0755)
	return dir
}

// configPath returns plugins/<name>/config.json.
func (m *Manager) configPath(name string) string {
	base := m.ctx.PluginDir
	if base == "" {
		base = "data/plugins"
	}
	return filepath.Join(base, name, "config.json")
}

// ConfigSchema returns the plugin's config schema exported via GetConfigSchema().
func (m *Manager) ConfigSchema(name string) map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, p := range m.plugins {
		if p.Name != name {
			continue
		}
		sym, err := p.so.Lookup("GetConfigSchema")
		if err != nil {
			logger.Warn("ConfigSchema(%s): Lookup failed: %v", name, err)
			return map[string]interface{}{}
		}
		fn, ok := sym.(func() map[string]interface{})
		if !ok {
			logger.Warn("ConfigSchema(%s): type assertion failed: %T", name, sym)
			return map[string]interface{}{}
		}
		return fn()
	}
	return map[string]interface{}{}
}

// LoadConfig reads the plugin config from plugins/<name>/config.json.
func (m *Manager) LoadConfig(name string) map[string]interface{} {
	data, err := os.ReadFile(m.configPath(name))
	if err != nil {
		return map[string]interface{}{}
	}
	var cfg map[string]interface{}
	if json.Unmarshal(data, &cfg) != nil {
		return map[string]interface{}{}
	}
	return cfg
}

// SaveConfig persists the plugin config to plugins/<name>/config.json.
func (m *Manager) SaveConfig(name string, cfg map[string]interface{}) error {
	if cfg == nil {
		cfg = map[string]interface{}{}
	}
	path := m.configPath(name)
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// Reload hot-reloads a single plugin by copying its .so to a fresh file
// (Go's plugin package caches by path) and loading the copy.
func (m *Manager) Reload(name string) error {
	m.mu.Lock()
	// unload existing instance
	for path, p := range m.plugins {
		if p.Name != name {
			continue
		}
		if p.cleanup != nil {
			_ = p.cleanup()
		}
		delete(m.plugins, path)
	}
	m.mu.Unlock()

	dir := m.ctx.PluginDir
	if dir == "" {
		dir = "data/plugins"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".so") {
			continue
		}
		if strings.TrimSuffix(e.Name(), ".so") != name {
			continue
		}
		src := filepath.Join(dir, e.Name())
		tmp := filepath.Join(dir, fmt.Sprintf(".reload_%d_%s", time.Now().UnixNano(), e.Name()))
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(tmp, data, 0755); err != nil {
			return err
		}
		_, err = m.LoadPlugin(tmp)
		_ = os.Remove(tmp)
		return err
	}
	return fmt.Errorf("plugin %s not found in %s", name, dir)
}

// Uninstall removes a plugin: unloads it and optionally deletes its files.
func (m *Manager) Uninstall(name string, deleteConfig bool) error {
	m.mu.Lock()
	for path, p := range m.plugins {
		if p.Name != name {
			continue
		}
		if p.cleanup != nil {
			_ = p.cleanup()
		}
		delete(m.plugins, path)
	}
	m.mu.Unlock()

	dir := m.ctx.PluginDir
	if dir == "" {
		dir = "data/plugins"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".so") {
			continue
		}
		if strings.TrimSuffix(e.Name(), ".so") != name {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
		_ = os.Remove(filepath.Join(dir, e.Name()+".disable"))
	}
	if deleteConfig {
		_ = os.RemoveAll(filepath.Join(dir, name))
	}
	logger.Info("Plugin %s uninstalled (deleteConfig=%v)", name, deleteConfig)
	return nil
}
