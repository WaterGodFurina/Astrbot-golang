package pysdk

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDiscoverBundledPython verifies that a previously downloaded bundled
// Python is discovered without any system interpreter (PATH stripped).
func TestDiscoverBundledPython(t *testing.T) {
	oldPath := os.Getenv("PATH")
	oldBin := os.Getenv(EnvPythonBin)
	oldHome := os.Getenv("HOME")
	t.Cleanup(func() {
		_ = os.Setenv("PATH", oldPath)
		_ = os.Setenv("HOME", oldHome)
		if oldBin == "" {
			_ = os.Unsetenv(EnvPythonBin)
		} else {
			_ = os.Setenv(EnvPythonBin, oldBin)
		}
	})

	// 临时 HOME + 空 PATH（无系统 python）
	home := t.TempDir()
	_ = os.Setenv("HOME", home)
	_ = os.Setenv("PATH", t.TempDir())
	_ = os.Unsetenv(EnvPythonBin)

	if os.Getenv("ASTRBOT_PYTHON_DOWNLOAD_TEST") != "1" {
		t.Skip("ASTRBOT_PYTHON_DOWNLOAD_TEST=1 时执行真实下载验证")
	}

	stage := func(s string) { t.Logf("[stage] %s", s) }
	py, err := EnsurePythonBin(stage)
	if err != nil {
		t.Fatalf("EnsurePythonBin: %v", err)
	}
	t.Logf("bundled python: %s", py)
	if info, err := os.Stat(py); err != nil || info.IsDir() {
		t.Fatalf("解释器不存在: %s (%v)", py, err)
	}
	// 幂等：二次调用直接命中缓存，无需下载
	py2, err := EnsurePythonBin(nil)
	if err != nil || py2 != py {
		t.Fatalf("二次调用未复用缓存: %q vs %q err=%v", py2, py, err)
	}
	// DiscoverPythonBin 也应能发现（重启场景）
	if got := DiscoverPythonBin(); got != py {
		t.Fatalf("DiscoverPythonBin = %q, want %q", got, py)
	}
	// 解压布局
	base := filepath.Join(pythonBaseDir(), pyVersion())
	for _, f := range []string{"python/bin/python3"} {
		if _, err := os.Stat(filepath.Join(base, f)); err != nil {
			t.Fatalf("解压缺少 %s: %v", f, err)
		}
	}
}

// TestPyTarget verifies the GOOS/GOARCH → python-build-standalone target map
// for the platforms we support.
func TestPyTarget(t *testing.T) {
	for goos, want := range map[string]string{
		"linux":   "unknown-linux-gnu",
		"darwin":  "apple-darwin",
		"windows": "pc-windows-msvc",
	} {
		for _, arch := range []string{"amd64", "arm64"} {
			target, err := pyTargetFor(goos, arch)
			if err != nil {
				t.Errorf("pyTargetFor(%s/%s): %v", goos, arch, err)
				continue
			}
			wantArch := "x86_64"
			if arch == "arm64" {
				wantArch = "aarch64"
			}
			if target != wantArch+"-"+want {
				t.Errorf("pyTargetFor(%s/%s) = %q, want %q", goos, arch, target, wantArch+"-"+want)
			}
		}
	}
	// 未知平台报错
	if _, err := pyTargetFor("android", "arm64"); err == nil {
		t.Error("android/arm64 应报错")
	}
	if _, err := pyTargetFor("linux", "mips"); err == nil {
		t.Error("linux/mips 应报错")
	}
}

// TestDownloadURLMirror verifies mirror prefixing.
func TestDownloadURLMirror(t *testing.T) {
	old := os.Getenv(EnvPythonMirror)
	t.Cleanup(func() {
		if old == "" {
			_ = os.Unsetenv(EnvPythonMirror)
		} else {
			_ = os.Setenv(EnvPythonMirror, old)
		}
	})
	_ = os.Setenv(EnvPythonMirror, "https://ghfast.top/")
	u := pyDownloadURL("cpython-x.tar.gz")
	if u != "https://ghfast.top/https://github.com/astral-sh/python-build-standalone/releases/download/"+pyVersion()+"/cpython-x.tar.gz" {
		t.Fatalf("mirror URL 异常: %s", u)
	}
}

// setupFakeVenv 构造一个"伪 venv"（bin/python + 内容匹配的 READY +
// environment.json），绑定到一个必然失败的假解释器（脚本 exit 1）：
// 任何误触发的 venv 创建 / pip 安装 / import 探测都会失败，从而在断言
// EnsureVenv 直接复用标记就绪 venv 时，能确定性地发现回归。cacheDir 为空时
// 自动创建 TempDir 并设置 ASTRBOT_PYTHON_CACHE_DIR。返回假解释器路径与
// 期望的 venv python 路径。
func setupFakeVenv(t *testing.T, cacheDir string) (fakePy, wantVenvPython string) {
	t.Helper()
	if cacheDir == "" {
		cacheDir = t.TempDir()
		t.Setenv(EnvPythonCacheDir, cacheDir)
	}

	fakePy = filepath.Join(t.TempDir(), "fake-python3")
	if err := os.WriteFile(fakePy, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvPythonBin, fakePy)
	if got := DiscoverPythonBin(); got != fakePy {
		t.Fatalf("DiscoverPythonBin = %q, want %q", got, fakePy)
	}

	// 与 ensureVenvReady 相同的指纹算法（解释器路径原文，保持已有 venv 目录名兼容）
	sum := sha256.Sum256([]byte(fakePy))
	fp := hex.EncodeToString(sum[:6])
	venvRoot := filepath.Join(cacheDir, "astrbot-go", "python-venv-"+fp)
	binDir := filepath.Join(venvRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "python"), []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeVenvMarkers(venvRoot, fakePy, SDKVersion, baseDepsVersion); err != nil {
		t.Fatal(err)
	}
	return fakePy, filepath.Join(binDir, "python")
}

