package plugin

import (
	"encoding/json"
	"testing"

	sdkv1 "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/gen/sdkv1"
)

// TestNormalizeSchemaAndDefaults verifies FlatSchema normalizes the
// {"type":"object","properties":{...}} layout (with nested object groups) into
// the WebUI "items" shape, and LoadConfigWithDefaults fills nested defaults.
func TestNormalizeSchemaAndDefaults(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"enabled": map[string]interface{}{
				"type": "boolean", "description": "on/off", "default": true,
			},
			"group": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"admins": map[string]interface{}{
						"type": "array", "default": []interface{}{"1"},
						"items": map[string]interface{}{"type": "string"},
					},
				},
			},
		},
	}

	flat := normalizeSchema(flatProps(schema))
	items := flat["group"].(map[string]interface{})
	if _, ok := items["items"]; !ok {
		t.Fatalf("group not normalized to items: %+v", items)
	}
	admins := items["items"].(map[string]interface{})["admins"].(map[string]interface{})
	if _, ok := admins["items"]; !ok {
		t.Fatalf("array element schema clobbered: %+v", admins)
	}

	cfg := map[string]interface{}{}
	mergeSchemaDefaults(cfg, flat)
	if cfg["enabled"] != true {
		t.Fatalf("top-level default not applied: %+v", cfg)
	}
	g, ok := cfg["group"].(map[string]interface{})
	if !ok {
		t.Fatalf("group defaults not applied: %+v", cfg)
	}
	if len(g["admins"].([]interface{})) != 1 {
		t.Fatalf("nested default not applied: %+v", g)
	}
}

func flatProps(schema map[string]interface{}) map[string]interface{} {
	props, _ := schema["properties"].(map[string]interface{})
	return props
}

// TestLoadConfigWithDefaults uses a real SubprocessManager against a temp dir
// with a fake plugin instance carrying a ConfigSchemaJson.
func TestLoadConfigWithDefaults(t *testing.T) {
	m := NewSubprocessManager(nil, t.TempDir())
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"foo": map[string]interface{}{"type": "string", "default": "bar"},
		},
	}
	raw, _ := json.Marshal(schema)
	m.instances["test"] = &PluginInstance{
		Meta: &sdkv1.RegisterResponse{ConfigSchemaJson: raw},
	}

	cfg := m.LoadConfigWithDefaults("test")
	if cfg["foo"] != "bar" {
		t.Fatalf("default not merged: %+v", cfg)
	}
	cfg2 := m.LoadConfig("test")
	if len(cfg2) != 0 {
		t.Fatalf("LoadConfig must not inject defaults: %+v", cfg2)
	}
}
