// Package config implements AstrBot's configuration management.
// Ported from astrbot/core/config/astrbot_config.py
//
// Bug fix for issue #9512: check_config_integrity would clear user-created
// keys in dict-type config fields. The Python code recursed into dict
// references that were empty {} and treated user keys as "not in refer_conf",
// then conf.clear() + conf.update(new_conf) wiped them.
//
// In Go we preserve user keys when the reference dict is empty (i.e. the
// schema declares type=dict with no fixed children).
package config

import (
        "encoding/json"
        "fmt"
        "os"
        "path/filepath"
        "sync"
        "time"

        "github.com/AstrBotDevs/AstrBot/internal/log"
)

var logger = log.GetDefault().WithComponent("Config")

// SchemaType identifies a config schema field type.
type SchemaType string

const (
        SchemaString  SchemaType = "string"
        SchemaInt     SchemaType = "int"
        SchemaFloat   SchemaType = "float"
        SchemaBool    SchemaType = "bool"
        SchemaList    SchemaType = "list"
        SchemaObject  SchemaType = "object"
        SchemaDict    SchemaType = "dict"
        SchemaText    SchemaType = "text"
        SchemaSelect  SchemaType = "select"
        SchemaTemplateList SchemaType = "template_list"
)

// defaultForType returns the default value for a schema type.
func defaultForType(t SchemaType) interface{} {
        switch t {
        case SchemaString, SchemaText, SchemaSelect:
                return ""
        case SchemaInt:
                return 0
        case SchemaFloat:
                return 0.0
        case SchemaBool:
                return false
        case SchemaList, SchemaTemplateList:
                return []interface{}{}
        case SchemaObject, SchemaDict:
                return map[string]interface{}{}
        default:
                return nil
        }
}

// AstrBotConfig wraps a JSON config file with integrity checking and atomic writes.
type AstrBotConfig struct {
        mu           sync.RWMutex
        configPath   string
        data         map[string]interface{}
        defaultConf  map[string]interface{}
        schema       map[string]interface{}
        saveRevision int64
        committedRev int64
}

// New creates a new AstrBotConfig. If the config file doesn't exist,
// it's created from defaults. Integrity is checked and missing keys are added.
func New(configPath string, defaults map[string]interface{}, schema map[string]interface{}) (*AstrBotConfig, error) {
        cfg := &AstrBotConfig{
                configPath:  configPath,
                data:        make(map[string]interface{}),
                schema:      schema,
        }

        if schema != nil {
                defaults = schemaToDefaults(schema)
        }
        if defaults == nil {
                defaults = make(map[string]interface{})
        }
        cfg.defaultConf = defaults

        if !fileExists(configPath) {
                cfg.data = copyMap(defaults)
                if err := cfg.save(4); err != nil {
                        return nil, fmt.Errorf("create default config: %w", err)
                }
                logger.Info("Config file not found, created with defaults: %s", configPath)
        }

        raw, err := os.ReadFile(configPath)
        if err != nil {
                return nil, fmt.Errorf("read config: %w", err)
        }
        // Strip UTF-8 BOM
        raw = stripBOM(raw)

        var conf map[string]interface{}
        if err := json.Unmarshal(raw, &conf); err != nil {
                return nil, fmt.Errorf("parse config JSON: %w", err)
        }

        hasNew := checkConfigIntegrity(defaults, conf, "")
        if hasNew {
                data, err := json.MarshalIndent(conf, "", "    ")
                if err != nil {
                        return nil, fmt.Errorf("marshal config: %w", err)
                }
                if err := os.WriteFile(configPath, data, 0644); err != nil {
                        return nil, fmt.Errorf("write config: %w", err)
                }
        }

        cfg.data = conf
        return cfg, nil
}

// Get retrieves a value by key. Returns nil if not found.
func (c *AstrBotConfig) Get(key string) interface{} {
        c.mu.RLock()
        defer c.mu.RUnlock()
        return c.data[key]
}

