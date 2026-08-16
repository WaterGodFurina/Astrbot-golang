package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	sdkv1 "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/gen/sdkv1"
)

// schemaFixture returns a JSON-Schema covering scalar defaults and a nested
// object group (the shape Go SDK plugins register via ConfigSchemaJson).
func schemaFixture() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"enabled": map[string]interface{}{
				"type": "boolean", "description": "on/off", "default": true,
			},
			"greeting": map[string]interface{}{
				"type": "string", "description": "hello", "default": "你好",
			},
			"group": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"admins": map[string]interface{}{
						"type": "array", "default": []interface{}{"1"},
						"items": map[string]interface{}{"type": "string"},
					},
					"rate": map[string]interface{}{
						"type": "number", "default": 0.5,
					},
				},
			},
		},
	}
}

func newResolverManager(t *testing.T, storedJSON string) *SubprocessManager {
	t.Helper()
	m := NewSubprocessManager(nil, t.TempDir())
	raw, _ := json.Marshal(schemaFixture())
	m.instances["test"] = &PluginInstance{
		Meta: &sdkv1.RegisterResponse{ConfigSchemaJson: raw},
	}
	if storedJSON != "" {
		cfgDir := filepath.Join(m.dataDir, "plugins_config", "test")
		if err := os.MkdirAll(cfgDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(storedJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// TestResolvePluginConfigNoStored verifies that with no stored config the
// resolver returns the full schema default set, nested object groups included.
func TestResolvePluginConfigNoStored(t *testing.T) {
	m := newResolverManager(t, "")
	cfg := m.ConfigResolver().ResolvePluginConfig("test")

	if cfg["enabled"] != true {
		t.Fatalf("scalar default missing: %+v", cfg)
	}
	if cfg["greeting"] != "你好" {
		t.Fatalf("string default missing: %+v", cfg)
	}
	g, ok := cfg["group"].(map[string]interface{})
	if !ok {
		t.Fatalf("nested group defaults not applied: %+v", cfg)
	}
	if len(g["admins"].([]interface{})) != 1 {
		t.Fatalf("nested array default missing: %+v", g)
	}
	if g["rate"] != 0.5 {
		t.Fatalf("nested number default missing: %+v", g)
	}
}

// TestResolvePluginConfigStoredOverrides verifies stored values win over
// defaults while missing keys are filled from the schema.
func TestResolvePluginConfigStoredOverrides(t *testing.T) {
	m := newResolverManager(t, `{"enabled": false, "greeting": "保存值"}`)
	cfg := m.ConfigResolver().ResolvePluginConfig("test")

	if cfg["enabled"] != false {
		t.Fatalf("stored value must override default: %+v", cfg)
	}
	if cfg["greeting"] != "保存值" {
		t.Fatalf("stored value must override default: %+v", cfg)
	}
	// 缺失键（含嵌套分组）补默认。
	g, ok := cfg["group"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing nested group must be default-filled: %+v", cfg)
	}
	if len(g["admins"].([]interface{})) != 1 {
		t.Fatalf("missing nested default missing: %+v", g)
	}
}

// TestResolvePluginConfigStripsMetadataKeys verifies packaged-metadata keys
// never reach the resolved config, even when a legacy config.json mixes them
// in (LoadConfig strips them as part of the stored-read source).
func TestResolvePluginConfigStripsMetadataKeys(t *testing.T) {
	m := newResolverManager(t, `{"display_name": "Legacy", "version": "9.9", "greeting": "x"}`)
	cfg := m.ConfigResolver().ResolvePluginConfig("test")

	for _, k := range metadataConfigKeys {
		if _, ok := cfg[k]; ok {
			t.Fatalf("metadata key %q leaked into resolved config: %+v", k, cfg)
		}
	}
	if cfg["greeting"] != "x" {
		t.Fatalf("real config key lost during stripping: %+v", cfg)
	}
	if cfg["enabled"] != true {
		t.Fatalf("defaults still applied after stripping: %+v", cfg)
	}
}

// TestResolvePluginConfigCallPathsConsistent verifies the HostService GetConfig
// hook seam, the manager-bound resolver (what the dashboard uses) and a
// directly-built resolver all yield the same result for one scenario.
func TestResolvePluginConfigCallPathsConsistent(t *testing.T) {
	m := newResolverManager(t, `{"enabled": false}`)

	want := map[string]interface{}{
		"enabled":  false,
		"greeting": "你好",
		"group": map[string]interface{}{
			"admins": []interface{}{"1"},
			"rate":   0.5,
		},
	}

	// HostService.GetConfig hook seam（nil manager → 空配置）。
	if got := resolvePluginConfig(m, "test"); !reflect.DeepEqual(got, want) {
		t.Fatalf("HostService path mismatch:\n got %+v\nwant %+v", got, want)
	}
	if got := resolvePluginConfig(nil, "test"); len(got) != 0 {
		t.Fatalf("HostService path with nil manager must be empty: %+v", got)
	}
	// Dashboard pluginConfigPayload path（与 dashboard 端同一表达式）。
	if got := m.ConfigResolver().ResolvePluginConfig("test"); !reflect.DeepEqual(got, want) {
		t.Fatalf("manager resolver path mismatch:\n got %+v\nwant %+v", got, want)
	}
	// 直接构造 resolver（源与 manager 绑定一致）。
	direct := NewConfigResolver(m.LoadConfig, m.FlatSchema).ResolvePluginConfig("test")
	if !reflect.DeepEqual(direct, want) {
		t.Fatalf("direct resolver path mismatch:\n got %+v\nwant %+v", direct, want)
	}
}
