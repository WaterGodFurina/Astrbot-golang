package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInstallPythonPluginYAMLMetadata installs a Python plugin that ships
// metadata.yaml (no metadata.json, no language field) through InstallFromSource
// and verifies the language fallback + metadata parse + install flow.
func TestInstallPythonPluginYAMLMetadata(t *testing.T) {
	dataDir := t.TempDir()
	m := NewSubprocessManager(nil, dataDir)
	m.MaxRestarts = 2
	m.MinPort = 50300
	m.MaxPort = 50400
	t.Cleanup(m.Shutdown)

	inst, err := m.InstallFromSource(context.Background(), "yaml_demo", filepath.Join("testdata", "yaml_plugin"), InstallOptions{})
	if err != nil {
		t.Fatalf("InstallFromSource: %v", err)
	}
	if inst.Language != "python" {
		t.Fatalf("Language = %q, want python（metadata.yaml 无 language 字段应经 ResolveLanguage 推断）", inst.Language)
	}
	if inst.Name != "yaml_demo" {
		t.Fatalf("Name = %q", inst.Name)
	}
	// 元数据写入独立 metadata.json：config.json 只含真实配置（不含元数据键）。
	meta := m.readPluginMetadataFile(inst.ID)
	if meta == nil {
		t.Fatal("独立 metadata.json 未写入")
	}
	if meta["display_name"] != "YAML演示" {
		t.Fatalf("metadata.json 缺 display_name: %v", meta)
	}
	cfg := m.LoadConfig(inst.ID)
	if cfg == nil {
		t.Fatal("LoadConfig 返回 nil")
	}
	for _, k := range []string{"display_name", "short_desc", "desc", "version", "author", "repo", "cgo", "name"} {
		if _, exists := cfg[k]; exists {
			t.Fatalf("config.json 不得含元数据键 %q: %v", k, cfg)
		}
	}
	// 迁移语义：即使 config.json 被旧版本混入元数据，LoadConfig 也会剥离。
	if err := os.WriteFile(m.configPath(inst.ID), []byte(`{"display_name":"残留","real_key":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := m.LoadConfig(inst.ID); got["display_name"] != nil {
		t.Fatalf("残留元数据键必须被剥离: %v", got)
	}
	if got := m.LoadConfig(inst.ID); got["real_key"] != true {
		t.Fatalf("真实配置键必须保留: %v", got)
	}
	// Logo 缓存 + 列表 URL
	if logo := m.PluginLogoFile(inst.ID); logo == "" {
		t.Fatal("logo 未缓存")
	}
	url := m.pluginLogoURL(inst.ID)
	if url == "" || !strings.Contains(url, "plugins/logo") {
		t.Fatalf("logo URL 异常: %q", url)
	}
	// 命令已注册
	found := false
	for _, c := range inst.Meta.Commands {
		if c.Name == "yamlhello" {
			found = true
		}
	}
	if !found {
		t.Fatalf("yamlhello 命令未注册: %+v", inst.Meta.Commands)
	}
	// 卸载清理
	_ = m.Uninstall(inst.ID, true, true)
	if m.Get(inst.ID) != nil {
		t.Fatal("卸载后实例仍在")
	}
}
