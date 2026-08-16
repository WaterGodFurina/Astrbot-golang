package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestMigratePluginLayout 覆盖旧布局 → 新布局的一次性迁移：
//   - plugins-src/<id> → plugins/<id>（Python 源码本体）
//   - plugins_config/<name> → plugins_config/<id>（配置按实例 id 隔离）
//   - plugins/<name> 文档并入 plugins/<id>
//   - manifest Binary / ConfigDir / DocsDir 足迹更新
//   - legacy id 归一化：id → sanitizePluginName(name)_language（语言按入口文件）
func TestMigratePluginLayout(t *testing.T) {
	dataDir := t.TempDir()
	m := NewSubprocessManager(nil, dataDir)

	// 构造旧布局：Python 插件（legacy id=box，name=box）
	oldSrc := filepath.Join(dataDir, "plugins-src", "box")
	if err := os.MkdirAll(filepath.Join(oldSrc, "astrbot_plugin_box"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldSrc, "main.py"), []byte("print('hi')"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 旧配置目录 plugins_config/<name>
	oldCfg := filepath.Join(dataDir, "plugins_config", "box")
	if err := os.MkdirAll(oldCfg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldCfg, "config.json"), []byte(`{"enabled": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// 旧文档缓存 plugins/<name>
	oldDocs := filepath.Join(dataDir, "plugins", "box")
	if err := os.MkdirAll(oldDocs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDocs, "README.md"), []byte("# box"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 旧 manifest：Binary 指向 plugins-src，无 language 字段
	man := &Manifest{Version: 1, Plugins: []ManifestEntry{{
		ID: "box", Name: "box", Version: "1.0",
		Binary:    filepath.Join("data", "plugins-src", "box"),
		ConfigDir: filepath.Join("plugins_config", "box"),
		DocsDir:   filepath.Join("plugins", "box"),
		DataDir:   filepath.Join("plugins_data", "box"),
		Enabled:   true,
	}}}
	if err := man.Save(filepath.Join(dataDir, "plugins-manifest.json")); err != nil {
		t.Fatal(err)
	}

	m.migratePluginLayout()

	// 1) 源码迁移 + id 归一化（box → box_python，语言按 main.py 推断）
	if _, err := os.Stat(filepath.Join(dataDir, "plugins", "box_python", "main.py")); err != nil {
		t.Fatalf("Python 源码未迁移/归一化到 plugins/box_python: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "plugins-src", "box")); err == nil {
		t.Fatal("plugins-src 旧目录应已移除")
	}
	// 2) 配置迁移
	if _, err := os.Stat(filepath.Join(dataDir, "plugins_config", "box_python", "config.json")); err != nil {
		t.Fatalf("配置未迁移到 plugins_config/box_python: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "plugins_config", "box")); err == nil {
		t.Fatal("旧 name 配置目录应已移除")
	}
	// 3) 文档并入
	if _, err := os.Stat(filepath.Join(dataDir, "plugins", "box_python", "README.md")); err != nil {
		t.Fatalf("文档未并入 plugins/box_python: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "plugins", "box")); err == nil {
		t.Fatal("旧文档目录应已移除")
	}
	// 4) manifest 足迹
	saved, err := LoadManifest(filepath.Join(dataDir, "plugins-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	e := saved.Get("box_python")
	if e == nil {
		t.Fatal("manifest 条目丢失（id 应归一化为 box_python）")
	}
	if e.Language != "python" {
		t.Errorf("language 未补全: %q", e.Language)
	}
	if e.Binary != filepath.Join("data", "plugins", "box_python") {
		t.Errorf("Binary 未更新: %q", e.Binary)
	}
	if e.ConfigDir != filepath.Join("plugins_config", "box_python") || e.DocsDir != filepath.Join("plugins", "box_python") {
		t.Errorf("目录足迹未更新: %+v", e)
	}
	// 5) 配置读取走新路径（幂等）
	cfg := m.LoadConfig("box_python")
	if cfg["enabled"] != true {
		t.Errorf("迁移后配置不可读: %+v", cfg)
	}
}

// TestMigratePluginLayoutIdempotent：二次迁移无副作用（新布局原样保留）。
func TestMigratePluginLayoutIdempotent(t *testing.T) {
	dataDir := t.TempDir()
	m := NewSubprocessManager(nil, dataDir)
	if err := os.MkdirAll(filepath.Join(dataDir, "plugins", "x_go"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "plugins", "x_go", "README.md"), []byte("# x"), 0o644); err != nil {
		t.Fatal(err)
	}
	man := &Manifest{Version: 1, Plugins: []ManifestEntry{{
		ID: "x_go", Name: "x", Version: "1.0", Language: "go",
		Binary:    filepath.Join("data", "plugins-bin", "x_go", "x-linux-amd64"),
		ConfigDir: filepath.Join("plugins_config", "x_go"),
		DocsDir:   filepath.Join("plugins", "x_go"),
		DataDir:   filepath.Join("plugins_data", "x_go"),
		Enabled:   true,
	}}}
	if err := man.Save(filepath.Join(dataDir, "plugins-manifest.json")); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(man)
	m.migratePluginLayout()
	after, _ := json.Marshal(man)
	if string(before) != string(after) {
		t.Errorf("二次迁移不应修改 manifest:\n before %s\n after  %s", before, after)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "plugins", "x_go", "README.md")); err != nil {
		t.Fatalf("新布局文件被误删: %v", err)
	}
}