// GetString returns a string value, or "" if not a string.
func (c *AstrBotConfig) GetString(key string) string {
        v := c.Get(key)
        if s, ok := v.(string); ok {
                return s
        }
        return ""
}

// GetBool returns a bool value, or false.
func (c *AstrBotConfig) GetBool(key string) bool {
        v := c.Get(key)
        if b, ok := v.(bool); ok {
                return b
        }
        return false
}

// GetInt returns an int value, or 0.
func (c *AstrBotConfig) GetInt(key string) int {
        v := c.Get(key)
        switch n := v.(type) {
        case int:
                return n
        case int64:
                return int(n)
        case float64:
                return int(n)
        }
        return 0
}

// Set updates a key and saves to disk.
func (c *AstrBotConfig) Set(key string, value interface{}) error {
        c.mu.Lock()
        c.data[key] = value
        c.mu.Unlock()
        return c.Save()
}

// Save persists the current config to disk atomically.
func (c *AstrBotConfig) Save() error {
        return c.save(2)
}

// SaveAsync saves in a goroutine (fire-and-forget).
func (c *AstrBotConfig) SaveAsync() {
        go func() {
                if err := c.Save(); err != nil {
                        logger.Error("async save failed: %v", err)
                }
        }()
}

// Update merges the given map into the config and saves.
func (c *AstrBotConfig) Update(updates map[string]interface{}) error {
        c.mu.Lock()
        for k, v := range updates {
                c.data[k] = v
        }
        c.mu.Unlock()
        return c.Save()
}

// All returns a copy of the full config map.
func (c *AstrBotConfig) All() map[string]interface{} {
        c.mu.RLock()
        defer c.mu.RUnlock()
        return copyMap(c.data)
}

// Path returns the config file path.
func (c *AstrBotConfig) Path() string { return c.configPath }

// save writes the config to a temp file then renames atomically.
func (c *AstrBotConfig) save(indent int) error {
        c.mu.Lock()
        snapshot := copyMap(c.data)
        c.saveRevision++
        rev := c.saveRevision
        c.mu.Unlock()

        dir := filepath.Dir(c.configPath)
        if dir == "" {
                dir = "."
        }
        tmp, err := os.CreateTemp(dir, filepath.Base(c.configPath)+".*.tmp")
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

        enc := json.NewEncoder(tmp)
        enc.SetIndent("", repeatSpace(indent))
        enc.SetEscapeHTML(false)
        if err := enc.Encode(snapshot); err != nil {
                tmp.Close()
                return fmt.Errorf("encode: %w", err)
        }
        if err := tmp.Sync(); err != nil {
                tmp.Close()
                return fmt.Errorf("sync: %w", err)
        }
        tmp.Close()

        c.mu.Lock()
        defer c.mu.Unlock()
        if rev > c.committedRev {
                if err := os.Rename(tmpName, c.configPath); err != nil {
                        return fmt.Errorf("rename: %w", err)
                }
                c.committedRev = rev
                committed = true
        }
        return nil
}

