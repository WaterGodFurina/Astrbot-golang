package plugin

import (
	"encoding/json"
	"os"
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

// Save writes the manifest atomically.
func (m *Manifest) Save(path string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
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
