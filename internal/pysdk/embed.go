// Package pysdk manages the Python subprocess environment for plugins: the
// Python SDK runtime (downloaded from the astrbot-python-sdk GitHub repo,
// NOT embedded), interpreter discovery and the grpcio/protobuf dependency
// (installed into a dedicated venv).
package pysdk

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	sdkfs "github.com/WaterGodFurina/astrbot-golang-plugin-python-sdk/sdkfs"
)

var logger = log.GetDefault().WithComponent("PySDK")

// ErrRuntimeUnavailable is returned by PrepareRuntimeWithStage when no usable
// Python runtime can be prepared (missing interpreter / host base deps cannot
// be installed). Installers surface it to the user as a download prompt.
var ErrRuntimeUnavailable = errors.New("无法准备 Python 运行时（缺少宿主基础依赖且无法自动安装）")

// SDKRootName is the relative directory (under the data dir) that the SDK is
// downloaded/extracted to.
const SDKRootName = "python-sdk"

// sdkRepoBase 是 Python SDK 的发布仓库：下载兜底 URL
// https://github.com/WaterGodFurina/astrbot-golang-plugin-python-sdk/archive/refs/tags/v<SDKVersion>.tar.gz
// （模块解析不可用时才走网络下载）。githubProxyOverride 是 config github_proxy
// 的加速前缀（SetSDKGitHubProxy 注入，与插件安装一致）。
const sdkRepoBase = "https://github.com/WaterGodFurina/astrbot-golang-plugin-python-sdk"

var githubProxyOverride string

// SetSDKGitHubProxy overrides the GitHub accelerator prefix used for the SDK
// download (called by the host with config github_proxy; empty restores direct).
func SetSDKGitHubProxy(prefix string) {
	githubProxyOverride = strings.TrimRight(strings.TrimSpace(prefix), "/")
}

func sdkDownloadURL() string {
	u := sdkRepoBase + "/archive/refs/tags/v" + SDKVersion + ".tar.gz"
	if githubProxyOverride != "" {
		return githubProxyOverride + "/" + u
	}
	return u
}

// Python 解释器自动下载（python-build-standalone，Astral 维护的独立发行版，
// 与 uv 同源）。环境变量：
//   - ASTRBOT_PYTHON_BIN       显式指定解释器路径（最高优先）
//   - ASTRBOT_PYTHON_VERSION   python-build-standalone 版本 tag（默认 20260814）
//   - ASTRBOT_PYTHON_MIRROR    下载镜像前缀（如 https://ghfast.top/），会拼在
//     官方 URL 之前
//   - ASTRBOT_PYTHON_SKIP_VERIFY  跳过下载后的完整性检查
const (
	EnvPythonBin        = "ASTRBOT_PYTHON_BIN"
	EnvPythonVersion    = "ASTRBOT_PYTHON_VERSION"
	EnvPythonMirror     = "ASTRBOT_PYTHON_MIRROR"
	EnvPythonSkipVerify = "ASTRBOT_PYTHON_SKIP_VERIFY"

	// EnvPythonCacheDir overrides the per-user cache dir used for the Python
	// venvs (EnsureVenv) and the bundled-Python download tree (pythonBaseDir).
	// Tests set it to a t.TempDir() for isolation; embedded devices use it to
	// pin a writable location.
	EnvPythonCacheDir = "ASTRBOT_PYTHON_CACHE_DIR"

	defaultPythonVersion = "20260814"
	defaultPythonMinor   = "3.12"
	// minSupportedPythonMinor 是宿主最低支持的 Python 版本（对齐 Python SDK
	// 的 requires-python >=3.10）。低于此版本的系统解释器会被 DiscoverPythonBin
	// 拒绝，回退下载 bundled CPython（3.12）——避免 3.8 这类装不了插件的旧版本。
	minSupportedPythonMinor = "3.10"
	pyBuildStandaloneURL    = "https://github.com/astral-sh/python-build-standalone/releases/download"

	// defaultPyPIIndex 是国内默认 pip 镜像（阿里云）。用户环境可通过
	// ASTRBOT_PYPI_INDEX（或 PIP_INDEX_URL）覆盖为其他源。
	defaultPyPIIndex = "https://mirrors.aliyun.com/pypi/simple/"
)

// pyPIIndexOverride 是宿主注入的 pip 镜像（config pypi_index_url），优先于
// 环境变量与默认镜像（EnsureVenv 宿主基础依赖安装与插件 requirements 安装共用）。
var pyPIIndexOverride string

// SetPyPIIndex overrides the pip index used for host-base-dependency
// installation (called by the host with config pypi_index_url; empty restores
// the env/default resolution).
func SetPyPIIndex(url string) {
	pyPIIndexOverride = strings.TrimSpace(url)
}