// checkConfigIntegrity recursively checks config against a reference.
// Missing keys are inserted, type mismatches are corrected.
//
// FIXED (Issue #9512): When the reference value is an empty map (schema type=dict),
// user-created keys are PRESERVED instead of being wiped. The Python code
// treated user keys as "not in refer_conf" and conf.clear()+conf.update(new_conf)
// destroyed them. We now:
//   - Preserve all keys the user added to dict-type fields
//   - Only recurse into dict references that have known children
//   - Never remove user keys from an empty reference dict
func checkConfigIntegrity(refer, conf map[string]interface{}, path string) bool {
        hasNew := false
        newConf := make(map[string]interface{})

        // Process keys present in the reference
        for key, refVal := range refer {
                fullPath := joinPath(path, key)
                userVal, exists := conf[key]

                if !exists {
                        logger.Info("Config key missing; added default: %s", fullPath)
                        newConf[key] = refVal
                        hasNew = true
                        continue
                }

                if userVal == nil {
                        newConf[key] = refVal
                        hasNew = true
                        continue
                }

                // Both are maps: recurse
                refMap, refIsMap := refVal.(map[string]interface{})
                userMap, userIsMap := userVal.(map[string]interface{})

                if refIsMap && userIsMap {
                        // BUGFIX #9512: If the reference map is empty (dict type with no
                        // fixed children), preserve ALL user keys without modification.
                        // Only recurse when the reference has known child keys.
                        if len(refMap) == 0 {
                                // Empty reference dict = user-defined key-value pairs.
                                // Preserve everything as-is.
                                newConf[key] = userVal
                        } else {
                                // Reference has fixed children: recurse to check integrity.
                                childNew := checkConfigIntegrity(refMap, userMap, fullPath)
                                // Also preserve any extra keys the user added that aren't
                                // in the reference (important for dict-type extensibility).
                                preserved := make(map[string]interface{})
                                for k, v := range userMap {
                                        preserved[k] = v
                                }
                                // Overwrite with integrity-checked reference keys
                                for k, v := range refMap {
                                        if _, ok := preserved[k]; !ok {
                                                preserved[k] = v
                                                childNew = true
                                        } else if preserved[k] == nil {
                                                preserved[k] = v
                                                childNew = true
                                        }
                                }
                                newConf[key] = preserved
                                if childNew {
                                        hasNew = true
                                }
                        }
                } else if refIsMap && !userIsMap {
                        // Type mismatch: user has non-map but reference expects map
                        logger.Warn("Config type mismatch at %s, using default", fullPath)
                        newConf[key] = refVal
                        hasNew = true
                } else {
                        newConf[key] = userVal
                }
        }

        // Check for user keys not in reference.
        // BUGFIX #9512: For dict types (where reference is empty), we PRESERVE
        // these keys. We only log/remove extra keys for non-dict container types
        // where the reference has known children.
        for key := range conf {
                if _, exists := refer[key]; !exists {
                        fullPath := joinPath(path, key)
                        // If the reference is empty or has no such key, we preserve
                        // the user's key (this is the fix for dict-type fields).
                        // Previously the Python code would log "Config key removed" and
                        // the subsequent conf.clear() + conf.update(new_conf) would drop it.
                        newConf[key] = conf[key]
                        logger.Debug("Preserving user config key not in schema: %s", fullPath)
                }
        }

        // Replace conf contents with newConf (preserving order)
        for k := range conf {
                delete(conf, k)
        }
        for k, v := range newConf {
                conf[k] = v
        }

        return hasNew
}

// schemaToDefaults converts a schema to a default config map.
func schemaToDefaults(schema map[string]interface{}) map[string]interface{} {
        result := make(map[string]interface{})
        for k, v := range schema {
                item, ok := v.(map[string]interface{})
                if !ok {
                        continue
                }
                typeStr, _ := item["type"].(string)
                st := SchemaType(typeStr)

                if st == SchemaObject {
                        childSchema, _ := item["items"].(map[string]interface{})
                        result[k] = schemaToDefaults(childSchema)
                } else {
                        if def, exists := item["default"]; exists {
                                result[k] = def
                        } else {
                                result[k] = defaultForType(st)
                        }
                }
        }
        return result
}

// Helper functions

func fileExists(p string) bool {
        _, err := os.Stat(p)
        return err == nil
}

func stripBOM(b []byte) []byte {
        if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
                return b[3:]
        }
        return b
}

func copyMap(m map[string]interface{}) map[string]interface{} {
        out := make(map[string]interface{}, len(m))
        for k, v := range m {
                out[k] = deepCopyValue(v)
        }
        return out
}

func deepCopyValue(v interface{}) interface{} {
        switch val := v.(type) {
        case map[string]interface{}:
                return copyMap(val)
        case []interface{}:
                out := make([]interface{}, len(val))
                for i, e := range val {
                        out[i] = deepCopyValue(e)
                }
                return out
        default:
                return v
        }
}

