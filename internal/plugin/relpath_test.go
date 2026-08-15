package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPythonPluginRelativeDataDir reproduces the production layout: the data
// dir is a RELATIVE path ("data") while the plugin subprocess runs with a
// different cwd. PYTHONPATH must still resolve (SDK + plugin dir absolute).
func TestPythonPluginRelativeDataDir(t *testing.T) {
	root := t.TempDir()
	pluginDir := pythonPluginTestdataAbs()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(oldWd) })

	m := NewSubprocessManager(nil, "data")
	m.MinPort = 50700
	m.MaxPort = 50800
	m.MaxRestarts = 2
	t.Cleanup(m.Shutdown)

	inst, err := m.LoadLang(context.Background(), "test_pyplugin", pluginDir, "python")
	if err != nil {
		t.Fatalf("LoadLang(相对 dataDir): %v", err)
	}
	if !strings.Contains(inst.Binary, filepath.ToSlash(filepath.Join(root, "data"))) {
		t.Logf("注意：inst.Binary = %s", inst.Binary)
	}
	found := false
	for _, c := range inst.Meta.Commands {
		if c.Name == "pyhello" {
			found = true
		}
	}
	if !found {
		t.Fatal("pyhello 未注册")
	}
	t.Log("相对 dataDir 加载成功")
}

func pythonPluginTestdataAbs() string {
	p, err := filepath.Abs("testdata/python_plugin")
	if err != nil {
		return "testdata/python_plugin"
	}
	return p
}
