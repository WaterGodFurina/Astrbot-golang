// Package dashboard - MCP server config store.
// Persists to data/mcp_server.json, compatible with
// astrbot/core/provider/func_tool_manager.py load/save_mcp_config.
package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

const mcpConfigFileName = "mcp_server.json"

// mcpConfig mirrors the Python DEFAULT_MCP_CONFIG layout.
type mcpConfig struct {
	McpServers map[string]map[string]interface{} `json:"mcpServers"`
}

type mcpStore struct {
	mu     sync.Mutex
	path   string
	config *mcpConfig
}

func newMCPStore(dataDir string) *mcpStore {
	ms := &mcpStore{
		path:   filepath.Join(dataDir, mcpConfigFileName),
		config: &mcpConfig{McpServers: map[string]map[string]interface{}{}},
	}
	ms.load()
	return ms
}

func (ms *mcpStore) load() {
	data, err := os.ReadFile(ms.path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, ms.config)
	if ms.config.McpServers == nil {
		ms.config.McpServers = map[string]map[string]interface{}{}
	}
}

func (ms *mcpStore) save() error {
	data, err := json.MarshalIndent(ms.config, "", "  ")
	if err != nil {
		return err
	}
	// 原子写：临时文件 + fsync + rename，避免非原子 WriteFile 崩溃丢数据。
	return writeFileAtomic(ms.path, data, 0644)
}

// list returns all servers in the dashboard list format:
// [{name, active, connected, ...config fields}]
func (ms *mcpStore) list() []map[string]interface{} {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	result := []map[string]interface{}{}
	for name, cfg := range ms.config.McpServers {
		info := map[string]interface{}{"name": name, "active": true, "connected": true}
		if active, ok := cfg["active"].(bool); ok {
			info["active"] = active
			info["connected"] = active
		}
		for k, v := range cfg {
			if k != "active" {
				info[k] = deepCopyValue(v)
			}
		}
		result = append(result, info)
	}
	return result
}

// upsert adds or updates a server (config uses "active" for enabled).
func (ms *mcpStore) upsert(name string, cfg map[string]interface{}) error {
	if name == "" {
		return errMCPEmptyName
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if cfg == nil {
		cfg = map[string]interface{}{}
	}
	if _, ok := cfg["active"]; !ok {
		cfg["active"] = true
	}
	cfg["name"] = name
	ms.config.McpServers[name] = cfg
	return ms.save()
}

// delete removes a server by name.
func (ms *mcpStore) delete(name string) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if _, ok := ms.config.McpServers[name]; !ok {
		return errMCPNotFound
	}
	delete(ms.config.McpServers, name)
	return ms.save()
}

// setEnabled toggles a server's active flag.
func (ms *mcpStore) setEnabled(name string, enabled bool) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	cfg, ok := ms.config.McpServers[name]
	if !ok {
		return errMCPNotFound
	}
	cfg["active"] = enabled
	return ms.save()
}

// get returns a server config by name.
func (ms *mcpStore) get(name string) map[string]interface{} {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	cfg, ok := ms.config.McpServers[name]
	if !ok {
		return map[string]interface{}{}
	}
	return copyPersonaMap(cfg)
}

var (
	errMCPEmptyName = &mcpError{"Server name cannot be empty"}
	errMCPNotFound  = &mcpError{"Server does not exist"}
)

type mcpError struct{ msg string }

func (e *mcpError) Error() string { return e.msg }