func joinPath(parent, key string) string {
        if parent == "" {
                return key
        }
        return parent + "." + key
}

func repeatSpace(n int) string {
        if n <= 0 {
                return ""
        }
        b := make([]byte, n)
        for i := range b {
                b[i] = ' '
        }
        return string(b)
}

// SchemaNode describes a config field's type and default.
// Used by defaults.go to build the default config tree.
type SchemaNode struct {
        Type         SchemaType
        DefaultValue interface{}
        Optional     bool
        Description  string
        Children      map[string]*SchemaNode // for object type
        Items        *SchemaNode            // for list type
}

// NewConfig creates an AstrBotConfig from a path and schema.
func NewConfig(path string, schema *SchemaNode) *AstrBotConfig {
        defaults := schemaToDefaultsMap(schema)
        cfg := &AstrBotConfig{
                configPath:  path,
                data:        make(map[string]interface{}),
                defaultConf: defaults,
        }
        if !fileExists(path) {
                cfg.data = copyMap(defaults)
                if err := cfg.save(4); err != nil {
                        logger.Error("Failed to create default config: %v", err)
                }
        }
        return cfg
}

// Load reads and validates the config file.
func (c *AstrBotConfig) Load() error {
        if !fileExists(c.configPath) {
                return fmt.Errorf("config file not found: %s", c.configPath)
        }
        raw, err := os.ReadFile(c.configPath)
        if err != nil {
                return fmt.Errorf("read config: %w", err)
        }
        raw = stripBOM(raw)
        var conf map[string]interface{}
        if err := json.Unmarshal(raw, &conf); err != nil {
                return fmt.Errorf("parse config JSON: %w", err)
        }
        hasNew := checkConfigIntegrity(c.defaultConf, conf, "")
        if hasNew {
                data, _ := json.MarshalIndent(conf, "", "    ")
                _ = os.WriteFile(c.configPath, data, 0644)
        }
        c.data = conf
        return nil
}

// schemaToDefaultsMap converts a SchemaNode tree to a default value map.
func schemaToDefaultsMap(node *SchemaNode) map[string]interface{} {
        result := make(map[string]interface{})
        if node == nil || node.Children == nil {
                return result
        }
        for k, child := range node.Children {
                switch child.Type {
                case SchemaObject:
                        if len(child.Children) > 0 {
                                result[k] = schemaToDefaultsMap(child)
                        } else {
                                result[k] = make(map[string]interface{})
                        }
                case SchemaList, SchemaTemplateList:
                        result[k] = []interface{}{}
                default:
                        if child.DefaultValue != nil {
                                result[k] = child.DefaultValue
                        } else {
                                result[k] = defaultForType(child.Type)
                        }
                }
        }
        return result
}

// ConfigManager manages multiple config files (global + per-platform).
type ConfigManager struct {
        confs  map[string]*AstrBotConfig
        mu     sync.RWMutex
        logger *log.ComponentLogger
}

// NewConfigManager creates an empty ConfigManager.
func NewConfigManager() *ConfigManager {
        return &ConfigManager{
                confs:  make(map[string]*AstrBotConfig),
                logger: log.GetDefault().WithComponent("ConfigMgr"),
        }
}

// Register adds a config under the given ID.
func (cm *ConfigManager) Register(id string, cfg *AstrBotConfig) {
        cm.mu.Lock()
        cm.confs[id] = cfg
        cm.mu.Unlock()
}

// Get returns the config for the given ID.
func (cm *ConfigManager) Get(id string) *AstrBotConfig {
        cm.mu.RLock()
        defer cm.mu.RUnlock()
        return cm.confs[id]
}

// IDs returns all registered config IDs.
func (cm *ConfigManager) IDs() []string {
        cm.mu.RLock()
        defer cm.mu.RUnlock()
        ids := make([]string, 0, len(cm.confs))
        for id := range cm.confs {
                ids = append(ids, id)
        }
        return ids
}

// Timestamp returns the current time (used for config metadata).
func Timestamp() time.Time { return time.Now() }
