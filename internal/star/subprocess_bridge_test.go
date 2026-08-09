package star

import (
	"context"
	"testing"

	"github.com/AstrBotDevs/AstrBot/internal/core"
	"github.com/AstrBotDevs/AstrBot/internal/plugin"
	"github.com/AstrBotDevs/AstrBot/internal/toolchain"
	"github.com/AstrBotDevs/AstrBot/pkg/message"
)

// newTestSubprocessManager builds a subprocess manager with fast polling for
// tests, using a temp data dir.
func newTestSubprocessManager(t *testing.T) *plugin.SubprocessManager {
	t.Helper()
	return plugin.NewSubprocessManager(toolchain.New(), t.TempDir())
}

func bridgeTestEvent(msg string, admin bool) *core.Event {
	return &core.Event{
		Type:       core.EventMessage,
		Source:     core.EventSource{Platform: "test", ConvID: "c1", SenderID: "u1", SenderName: "alice", IsAdmin: admin},
		Message:    message.PlainChain(msg),
		MessageStr: msg,
		PlainText:  msg,
	}
}

func runFilterHandlers(starMgr *Manager, ev *core.Event) {
	for _, h := range starMgr.Handlers().GetFilterHandlers() {
		_ = h.Handler(ev)
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
	RegisterSubprocessPlugin(starMgr, inst)

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
	RegisterSubprocessPlugin(starMgr, inst)

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
	RegisterSubprocessPlugins(starMgr, []*plugin.PluginInstance{inst})

	RemovePluginCommands(starMgr)
	RemovePluginFilters(starMgr)
	RemovePluginHooks(starMgr)
	if n := len(starMgr.Handlers().GetFilterHandlers()); n != 0 {
		t.Fatalf("expected 0 handlers after removal, got %d", n)
	}

	RegisterSubprocessPlugins(starMgr, []*plugin.PluginInstance{inst})
	if n := len(starMgr.Handlers().GetFilterHandlers()); n != 2 {
		t.Fatalf("expected 2 filter handlers (command+filter) after re-bridge, got %d", n)
	}
}
