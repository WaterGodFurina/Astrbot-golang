package plugin

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSDKInModCacheUsesExplicitGoAndWorkDir verifies that the module-cache
// lookup runs the provided go binary (the bundled toolchain, not the system
// `go` on PATH) with the working directory pinned to the go.mod location
// instead of the process CWD.
func TestSDKInModCacheUsesExplicitGoAndWorkDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based fake go binary not portable to windows")
	}
	fakeGoDir := t.TempDir()
	sdkDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sdkDir, "go.mod"), []byte("module fake/sdk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(fakeGoDir, "env.txt")
	fakeGo := filepath.Join(fakeGoDir, "go")
	script := "#!/bin/sh\npwd > '" + envFile + "'\nprintf '%s\\n' '" + sdkDir + "'\n"
	if err := os.WriteFile(fakeGo, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	got, err := sdkInModCache(fakeGo, workDir, "v0.0.0")
	if err != nil {
		t.Fatalf("sdkInModCache: %v", err)
	}
	if got != sdkDir {
		t.Errorf("sdkInModCache dir = %q, want %q", got, sdkDir)
	}
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("fake go never invoked: %v", err)
	}
	if cwd := strings.TrimSpace(string(data)); cwd != workDir {
		t.Errorf("go should run with workDir %q, ran in %q", workDir, cwd)
	}
}

// TestFindSDKDirWithGoFallback verifies that the empty-goBin variant still
// resolves the SDK source directory, keeping the toolchain-less fallback path
// (used by BuildTestPlugin) functional.
func TestFindSDKDirWithGoFallback(t *testing.T) {
	sdk, err := findSDKDirWithGo("")
	if err != nil {
		t.Fatalf("findSDKDirWithGo with system go fallback: %v", err)
	}
	if !filepath.IsAbs(sdk) {
		t.Fatalf("SDK dir should be absolute, got %q", sdk)
	}
	if _, err := os.Stat(filepath.Join(sdk, "go.mod")); err != nil {
		t.Errorf("SDK dir %q missing go.mod: %v", sdk, err)
	}
}
