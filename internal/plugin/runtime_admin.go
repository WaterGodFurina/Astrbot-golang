// Package plugin - subprocess runtime management extensions: dashboard-facing
// list/enable/disable/uninstall and config storage, mirroring the legacy .so
// manager's extensions.go semantics but backed by the install manifest and
// compiled binaries instead of .so files.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ListInfo returns dashboard-compatible plugin info: running subprocess
// instances plus installed-but-disabled plugins from the persisted manifest,
// so the WebUI keeps showing plugins that were switched off.
func (m *SubprocessManager) ListInfo() []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(m.instances))
	for _, inst := range m.List() {
		desc := ""
		if inst.Meta != nil {
			desc = inst.Meta.Description
		}
		result = append(result, map[string]interface{}{
			"name":        inst.Name,
			"version":     inst.Version,
			"description": desc,
			"path":        inst.Binary,
			"id":          inst.ID,
			"loaded":      true,
			"enabled":     true,
			"activated":   true,
			"reserved":    false,
			"author":      "",
		})
	}
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		return result
	}
	for _, e := range man.Plugins {
		if e.Enabled {
			continue
		}
		if m.Get(e.ID) != nil {
			continue
		}
		result = append(result, map[string]interface{}{
			"name":        e.Name,
			"version":     e.Version,
			"description": "插件已禁用",
			"path":        e.Binary,
			"id":          e.ID,
			"loaded":      false,
			"enabled":     false,
			"activated":   false,
			"reserved":    false,
			"author":      "",
		})
	}
	return result
}

// ListFailedPlugins wraps Failed() into the dashboard's /plugins/failed
// contract (name -> {name, path, reason, enabled}).
func (m *SubprocessManager) ListFailedPlugins() map[string]interface{} {
	result := map[string]interface{}{}
	for id, err := range m.Failed() {
		name, binary := id, ""
		if man, merr := LoadManifest(m.manifestPath()); merr == nil {
			if e := man.Get(id); e != nil {
				name, binary = e.Name, e.Binary
			}
		}
		result[id] = map[string]interface{}{
			"name":    name,
			"path":    binary,
			"reason":  err.Error(),
			"enabled": false,
		}
	}
	return result
}

// SetEnabled enables/disables an installed plugin: enable loads the cached
// binary from the manifest (idempotent when already running), disable unloads
// it; the manifest Enabled flag is persisted either way.
func (m *SubprocessManager) SetEnabled(id string, enabled bool) error {
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		return err
	}
	entry := man.Get(id)
	if entry == nil {
		return fmt.Errorf("plugin %s not in install manifest", id)
	}
	if enabled {
		if m.Get(id) == nil {
			if _, err := m.Load(context.Background(), id, entry.Binary); err != nil {
				return err
			}
		}
		entry.Enabled = true
	} else {
		if m.Get(id) != nil {
			if err := m.Unload(id); err != nil {
				return err
			}
		}
		entry.Enabled = false
	}
	return man.Save(m.manifestPath())
}

// Uninstall removes an installed plugin: unloads it, drops the manifest entry,
// deletes its compiled binary directory and optionally its config directory.
func (m *SubprocessManager) Uninstall(id string, deleteConfig bool) error {
	name := id
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		return err
	}
	if e := man.Get(id); e != nil {
		name = e.Name
	}
	if m.Get(id) != nil {
		if err := m.Unload(id); err != nil {
			return err
		}
	}
	man.Remove(id)
	if err := man.Save(m.manifestPath()); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(m.dataDir, "plugins-bin", sanitizeID(id))); err != nil {
		return err
	}
	if deleteConfig {
		if err := os.RemoveAll(filepath.Join(m.dataDir, "plugins", name)); err != nil {
			return err
		}
	}
	logger.Info("Plugin %s uninstalled (deleteConfig=%v)", id, deleteConfig)
	return nil
}

// instanceByName returns the running instance for a plugin identifier (id or
// name), or nil.
func (m *SubprocessManager) instanceByName(name string) *PluginInstance {
	if inst := m.Get(name); inst != nil {
		return inst
	}
	for _, inst := range m.List() {
		if inst.Name == name {
			return inst
		}
	}
	return nil
}

// ConfigSchema returns the plugin's config schema exported via Register().
func (m *SubprocessManager) ConfigSchema(name string) map[string]interface{} {
	inst := m.instanceByName(name)
	if inst == nil || inst.Meta == nil {
		return map[string]interface{}{}
	}
	if len(inst.Meta.ConfigSchemaJson) == 0 {
		return map[string]interface{}{}
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(inst.Meta.ConfigSchemaJson, &schema); err != nil {
		logger.Warn("ConfigSchema(%s): %v", name, err)
		return map[string]interface{}{}
	}
	return schema
}

// configPath returns plugins/<name>/config.json.
func (m *SubprocessManager) configPath(name string) string {
	return filepath.Join(m.dataDir, "plugins", name, "config.json")
}

// LoadConfig reads the plugin config from plugins/<name>/config.json.
func (m *SubprocessManager) LoadConfig(name string) map[string]interface{} {
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
func (m *SubprocessManager) SaveConfig(name string, cfg map[string]interface{}) error {
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

// PluginDataDir returns the per-plugin data directory (plugins/<name>/data),
// creating it if needed.
func (m *SubprocessManager) PluginDataDir(name string) string {
	dir := filepath.Join(m.dataDir, "plugins", name, "data")
	_ = os.MkdirAll(dir, 0755)
	return dir
}