// PyPIIndex returns the pip index URL: ASTRBOT_PYPI_INDEX env wins, then
// PIP_INDEX_URL, then the Aliyun mirror (fast inside mainland China).
func PyPIIndex() string {
	if pyPIIndexOverride != "" {
		return pyPIIndexOverride
	}
	for _, env := range []string{"ASTRBOT_PYPI_INDEX", "PIP_INDEX_URL"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	return defaultPyPIIndex
}

// userCacheDir returns the per-user cache dir, overridable via
// ASTRBOT_PYTHON_CACHE_DIR (venv 根目录、bundled python 下载目录都尊重它：
// 测试各自 TempDir 隔离，避免共享 ~/.cache/astrbot-go 下的 venv 与 pip
// 并发冲突；嵌入式设备可指定可写目录）。空串表示不可用（调用方兜底）。
func userCacheDir() string {
	if v := strings.TrimSpace(os.Getenv(EnvPythonCacheDir)); v != "" {
		return v
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return dir
}

// pythonBaseDir returns the per-user directory holding downloaded Python
// distributions (~/.local/share/astrbot-go/python), mirroring the Go
// toolchain's ~/.local/share/astrbot-go/toolchain. ASTRBOT_PYTHON_CACHE_DIR
// overrides the root (the python/ subdir is preserved).
func pythonBaseDir() string {
	if v := strings.TrimSpace(os.Getenv(EnvPythonCacheDir)); v != "" {
		return filepath.Join(v, "astrbot-go", "python")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	if runtime.GOOS == "windows" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "AstrBot-Go", "python")
		}
		return filepath.Join(home, "AppData", "Local", "AstrBot-Go", "python")
	}
	return filepath.Join(home, ".local", "share", "astrbot-go", "python")
}

// pyTarget maps GOOS/GOARCH to a python-build-standalone target triple.
func pyTarget() (string, error) {
	return pyTargetFor(runtime.GOOS, runtime.GOARCH)
}

// pyTargetFor is pyTarget's pure logic (testable across platforms).
func pyTargetFor(goos, goarch string) (string, error) {
	archMap := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}
	a, ok := archMap[goarch]
	if !ok {
		return "", fmt.Errorf("bundled Python is not supported on %s/%s (install python3 or set %s)",
			goos, goarch, EnvPythonBin)
	}
	switch goos {
	case "linux":
		return a + "-unknown-linux-gnu", nil
	case "darwin":
		return a + "-apple-darwin", nil
	case "windows":
		return a + "-pc-windows-msvc", nil
	}
	return "", fmt.Errorf("bundled Python is not supported on %s/%s (install python3 or set %s)",
		goos, goarch, EnvPythonBin)
}

// pyVersion returns the python-build-standalone tag (ASTRBOT_PYTHON_VERSION or
// the default). The tag is a date (e.g. 20260814); the concrete CPython micro
// version is pinned by the release itself.
func pyVersion() string {
	if v := strings.TrimSpace(os.Getenv(EnvPythonVersion)); v != "" {
		return v
	}
	return defaultPythonVersion
}

func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// DiscoverPythonBin finds the Python interpreter: ASTRBOT_PYTHON_BIN env wins,
// then PATH's python3, then a previously downloaded bundled Python.
func DiscoverPythonBin() string {
	if bin := strings.TrimSpace(os.Getenv(EnvPythonBin)); bin != "" {
		if info, err := os.Stat(bin); err == nil && !info.IsDir() {
			return bin
		}
		logger.Warn("%s 指向的路径无效: %s", EnvPythonBin, bin)
	}
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			if pythonUsable(p) {
				return p
			}
			// 不可用（Windows 商店别名桩等）→ 继续回退 bundled。
			logger.Warn("PATH 上的 %s 不可用（%s），跳过回退 bundled Python", name, p)
		}
	}
	// 已下载的 bundled Python（幂等：重启后直接复用）
	for _, minor := range []string{pyVersion(), defaultPythonVersion} {
		cand := filepath.Join(pythonBaseDir(), minor, "python", "bin", exe("python3"))
		if info, err := os.Stat(cand); err == nil && !info.IsDir() {
			return cand
		}
	}
	return ""
}

// pythonUsable 判断解释器是否真实可用：拒绝 Windows 商店的 App Execution Alias
// 桩（WindowsApps 下的 python3.exe 不是真解释器，创建 venv 必然失败 exit 9009），
// 并校验版本不低于宿主最低支持（对齐 SDK requires-python），否则视为不可用，
// 由 DiscoverPythonBin 回退下载 bundled CPython。
func pythonUsable(p string) bool {
	if strings.Contains(strings.ToLower(p), `windowsapps`) {
		return false
	}
	ver := probeMinor(p)
	if ver == "" {
		return false
	}
	if !minorAtLeast(ver, minSupportedPythonMinor) {
		logger.Warn("Python %s（%s）低于宿主最低支持版本 %s，跳过回退 bundled Python",
			ver, p, minSupportedPythonMinor)
		return false
	}
	return true
}

