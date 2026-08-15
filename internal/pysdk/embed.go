// Package pysdk embeds the Python plugin runtime (the astrbot-compatible
// package + gRPC bridge) into the host binary and manages the Python
// subprocess environment: SDK extraction, interpreter discovery and the
// grpcio/protobuf dependency (installed into a dedicated venv).
package pysdk

import (
	"crypto/sha256"
	"encoding/hex"
	"archive/tar"
	"compress/gzip"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

//go:embed all:astrbot
var sdkFS embed.FS

var logger = log.GetDefault().WithComponent("PySDK")

// SDKRootName is the relative directory (under the data dir) that the
// embedded SDK is extracted to.
const SDKRootName = "python-sdk"

// SDKVersion must be bumped whenever the embedded Python SDK (internal/pysdk/
// astrbot) changes: Ensure() re-extracts the SDK when the on-disk version
// differs, so a restarted host picks up SDK updates automatically.
const SDKVersion = "14"

// Python 解释器自动下载（python-build-standalone，Astral 维护的独立发行版，
// 与 uv 同源）。环境变量：
//   - ASTRBOT_PYTHON_BIN       显式指定解释器路径（最高优先）
//   - ASTRBOT_PYTHON_VERSION   python-build-standalone 版本 tag（默认 20260814）
//   - ASTRBOT_PYTHON_MIRROR    下载镜像前缀（如 https://ghfast.top/），会拼在
//                              官方 URL 之前
//   - ASTRBOT_PYTHON_SKIP_VERIFY  跳过下载后的完整性检查
const (
	EnvPythonBin        = "ASTRBOT_PYTHON_BIN"
	EnvPythonVersion    = "ASTRBOT_PYTHON_VERSION"
	EnvPythonMirror     = "ASTRBOT_PYTHON_MIRROR"
	EnvPythonSkipVerify = "ASTRBOT_PYTHON_SKIP_VERIFY"

	defaultPythonVersion = "20260814"
	defaultPythonMinor   = "3.12"
	pyBuildStandaloneURL = "https://github.com/astral-sh/python-build-standalone/releases/download"

	// defaultPyPIIndex 是国内默认 pip 镜像（阿里云）。用户环境可通过
	// ASTRBOT_PYPI_INDEX（或 PIP_INDEX_URL）覆盖为其他源。
	defaultPyPIIndex = "https://mirrors.aliyun.com/pypi/simple/"
)

// PyPIIndex returns the pip index URL: ASTRBOT_PYPI_INDEX env wins, then
// PIP_INDEX_URL, then the Aliyun mirror (fast inside mainland China).
func PyPIIndex() string {
	for _, env := range []string{"ASTRBOT_PYPI_INDEX", "PIP_INDEX_URL"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	return defaultPyPIIndex
}

// pythonBaseDir returns the per-user directory holding downloaded Python
// distributions (~/.local/share/astrbot-go/python), mirroring the Go
// toolchain's ~/.local/share/astrbot-go/toolchain.
func pythonBaseDir() string {
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
			return p
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

// pyDownloadURL applies the mirror prefix (ASTRBOT_PYTHON_MIRROR) to the
// official release URL.
func pyDownloadURL(archive string) string {
	u := pyBuildStandaloneURL + "/" + pyVersion() + "/" + archive
	if m := strings.TrimSpace(os.Getenv(EnvPythonMirror)); m != "" {
		return strings.TrimRight(m, "/") + "/" + u
	}
	return u
}

// downloadFile streams url to dest with 10%-step progress logs and the stage
// callback (download phase text). The default http.Client transport honors
// the host's global proxy configuration.
func downloadFile(url, dest string, stage func(string)) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	out, err := os.Create(dest)
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
	f, err := os.Open(src)
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
			w, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(w, tr); err != nil {
				w.Close()
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
			if err := os.Symlink(hdr.Linkname, target); err != nil {
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

// Ensure extracts the embedded SDK into <dataDir>/python-sdk (idempotent) and
// returns the SDK root as an ABSOLUTE path (the subprocess cwd may differ, so
// relative paths in PYTHONPATH would not resolve). A version marker file is
// written next to the SDK; when the marker differs from the embedded
// SDKVersion the stale copy is wiped and re-extracted (SDK updates otherwise
// would never reach already-running data dirs).
func Ensure(dataDir string) (string, error) {
	absData, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("resolve data dir: %w", err)
	}
	root := filepath.Join(absData, SDKRootName)
	marker := filepath.Join(root, "astrbot", "__init__.py")
	versionFile := filepath.Join(root, "VERSION")
	needExtract := false
	if _, err := os.Stat(marker); err != nil {
		needExtract = true
	} else if data, err := os.ReadFile(versionFile); err != nil || strings.TrimSpace(string(data)) != SDKVersion {
		logger.Info("Python SDK 版本变化（磁盘 %q vs 嵌入 %q），重新解压…",
			strings.TrimSpace(string(data)), SDKVersion)
		needExtract = true
		_ = os.RemoveAll(root)
	}
	if !needExtract {
		return root, nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("创建 Python SDK 目录: %w", err)
	}
	if err := fs.WalkDir(sdkFS, "astrbot", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(root, path)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := sdkFS.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	}); err != nil {
		return "", fmt.Errorf("解压 Python SDK: %w", err)
	}
	if err := os.WriteFile(versionFile, []byte(SDKVersion+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("写入 Python SDK 版本标记: %w", err)
	}
	logger.Info("Python SDK 已解压到 %s (v%s)", root, SDKVersion)
	return root, nil
}

// EnsureVenv makes sure a venv with grpcio+protobuf exists (under the user
// cache dir, shared across data dirs and restarts), returning the venv python
// path. Returns "" when grpcio is unavailable and no venv can be prepared
// (caller falls back to the system interpreter or fails with a clear message).
// The venv is bound to the interpreter: a different base interpreter gets its
// own venv so switching between system Python and the bundled Python cannot
// reuse a stale environment.
func EnsureVenv(dataDir string) string {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = filepath.Join(dataDir, SDKRootName)
	}
	base := DiscoverPythonBin()
	if base == "" {
		return ""
	}
	// 解释器指纹：系统 Python 与 bundled Python 各用独立 venv
	sum := sha256.Sum256([]byte(base))
	fingerprint := hex.EncodeToString(sum[:6])
	root := filepath.Join(cacheDir, "astrbot-go", "python-venv-"+fingerprint)
	venvPython := filepath.Join(root, "bin", "python")
	if runtime.GOOS == "windows" {
		venvPython = filepath.Join(root, "Scripts", "python.exe")
	}
	if info, err := os.Stat(venvPython); err == nil && !info.IsDir() {
		if hasGRPC(venvPython) {
			return venvPython
		}
		logger.Warn("venv 缺少 grpcio，尝试安装…")
		if err := installGRPC(venvPython); err != nil {
			logger.Warn("venv 安装 grpcio 失败: %v", err)
			return ""
		}
		return venvPython
	}
	if hasGRPC(base) {
		return base
	}
	logger.Info("Python %s 缺少 grpcio/protobuf，创建独立 venv 安装…", base)
	venvDir := root
	if err := exec.Command(base, "-m", "venv", venvDir).Run(); err != nil {
		logger.Warn("创建 venv 失败: %v（插件将无法启动）", err)
		return ""
	}
	if err := installGRPC(venvPython); err != nil {
		logger.Warn("venv 安装 grpcio 失败: %v（插件将无法启动）", err)
		return ""
	}
	return venvPython
}

func hasGRPC(pythonBin string) bool {
	cmd := exec.Command(pythonBin, "-c", "import grpc, google.protobuf")
	return cmd.Run() == nil
}

// hostBaseDeps 是 Python AstrBot 本体的常驻依赖子集（插件不声明但依赖，
// 因为在本体中天然存在）：Web 框架 / HTTP 客户端 / 序列化 / 图像 / 通用
// 工具。安装在宿主 venv 里，使大量 Python 插件开箱可用。安装在首次创建
// venv 时一次性执行（走默认 pip 镜像，见 PyPIIndex）。
var hostBaseDeps = []string{
	"grpcio", "protobuf",
	"quart", "werkzeug", "jinja2",
	"aiohttp", "httpx", "requests",
	"pydantic", "pyyaml", "pillow",
	"deprecated", "docstring-parser", "markdown", "psutil",
}

func installGRPC(pythonBin string) error {
	args := append([]string{"-m", "pip", "install", "--disable-pip-version-check", "-q"}, hostBaseDeps...)
	args = append(args, "-i", PyPIIndex())
	out, err := exec.Command(pythonBin, args...).CombinedOutput()
	if err != nil {
		logger.Warn("pip install 输出: %s", strings.TrimSpace(string(out)))
		return err
	}
	logger.Info("venv 基础依赖安装完成（grpcio/protobuf + 本体常驻依赖）")
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
	if !hasGRPC(py) {
		py = EnsureVenv(dataDir)
	}
	if py == "" {
		return nil, fmt.Errorf("无法准备 Python 运行时（缺少 grpcio/protobuf 且无法自动安装）")
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
