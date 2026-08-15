package pysdk

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDiscoverBundledPython verifies that a previously downloaded bundled
// Python is discovered without any system interpreter (PATH stripped).
func TestDiscoverBundledPython(t *testing.T) {
	oldPath := os.Getenv("PATH")
	oldBin := os.Getenv(EnvPythonBin)
	oldHome := os.Getenv("HOME")
	t.Cleanup(func() {
		os.Setenv("PATH", oldPath)
		os.Setenv("HOME", oldHome)
		if oldBin == "" {
			os.Unsetenv(EnvPythonBin)
		} else {
			os.Setenv(EnvPythonBin, oldBin)
		}
	})

	// 临时 HOME + 空 PATH（无系统 python）
	home := t.TempDir()
	os.Setenv("HOME", home)
	os.Setenv("PATH", t.TempDir())
	os.Unsetenv(EnvPythonBin)

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
			os.Unsetenv(EnvPythonMirror)
		} else {
			os.Setenv(EnvPythonMirror, old)
		}
	})
	os.Setenv(EnvPythonMirror, "https://ghfast.top/")
	u := pyDownloadURL("cpython-x.tar.gz")
	if u != "https://ghfast.top/https://github.com/astral-sh/python-build-standalone/releases/download/"+pyVersion()+"/cpython-x.tar.gz" {
		t.Fatalf("mirror URL 异常: %s", u)
	}
}