// probeMinor 运行解释器打印 "major.minor"（与 pythonUsable 共用同一探测），
// 失败（不可执行/超时等）返回 ""。
func probeMinor(p string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p, "-c", "import sys; print(f'{sys.version_info[0]}.{sys.version_info[1]}')")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// DetectTooLowPython 探测 PATH 上是否存在版本过低的系统 Python（低于宿主
// 最低支持版本 minSupportedPythonMinor）：存在可执行但版本过低的解释器且无
// 可用解释器时返回其版本号（如 "3.8"），否则返回 ""。仅探测 PATH，不触发
// 任何下载副作用。供插件安装时提示"系统 Python 版本过低，将自动下载
// CPython"。
func DetectTooLowPython() string {
	tooLow := ""
	for _, name := range []string{"python3", "python"} {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if strings.Contains(strings.ToLower(p), `windowsapps`) {
			continue
		}
		ver := probeMinor(p)
		if ver == "" {
			continue // 不可执行/探测失败，非"版本过低"情形
		}
		if minorAtLeast(ver, minSupportedPythonMinor) {
			// 存在可用解释器（如 python3 过低但 python 可用）→ 无需提示。
			return ""
		}
		tooLow = ver
	}
	return tooLow
}

// minorAtLeast 比较 "major.minor" 是否 >= 基准（如 "3.10"）。
func minorAtLeast(v, min string) bool {
	parse := func(s string) (int, int) {
		var a, b int
		if _, err := fmt.Sscanf(s, "%d.%d", &a, &b); err != nil {
			return -1, -1
		}
		return a, b
	}
	va, vb := parse(v)
	ma, mb := parse(min)
	if va < 0 || ma < 0 {
		return false
	}
	if va != ma {
		return va > ma
	}
	return vb >= mb
}

// EnsurePythonBin resolves an interpreter, downloading a bundled Python
// (python-build-standalone) when the system has none. stage receives
// human-readable phase text (may be nil). The downloaded tree is cached under
// ~/.local/share/astrbot-go/python/<version> and reused across restarts.
func EnsurePythonBin(stage func(string)) (string, error) {
	if py := DiscoverPythonBin(); py != "" {
		return py, nil
	}
	return downloadPython(stage)
}

// downloadPython fetches and extracts the python-build-standalone install_only
// archive, returning the interpreter path.
func downloadPython(stage func(string)) (string, error) {
	target, err := pyTarget()
	if err != nil {
		return "", err
	}
	version := pyVersion()
	// 具体 micro 版本由 release tag 决定（3.12.x + <tag>）；这里用通配由
	// archiveName 固定到该 release 的已知 micro 版本。
	archive := fmt.Sprintf("cpython-%s+%s-%s-install_only.tar.gz", bundledMicroVersion(), version, target)
	base := filepath.Join(pythonBaseDir(), version)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", fmt.Errorf("create python dir: %w", err)
	}
	dest := filepath.Join(base, archive)
	url := pyDownloadURL(archive)

	if stage != nil {
		stage("下载 Python 解释器…")
	}
	logger.Info("Downloading Python %s from %s", version, url)
	if err := downloadFile(url, dest, stage); err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}

	if stage != nil {
		stage("解压 Python 解释器…")
	}
	if err := extractTarGzKeepTop(dest, base); err != nil {
		_ = os.Remove(dest)
		return "", fmt.Errorf("extract %s: %w", archive, err)
	}
	_ = os.Remove(dest)

	bin := filepath.Join(base, "python", "bin", exe("python3"))
	if info, err := os.Stat(bin); err != nil || info.IsDir() {
		return "", fmt.Errorf("extracted Python missing interpreter: %s", bin)
	}
	logger.Info("Bundled Python 就绪: %s", bin)
	return bin, nil
}

// bundledMicroVersion returns the concrete CPython micro version shipped in
// the pinned python-build-standalone release. Kept in sync with
// defaultPythonVersion; overridable via ASTRBOT_PYTHON_MICRO for testing.
func bundledMicroVersion() string {
	if v := strings.TrimSpace(os.Getenv("ASTRBOT_PYTHON_MICRO")); v != "" {
		return v
	}
	return defaultPythonMinor + ".14"
}

// pyMirrorOverride 是宿主注入的 CPython 下载镜像前缀（插件安装下载时经
// SetPythonMirror 设置），非空时优先于 ASTRBOT_PYTHON_MIRROR 环境变量与官方
// URL。对齐 githubProxyOverride（SetSDKGitHubProxy）的注入模式。
var pyMirrorOverride string

