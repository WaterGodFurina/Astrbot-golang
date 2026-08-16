package pipeline

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/plugin"
	"github.com/WaterGodFurina/Astrbot-golang/internal/star"
)

// TestPluginToolWakesIdlePlugin: 插件闲置休眠后，LLM 调用其函数工具必须按
// 工具名查注册表并 EnsureLoaded 唤醒插件，而不是返回"工具未找到"。
// 回归：executePluginTool 只遍历运行中实例（instances 表），休眠插件被
// 移出后其工具无人能分发，也不会被唤醒。
func TestPluginToolWakesIdlePlugin(t *testing.T) {
	bin := plugin.BuildTestPlugin()
	if bin == "" {
		t.Skip("test plugin unavailable (toolchain/SDK missing)")
	}
	m := plugin.NewSubprocessManager(nil, t.TempDir())
	ctx := context.Background()
	inst, err := m.InstallFromSource(ctx, "toolwake", filepath.Join("..", "plugin", "testdata", "plugin"), plugin.InstallOptions{})
	if err != nil {
		t.Fatalf("InstallFromSource: %v", err)
	}
	m.SetIdleUnload(10 * time.Millisecond)
	t.Cleanup(m.Shutdown)

	// Register 元数据工具先入注册表（echo_tool → toolwake）。
	if owner, ok := m.ToolOwner("echo_tool"); !ok || owner != inst.ID {
		t.Fatalf("echo_tool must be in the tool registry: owner=%q ok=%v", owner, ok)
	}

	// 模拟闲置：清扫 → 进程卸载，但注册表条目保留。
	time.Sleep(40 * time.Millisecond)
	m.SweepIdle()
	if m.Get(inst.ID) != nil {
		t.Fatal("idle plugin process must be unloaded")
	}
	if _, ok := m.ToolOwner("echo_tool"); !ok {
		t.Fatal("tool registry entry must survive idle sleep (for wake-on-call)")
	}

	s := &ProcessStage{subPlugins: m}
	event := &core.Event{
		Source: core.EventSource{Platform: "test", ConvID: "c1", SenderID: "u1"},
		MessageStr: "x",
	}
	text, handled := s.executePluginTool(event, "echo_tool", map[string]interface{}{"text": "hi"})
	if !handled {
		t.Fatal("sleeping plugin's tool call must be handled (wake + dispatch)")
	}
	if text != "tool:hi" {
		t.Errorf("unexpected tool result after wake: %q", text)
	}
	if m.Get(inst.ID) == nil {
		t.Fatal("plugin must be loaded again after tool-triggered wake")
	}
}

// TestPluginToolRegistryClearedOnUnload: 真实卸载（非休眠）后工具注册表
// 条目必须清除，避免向 LLM 注入已卸载插件的工具。
func TestPluginToolRegistryClearedOnUnload(t *testing.T) {
	bin := plugin.BuildTestPlugin()
	if bin == "" {
		t.Skip("test plugin unavailable (toolchain/SDK missing)")
	}
	m := plugin.NewSubprocessManager(nil, t.TempDir())
	ctx := context.Background()
	inst, err := m.InstallFromSource(ctx, "toolunload", filepath.Join("..", "plugin", "testdata", "plugin"), plugin.InstallOptions{})
	if err != nil {
		t.Fatalf("InstallFromSource: %v", err)
	}
	t.Cleanup(m.Shutdown)

	if _, ok := m.ToolOwner("echo_tool"); !ok {
		t.Fatal("echo_tool must be registered")
	}
	if err := m.Unload(inst.ID); err != nil {
		t.Fatalf("Unload: %v", err)
	}
	if _, ok := m.ToolOwner("echo_tool"); ok {
		t.Fatal("tool registry entry must be removed on real unload")
	}
	// 重载后工具重新注册。
	if _, err := m.LoadLang(ctx, inst.ID, inst.Binary, "go"); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := m.ToolOwner("echo_tool"); !ok {
		t.Fatal("echo_tool must be re-registered after reload")
	}
}

var _ = star.CoreEventToSDK
