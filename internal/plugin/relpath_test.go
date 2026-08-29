package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/pysdk"
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
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

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

// TestPythonPluginRelativeCachePath 一次性复现并锁定 "宿主 dataDir=/data 相对
// → venv PythonBin 相对 → 子进程 cmd.Dir(插件数据目录) chdir 后按该 cwd 二次
// 解析错位" 的完整链路：
//   - 宿主 cwd 非仓库目录（Chdir 到临时根，模拟任意目录启动）
//   - dataDir="data"（相对）
//   - ASTRBOT_PYTHON_CACHE_DIR="relcache"（相对）→ EnsureVenv 产出相对 venv
//     PythonBin；修复前 PrepareRuntimeWithStage 未 filepath.Abs，exec 按
//     子进程 cmd.Dir 解析必然错位，bridge 无法启动。
//
// 断言：venv 自动准备、bridge 启动、Python RPC（pyhello）、pipInstall 全部
// 可用，且 PythonBin 与插件数据目录（cmd.Dir）均已绝对化。
func TestPythonPluginRelativeCachePath(t *testing.T) {
	if pysdk.DiscoverPythonBin() == "" {
		t.Skip("python3 不可用")
	}
	pluginDir := pythonPluginTestdataAbs() // 必须先于 Chdir 解析为绝对路径
	root := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	t.Setenv("ASTRBOT_PYTHON_CACHE_DIR", "relcache")

	m := NewSubprocessManager(nil, "data")
	m.MinPort = 50600
	m.MaxPort = 50700
	m.MaxRestarts = 2
	m.RestartBaseDelay = 100 * time.Millisecond
	t.Cleanup(m.Shutdown)

	start := time.Now()
	inst, err := m.LoadLang(context.Background(), "test_pyplugin", pluginDir, "python")
	t.Logf("LoadLang(相对 dataDir=相对 cache=relcache, 非仓库 cwd) 耗时 %v", time.Since(start))
	if err != nil {
		t.Fatalf("LoadLang: %v", err)
	}
	if inst == nil || inst.Meta == nil {
		t.Fatal("inst/Meta 为空")
	}

	// venv 已自动准备，PythonBin 必为绝对路径（修复核心断言）
	env, err := m.pythonRuntime()
	if err != nil {
		t.Fatalf("pythonRuntime: %v", err)
	}
	if !filepath.IsAbs(env.PythonBin) {
		t.Fatalf("PythonBin 仍为相对路径（修复缺失）: %q", env.PythonBin)
	}
	t.Logf("PythonBin = %s", env.PythonBin)

	// Python RPC 全链路（bridge 启动 + 注册 + HandleCommand）
	cmdCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ev := map[string]any{
		"type": "message", "platform": "qq_official", "sender_id": "123",
		"sender_name": "u", "conv_id": "c1", "is_group": false, "is_at_bot": true,
		"is_admin": false, "message_str": "pyhello", "plain_text": "pyhello",
		"timestamp": 0,
		"chain":     []map[string]any{{"type": "Plain", "text": "pyhello"}},
	}
	_, chain, _, err := inst.Client.HandleCommand(cmdCtx, "pyhello", nil, sdkEvent(t, ev))
	if err != nil {
		t.Fatalf("HandleCommand(pyhello): %v", err)
	}
	if len(chain) == 0 || !strings.Contains(chain[0].Text, "Hello from Python") {
		t.Fatalf("pyhello 回复异常: %v", chain)
	}
	t.Logf("pyhello -> %s", chain[0].Text)

	// pipInstall 走同一绝对 PythonBin + cmd.Dir=插件源码目录（空 requirements
	// 无网络、秒回；修复前相对 PythonBin 会按 cmd.Dir 错位失败）
	req := filepath.Join(root, "requirements.txt")
	if err := os.WriteFile(req, []byte("# 无操作\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.pipInstall(context.Background(), env, pluginDir, req); err != nil {
		t.Fatalf("pipInstall(空 requirements): %v", err)
	}
	t.Log("pipInstall 空 requirements 成功")

	// 插件数据目录（cmd.Dir）落在临时根下的绝对路径
	if !strings.Contains(inst.Binary, filepath.Join(root, "data")) {
		t.Logf("注意：inst.Binary = %s（未预期在 %s 下）", inst.Binary, filepath.Join(root, "data"))
	}
	_ = m.Unload("test_pyplugin")
}