// SetPythonMirror overrides the mirror prefix used by pyDownloadURL (called by
// the host with the user's chosen mirror before a plugin-install download;
// empty restores env/default resolution).
func SetPythonMirror(prefix string) {
	pyMirrorOverride = strings.TrimRight(strings.TrimSpace(prefix), "/")
}

// pyDownloadURL applies the mirror prefix to the official release URL: the
// SetPythonMirror override wins, then ASTRBOT_PYTHON_MIRROR.
func pyDownloadURL(archive string) string {
	u := pyBuildStandaloneURL + "/" + pyVersion() + "/" + archive
	if pyMirrorOverride != "" {
		return pyMirrorOverride + "/" + u
	}
	if m := strings.TrimSpace(os.Getenv(EnvPythonMirror)); m != "" {
		return strings.TrimRight(m, "/") + "/" + u
	}
	return u
}

// dlClient 是 Python 解释器下载专用 HTTP 客户端：30 分钟超时防止镜像源
// 挂起时下载永远阻塞（与 Go 工具链下载对齐）。Transport 沿用默认配置，
// 仍会走宿主的全局代理。
var dlClient = &http.Client{Timeout: 30 * time.Minute}

// downloadFile streams url to dest with 10%-step progress logs and the stage
// callback (download phase text). The default http.Client transport honors
// the host's global proxy configuration.
func downloadFile(url, dest string, stage func(string)) error {
	resp, err := dlClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	out, err := os.Create(dest) // #nosec G304 -- dest is the caller-chosen download destination under the cache dir
	if err != nil {
		return err
	}
	defer out.Close()

	total := resp.ContentLength
	var written int64
	lastPct := -1
	buf := make([]byte, 64*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
			written += int64(n)
			if total > 0 {
				pct := int(written * 100 / total)
				if pct != lastPct && pct%10 == 0 {
					lastPct = pct
					logger.Info("Downloading Python: %d%% (%s / %s)", pct, humanSize(written), humanSize(total))
					if stage != nil {
						stage(fmt.Sprintf("下载 Python 解释器… %d%%", pct))
					}
				}
			}
		}
		if rerr == io.EOF {
			return out.Sync()
		}
		if rerr != nil {
			return rerr
		}
	}
}

// humanSize formats a byte count as a human-readable size.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// extractTarGzKeepTop unpacks a tar.gz into dir, preserving the leading "python/"
// component (python-build-standalone install_only layout: python/bin/python3).
func extractTarGzKeepTop(src, dir string) error {
	f, err := os.Open(src) // #nosec G304 -- src is the just-downloaded archive under the cache dir
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := hdr.Name
		// 保留 python/ 顶层目录，完整路径解到 dir（安全 join 防穿越）
		if name == "" {
			continue
		}
		target, err := safeJoin(dir, name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			w, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(uint32(hdr.Mode&0o777))) // #nosec G304 -- target validated by safeJoin
			if err != nil {
				return err
			}
			// #nosec decompression_bomb -- Python SDK/CPython 归档来自受信任的固定 URL（宿主自控下载源，
			// 版本固定），safeJoin 已防路径穿越。
			if _, err := io.Copy(w, tr); err != nil { // nosemgrep: go.lang.security.decompression_bomb.potential-dos-via-decompression-bomb
				_ = w.Close()
				return err
			}
			if err := w.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			_ = os.Remove(target)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			link := hdr.Linkname
			// 拒绝绝对路径目标与逃逸根目录的相对目标，防止恶意归档利用
			// symlink 把后续条目写到根目录之外。
			if filepath.IsAbs(link) {
				return fmt.Errorf("symlink %q -> absolute target %q rejected", name, link)
			}
			if _, err := safeJoin(filepath.Dir(target), link); err != nil {
				return fmt.Errorf("symlink %q -> %q escapes root: %w", name, link, err)
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		}
	}
}

// safeJoin joins root + name, rejecting path traversal (.. / absolute paths).
func safeJoin(root, name string) (string, error) {
	target := filepath.Join(root, filepath.FromSlash(name))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes root: %q", name)
	}
	return target, nil
}

// runtimeEnv holds the resolved Python subprocess environment.
type RuntimeEnv struct {
	// PythonBin is the interpreter used to launch plugin subprocesses.
	PythonBin string
	// SDKDir is the extracted SDK root (contains the astrbot/ package).
	SDKDir string
	// PyPath is the PYTHONPATH value (SDK dir + plugin dirs).
	PyPath string
}

// sdkModuleDir resolves the Python SDK directory via the Go module graph
// (`go list -m -f {{.Dir}} <module>` run from the working directory, which the
// host shares with the module's go.mod). With a local replace in go.mod the
// module resolves to the development checkout; in release builds it resolves
// to the module cache (requires the module to have been downloaded, e.g. at
// build time). Returns "" when the module cannot be resolved (no go.mod /
// module graph) — the caller falls back to the GitHub download.
func sdkModuleDir() string {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", sdkfs.ModulePath).Output()
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(dir, "astrbot", "__init__.py")); err != nil {
		return ""
	}
	return dir
}

