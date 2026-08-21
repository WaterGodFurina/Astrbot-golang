package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
	sdkv1 "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/gen/sdkv1"
	"github.com/WaterGodFurina/Astrbot-golang/internal/pysdk"
	"github.com/WaterGodFurina/Astrbot-golang/internal/toolchain"
)

// testPluginBin is built once in TestMain by compiling testdata/plugin with
// the host Go toolchain against the SDK module (resolved from go.mod).
var testPluginBin string

func TestMain(m *testing.M) {
	testPluginBin = BuildTestPlugin()
	code := m.Run()
	// Give managed child processes a moment to be reaped by go-plugin.
	time.Sleep(300 * time.Millisecond)
	CleanupTestPlugin()
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
	text, _, _, err := inst.Client.HandleCommand(ctx, "test", nil, &pluginsdk.Event{})
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
	defer func() { _ = m.Unload(id) }()

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
	text, isErr, _, err := inst.Client.HandleTool(ctx, "echo_tool", map[string]any{"text": "hi"}, &pluginsdk.Event{})
	if err != nil || isErr {
		t.Fatalf("HandleTool: err=%v isErr=%v", err, isErr)
	}
	if text != "tool:hi" {
		t.Fatalf("HandleTool result = %q, want %q", text, "tool:hi")
	}

	// on_llm_request hook: modifies the system prompt.
	sp, stop, _, err := inst.Client.HandleLLMRequest(ctx, "inject", &pluginsdk.Event{}, "base", "user")
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
	newChain, stop, _, err := inst.Client.HandleHook(ctx, "decorate", &pluginsdk.Event{}, chain)
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
		ChatLLM: func(req *sdkv1.ChatLLMRequest) (string, error) {
			return "echo:" + req.Prompt, nil
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
	defer func() { _ = m.Unload(id) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	inst := m.Get(id)
	text, _, _, err := inst.Client.HandleCommand(ctx, "hosttest", nil, &pluginsdk.Event{})
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
	inst, err := m1.InstallFromSource(ctx, "persist", filepath.Join("testdata", "plugin"), InstallOptions{GoChoice: "download"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := m1.Unload(inst.ID); err != nil {
		t.Fatal(err)
	}
	_ = inst

	// A fresh manager over the same data dir must reload the plugin from the
	// persisted manifest + cached artifact.
	m2 := NewSubprocessManager(toolchain.New(), dir)
	m2.LoadInstalled(ctx)
	if m2.Get(inst.ID) == nil {
		t.Fatal("plugin not reloaded from manifest on a fresh manager")
	}
	if got := pluginReply(t, m2, inst.ID); got != "pong" {
		t.Errorf("reloaded plugin reply: %q", got)
	}
	if err := m2.Unload(inst.ID); err != nil {
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

	// With MaxRestarts=2 the plugin gets exactly two start chances: the
	// initial start plus one automatic restart; the second crash trips
	// count >= maxRestarts, marking it failed and removing it.
	seen := map[*PluginInstance]bool{}
	starts := 0
	waitFor(t, 12*time.Second, "max-restart failure", func() bool {
		if inst := m.Get("test"); inst != nil && !seen[inst] {
			seen[inst] = true
			starts++
		}
		return m.Get("test") == nil && len(m.Failed()) > 0
	})
	if starts != 2 {
		t.Errorf("MaxRestarts=2 should allow exactly 2 start chances (initial + 1 restart), got %d", starts)
	}
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
	inst, err := m.InstallFromSource(ctx, "installed", src, InstallOptions{GoChoice: "download"})
	if err != nil {
		t.Fatalf("InstallFromSource: %v", err)
	}
	if inst.Name != "testplugin" || inst.Version != "1.0.0" {
		t.Errorf("metadata mismatch: %+v", inst)
	}
	if got := pluginReply(t, m, inst.ID); got != "pong" {
		t.Errorf("installed plugin reply: %q", got)
	}

	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	entry := man.Get(inst.ID)
	if entry == nil || !entry.Enabled {
		t.Fatalf("install not persisted in manifest: %+v", man.Plugins)
	}
	if entry.Binary == "" {
		t.Error("manifest entry missing binary path")
	}

	// 元数据必须写入独立的 metadata.json；config.json 只承载真实配置。
	metaData, err := os.ReadFile(filepath.Join(m.dataDir, "plugins_config", inst.ID, "metadata.json"))
	if err != nil {
		t.Fatalf("read standalone metadata.json: %v", err)
	}
	var meta map[string]interface{}
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("parse metadata.json: %v", err)
	}
	if meta["name"] != "testplugin" || meta["cgo"] != "no" {
		t.Errorf("metadata.json missing packaged info: %+v", meta)
	}
	cfg := m.LoadConfig(inst.ID)
	if cfg == nil {
		t.Fatal("LoadConfig 返回 nil")
	}
	for _, k := range []string{"name", "desc", "author", "version", "repo", "cgo", "display_name", "short_desc"} {
		if _, exists := cfg[k]; exists {
			t.Errorf("config.json must not contain metadata key %q: %+v", k, cfg)
		}
	}

	if err := m.Unload(inst.ID); err != nil {
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
	if err := os.WriteFile(filepath.Join(src, "metadata.json"), []byte(`{"name":"evil","version":"1.0.0","cgo":false}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Without IgnoreRisk the install is blocked with the offending locations.
	_, err := m.InstallFromSource(ctx, "evil", src, InstallOptions{GoChoice: "download"})
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
	inst, err := m.InstallFromSource(ctx, "evil", src, InstallOptions{IgnoreRisk: true, GoChoice: "download"})
	if err != nil {
		t.Fatalf("IgnoreRisk install: %v", err)
	}
	if inst == nil || inst.Name != "evil" {
		t.Fatalf("expected loaded plugin, got %+v", inst)
	}
	if err := m.Unload(inst.ID); err != nil {
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
		GoChoice:       "download",
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

	if err := m.Unload(inst.ID); err != nil {
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
	if err := os.WriteFile(filepath.Join(src, "metadata.json"), []byte(`{"name":"docplugin","desc":"doc","version":"1.0.0","cgo":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	readme := "# Doc Plugin\n\nDocumentation here.\n"
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte(readme), 0o644); err != nil {
		t.Fatal(err)
	}

	inst, err := m.InstallFromSource(ctx, "docplugin", src, InstallOptions{GoChoice: "download"})
	if err != nil {
		t.Fatalf("InstallFromSource: %v", err)
	}
	if got := m.Readme(inst.ID); got != readme {
		t.Errorf("Readme should read cached README, got %q", got)
	}
	if err := m.Unload(inst.ID); err != nil {
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

// TestRawRepoDocURLsNoPanic guards the git@ shorthand parser against inputs
// where the colon is missing: the previous slicing panicked with an
// out-of-range slice on "git@host/owner/repo".
func TestRawRepoDocURLsNoPanic(t *testing.T) {
	for _, repo := range []string{
		"git@example.com/Owner/Repo", // no ':' separator
		"git@github.com",             // host only
		"git@github.com:",            // empty path
		"git@github.com:/Owner/Repo", // leading ':' trimmed, still no owner
		"git@github.com:Owner",       // no '/'
	} {
		if got := rawRepoDocURLs(repo, []string{"README.md"}); got != nil {
			t.Errorf("rawRepoDocURLs(%q) should return nil, got %v", repo, got)
		}
	}
}

// TestBuildTestPluginConcurrentCalls verifies the test-plugin cache is safe
// under concurrent access (previously a bare global written without a lock,
// which the race detector flags when tests build the plugin in parallel).
func TestBuildTestPluginConcurrentCalls(t *testing.T) {
	if testPluginBin == "" {
		t.Skip("test plugin not built in this run")
	}
	var wg sync.WaitGroup
	results := make([]string, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = BuildTestPlugin()
		}(i)
	}
	wg.Wait()
	for i, r := range results {
		if r != testPluginBin {
			t.Errorf("result %d = %q, want %q", i, r, testPluginBin)
		}
	}
}

// TestUninstallSerializesWithInstallTail verifies Uninstall participates in the
// per-plugin lifecycle lock: while an in-flight InstallFromSource tail (or any
// other op) holds lockOp(id), Uninstall must block, so a concurrent
// "install + uninstall" cannot interleave and resurrect the plugin.
func TestUninstallSerializesWithInstallTail(t *testing.T) {
	requirePlugin(t)
	m := newTestManager(t)
	ctx := context.Background()
	src := filepath.Join("testdata", "plugin")

	if _, err := m.InstallFromSource(ctx, "racey", src, InstallOptions{GoChoice: "download"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	defer func() { _ = m.Unload("racey") }()

	// Hold the lifecycle lock as an in-flight install tail would.
	release := m.lockOp("racey")
	done := make(chan error, 1)
	go func() {
		done <- m.Uninstall("racey", false, false)
	}()

	select {
	case err := <-done:
		t.Fatalf("Uninstall should block while the lifecycle lock is held, got %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Uninstall after lock release: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Uninstall did not complete after the lock was released")
	}

	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		t.Fatal(err)
	}
	if man.Get("racey") != nil {
		t.Error("manifest entry should be gone after Uninstall")
	}
	if m.Get("racey") != nil {
		t.Error("instance should be gone after Uninstall")
	}
}

func TestBindSourceAndReinstall(t *testing.T) {
	requirePlugin(t)
	m := newTestManager(t)
	ctx := context.Background()

	src := filepath.Join("testdata", "plugin")
	inst1, err := m.InstallFromSource(ctx, "reinst", src, InstallOptions{GoChoice: "download"})
	if err != nil {
		t.Fatalf("InstallFromSource: %v", err)
	}

	if err := m.BindSource(inst1.ID, "market", "https://registry.example.com/pkg.json",
		"示例市场", "Owner/plugin", "https://github.com/Owner/plugin", ""); err != nil {
		t.Fatalf("BindSource: %v", err)
	}
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	e := man.Get(inst1.ID)
	if e == nil || e.InstallMethod != "market" || e.MarketPluginID != "Owner/plugin" {
		t.Fatalf("bind source not persisted: %+v", e)
	}

	if err := m.Unload(inst1.ID); err != nil {
		t.Fatal(err)
	}
	// Reinstall from persisted source (local dir source is carried in manifest).
	inst2, err := m.ReinstallSource(ctx, inst1.ID, InstallOptions{GoChoice: "download"})
	if err != nil {
		t.Fatalf("ReinstallSource: %v", err)
	}
	if inst2 == nil || m.Get(inst1.ID) == nil {
		t.Fatalf("plugin not reloaded after reinstall")
	}
	if got := pluginReply(t, m, inst1.ID); got != "pong" {
		t.Errorf("reinstalled plugin reply: %q", got)
	}
	if err := m.Unload(inst1.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSafeDataDirPathRejectsEmptyAndRoot(t *testing.T) {
	m := newTestManager(t)
	for _, sub := range []string{"", ".", ".."} {
		if p, err := m.safeDataDirPath(sub); err == nil {
			t.Fatalf("safeDataDirPath(%q) = %q, want error", sub, p)
		}
	}
	if _, err := m.safeDataDirPath("../evil"); err == nil {
		t.Fatal("safeDataDirPath(\"../evil\") accepted, want error")
	}
	p, err := m.safeDataDirPath("plugins_config/test")
	if err != nil {
		t.Fatalf("safeDataDirPath(valid sub): %v", err)
	}
	if p != filepath.Join(m.dataDir, "plugins_config", "test") {
		t.Fatalf("safeDataDirPath resolved to %q, want %q", p, filepath.Join(m.dataDir, "plugins_config", "test"))
	}
}

func TestUninstallLegacyEntryDoesNotWipeDataDir(t *testing.T) {
	m := newTestManager(t)
	man := &Manifest{Version: 1, Plugins: []ManifestEntry{
		{ID: "legacy_plugin", Name: "legacy_plugin", Binary: "unused", Enabled: false},
	}}
	if err := man.Save(m.manifestPath()); err != nil {
		t.Fatalf("save manifest: %v", err)
	}

	// Unrelated data that must survive the uninstall.
	keepFile := filepath.Join(m.dataDir, "skills", "keep.txt")
	if err := os.MkdirAll(filepath.Dir(keepFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keepFile, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The plugin's own config dir that must be removed.
	cfgFile := filepath.Join(m.dataDir, "plugins_config", "legacy_plugin", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfgFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgFile, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.Uninstall("legacy_plugin", true, true); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(keepFile); err != nil {
		t.Fatalf("unrelated data under dataDir was deleted: %v", err)
	}
	if _, err := os.Stat(cfgFile); !os.IsNotExist(err) {
		t.Fatalf("plugin config dir should have been removed, stat err=%v", err)
	}
}

// TestIdleUnloadAndLazyReload: 闲置自动卸载 + 懒加载唤醒（进程池语义）。
// 插件闲置超过阈值 → sweep 卸载（进程内存回收）；再次 EnsureLoaded →
// 从 manifest 重新加载并恢复 RPC 服务。
func TestIdleUnloadAndLazyReload(t *testing.T) {
	requirePlugin(t)
	m := newTestManager(t)

	inst, err := m.InstallFromSource(context.Background(), "pooled", filepath.Join("testdata", "plugin"), InstallOptions{GoChoice: "download"})
	if err != nil {
		t.Fatalf("InstallFromSource: %v", err)
	}
	// 刚加载视为活跃。
	if inst.IsIdle(time.Now(), time.Minute) {
		t.Fatal("freshly loaded plugin must be active")
	}

	// 启用闲置卸载（阈值极短）并手动触发清扫（不依赖 ticker 定时）。
	m.SetIdleUnload(10 * time.Millisecond)
	inst.lastActiveNano.Store(time.Now().Add(-time.Minute).UnixNano()) // 模拟闲置
	m.sweepIdlePlugins()
	if m.Get(inst.ID) != nil {
		t.Fatal("idle plugin must be unloaded by the sweep")
	}

	// 懒加载：EnsureLoaded 从 manifest 拉回并恢复服务。
	back, err := m.EnsureLoaded(context.Background(), inst.ID)
	if err != nil {
		t.Fatalf("EnsureLoaded: %v", err)
	}
	if back == nil || back.Client == nil {
		t.Fatal("reloaded instance must have a live client")
	}
	if got := pluginReply(t, m, inst.ID); got != "pong" {
		t.Fatalf("lazy-reloaded plugin reply: %q", got)
	}

	// 卸载与加载都幂等。
	if err := m.Unload(inst.ID); err != nil {
		t.Fatal(err)
	}
}

// TestSweepSkipsActivePlugins: 活跃插件（最近 touch 过）不会被闲置清扫卸载。
func TestSweepSkipsActivePlugins(t *testing.T) {
	requirePlugin(t)
	m := newTestManager(t)
	m.SetIdleUnload(10 * time.Millisecond)

	inst, err := m.InstallFromSource(context.Background(), "pooled2", filepath.Join("testdata", "plugin"), InstallOptions{GoChoice: "download"})
	if err != nil {
		t.Fatalf("InstallFromSource: %v", err)
	}
	inst.Touch() // 活跃
	m.sweepIdlePlugins()
	if m.Get(inst.ID) == nil {
		t.Fatal("active plugin must survive the sweep")
	}
	if err := m.Unload(inst.ID); err != nil {
		t.Fatal(err)
	}
}

// TestIdleUnloadBlockedKeepsPluginResident: 行为页配置"不允许休眠"的插件
// 即使闲置也不会被清扫卸载；设置持久化到 manifest，重启后仍生效。
func TestIdleUnloadBlockedKeepsPluginResident(t *testing.T) {
	requirePlugin(t)
	m := newTestManager(t)
	m.SetIdleUnload(10 * time.Millisecond)

	inst, err := m.InstallFromSource(context.Background(), "resident", filepath.Join("testdata", "plugin"), InstallOptions{GoChoice: "download"})
	if err != nil {
		t.Fatalf("InstallFromSource: %v", err)
	}
	if m.IdleUnloadBlocked(inst.ID) {
		t.Fatal("fresh install must default to allow-sleep")
	}

	if err := m.SetIdleUnloadBlocked(inst.ID, true); err != nil {
		t.Fatalf("SetIdleUnloadBlocked: %v", err)
	}
	if !m.IdleUnloadBlocked(inst.ID) {
		t.Fatal("blocked flag must be readable")
	}

	inst.lastActiveNano.Store(time.Now().Add(-time.Hour).UnixNano()) // 闲置一小时
	m.sweepIdlePlugins()
	if m.Get(inst.ID) == nil {
		t.Fatal("blocked plugin must survive the idle sweep")
	}

	// 重新打开允许休眠后，闲置会被清扫。
	if err := m.SetIdleUnloadBlocked(inst.ID, false); err != nil {
		t.Fatal(err)
	}
	m.sweepIdlePlugins()
	if m.Get(inst.ID) != nil {
		t.Fatal("unblocked idle plugin must be swept")
	}
}

// ---------------------------------------------------------------------------
// [ASTRBOT] 启动错误协议 + 进程组生命周期测试（任务追加部分）
// ---------------------------------------------------------------------------

// TestStartupErrorParser 验证 stderr [ASTRBOT] 协议解析：phase 行、跨块
// 半行缓冲、STARTUP_ERROR 行解析（error 值含空格）、普通行转发；无
// STARTUP_ERROR 时 Err() 为 nil。
func TestStartupErrorParser(t *testing.T) {
	p := newAstrbotStartupParser()
	// 模拟 go-plugin 逐行转发：行与 '\n' 分两次 Write（跨块半行）。
	for _, chunk := range [][]byte{
		[]byte("[ASTRBOT] phase=dependency_check"),
		[]byte("\n普通日志行: 正在检查依赖\n"),
		[]byte("[ASTRBOT] phase=bridge_init\n[ASTRBOT] phase=gr"),
		[]byte("pc_start\n"),
		[]byte("[ASTRBOT] STARTUP_ERROR phase=plugin_import type=ModuleNotFoundError plugin=my_plugin error=No module named 'xyz'\n"),
		[]byte("尾行无换行"),
	} {
		if _, err := p.Write(chunk); err != nil {
			t.Fatalf("Write(%q): %v", chunk, err)
		}
	}

	phases := p.Phases()
	want := []string{"dependency_check", "bridge_init", "grpc_start"}
	if len(phases) != len(want) {
		t.Fatalf("phases = %v, want %v", phases, want)
	}
	for i := range want {
		if phases[i] != want[i] {
			t.Errorf("phases[%d] = %q, want %q", i, phases[i], want[i])
		}
	}

	se := p.WaitError(time.Second)
	if se == nil {
		t.Fatal("WaitError 应返回 STARTUP_ERROR")
	}
	if se.Phase != "plugin_import" || se.Type != "ModuleNotFoundError" || se.Plugin != "my_plugin" {
		t.Errorf("解析错误字段异常: %+v", se)
	}
	if !strings.Contains(se.Error, "No module named 'xyz'") {
		t.Errorf("error 字段解析异常: %q", se.Error)
	}
}

// TestStartupErrorParserNoError: 没有 STARTUP_ERROR 时 StartupError/WaitError
// 必须返回 nil。
func TestStartupErrorParserNoError(t *testing.T) {
	p := newAstrbotStartupParser()
	for _, chunk := range [][]byte{
		[]byte("[ASTRBOT] phase=dependency_check\n"),
		[]byte("[ASTRBOT] phase=running\n"),
		[]byte("插件普通日志\n"),
	} {
		if _, err := p.Write(chunk); err != nil {
			t.Fatalf("Write(%q): %v", chunk, err)
		}
	}
	if se := p.StartupError(); se != nil {
		t.Fatalf("不应有 STARTUP_ERROR: %+v", se)
	}
	if se := p.WaitError(300 * time.Millisecond); se != nil {
		t.Fatalf("WaitError 不应有 STARTUP_ERROR: %+v", se)
	}
	if got := p.Phases(); len(got) != 2 {
		t.Errorf("phases = %v, want 2 个 phase", got)
	}
}

// TestStartupErrorParserNoErrorField: STARTUP_ERROR 行缺少 error= 时，整行
// 截断作为错误消息，信息不丢失。
func TestStartupErrorParserNoErrorField(t *testing.T) {
	p := newAstrbotStartupParser()
	if _, err := p.Write([]byte("[ASTRBOT] phase=bridge_init\n[ASTRBOT] STARTUP_ERROR phase=bridge_init type=RuntimeError plugin=x\n")); err != nil {
		t.Fatal(err)
	}
	se := p.WaitError(time.Second)
	if se == nil {
		t.Fatal("应解析到 STARTUP_ERROR")
	}
	if se.Phase != "bridge_init" || se.Type != "RuntimeError" {
		t.Errorf("字段解析异常: %+v", se)
	}
	if !strings.Contains(se.Error, "STARTUP_ERROR") || !strings.Contains(se.Error, "RuntimeError") {
		t.Errorf("缺少 error= 时应整行截断为错误消息: %q", se.Error)
	}
}

// TestStartupErrorWrappedInLoadError 端到端验证错误提升：用 ASTRBOT_PYTHON_BIN
// 注入一个假 Python 解释器（经 hasHostDeps 探测后由 startInstance 启动），
// 它按 [ASTRBOT] 协议在 stderr 打 phase + STARTUP_ERROR 行后退出 1。宿主
// LoadLang 返回的错误必须包含 phase 化的 STARTUP_ERROR 信息
// （phase=plugin_import + ModuleNotFoundError），而不是笼统的 go-plugin
// 握手错误（"failed to read any lines from plugin's stdout"）。不依赖真实
// Python 桥接侧（该侧协议由另一子任务实现，测试用假输出自行验证）。
func TestStartupErrorWrappedInLoadError(t *testing.T) {
	fakePy := filepath.Join(t.TempDir(), "fake_python")
	script := `#!/bin/sh
if [ "$1" = "-c" ]; then
	# hasHostDeps 探测：宿主基础依赖"存在"
	exit 0
fi
# 模拟 Python 桥：打协议行到 stderr 后退出 1
echo '[ASTRBOT] phase=dependency_check' >&2
echo '[ASTRBOT] phase=plugin_import' >&2
echo "[ASTRBOT] STARTUP_ERROR phase=plugin_import type=ModuleNotFoundError plugin=fake_plugin error=ModuleNotFoundError: No module named 'fake_dep'" >&2
exit 1
`
	if err := os.WriteFile(fakePy, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(pysdk.EnvPythonBin, fakePy)

	m := NewSubprocessManager(toolchain.New(), t.TempDir())
	m.MaxRestarts = 2
	m.RestartBaseDelay = 100 * time.Millisecond
	// 独立端口区间，避免与真实宿主（10000-25000）及并发测试互相干扰。
	m.MinPort = 50300
	m.MaxPort = 50400
	t.Cleanup(m.Shutdown)

	_, err := m.LoadLang(context.Background(), "py_broken", filepath.Join("testdata", "python_plugin"), "python")
	if err == nil {
		t.Fatal("期望启动失败（假解释器按协议报错后退出）")
	}
	msg := err.Error()
	t.Logf("LoadLang 错误: %s", msg)
	if !strings.Contains(msg, "Python 插件启动失败") {
		t.Errorf("错误应被提升为 Python 插件启动失败: %s", msg)
	}
	if !strings.Contains(msg, "phase=plugin_import") {
		t.Errorf("错误应包含 phase=plugin_import: %s", msg)
	}
	if !strings.Contains(msg, "ModuleNotFoundError") {
		t.Errorf("错误应包含 ModuleNotFoundError: %s", msg)
	}
	if !strings.Contains(msg, "fake_plugin") {
		t.Errorf("错误应包含 plugin=fake_plugin: %s", msg)
	}
}

// TestProcessGroupLifecycle 验证进程组生命周期：Setpgid 生效（/proc/<pid>
// 的 pgrp 字段 == pid），卸载后整组进程真退出。
func TestProcessGroupLifecycle(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("进程组测试仅支持 Linux")
	}
	requirePlugin(t)
	m := newTestManager(t)
	ctx := context.Background()

	inst, err := m.Load(ctx, "test", testPluginBin)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if inst.pgid <= 0 {
		t.Fatal("pgid 未记录（expected cmd.Process.Pid）")
	}
	// Setpgid 生效：插件进程的 pgrp 必须是它自己的 pid。
	pgrp, ok := processPGRP(inst.pgid)
	if !ok {
		t.Fatalf("读取 /proc/%d/stat 失败", inst.pgid)
	}
	if pgrp != inst.pgid {
		t.Fatalf("进程组未生效: pgrp=%d, 期望 %d (pid)", pgrp, inst.pgid)
	}

	if err := m.Unload("test"); err != nil {
		t.Fatalf("Unload: %v", err)
	}
	waitFor(t, 5*time.Second, "插件进程退出", func() bool {
		return !processAlive(inst.pgid)
	})
}

// TestTerminateProcessGroupKillsWholeTree 验证 killProcessGroup 的核心原语
// terminateProcessGroup：进程组内除直接子进程外再拉起的子进程（模拟 Python
// 桥再拉起子进程）也被一并回收。
func TestTerminateProcessGroupKillsWholeTree(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("进程组测试仅支持 Linux")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh 不可用")
	}
	// sh 作为进程组组长，sleep 由它拉起并继承同组。
	cmd := exec.Command("sh", "-c", "sleep 100 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("启动 sh 失败: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	pgid := cmd.Process.Pid

	waitFor(t, 3*time.Second, "组内出现 sh + sleep", func() bool {
		return len(processesInGroup(pgid)) >= 2
	})
	if got := processesInGroup(pgid); len(got) < 2 {
		t.Fatalf("期望组内至少 2 个进程，实际 %v", got)
	}

	if !terminateProcessGroup(pgid, nil) {
		t.Fatal("terminateProcessGroup 返回 false")
	}
	_ = cmd.Wait() // 直接子进程已被组信号回收
	waitFor(t, 3*time.Second, "进程组清空", func() bool {
		return len(processesInGroup(pgid)) == 0
	})
}

// processPGRP 读取 /proc/<pid>/stat 的第 5 字段（pgrp）。
func processPGRP(pid int) (int, bool) {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	s := string(stat)
	idx := strings.LastIndex(s, ")")
	if idx < 0 {
		return 0, false
	}
	fields := strings.Fields(s[idx+1:])
	if len(fields) < 3 {
		return 0, false
	}
	pgrp, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, false
	}
	return pgrp, true
}

// processAlive 通过 kill(pid, 0) 探测进程是否存在（ESRCH = 已退出）。
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// processesInGroup 扫描 /proc 中所有 pgrp == pgid 的进程。
func processesInGroup(pgid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		if g, ok := processPGRP(pid); ok && g == pgid {
			out = append(out, pid)
		}
	}
	return out
}
