package plugin

import (
	"context"
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
	// 配置目录里应写入 metadata 块（display_name/short_desc）
	cfg := m.LoadConfig(inst.Name)
	if cfg == nil {
		t.Fatal("LoadConfig 返回 nil")
	}
	if cfg["display_name"] != "YAML演示" {
		t.Fatalf("config 缺 display_name: %v", cfg)
	}
	// Logo 缓存 + 列表 URL
	if logo := m.PluginLogoFile(inst.Name); logo == "" {
		t.Fatal("logo 未缓存")
	}
	url := m.pluginLogoURL(inst.ID, inst.Name)
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
	m.Uninstall(inst.ID, true, true)
	if m.Get(inst.ID) != nil {
		t.Fatal("卸载后实例仍在")
	}
}