// Ensure prepares the Python SDK at <dataDir>/python-sdk and returns the SDK
// root as an ABSOLUTE path (the subprocess cwd may differ, so relative paths
// in PYTHONPATH would not resolve). Resolution order:
//  1. Go module dir (`go list -m`, honors the go.mod require/replace) — no
//     copy needed, the versioned module source is used in place;
//  2. otherwise the repo tarball is downloaded from GitHub at tag
//     v<SDKVersion> and extracted (version marker written next to it; a
//     marker mismatch wipes and re-fetches).
func Ensure(dataDir string) (string, error) {
	absData, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("resolve data dir: %w", err)
	}
	root := filepath.Join(absData, SDKRootName)

	// 1) Go 模块解析优先（require/replace 声明的版本化源码，无需落盘拷贝）。
	if dir := sdkModuleDir(); dir != "" {
		logger.Info("Python SDK 经 Go 模块解析: %s (v%s)", dir, SDKVersion)
		return dir, nil
	}

	marker := filepath.Join(root, "astrbot", "__init__.py")
	versionFile := filepath.Join(root, "VERSION")
	needFetch := false
	if _, err := os.Stat(marker); err != nil {
		needFetch = true
	} else if data, err := os.ReadFile(versionFile); err != nil || strings.TrimSpace(string(data)) != SDKVersion { // #nosec G304 -- versionFile is a host-controlled marker under root
		logger.Info("Python SDK 版本变化（磁盘 %q vs 期望 %q），重新下载…",
			strings.TrimSpace(string(data)), SDKVersion)
		needFetch = true
		_ = os.RemoveAll(root)
	}
	if !needFetch {
		return root, nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("创建 Python SDK 目录: %w", err)
	}

	// 2) 下载 SDK 仓库 tarball（GitHub 归档布局 <repo>-v<ver>/…，顶层目录剥离）。
	url := sdkDownloadURL()
	logger.Info("下载 Python SDK v%s（%s）…", SDKVersion, url)
	tmp, err := os.CreateTemp("", "astrbot-pysdk-*.tar.gz")
	if err != nil {
		return "", fmt.Errorf("创建临时文件: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := downloadFile(url, tmpPath, nil); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("下载 Python SDK 失败（%s）: %w", url, err)
	}
	_ = tmp.Close()

	if err := extractTarGzStripTop(tmpPath, root); err != nil {
		_ = os.RemoveAll(root)
		return "", fmt.Errorf("解压 Python SDK: %w", err)
	}
	if _, err := os.Stat(marker); err != nil {
		_ = os.RemoveAll(root)
		return "", fmt.Errorf("python SDK 下载内容缺少 astrbot/ 包（标记 %s 不存在）", marker)
	}
	if err := os.WriteFile(versionFile, []byte(SDKVersion+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("写入 Python SDK 版本标记: %w", err)
	}
	logger.Info("Python SDK 已就绪 %s (v%s)", root, SDKVersion)
	return root, nil
}

// extractTarGzStripTop unpacks a GitHub-style archive into dir, stripping a
// single top-level directory ("<repo>-v<ver>/..." layout).
func extractTarGzStripTop(src, dir string) error {
	f, err := os.Open(src) // #nosec G304 -- src is the just-downloaded SDK archive (temp file)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	var top string
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := hdr.Name
		if name == "" {
			continue
		}
		// GitHub 生成的 tar.gz 首条是 pax_global_header 元数据伪条目（非目录，
		// 无任何文件内容）。若把它当作顶层目录，后续真实路径（<repo>-v<ver>/…）
		// 全部无法剥离，解压结果会错位/丢失。必须跳过。
		if name == "pax_global_header" {
			continue
		}
		parts := strings.SplitN(strings.TrimPrefix(name, "./"), "/", 2)
		if top == "" && len(parts) > 0 {
			top = parts[0]
		}
		rel := name
		if top != "" && (name == top || strings.HasPrefix(name, top+"/")) {
			rel = strings.TrimPrefix(strings.TrimPrefix(name, top), "/")
		}
		if rel == "" {
			continue
		}
		target, err := safeJoin(dir, rel)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			w, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(uint32(hdr.Mode&0o777))) // #nosec G304 -- target validated by safeJoin
			if err != nil {
				return err
			}
			// #nosec decompression_bomb -- Python SDK/CPython 归档来自受信任的固定 URL（宿主自控下载源，
			// 版本固定），safeJoin 已防路径穿越。
			if _, err := io.Copy(w, tr); err != nil { // nosemgrep: go.lang.security.decompression_bomb.potential-dos-via-decompression-bomb
				_ = w.Close()
				return err
			}
			if err := w.Close(); err != nil {
				return err
			}
		}
	}
}

