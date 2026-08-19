package plugin

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// BuildTestPlugin compiles the internal test plugin (testdata/plugin) against
// the local SDK module and returns the binary path, caching the result.
//
// It is a test-support helper for cross-package integration tests (e.g. the
// star pipeline bridge test). Returns "" (after logging) when the Go toolchain
// or SDK module is unavailable.
//
// The cached artifact is built exactly once per process (parallel test
// packages share the safe cache); callers should arrange for CleanupTestPlugin
// to run at the end of their test run so the /tmp binary is not left behind.
func BuildTestPlugin() string {
	testPluginBinOnce.Do(func() {
		testPluginBinCache = buildTestPlugin()
	})
	return testPluginBinCache
}

// CleanupTestPlugin removes the cached test plugin binary (idempotent, safe to
// call from TestMain or t.Cleanup).
func CleanupTestPlugin() {
	if testPluginBinCache == "" {
		return
	}
	_ = os.Remove(testPluginBinCache)
	testPluginBinCache = ""
}

var testPluginBinOnce sync.Once
var testPluginBinCache string

func buildTestPlugin() string {
	if _, err := exec.LookPath("go"); err != nil {
		logger.I18nWarn("BuildTestPlugin: PATH 中未找到 go")
		return ""
	}
	sdkDir, err := findSDKDir()
	if err != nil {
		logger.I18nWarn("BuildTestPlugin: %v", err)
		return ""
	}

	bin := filepath.Join(os.TempDir(), fmt.Sprintf("astrbot-test-plugin-%d", time.Now().UnixNano()))
	// Locate testdata relative to THIS package's source (not the caller's cwd),
	// so the helper works from any package's tests.
	var src []byte
	if _, srcFile, _, ok := runtime.Caller(0); ok {
		pkgDir := filepath.Dir(srcFile)
		// #nosec G304 -- 读取包内 testdata 固定路径
		if data, err := os.ReadFile(filepath.Join(pkgDir, "testdata", "plugin", "main.go")); err == nil {
			src = data
		}
	}
	if src == nil {
		logger.I18nWarn("BuildTestPlugin: 读取测试插件源码失败")
		return ""
	}
	tmp, err := os.MkdirTemp("", "astrbot-test-plugin-src-*")
	if err != nil {
		logger.I18nWarn("BuildTestPlugin: %v", err)
		return ""
	}
	defer os.RemoveAll(tmp)
	// #nosec G306 -- 临时测试模块源码
	if err := os.WriteFile(filepath.Join(tmp, "main.go"), src, 0o644); err != nil {
		logger.I18nWarn("BuildTestPlugin: %v", err)
		return ""
	}
	goMod := fmt.Sprintf(`module example.com/astrbot-test-plugin

go 1.23

require github.com/WaterGodFurina/Astrbot-go-plugin-sdk v0.0.0

replace github.com/WaterGodFurina/Astrbot-go-plugin-sdk => %s
`, sdkDir)
	// #nosec G306 -- 临时测试模块 go.mod
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte(goMod), 0o644); err != nil {
		logger.I18nWarn("BuildTestPlugin: %v", err)
		return ""
	}

	cmd := exec.Command("go", "build", "-o", bin, ".") // #nosec G204 -- 测试辅助：编译固定测试插件模块
	cmd.Dir = tmp
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOFLAGS=-mod=mod",
		"GOPROXY=https://goproxy.cn,direct",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		logger.I18nWarn("BuildTestPlugin: go build: %v\n%s", err, out)
		return ""
	}
	return bin
}
