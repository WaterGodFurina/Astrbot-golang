package plugin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/v2"
)

// buildTestNativeLib compiles a Native (.so) test plugin with the local SDK.
// It skips when a C toolchain / Go toolchain / SDK is unavailable.
func buildTestNativeLib(t *testing.T, source string) string {
	t.Helper()
	if !nativeTestCgoEnabled {
		t.Skip("CGO disabled: Go plugin.Open is a stub; Native runtime unavailable")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not in PATH")
	}
	sdkDir, err := findSDKDir()
	if err != nil {
		t.Skipf("local SDK not found: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	mod := fmt.Sprintf("example.com/native-test-plugin-%d", time.Now().UnixNano())
	goMod := "module " + mod + "\n\ngo 1.23\n\n" +
		"require github.com/WaterGodFurina/Astrbot-go-plugin-sdk/v2 v2.0.0\n\n" +
		"replace github.com/WaterGodFurina/Astrbot-go-plugin-sdk/v2 => " + sdkDir + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "native_plugin.so")
	args := []string{"build", "-buildmode=plugin", "-o", out}
	if nativeTestRaceEnabled {
		// Match the -race host test binary; otherwise plugin.Open reports a
		// mismatched internal/race package.
		args = append(args, "-race")
	}
	args = append(args, ".")
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1", "GOFLAGS=-mod=mod", "GOPROXY=https://goproxy.cn,direct")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("native build failed (no cgo toolchain?): %v\n%s", err, b)
	}
	return out
}

const nativeTestSrc = `package main

import (
	"strings"

	sdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/v2"
	native "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/v2/native"
)

var plugin = &sdk.Plugin{
	Name:    "native_test",
	Version: "1.0.0",
	Commands: []sdk.Command{{
		Name: "echo",
		Handler: func(e *sdk.Event, args []string) (string, error) {
			return "native:" + strings.Join(args, " "), nil
		},
	}},
}

func AstrBotNativePlugin(pluginID string) (native.Plugin, error) {
	return native.Serve(plugin, pluginID)
}

func main() {}
`

// TestNativeLoadAndCall exercises the real path: plugin.Open -> entry ->
// direct in-process call (no subprocess, no gRPC).
func TestNativeLoadAndCall(t *testing.T) {
	so := buildTestNativeLib(t, nativeTestSrc)
	client, cleanup, err := openNativePlugin(so, "native_test")
	if err != nil {
		t.Fatalf("openNativePlugin: %v", err)
	}
	defer func() { _ = cleanup() }()

	meta, err := client.Register(context.Background())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if meta.Name != "native_test" {
		t.Fatalf("name = %q", meta.Name)
	}
	cmdRes, err := client.HandleCommand(context.Background(), "echo", []string{"hi", "there"}, &pluginsdk.Event{Type: "test"})
	if err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	if cmdRes.Text != "native:hi there" {
		t.Fatalf("HandleCommand = %q", cmdRes.Text)
	}
}

func TestNativeSymbolMissing(t *testing.T) {
	so := buildTestNativeLib(t, "package main\n\nvar plugin = 1\n\nfunc main() {}\n")
	_, _, err := openNativePlugin(so, "native_test")
	if err == nil || !strings.Contains(err.Error(), "Lookup(AstrBotNativePlugin)") {
		t.Fatalf("want Lookup error, got %v", err)
	}
}

func TestNativeSymbolWrongType(t *testing.T) {
	so := buildTestNativeLib(t, "package main\n\nvar AstrBotNativePlugin = 42\n\nfunc main() {}\n")
	_, _, err := openNativePlugin(so, "native_test")
	if err == nil || !strings.Contains(err.Error(), "unexpected type") {
		t.Fatalf("want type mismatch error, got %v", err)
	}
}

// TestNativeNotIdleUnloaded verifies the host keeps Native instances out of the
// idle-unload sweep.
func TestNativeNotIdleUnloaded(t *testing.T) {
	m := NewSubprocessManager(nil, t.TempDir())
	m.SetIdleUnload(time.Minute)
	inst := &PluginInstance{ID: "native_test", Runtime: "native", Language: "go"}
	m.mu.Lock()
	m.instances[inst.ID] = inst
	m.mu.Unlock()
	inst.BackdateIdle(time.Hour) // well past the threshold
	m.sweepIdlePlugins()
	m.mu.RLock()
	_, still := m.instances[inst.ID]
	m.mu.RUnlock()
	if !still {
		t.Fatal("Native instance was idle-unloaded; it must be resident for the process lifetime")
	}
}

func TestNativeLoadViaManager(t *testing.T) {
	so := buildTestNativeLib(t, nativeTestSrc)
	m := NewSubprocessManager(nil, t.TempDir())
	inst, err := m.loadLocked(context.Background(), "native_test", so, "go", "native", false)
	if err != nil {
		t.Fatalf("loadLocked native: %v", err)
	}
	if inst.Runtime != "native" {
		t.Fatalf("runtime = %q", inst.Runtime)
	}
	if inst.raw != nil || inst.pgid != 0 {
		t.Fatal("Native load must not create a subprocess (raw/pgid set)")
	}
	cmdRes, err := inst.Client.HandleCommand(context.Background(), "echo", []string{"x"}, &pluginsdk.Event{Type: "test"})
	if err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	if cmdRes.Text != "native:x" {
		t.Fatalf("got %q", cmdRes.Text)
	}
	// Re-open on a second manager (restart semantics: re-plugin.Open).
	m2 := NewSubprocessManager(nil, t.TempDir())
	if _, err := m2.loadLocked(context.Background(), "native_test", so, "go", "native", false); err != nil {
		t.Fatalf("reload native: %v", err)
	}
}

// TestNativeAndGRPCCoexist loads a Native instance and a gRPC subprocess
// instance in the same manager and calls both.
func TestNativeAndGRPCCoexist(t *testing.T) {
	so := buildTestNativeLib(t, nativeTestSrc)
	grpcBin := BuildTestPlugin()
	if grpcBin == "" {
		t.Skip("gRPC test plugin unavailable")
	}
	m := NewSubprocessManager(nil, t.TempDir())
	nat, err := m.loadLocked(context.Background(), "native_test", so, "go", "native", false)
	if err != nil {
		t.Fatalf("load native: %v", err)
	}
	grp, err := m.loadLocked(context.Background(), "grpc_test", grpcBin, "go", "grpc", false)
	if err != nil {
		t.Fatalf("load grpc: %v", err)
	}
	if nat.Runtime != "native" || (grp.Runtime != "" && grp.Runtime != "grpc") {
		t.Fatalf("runtimes = %q / %q", nat.Runtime, grp.Runtime)
	}
	natRes, err := nat.Client.HandleCommand(context.Background(), "echo", []string{"a"}, &pluginsdk.Event{Type: "t"})
	if err != nil || natRes.Text != "native:a" {
		t.Fatalf("native call: %q err=%v", natRes.Text, err)
	}
	grpRes, err := grp.Client.HandleCommand(context.Background(), "test", nil, &pluginsdk.Event{Type: "t"})
	if err != nil || grpRes.Text != "pong" {
		t.Fatalf("grpc call: %q err=%v", grpRes.Text, err)
	}
}
