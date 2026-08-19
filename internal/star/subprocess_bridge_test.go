package star

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/plugin"
	"github.com/WaterGodFurina/Astrbot-golang/internal/toolchain"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// TestMain cleans up the shared test-plugin binary (built on demand by
// plugin.BuildTestPlugin) so it is not left behind in /tmp after the run.
func TestMain(m *testing.M) {
	code := m.Run()
	plugin.CleanupTestPlugin()
	os.Exit(code)
}

// newTestSubprocessManager builds a subprocess manager with fast polling for
// tests, using a temp data dir.
func newTestSubprocessManager(t *testing.T) *plugin.SubprocessManager {
	t.Helper()
	return plugin.NewSubprocessManager(toolchain.New(), t.TempDir())
}

func bridgeTestEvent(msg string, admin bool) *core.Event {
	return &core.Event{
		Type:              core.EventMessage,
		Source:            core.EventSource{Platform: "test", ConvID: "c1", SenderID: "u1", SenderName: "alice", IsAdmin: admin, IsAtBot: true},
		Message:           message.PlainChain(msg),
		MessageStr:        msg,
		PlainText:         msg,
		IsAtOrWakeCommand: true,
	}
}

func runFilterHandlers(starMgr *Manager, ev *core.Event) {
	for _, h := range starMgr.Handlers().GetFilterHandlers() {
		if !h.Enabled {
			continue
		}
		fctx := &FilterContext{
			MessageStr:    ev.MessageStr,
			IsAtOrWake:    ev.IsAtOrWakeCommand,
			EventSenderID: ev.Source.SenderID,
			EventPlatform: ev.Source.Platform,
			EventRole:     ev.Role,
		}
		matched := false
		for _, filter := range h.EventFilters {
			if filter.Match(fctx) {
				matched = true
				break
			}
		}
		if matched {
			_ = h.Handler(ev)
		}
	}
}

// TestSubprocessPluginCommandInPipeline installs a real subprocess plugin and
// verifies its "test" command is bridged into the star pipeline and produces a
// reply on the event result.
func TestSubprocessPluginCommandInPipeline(t *testing.T) {
	bin := plugin.BuildTestPlugin()
	if bin == "" {
		t.Skip("test plugin unavailable (toolchain/SDK missing)")
	}
	m := newTestSubprocessManager(t)
	ctx := context.Background()
	inst, err := m.Load(ctx, "bridge", bin)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer func() { _ = m.Unload("bridge") }()

	starMgr := NewManagerSimple()
	RegisterSubprocessPlugin(starMgr, m, inst)

	ev := bridgeTestEvent("test hello", false)
	runFilterHandlers(starMgr, ev)

	if ev.Result == nil {
		t.Fatal("expected plugin command to set event.Result")
	}
	if got := ev.Result.GetPlainText(); got != "pong" {
		t.Errorf("unexpected reply: %q", got)
	}
}

// TestSubprocessPluginFilterInPipeline verifies a bridged filter stops the
// event when it returns false (admin blocked).
func TestSubprocessPluginFilterInPipeline(t *testing.T) {
	bin := plugin.BuildTestPlugin()
	if bin == "" {
		t.Skip("test plugin unavailable (toolchain/SDK missing)")
	}
	m := newTestSubprocessManager(t)
	ctx := context.Background()
	inst, err := m.Load(ctx, "bridge", bin)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer func() { _ = m.Unload("bridge") }()

	starMgr := NewManagerSimple()
	RegisterSubprocessPlugin(starMgr, m, inst)

	ev := bridgeTestEvent("test hello", true) // admin -> filter denies
	runFilterHandlers(starMgr, ev)
	if !ev.IsStopped() {
		t.Error("admin event should be stopped by the bridged filter")
	}

	ev2 := bridgeTestEvent("test hello", false) // non-admin -> allowed
	runFilterHandlers(starMgr, ev2)
	if ev2.IsStopped() {
		t.Error("non-admin event must not be stopped")
	}
	if ev2.Result == nil {
		t.Error("command should still have run for non-admin event")
	}
}

// TestRegisterSubprocessPluginsBatch verifies the batch registration helper
// and that re-registration after removal is idempotent.
func TestRegisterSubprocessPluginsBatch(t *testing.T) {
	bin := plugin.BuildTestPlugin()
	if bin == "" {
		t.Skip("test plugin unavailable (toolchain/SDK missing)")
	}
	m := newTestSubprocessManager(t)
	ctx := context.Background()
	inst, err := m.Load(ctx, "bridge", bin)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer func() { _ = m.Unload("bridge") }()

	starMgr := NewManagerSimple()
	RegisterSubprocessPlugins(starMgr, m, []*plugin.PluginInstance{inst})

	RemovePluginCommands(starMgr)
	RemovePluginFilters(starMgr)
	RemovePluginHooks(starMgr)
	if n := len(starMgr.Handlers().GetFilterHandlers()); n != 0 {
		t.Fatalf("expected 0 handlers after removal, got %d", n)
	}

	RegisterSubprocessPlugins(starMgr, m, []*plugin.PluginInstance{inst})
	// test plugin registers 2 commands + 1 explicit filter → 3 filter handlers.
	if n := len(starMgr.Handlers().GetFilterHandlers()); n != 3 {
		t.Fatalf("expected 3 filter handlers (2 commands+1 filter) after re-bridge, got %d", n)
	}
}

