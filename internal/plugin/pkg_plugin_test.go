package plugin

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestPythonPluginPackageRelativeImport installs a package-style Python plugin
// (directory __init__.py + main.py with relative imports) and verifies the
// loader handles the Python-AstrBot convention.
func TestPythonPluginPackageRelativeImport(t *testing.T) {
	dataDir := t.TempDir()
	m := NewSubprocessManager(nil, dataDir)
	m.MaxRestarts = 2
	m.MinPort = 50500
	m.MaxPort = 50600
	t.Cleanup(m.Shutdown)

	inst, err := m.InstallFromSource(
		context.Background(), "pkg_plugin",
		filepath.Join("testdata", "pkg_plugin"), InstallOptions{},
	)
	if err != nil {
		t.Fatalf("InstallFromSource: %v", err)
	}
	if inst.Language != "python" {
		t.Fatalf("Language = %q", inst.Language)
	}
	// logo 缓存（metadata.yaml 无 logo_path，用根目录 logo.png 惯例）
	if m.PluginLogoFile(inst.ID) == "" {
		t.Fatal("logo 未缓存")
	}
	// 相对导入的模块正常：命令执行返回 backend.util.greet()
	ev := map[string]any{
		"type": "message", "platform": "qq_official", "sender_id": "1",
		"conv_id": "c", "is_group": false, "is_at_bot": true,
		"message_str": "pkghello", "plain_text": "pkghello", "timestamp": 0,
	}
	_, chain, _, err := inst.Client.HandleCommand(context.Background(), "pkghello", nil, sdkEvent(t, ev))
	if err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	if len(chain) == 0 || !strings.Contains(chain[0].Text, "pkg relative import ok") {
		t.Fatalf("相对导入结果异常: %v", chain)
	}
	_ = m.Uninstall(inst.ID, true, true)
}
