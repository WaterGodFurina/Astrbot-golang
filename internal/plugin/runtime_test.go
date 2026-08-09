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
	text, _, err := inst.Client.HandleCommand(ctx, "test", nil, &pluginsdk.Event{})
	if err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	return text
}

// TestNewSDKCapabilities exercises the extended SDK surface end-to-end across a
// real subprocess: LLM tools, on_llm_request hooks, and result hooks.
func TestNewSDKCapabilities(t *testing.T) {
	requirePlugin(t)
	m := newTestManager(t)
	id := "test_plugin"
	if _, err := m.Load(context.Background(), id, testPluginBin); err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer m.Unload(id)

	inst := m.Get(id)
	if inst == nil || inst.Meta == nil {
		t.Fatalf("plugin not loaded")
	}

	// Tools are reported in Register metadata.
	foundTool := false
	for _, td := range inst.Meta.Tools {
		if td.Name == "echo_tool" {
			foundTool = true
			break
		}
	}
	if !foundTool {
		t.Fatalf("echo_tool not reported in Register metadata: %+v", inst.Meta.Tools)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// LLM tool execution.
	text, isErr, err := inst.Client.HandleTool(ctx, "echo_tool", map[string]any{"text": "hi"}, &pluginsdk.Event{})
	if err != nil || isErr {
		t.Fatalf("HandleTool: err=%v isErr=%v", err, isErr)
	}
	if text != "tool:hi" {
		t.Fatalf("HandleTool result = %q, want %q", text, "tool:hi")
	}

	// on_llm_request hook: modifies the system prompt.
	sp, stop, err := inst.Client.HandleLLMRequest(ctx, "inject", &pluginsdk.Event{}, "base", "user")
	if err != nil {
		t.Fatalf("HandleLLMRequest: %v", err)
	}
	if stop {
		t.Fatalf("unexpected stop")
	}
	if !strings.Contains(sp, "[injected]") {
		t.Fatalf("system prompt not injected: %q", sp)
	}

	// Result hook: decorates the outgoing chain.
	chain := []pluginsdk.Component{pluginsdk.Text("hi")}
	newChain, stop, err := inst.Client.HandleHook(ctx, "decorate", &pluginsdk.Event{}, chain)
	if err != nil {
		t.Fatalf("HandleHook(decorate): %v", err)
	}
	if stop {
		t.Fatalf("unexpected stop")
	}
	if len(newChain) != 2 || newChain[1].Text != "[decorated]" {
		t.Fatalf("decorated chain mismatch: %+v", newChain)
	}
}

// TestHostServiceReverseCalls exercises the bidirectional HostService across a
// real subprocess: the plugin calls ChatLLM / SetConfig / GetConfig back into
// the host over the go-plugin broker.
func TestHostServiceReverseCalls(t *testing.T) {
	requirePlugin(t)

	// Install fake host hooks before launching the plugin client.
	pluginsdk.SetHostHooks(pluginsdk.HostServiceHooks{
		ChatLLM: func(prompt, systemPrompt string) (string, error) {
			return "echo:" + prompt, nil
		},
		SetConfig: func(pluginName string, cfg map[string]any) error {
			saved[pluginName] = cfg
			return nil
		},
		GetConfig: func(pluginName string) (map[string]any, error) {
			if cfg, ok := saved[pluginName]; ok {
				return cfg, nil
			}
			return map[string]any{}, nil
		},
	})
	defer pluginsdk.SetHostHooks(pluginsdk.HostServiceHooks{})

	m := newTestManager(t)
	id := "test_plugin"
	if _, err := m.Load(context.Background(), id, testPluginBin); err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer m.Unload(id)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	inst := m.Get(id)
	text, _, err := inst.Client.HandleCommand(ctx, "hosttest", nil, &pluginsdk.Event{})
	if err != nil {
		t.Fatalf("HandleCommand(hosttest): %v", err)
	}
	if text != "llm=echo:ping cfg=v" {
		t.Fatalf("hosttest result = %q, want %q", text, "llm=echo:ping cfg=v")
	}
}

// saved is shared state backing the fake SetConfig/GetConfig hooks in
// TestHostServiceReverseCalls.
var saved = map[string]map[string]any{}

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

func TestStaticScanDetectsDirectives(t *testing.T) {
	dir := t.TempDir()
	src := `package main

import (
	"reflect"

	sdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
)

//go:linkname getpid syscall.Getpid

//go:generate echo "hacked"

func main() { _ = reflect.ValueOf }
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := StaticScan(dir)
	if err != nil {
		t.Fatalf("StaticScan: %v", err)
	}
	var imports, directives []string
	for _, f := range findings {
		switch f.Import {
		case "os/exec", "syscall", "unsafe", "reflect":
			imports = append(imports, f.Import)
		default:
			directives = append(directives, f.Import)
		}
	}
	if !containsStr(imports, "reflect") {
		t.Errorf("reflect import should be flagged, got %v", imports)
	}
	if !containsStr(directives, "go:linkname") || !containsStr(directives, "go:generate") {
		t.Errorf("linkname/go:generate should be flagged, got %v", directives)
	}
}

func TestInstallSourceMetadata(t *testing.T) {
	requirePlugin(t)
	m := newTestManager(t)
	ctx := context.Background()

	src := filepath.Join("testdata", "plugin")
	inst, err := m.InstallFromSource(ctx, "meta", src, InstallOptions{
		InstallMethod:  "market",
		RegistryURL:    "https://astrbotgomarket.350430.xyz/package.json",
		RegistryName:   "AstrBot-Go 插件市场",
		MarketPluginID: "WaterGodFurina/echo",
		Repo:           "https://github.com/WaterGodFurina/Astrbot-golang",
	})
	if err != nil {
		t.Fatalf("InstallFromSource: %v", err)
	}
	_ = inst

	list := m.ListInfo()
	var found map[string]interface{}
	for _, p := range list {
		if name, _ := p["name"].(string); name == "testplugin" {
			found = p
			break
		}
	}
	if found == nil {
		t.Fatalf("installed plugin missing from ListInfo: %v", list)
	}
	if repo, _ := found["repo"].(string); repo != "https://github.com/WaterGodFurina/Astrbot-golang" {
		t.Errorf("repo not exposed: %v", found["repo"])
	}
	srcMap, ok := found["install_source"].(map[string]interface{})
	if !ok {
		t.Fatalf("install_source missing from ListInfo: %v", found)
	}
	if srcMap["install_method"] != "market" || srcMap["market_plugin_id"] != "WaterGodFurina/echo" {
		t.Errorf("install_source fields mismatch: %v", srcMap)
	}
	if up, _ := found["updates_enabled"].(bool); !up {
		t.Errorf("updates_enabled should be true for market installs")
	}
	if mn, _ := found["marketplace_name"].(string); mn != "testplugin" {
		t.Errorf("marketplace_name mismatch: %q", mn)
	}

	if err := m.Unload("meta"); err != nil {
		t.Fatal(err)
	}
}

func TestListInfoLegacySourceFallback(t *testing.T) {
	m := newTestManager(t)
	man := &Manifest{Version: 1}
	man.Upsert(ManifestEntry{
		ID:      "legacy",
		Name:    "legacy_plugin",
		Source:  "https://github.com/Owner/legacy",
		Binary:  "/tmp/nonexistent",
		Enabled: false,
	})
	if err := man.Save(m.manifestPath()); err != nil {
		t.Fatal(err)
	}

	list := m.ListInfo()
	var found map[string]interface{}
	for _, p := range list {
		if name, _ := p["name"].(string); name == "legacy_plugin" {
			found = p
			break
		}
	}
	if found == nil {
		t.Fatalf("legacy entry missing from ListInfo: %v", list)
	}
	if repo, _ := found["repo"].(string); repo != "https://github.com/Owner/legacy" {
		t.Errorf("legacy repo should fall back to source, got: %v", found["repo"])
	}
	srcMap, ok := found["install_source"].(map[string]interface{})
	if !ok {
		t.Fatalf("install_source missing: %v", found)
	}
	if srcMap["install_method"] != "repository" {
		t.Errorf("legacy URL source should be presented as repository, got: %v", srcMap["install_method"])
	}
	if up, _ := found["updates_enabled"].(bool); !up {
		t.Errorf("legacy entry should be updatable")
	}
}

func TestReadmeCachedAtInstall(t *testing.T) {
	requirePlugin(t)
	m := newTestManager(t)
	ctx := context.Background()

	src := t.TempDir()
	srcMain := `package main

import (
	sdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
)

func main() { sdk.Serve(&sdk.Plugin{Name: "docplugin"}) }
`
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(srcMain), 0o644); err != nil {
		t.Fatal(err)
	}
	readme := "# Doc Plugin\n\nDocumentation here.\n"
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte(readme), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := m.InstallFromSource(ctx, "docplugin", src, InstallOptions{}); err != nil {
		t.Fatalf("InstallFromSource: %v", err)
	}
	if got := m.Readme("docplugin"); got != readme {
		t.Errorf("Readme should read cached README, got %q", got)
	}
	if err := m.Unload("docplugin"); err != nil {
		t.Fatal(err)
	}
}

func TestRawRepoDocURLs(t *testing.T) {
	urls := rawRepoDocURLs("https://github.com/Owner/Repo.git", []string{"README.md"})
	if len(urls) != 3 {
		t.Fatalf("expected 3 branch URLs, got %d", len(urls))
	}
	if urls[0] != "https://raw.githubusercontent.com/Owner/Repo/HEAD/README.md" {
		t.Errorf("unexpected first URL: %s", urls[0])
	}
	if got := rawRepoDocURLs("https://gitlab.com/Owner/Repo", []string{"README.md"}); got != nil {
		t.Errorf("non-github repos should return nil, got %v", got)
	}
	if got := rawRepoDocURLs("git@github.com:Owner/Repo.git", []string{"README.md"}); len(got) != 3 {
		t.Errorf("ssh github shorthand should parse, got %v", got)
	}
}

func TestBindSourceAndReinstall(t *testing.T) {
	requirePlugin(t)
	m := newTestManager(t)
	ctx := context.Background()

	src := filepath.Join("testdata", "plugin")
	if _, err := m.InstallFromSource(ctx, "reinst", src, InstallOptions{}); err != nil {
		t.Fatalf("InstallFromSource: %v", err)
	}

	if err := m.BindSource("reinst", "market", "https://registry.example.com/pkg.json",
		"示例市场", "Owner/plugin", "https://github.com/Owner/plugin", ""); err != nil {
		t.Fatalf("BindSource: %v", err)
	}
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	e := man.Get("reinst")
	if e == nil || e.InstallMethod != "market" || e.MarketPluginID != "Owner/plugin" {
		t.Fatalf("bind source not persisted: %+v", e)
	}

	if err := m.Unload("reinst"); err != nil {
		t.Fatal(err)
	}
	// Reinstall from persisted source (local dir source is carried in manifest).
	inst, err := m.ReinstallSource(ctx, "reinst", InstallOptions{})
	if err != nil {
		t.Fatalf("ReinstallSource: %v", err)
	}
	if inst == nil || m.Get("reinst") == nil {
		t.Fatalf("plugin not reloaded after reinstall")
	}
	if got := pluginReply(t, m, "reinst"); got != "pong" {
		t.Errorf("reinstalled plugin reply: %q", got)
	}
	if err := m.Unload("reinst"); err != nil {
		t.Fatal(err)
	}
}