// EnsureVenv makes sure a venv with grpcio+protobuf exists (under the user
// cache dir, shared across data dirs and restarts), returning the venv python
// path. Returns "" when grpcio is unavailable and no venv can be prepared
// (caller falls back to the system interpreter or fails with a clear message).
// The venv is bound to the interpreter: a different base interpreter gets its
// own venv so switching between system Python and the bundled Python cannot
// reuse a stale environment.
//
// A venv is considered ready when its READY marker and environment.json exist
// and match the current interpreter / SDKVersion / baseDepsVersion; ready
// venvs are reused without re-probing imports (fast startup). Missing or
// mismatched markers trigger a host-deps reinstall under a cross-process lock
// (pip 并发安全；见 venv_lock_*.go)。
func EnsureVenv(dataDir string) string {
	venvPython, err := ensureVenvReady(dataDir)
	if err != nil {
		logger.Warn("Python venv 准备失败: %v（插件将无法启动）", err)
		return ""
	}
	return venvPython
}

// venvLockTimeout bounds how long a provisioner waits for the cross-process
// venv lock (a pip install normally finishes in a few minutes; 10 分钟超时
// 防止死锁）。
var venvLockTimeout = 10 * time.Minute

// ensureVenvReady resolves a Python interpreter that has the host base deps
// importable: a cached venv whose markers match is returned without probing;
// incomplete venvs (marker missing/mismatched, or a legacy venv without
// environment.json whose deps probe fails) are re-provisioned under the venv
// lock. 返回 "" 语义由调用方（EnsureVenv）处理。
func ensureVenvReady(dataDir string) (string, error) {
	cacheDir := userCacheDir()
	if cacheDir == "" {
		cacheDir = filepath.Join(dataDir, SDKRootName)
	}
	base := DiscoverPythonBin()
	if base == "" {
		return "", errors.New("未找到 Python 解释器（请安装 python3 或设置 " + EnvPythonBin + "）")
	}
	// 解释器指纹：系统 Python 与 bundled Python 各用独立 venv（保持原有
	// fingerprint 算法不变，已有 venv 目录名不失效）。
	sum := sha256.Sum256([]byte(base))
	fingerprint := hex.EncodeToString(sum[:6])
	root := filepath.Join(cacheDir, "astrbot-go", "python-venv-"+fingerprint)
	venvPython := filepath.Join(root, "bin", "python")
	if runtime.GOOS == "windows" {
		venvPython = filepath.Join(root, "Scripts", "python.exe")
	}

	// 快速路径：venv python 存在 + READY + environment.json 三者一致 →
	// 直接复用，不再跑 hasHostDeps import 探测。
	if info, err := os.Stat(venvPython); err == nil && !info.IsDir() &&
		venvMarkersMatch(root, base, SDKVersion, baseDepsVersion) {
		return venvPython, nil
	}

	venvExists := false
	if info, err := os.Stat(venvPython); err == nil && !info.IsDir() {
		venvExists = true
		// 旧版 venv 迁移：无 environment.json 但有完整依赖 → 补写标记直接复用
		//（避免已部署环境重新 pip 装一遍）。
		if _, err := os.Stat(environmentPath(root)); os.IsNotExist(err) && hasHostDeps(venvPython) {
			if werr := writeVenvMarkers(root, base, SDKVersion, baseDepsVersion); werr != nil {
				logger.Warn("补写 venv 标记失败: %v", werr)
			}
			return venvPython, nil
		}
	}

	// 宿主解释器本身已具备全部依赖 → 直接使用（无需 venv）。
	if !venvExists && hasHostDeps(base) {
		return base, nil
	}

	// 需要创建 venv 或修复：跨进程锁内进行（等锁期间他人可能已装好 →
	// 锁内双重检查）。
	lockPath := filepath.Join(cacheDir, "astrbot-go", ".venv-"+fingerprint+".lock")
	release, err := acquireVenvLock(lockPath, venvLockTimeout)
	if err != nil {
		return "", fmt.Errorf("获取 venv 锁失败: %w", err)
	}
	defer release()

	if info, err := os.Stat(venvPython); err == nil && !info.IsDir() {
		// 等锁期间其他人已修复完成 → 直接复用。
		if venvMarkersMatch(root, base, SDKVersion, baseDepsVersion) {
			return venvPython, nil
		}
		if _, err := os.Stat(environmentPath(root)); os.IsNotExist(err) && hasHostDeps(venvPython) {
			if werr := writeVenvMarkers(root, base, SDKVersion, baseDepsVersion); werr != nil {
				logger.Warn("补写 venv 标记失败: %v", werr)
			}
			return venvPython, nil
		}
		logger.Warn("venv 宿主依赖不完整（标记缺失/不匹配），锁内重新安装…")
	} else {
		logger.Info("Python %s 缺少宿主基础依赖，创建独立 venv 安装…", base)
		if err := exec.Command(base, "-m", "venv", root).Run(); err != nil { // #nosec G204 -- Python SDK venv 初始化核心：base 为宿主探测到的解释器，root 为宿主生成的目录路径; nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
			logger.Warn("创建 venv 失败: %v（插件将无法启动）", err)
			return "", fmt.Errorf("创建 venv 失败: %w", err)
		}
	}
	if err := installHostDeps(venvPython); err != nil {
		// 安装失败：移除 READY，下次启动重试。
		_ = os.Remove(readyPath(root))
		return "", fmt.Errorf("venv 安装宿主依赖失败: %w", err)
	}
	if err := writeVenvMarkers(root, base, SDKVersion, baseDepsVersion); err != nil {
		return "", fmt.Errorf("写入 venv 标记失败: %w", err)
	}
	return venvPython, nil
}

