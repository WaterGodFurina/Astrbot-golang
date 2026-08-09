package plugin

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// BuildTestPlugin compiles the internal test plugin (testdata/plugin) against
// the local SDK module and returns the binary path, caching the result.
//
// It is a test-support helper for cross-package integration tests (e.g. the
// star pipeline bridge test). Returns "" (after logging) when the Go toolchain
// or SDK module is unavailable.
func BuildTestPlugin() string {
	if testPluginBinCache != "" {
		return testPluginBinCache
	}
	if _, err := exec.LookPath("go"); err != nil {
		logger.Warn("BuildTestPlugin: go not on PATH")
		return ""
	}
	sdkDir, err := findSDKDir()
	if err != nil {
		logger.Warn("BuildTestPlugin: %v", err)
		return ""
	}

	bin := filepath.Join(os.TempDir(), fmt.Sprintf("astrbot-test-plugin-%d", time.Now().UnixNano()))
	// Locate testdata relative to THIS package's source (not the caller's cwd),
	// so the helper works from any package's tests.
	var src []byte
	if _, srcFile, _, ok := runtime.Caller(0); ok {
		pkgDir := filepath.Dir(srcFile)
		if data, err := os.ReadFile(filepath.Join(pkgDir, "testdata", "plugin", "main.go")); err == nil {
			src = data
		}
	}
	if src == nil {
		logger.Warn("BuildTestPlugin: read test plugin source failed")
		return ""
	}
	tmp, err := os.MkdirTemp("", "astrbot-test-plugin-src-*")
	if err != nil {
		logger.Warn("BuildTestPlugin: %v", err)
		return ""
	}
	defer os.RemoveAll(tmp)
	if err := os.WriteFile(filepath.Join(tmp, "main.go"), src, 0o644); err != nil {
		logger.Warn("BuildTestPlugin: %v", err)
		return ""
	}
	goMod := fmt.Sprintf(`module example.com/astrbot-test-plugin

go 1.23

require github.com/WaterGodFurina/Astrbot-go-plugin-sdk v0.0.0

replace github.com/WaterGodFurina/Astrbot-go-plugin-sdk => %s
`, sdkDir)
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte(goMod), 0o644); err != nil {
		logger.Warn("BuildTestPlugin: %v", err)
		return ""
	}

	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = tmp
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOFLAGS=-mod=mod",
		"GOPROXY=https://goproxy.cn,direct",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		logger.Warn("BuildTestPlugin: go build: %v\n%s", err, out)
		return ""
	}
	testPluginBinCache = bin
	return bin
}

var testPluginBinCache string