// TestVenvReadyMarker: venv 首次创建时写 READY + environment.json；再次
// EnsureVenv 时标记匹配 → 直接复用，不再跑 import 探测 / pip（用必失败的
// 假解释器证明没有触发任何供给动作）。依赖清单版本变化 → 视为不完整 →
// 触发重装（假解释器装不上 → 返回空串）。
func TestVenvReadyMarker(t *testing.T) {
	_, want := setupFakeVenv(t, "")

	// 标记匹配：必须直接返回 venv python（任何供给尝试都会因假解释器
	// exit 1 而失败，若返回非 want 即断言失败）。
	if got := EnsureVenv(t.TempDir()); got != want {
		t.Fatalf("标记匹配时应直接复用 venv: got %q, want %q", got, want)
	}
	// 二次调用（重启场景）同样直接复用。
	if got := EnsureVenv(t.TempDir()); got != want {
		t.Fatalf("二次调用未复用: got %q, want %q", got, want)
	}

	// 宿主依赖清单版本变化（baseDepsVersion+1）→ 标记不匹配 → 重新供给。
	// 假解释器装不上依赖 → EnsureVenv 返回空串（"安装失败"路径）。
	root := filepath.Dir(filepath.Dir(want))
	if err := writeVenvMarkers(root, os.Getenv(EnvPythonBin), SDKVersion, baseDepsVersion+1); err != nil {
		t.Fatal(err)
	}
	if got := EnsureVenv(t.TempDir()); got != "" {
		t.Fatalf("标记不匹配时应重新供给（返回空串），got %q", got)
	}
	// 安装失败路径必须移除 READY，下次启动重试。
	if _, err := os.Stat(readyPath(root)); !os.IsNotExist(err) {
		t.Fatalf("安装失败后 READY 应被删除, stat err=%v", err)
	}
}

// TestEnvironmentJSONRoundtrip: environment.json 写读往返与一致性判定
// （interpreter/sdk_version/base_deps_version 任一不一致、READY 缺失、
// JSON 损坏均判为不匹配）。
func TestEnvironmentJSONRoundtrip(t *testing.T) {
	root := t.TempDir()
	interpreter := "/usr/bin/python3"

	if err := writeVenvMarkers(root, interpreter, SDKVersion, baseDepsVersion); err != nil {
		t.Fatalf("writeVenvMarkers: %v", err)
	}
	env, err := readVenvEnvironment(root)
	if err != nil {
		t.Fatalf("readVenvEnvironment: %v", err)
	}
	if env.Interpreter != interpreter || env.SDKVersion != SDKVersion || env.BaseDepsVersion != baseDepsVersion {
		t.Fatalf("environment.json 往返不一致: %+v", env)
	}
	if !venvMarkersMatch(root, interpreter, SDKVersion, baseDepsVersion) {
		t.Fatal("一致时应匹配")
	}

	cases := []struct {
		name string
		run  func()
	}{
		{"interpreter 不一致", func() { _ = writeVenvMarkers(root, "/other/python3", SDKVersion, baseDepsVersion) }},
		{"sdk_version 不一致", func() { _ = writeVenvMarkers(root, interpreter, "999", baseDepsVersion) }},
		{"base_deps_version 不一致", func() { _ = writeVenvMarkers(root, interpreter, SDKVersion, baseDepsVersion+1) }},
		{"READY 缺失", func() { _ = os.Remove(readyPath(root)) }},
	}
	for _, c := range cases {
		c.run()
		if venvMarkersMatch(root, interpreter, SDKVersion, baseDepsVersion) {
			t.Errorf("%s: 不应匹配", c.name)
		}
	}

	// JSON 损坏
	if err := writeVenvMarkers(root, interpreter, SDKVersion, baseDepsVersion); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(environmentPath(root), []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if venvMarkersMatch(root, interpreter, SDKVersion, baseDepsVersion) {
		t.Error("损坏的 environment.json 不应匹配")
	}
	if _, err := readVenvEnvironment(root); err == nil {
		t.Error("损坏的 environment.json 读取应报错")
	}
}

// TestCacheDirEnvOverride: ASTRBOT_PYTHON_CACHE_DIR 指向 TempDir 时，
// EnsureVenv 的 venv 与 pythonBaseDir 都落在该目录下（测试隔离 / 嵌入式
// 设备指定可写目录）。
func TestCacheDirEnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvPythonCacheDir, dir)

	if got := userCacheDir(); got != dir {
		t.Fatalf("userCacheDir() = %q, want %q", got, dir)
	}
	if got := pythonBaseDir(); got != filepath.Join(dir, "astrbot-go", "python") {
		t.Fatalf("pythonBaseDir() = %q, want %q", got, filepath.Join(dir, "astrbot-go", "python"))
	}

	_, want := setupFakeVenv(t, dir)
	if !strings.HasPrefix(want, filepath.Join(dir, "astrbot-go", "python-venv-")) {
		t.Fatalf("venv 未落在 ASTRBOT_PYTHON_CACHE_DIR 下: %q", want)
	}
	if got := EnsureVenv(t.TempDir()); got != want {
		t.Fatalf("EnsureVenv = %q, want %q", got, want)
	}
}