// hostBaseDeps 是 Python AstrBot 本体的常驻依赖子集（插件不声明但依赖，
// 因为在本体中天然存在）：Web 框架 / HTTP 客户端 / 序列化 / 图像 / 通用
// 工具 / 平台 SDK。安装在宿主 venv 里，使大量 Python 插件开箱可用；安装在
// 首次创建 venv 时一次性执行（走默认 pip 镜像，见 PyPIIndex）。
// 对齐 Python AstrBot requirements.txt：aiocqhttp（OneBot）、apscheduler、
// tenacity、openai/anthropic/dashscope（LLM）、qq-botpy、python-telegram-bot
// 等为插件最常 import 的本体常驻库；重型（pandas/faiss/sqlmodel 等）留给
// 插件 requirements.txt 自装。
var hostBaseDeps = []string{
	"grpcio", "protobuf",
	"quart", "werkzeug", "jinja2",
	"aiohttp", "httpx", "requests", "mcp",
	"pydantic", "pyyaml", "pillow",
	"deprecated", "docstring-parser", "markdown", "psutil",
	"websockets", "apscheduler", "tenacity",
	"openai", "anthropic", "dashscope",
	"qq-botpy", "python-telegram-bot",
	"cryptography", "qrcode", "packaging", "pyjwt",
	"jieba", "rank-bm25", "pydub", "openpyxl", "pypdf",
	"click", "aiofiles",
}

// hostDepProbes 是 hostBaseDeps 的关键模块探测表（import 名）：任一缺失即
// 视为宿主依赖不完整，启动 Python 插件前自动补齐（EnsureVenv 检查用）。
var hostDepProbes = []string{
	"grpc", "google.protobuf",
	"quart", "werkzeug", "jinja2",
	"aiohttp", "httpx", "requests", "mcp", "apscheduler", "tenacity",
	"openai", "anthropic", "dashscope",
	"pydantic", "yaml", "PIL", "deprecated", "docstring_parser", "markdown", "psutil",
	"websockets", "cryptography", "qrcode", "packaging", "jwt",
	"jieba", "rank_bm25", "pydub", "openpyxl", "pypdf", "click", "aiofiles",
	// qq-botpy 的 import 包名是 botpy；python-telegram-bot 是 telegram。
	"botpy",
}

// baseDepsVersion 是宿主依赖清单（hostBaseDeps/hostDepProbes）的版本号：
// 修改清单内容时手动 +1，触发既有 venv 的 environment.json 不匹配而重新
// pip 安装（否则 venv 一旦 READY 就永久复用，清单变化不会生效）。
const baseDepsVersion = 3

const (
	// envFileName 记录 venv 的供给来源（解释器 / SDK 版本 / 依赖清单版本）。
	envFileName = "environment.json"
	// readyFileName 是空文件标记：安装成功后才写入；安装失败时删除，
	// 下次启动视为不完整而重新供给。
	readyFileName = "READY"
)

// venvEnvironment 是 environment.json 的结构（venv 根目录下）。
type venvEnvironment struct {
	Interpreter     string `json:"interpreter"`
	SDKVersion      string `json:"sdk_version"`
	BaseDepsVersion int    `json:"base_deps_version"`
}

func environmentPath(venvRoot string) string { return filepath.Join(venvRoot, envFileName) }
func readyPath(venvRoot string) string       { return filepath.Join(venvRoot, readyFileName) }

// readVenvEnvironment reads and parses environment.json (error when missing
// or malformed).
func readVenvEnvironment(venvRoot string) (venvEnvironment, error) {
	data, err := os.ReadFile(environmentPath(venvRoot))
	if err != nil {
		return venvEnvironment{}, err
	}
	var env venvEnvironment
	if err := json.Unmarshal(data, &env); err != nil {
		return venvEnvironment{}, err
	}
	return env, nil
}

