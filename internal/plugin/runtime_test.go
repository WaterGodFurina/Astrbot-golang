package plugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
	"github.com/AstrBotDevs/AstrBot/internal/toolchain"
)

// testPluginBin is built once in TestMain by compiling testdata/plugin with
// the host Go toolchain against the SDK module (resolved from go.mod).
var testPluginBin string

func TestMain(m *testing.M) {
	testPluginBin = BuildTestPlugin()
	code := m.Run()
	// Give managed child processes a moment to be reaped by go-plugin.
	time.Sleep(300 * time.Millisecond)
	os.Exit(code)
}

// requirePlugin skips the test when the test plugin binary is unavailable.
func requirePlugin(t *testing.T) {
	t.Helper()
	if testPluginBin == "" {
		t.Skip("test plugin binary unavailable")
	}
}

func newTestManager(t *testing.T) *SubprocessManager {
	m := NewSubprocessManager(toolchain.New(), t.TempDir())
	// Fast backoff + polling for tests.
	m.RestartBaseDelay = 100 * time.Millisecond
	m.PollInterval = 50 * time.Millisecond
	return m
}

func waitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

func pluginReply(t *testing.T, m *SubprocessManager, id string) string {
	t.Helper()
	inst := m.Get(id)
	if inst == nil {
		t.Fatalf("plugin %s not loaded", id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	text, err := inst.Client.HandleCommand(ctx, "test", nil, &pluginsdk.Event{})
	if err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	return text
}

func TestLoadUnload(t *testing.T) {
	requirePlugin(t)
	m := newTestManager(t)
	ctx := context.Background()

	inst, err := m.Load(ctx, "test", testPluginBin)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if inst.Name != "testplugin" || inst.Version != "1.0.0" {
		t.Errorf("metadata mismatch: %+v", inst)
	}
	if m.Get("test") == nil {
		t.Fatal("plugin not registered")
	}
	if got := pluginReply(t, m, "test"); got != "pong" {
		t.Errorf("unexpected reply: %q", got)
	}

	if err := m.Unload("test"); err != nil {
		t.Fatalf("Unload: %v", err)
	}
	if m.Get("test") != nil {
		t.Fatal("plugin still registered after unload")
	}
	waitFor(t, 2*time.Second, "process exit", func() bool {
		return m.Get("test") == nil
	})
}

func TestLoadIdempotent(t *testing.T) {
	requirePlugin(t)
	m := newTestManager(t)
	ctx := context.Background()

	a, err := m.Load(ctx, "test", testPluginBin)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b, err := m.Load(ctx, "test", testPluginBin)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if a != b {
		t.Error("second Load should return the existing instance")
	}
	if err := m.Unload("test"); err != nil {
		t.Fatal(err)
	}
}

func TestLoadInstalledFromManifest(t *testing.T) {
	requirePlugin(t)
	dir := t.TempDir()
	ctx := context.Background()

	m1 := NewSubprocessManager(toolchain.New(), dir)
	inst, err := m1.InstallFromSource(ctx, "persist", filepath.Join("testdata", "plugin"), InstallOptions{})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := m1.Unload("persist"); err != nil {
		t.Fatal(err)
	}
	_ = inst

	// A fresh manager over the same data dir must reload the plugin from the
	// persisted manifest + cached artifact.
	m2 := NewSubprocessManager(toolchain.New(), dir)
	m2.LoadInstalled(ctx)
	if m2.Get("persist") == nil {
		t.Fatal("plugin not reloaded from manifest on a fresh manager")
	}
	if got := pluginReply(t, m2, "persist"); got != "pong" {
		t.Errorf("reloaded plugin reply: %q", got)
	}
	if err := m2.Unload("persist"); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMissingBinary(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.Load(context.Background(), "x", filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestReloadZeroDowntime(t *testing.T) {
	requirePlugin(t)
	m := newTestManager(t)
	ctx := context.Background()

	old, err := m.Load(ctx, "test", testPluginBin)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := pluginReply(t, m, "test"); got != "pong" {
		t.Fatalf("pre-reload reply: %q", got)
	}

	if err := m.Reload(ctx, "test"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	cur := m.Get("test")
	if cur == nil {
		t.Fatal("plugin missing after reload")
	}
	if cur == old {
		t.Fatal("reload should have replaced the instance")
	}
	if got := pluginReply(t, m, "test"); got != "pong" {
		t.Errorf("post-reload reply: %q", got)
	}
	if err := m.Unload("test"); err != nil {
		t.Fatal(err)
	}
}

func TestCrashRestart(t *testing.T) {
	requirePlugin(t)
	t.Setenv("TEST_PLUGIN_CRASH_AFTER", "1500")
	m := newTestManager(t)
	ctx := context.Background()

	first, err := m.Load(ctx, "test", testPluginBin)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Crash after ~1.5s -> backoff -> a NEW instance takes over.
	var restarted *PluginInstance
	waitFor(t, 8*time.Second, "crash restart", func() bool {
		restarted = m.Get("test")
		return restarted != nil && restarted != first
	})
	if restarted.StartedAt.Equal(first.StartedAt) {
		t.Error("restarted instance should have a new start time")
	}
	if got := pluginReply(t, m, "test"); got != "pong" {
		t.Errorf("restarted plugin reply: %q", got)
	}

	// Stop before the restarted instance crashes again.
	if err := m.Unload("test"); err != nil {
		t.Fatal(err)
	}
}

func TestCrashMaxRestarts(t *testing.T) {
	requirePlugin(t)
	t.Setenv("TEST_PLUGIN_CRASH_AFTER", "800")
	m := newTestManager(t)
	m.MaxRestarts = 2
	ctx := context.Background()

	if _, err := m.Load(ctx, "test", testPluginBin); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// With MaxRestarts=2 the plugin is allowed two automatic restarts, then
	// the third crash marks it failed and removes it.
	waitFor(t, 12*time.Second, "max-restart failure", func() bool {
		return m.Get("test") == nil && len(m.Failed()) > 0
	})
	if _, ok := m.Failed()["test"]; !ok {
		t.Errorf("plugin should be recorded as failed: %v", m.Failed())
	}
}

func TestCrashAutoRestartDisabled(t *testing.T) {
	requirePlugin(t)
	t.Setenv("TEST_PLUGIN_CRASH_AFTER", "800")
	m := newTestManager(t)
	m.SetAutoRestart(false)
	ctx := context.Background()

	if _, err := m.Load(ctx, "test", testPluginBin); err != nil {
		t.Fatalf("Load: %v", err)
	}
	waitFor(t, 8*time.Second, "fail without restart", func() bool {
		return m.Get("test") == nil
	})
	if _, ok := m.Failed()["test"]; !ok {
		t.Errorf("plugin should be recorded as failed: %v", m.Failed())
	}
}

func TestInstallFromLocalDir(t *testing.T) {
	requirePlugin(t)
	m := newTestManager(t)
	ctx := context.Background()

	src := filepath.Join("testdata", "plugin")
	inst, err := m.InstallFromSource(ctx, "installed", src, InstallOptions{})
	if err != nil {
		t.Fatalf("InstallFromSource: %v", err)
	}
	if inst.Name != "testplugin" || inst.Version != "1.0.0" {
		t.Errorf("metadata mismatch: %+v", inst)
	}
	if got := pluginReply(t, m, "installed"); got != "pong" {
		t.Errorf("installed plugin reply: %q", got)
	}

	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	entry := man.Get("installed")
	if entry == nil || !entry.Enabled {
		t.Fatalf("install not persisted in manifest: %+v", man.Plugins)
	}
	if entry.Binary == "" {
		t.Error("manifest entry missing binary path")
	}

	if err := m.Unload("installed"); err != nil {
		t.Fatal(err)
	}
}

func TestInstallReportsRisk(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()

	src := t.TempDir()
	srcFile := `package main

import (
	"os/exec"
	"syscall"

	sdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
)

func main() { sdk.Serve(&sdk.Plugin{Name: "evil"}) }

var _ = exec.Command
var _ = syscall.Syscall
`
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(srcFile), 0o644); err != nil {
		t.Fatal(err)
	}

	// Without IgnoreRisk the install is blocked with the offending locations.
	_, err := m.InstallFromSource(ctx, "evil", src, InstallOptions{})
	if err == nil {
		t.Fatal("expected install to be blocked by the static scan")
	}
	var riskErr *RiskError
	if !errors.As(err, &riskErr) {
		t.Fatalf("expected *RiskError, got %T: %v", err, err)
	}
	if len(riskErr.Findings) != 2 {
		t.Fatalf("expected 2 findings, got %d: %+v", len(riskErr.Findings), riskErr.Findings)
	}
	if m.Get("evil") != nil {
		t.Fatal("blocked plugin must not be loaded")
	}
	for _, f := range riskErr.Findings {
		if f.File == "" || f.Line <= 0 {
			t.Errorf("finding missing file/line: %+v", f)
		}
	}
	if riskErr.Findings[0].Import != "os/exec" || riskErr.Findings[0].Line != 4 {
		t.Errorf("unexpected first finding: %+v", riskErr.Findings[0])
	}
	if !strings.Contains(riskErr.Findings[0].Snippet, "os/exec") {
		t.Errorf("snippet should contain the import: %+v", riskErr.Findings[0])
	}

	// With IgnoreRisk the same plugin installs and loads.
	inst, err := m.InstallFromSource(ctx, "evil", src, InstallOptions{IgnoreRisk: true})
	if err != nil {
		t.Fatalf("IgnoreRisk install: %v", err)
	}
	if inst == nil || inst.Name != "evil" {
		t.Fatalf("expected loaded plugin, got %+v", inst)
	}
	if err := m.Unload("evil"); err != nil {
		t.Fatal(err)
	}
}