// TestIdleSleepKeepsHandlersAndWakesUp: 插件闲置休眠（UnloadIdle）后 star
// handler 必须保留；再次触发命令时 resolveActive 懒加载唤醒插件并正常回复。
// 回归：此前闲置卸载走 Unload 触发 RebridgePlugins 移除全部 handler，休眠后
// 命令再也无法触发，插件永远无法唤醒。
func TestIdleSleepKeepsHandlersAndWakesUp(t *testing.T) {
	bin := plugin.BuildTestPlugin()
	if bin == "" {
		t.Skip("test plugin unavailable (toolchain/SDK missing)")
	}
	m := newTestSubprocessManager(t)
	ctx := context.Background()
	// 用 InstallFromSource（写 manifest）——懒加载 EnsureLoaded 需要从
	// manifest 读取二进制路径。
	inst, err := m.InstallFromSource(ctx, "sleepy", filepath.Join("..", "plugin", "testdata", "plugin"), plugin.InstallOptions{})
	if err != nil {
		t.Fatalf("InstallFromSource: %v", err)
	}
	m.SetIdleUnload(10 * time.Millisecond)

	starMgr := NewManagerSimple()
	RegisterSubprocessPlugin(starMgr, m, inst)
	before := len(starMgr.Handlers().GetFilterHandlers())
	if before == 0 {
		t.Fatal("expected bridged handlers")
	}

	// 模拟闲置：真实等待超过阈值后触发一次清扫 → 进程被卸载，但 handler 保留。
	time.Sleep(40 * time.Millisecond)
	m.SweepIdle()
	if m.Get(inst.ID) != nil {
		t.Fatal("idle plugin process must be unloaded")
	}
	if got := len(starMgr.Handlers().GetFilterHandlers()); got != before {
		t.Fatalf("handlers must be kept after idle sleep: before=%d after=%d", before, got)
	}

	// 触发命令：懒加载自动唤醒并正常回复。
	ev := bridgeTestEvent("test hello", false)
	runFilterHandlers(starMgr, ev)
	if ev.Result == nil {
		t.Fatal("sleeping plugin's command must wake it up and produce a result")
	}
	if got := ev.Result.GetPlainText(); got != "pong" {
		t.Errorf("unexpected reply after wake: %q", got)
	}
	if m.Get(inst.ID) == nil {
		t.Fatal("plugin must be loaded again after wake")
	}
}

// TestPythonPluginCommandMatchesInPipeline: Python 插件命令经 star 管线匹配
// 并执行（回归：box 的 /盒 命令不命中直接走 LLM）。
func TestPythonPluginCommandMatchesInPipeline(t *testing.T) {
	m := newTestSubprocessManager(t)
	ctx := context.Background()
	inst, err := m.LoadLang(ctx, "pypipe", filepath.Join("..", "plugin", "testdata", "python_plugin"), "python")
	if err != nil {
		t.Fatalf("LoadLang: %v", err)
	}
	defer func() { _ = m.Unload("pypipe") }()

	starMgr := NewManagerSimple()
	RegisterSubprocessPlugin(starMgr, m, inst)

	ev := bridgeTestEvent("pyhello world", false)
	runFilterHandlers(starMgr, ev)
	if ev.Result == nil {
		t.Fatal("Python 插件命令必须被 star 管线匹配并设置 Result（当前直接走 LLM）")
	}
	if got := ev.Result.GetPlainText(); !strings.Contains(got, "Hello from Python") {
		t.Errorf("回复异常: %q", got)
	}

	// 用真实 box 插件验证中文命令 + 带参命令的匹配（回归：/盒 不命中走 LLM）。
	boxInst, err := m.LoadLang(ctx, "boxpipe", filepath.Join("..", "..", "data", "plugins-src", "astrbot_plugin_box"), "python")
	if err != nil {
		t.Skipf("box 插件不可用: %v", err)
	}
	defer func() { _ = m.Unload("boxpipe") }()
	starMgr2 := NewManagerSimple()
	RegisterSubprocessPlugin(starMgr2, m, boxInst)
	ev2 := bridgeTestEvent("盒 3442359407", false)
	runFilterHandlers(starMgr2, ev2)
	if ev2.Result == nil {
		t.Fatal("box 的中文命令 /盒 必须被匹配并设置 Result（当前直接走 LLM）")
	}
	t.Logf("box 回复: %.80s", ev2.Result.GetPlainText())
}
