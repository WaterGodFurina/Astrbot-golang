package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/plugin"
)

// TestPluginConfigPayloadResolved verifies the dashboard plugin-config GET
// payload ("config" field) comes from the same unified ConfigResolver entry
// as the HostService/direct paths: schema defaults are merged under the
// stored config.
func TestPluginConfigPayloadResolved(t *testing.T) {
	dataDir := t.TempDir()
	m := plugin.NewSubprocessManager(nil, dataDir)

	// 磁盘布局：manifest 条目（让 resolveSubprocessPlugin 命中）+ schema 缓存
	// + 预置 config.json（含元数据键与真实键）。
	man := &plugin.Manifest{Version: 1, Plugins: []plugin.ManifestEntry{{
		ID: "test", Name: "test", Version: "1.0", Binary: "/tmp/x", Enabled: true, Language: "go",
	}}}
	if err := man.Save(filepath.Join(dataDir, "plugins-manifest.json")); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"enabled":  map[string]interface{}{"type": "boolean", "default": true},
			"greeting": map[string]interface{}{"type": "string", "default": "你好"},
			"group": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"rate": map[string]interface{}{"type": "number", "default": 0.5},
				},
			},
		},
	}
	raw, _ := json.Marshal(schema)
	cfgDir := filepath.Join(dataDir, "plugins_config", "test")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config_schema.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"enabled": false, "display_name": "Legacy"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{subPluginMgr: m}
	payload := s.pluginConfigPayload("test")
	cfg, ok := payload["config"].(map[string]interface{})
	if !ok {
		t.Fatalf("payload.config missing: %+v", payload)
	}

	want := m.ConfigResolver().ResolvePluginConfig("test")
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("dashboard payload config mismatch:\n got %+v\nwant %+v", cfg, want)
	}
	if cfg["enabled"] != false {
		t.Fatalf("stored value must win: %+v", cfg)
	}
	if cfg["greeting"] != "你好" {
		t.Fatalf("schema default must be merged: %+v", cfg)
	}
	if _, ok := cfg["group"].(map[string]interface{}); !ok {
		t.Fatalf("nested group default must be merged: %+v", cfg)
	}
	if _, ok := cfg["display_name"]; ok {
		t.Fatalf("metadata key leaked into payload: %+v", cfg)
	}
	// metadata.items 仍以 FlatSchema 呈现（WebUI 渲染用）。
	meta, ok := payload["metadata"].(map[string]interface{})
	if !ok {
		t.Fatalf("payload.metadata missing: %+v", payload)
	}
	entry, ok := meta["test"].(map[string]interface{})
	if !ok || entry["items"] == nil {
		t.Fatalf("payload.metadata.test.items missing: %+v", meta)
	}
}
