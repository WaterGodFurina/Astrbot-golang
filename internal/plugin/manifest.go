package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Manifest persists installed plugins across restarts (source URL, enabled
// state, compiled artifact path) so the runtime can recompile/reload from the
// cache instead of re-downloading and re-installing everything.
type Manifest struct {
	Version int             `json:"version"`
	Plugins []ManifestEntry `json:"plugins"`
}

// ManifestEntry describes one installed plugin.
type ManifestEntry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Source  string `json:"source,omitempty"` // download/clone URL
	Binary  string `json:"binary"`           // cached compiled artifact path
	Enabled bool   `json:"enabled"`

	// Install source metadata (market / repository) persisted so the WebUI can
	// offer reinstall / change-source, mirroring Python's install source
	// records.
	InstallMethod  string `json:"install_method,omitempty"` // "market" | "repository" | "url" | "upload"
	RegistryURL    string `json:"registry_url,omitempty"`
	RegistryName   string `json:"registry_name,omitempty"`
	MarketPluginID string `json:"market_plugin_id,omitempty"`
	Repo           string `json:"repo,omitempty"`
	DownloadURL    string `json:"download_url,omitempty"`

	// Language is "go" (compiled binary) or "python" (source tree run under
	// the embedded Python SDK). Empty/absent = "go" (legacy entries).
	Language string `json:"language,omitempty"`
	// DisplayName / ShortDesc are the plugin's display metadata (Python
	// metadata.yaml display_name / short_desc), surfaced to the WebUI.
	DisplayName string `json:"display_name,omitempty"`
	ShortDesc   string `json:"short_desc,omitempty"`

	// 插件在 dataDir 下创建的目录足迹（相对 dataDir），供卸载时按
	// "删除配置文件 / 删除插件数据" 选项精确清理。
	ConfigDir string `json:"config_dir,omitempty"` // plugins_config/<name>  (config.json + config_schema.json)
	DataDir   string `json:"data_dir,omitempty"`   // plugins/<name>/data     (插件运行时数据)
	DocsDir   string `json:"docs_dir,omitempty"`   // plugins/<name>          (README/CHANGELOG 缓存)
}

// LoadManifest reads a manifest file, tolerating absence.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Manifest{Version: 1}, nil
		}
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Save writes the manifest atomically: 先写临时文件并 fsync，再 os.Rename 原子
// 替换，避免并发覆盖/崩溃截断导致条目丢失（参照 internal/config 的 save 写法）。
func (m *Manifest) Save(path string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	committed = true
	return nil
}

// Get returns the entry for a plugin ID, or nil.
func (m *Manifest) Get(id string) *ManifestEntry {
	for i := range m.Plugins {
		if m.Plugins[i].ID == id {
			return &m.Plugins[i]
		}
	}
	return nil
}

// Upsert adds or updates an entry.
func (m *Manifest) Upsert(e ManifestEntry) {
	for i := range m.Plugins {
		if m.Plugins[i].ID == e.ID {
			m.Plugins[i] = e
			return
		}
	}
	m.Plugins = append(m.Plugins, e)
}

// Remove deletes an entry by ID.
func (m *Manifest) Remove(id string) {
	out := m.Plugins[:0]
	for _, e := range m.Plugins {
		if e.ID != id {
			out = append(out, e)
		}
	}
	m.Plugins = out
}