// writeVenvMarkers atomically-ish writes environment.json and the READY marker
// for a provisioned venv.
func writeVenvMarkers(venvRoot, interpreter, sdkVersion string, depsVersion int) error {
	env := venvEnvironment{Interpreter: interpreter, SDKVersion: sdkVersion, BaseDepsVersion: depsVersion}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(venvRoot, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(environmentPath(venvRoot), data, 0o600); err != nil {
		return err
	}
	f, err := os.OpenFile(readyPath(venvRoot), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// venvMarkersMatch reports whether the venv at venvRoot is marked READY with an
// environment.json matching the given interpreter / SDK version / deps version
// (快路径校验：只读两个文件，不跑 python import 探测）。
func venvMarkersMatch(venvRoot, interpreter, sdkVersion string, depsVersion int) bool {
	if info, err := os.Stat(readyPath(venvRoot)); err != nil || info.IsDir() {
		return false
	}
	env, err := readVenvEnvironment(venvRoot)
	if err != nil {
		return false
	}
	return env.Interpreter == interpreter && env.SDKVersion == sdkVersion && env.BaseDepsVersion == depsVersion
}

// hasHostDeps reports whether all key host base dependencies are importable in
// the given interpreter. Missing ones trigger a venv re-provisioning
// (installHostDeps) so Python plugins that rely on Python-AstrBot's resident
// deps (e.g. aiocqhttp) start without a module-not-found crash.
func hasHostDeps(pythonBin string) bool {
	script := "import importlib; mods=" + strconv.Quote(strings.Join(hostDepProbes, " ")) + "; missing=[m for m in mods.split() if importlib.util.find_spec(m) is None]; import sys; sys.exit(1 if missing else 0)"
	cmd := exec.Command(pythonBin, "-c", script) // #nosec G204 -- 宿主依赖探测脚本：script 由固定常量 hostDepProbes 拼装并经 strconv.Quote 转义，pythonBin 为宿主解析的解释器路径; nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	return cmd.Run() == nil
}

func installHostDeps(pythonBin string) error {
	args := append([]string{"-m", "pip", "install", "--disable-pip-version-check", "-q"}, hostBaseDeps...)
	args = append(args, "-i", PyPIIndex())
	out, err := exec.Command(pythonBin, args...).CombinedOutput() // #nosec G204 -- pip 安装插件宿主基础依赖：args 由固定常量 hostBaseDeps + 固定 pip 参数组成，pythonBin 为宿主解析的解释器路径; nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	if err != nil {
		logger.Warn("pip install 输出: %s", strings.TrimSpace(string(out)))
		return err
	}
	logger.Info("venv 宿主基础依赖安装完成（grpcio/protobuf + 本体常驻依赖）")
	return nil
}

// PrepareRuntime resolves the full Python subprocess environment for plugin
// launch. It must be called lazily (first Python plugin load), not at startup,
// because it may download packages.
func PrepareRuntime(dataDir string) (*RuntimeEnv, error) {
	return PrepareRuntimeWithStage(dataDir, nil)
}

// PrepareRuntimeWithStage is PrepareRuntime with a stage callback receiving
// human-readable phase text (e.g. "下载 Python 解释器…") surfaced to the WebUI
// install dialog.
func PrepareRuntimeWithStage(dataDir string, stage func(string)) (*RuntimeEnv, error) {
	sdkDir, err := Ensure(dataDir)
	if err != nil {
		return nil, err
	}
	py, err := EnsurePythonBin(stage)
	if err != nil {
		return nil, err
	}
	if !hasHostDeps(py) {
		py = EnsureVenv(dataDir)
	}
	if py == "" {
		return nil, ErrRuntimeUnavailable
	}
	return &RuntimeEnv{
		PythonBin: py,
		SDKDir:    sdkDir,
		PyPath:    sdkDir,
	}, nil
}

// Env returns the OS environment for a Python plugin subprocess. pluginDir is
// prepended to PYTHONPATH so the plugin's own modules import first. Both
// paths are made absolute: the subprocess runs with a different cwd
// (data/plugins_data/<id>), so relative entries would silently not resolve.
func (r *RuntimeEnv) Env(pluginDir, dataDir string) []string {
	sep := ":"
	if runtime.GOOS == "windows" {
		sep = ";"
	}
	env := os.Environ()
	pypath := r.PyPath
	if pluginDir != "" {
		if abs, err := filepath.Abs(pluginDir); err == nil {
			pluginDir = abs
		}
		pypath = pluginDir + sep + pypath
	}
	if absData, err := filepath.Abs(dataDir); err == nil {
		dataDir = absData
	}
	env = append(env,
		"PYTHONPATH="+pypath,
		"PYTHONUNBUFFERED=1",
		"ASTRBOT_DATA_PATH="+dataDir,
		"ASTRBOT_PLUGIN_DATA_DIR="+filepath.Join(dataDir, "plugins_data"),
	)
	return env
}
