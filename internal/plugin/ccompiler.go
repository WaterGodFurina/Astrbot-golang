package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mholt/archives"

	"github.com/WaterGodFurina/Astrbot-golang/internal/toolchain"
)

// CCompilerPromptKind identifies which prompt the user must answer before a
// cgo-requiring plugin can be built.
type CCompilerPromptKind string

const (
	// PromptChooseCompiler is shown when a system GCC exists: the user picks
	// GCC, Clang (bundled/downloaded), or cancels.
	PromptChooseCompiler CCompilerPromptKind = "choose_compiler"
	// PromptDownloadClang is shown when no C compiler is present at all: the
	// user chooses whether to download Clang or cancel.
	PromptDownloadClang CCompilerPromptKind = "download_clang"
)

// CCompilerChoice is the user's answer to a CCompilerPromptError.
type CCompilerChoice string

const (
	CCChoiceGCC      CCompilerChoice = "gcc"      // use the system GCC (CC/CXX env)
	CCChoiceClang    CCompilerChoice = "clang"    // use bundled/auto-downloaded Clang
	CCChoiceDownload CCompilerChoice = "download" // auto-download Clang
	CCChoiceCancel   CCompilerChoice = "cancel"   // abort installation
)

// CCompilerPromptError is returned by InstallFromSource when a plugin declares
// cgo but the host cannot (or the user has not decided to) pick a C compiler.
// The dashboard surfaces it to the WebUI so the user can review the options.
type CCompilerPromptError struct {
	// Kind is which dialog the frontend should render.
	Kind CCompilerPromptKind `json:"kind"`
	// HasGCC reports whether a usable system GCC was detected (only meaningful
	// for PromptChooseCompiler).
	HasGCC bool `json:"has_gcc"`
	// GCCPath / GCCXXPath are the detected system compiler paths.
	GCCPath  string `json:"gcc_path,omitempty"`
	GCCXXPath string `json:"gcc_xx_path,omitempty"`
	// GCCVersion is a short version string of the detected GCC (for display).
	GCCVersion string `json:"gcc_version,omitempty"`
}

func (e *CCompilerPromptError) Error() string {
	switch e.Kind {
	case PromptChooseCompiler:
		return "检测到系统已安装 GCC，需要选择使用 GCC 还是 Clang"
	default:
		return "未检测到 C 编译器，需要确认是否自动下载并安装 Clang"
	}
}

// ClangEnv is returned by the compiler-selection logic: the resolved CC/CXX
// paths to put into the build environment.
type ClangEnv struct {
	CC  string
	CXX string
}

// ensureCCompiler resolves the C compiler to use for a cgo plugin.
//
// The flow (all prompts surface through WebUI/CLI via CCompilerPromptError):
//
//  1. If the user already answered (options.CCChoice), honor it:
//     - gcc      → return the detected system GCC/CC/CXX.
//     - clang    → use bundled/downloaded Clang (download if needed).
//     - download → download Clang for the current platform.
//     - cancel   → return a user-cancelled error.
//  2. Otherwise detect: if a system GCC exists (via ASTRBOT_CC > CC > PATH
//     gcc), return a PromptChooseCompiler error; if only Clang exists it is
//     used directly; if neither exists, return a PromptDownloadClang error.
//
// It returns the compiler paths (ccPath/cxxPath) once the user choice is
// resolved, or an error (CCompilerPromptError for decisions still needed).
func ensureCCompiler(ctx context.Context, options InstallOptions) (ccPath, cxxPath string, err error) {
	choice := CCompilerChoice(strings.ToLower(strings.TrimSpace(options.CCChoice)))
	if choice != "" {
		return resolveCCChoice(ctx, choice, options)
	}

	// No explicit choice yet: detect what the host already has.
	if gcc, cxx, ver, ok := detectSystemGCC(); ok {
		return "", "", &CCompilerPromptError{
			Kind:       PromptChooseCompiler,
			HasGCC:     true,
			GCCPath:    gcc,
			GCCXXPath:  cxx,
			GCCVersion: ver,
		}
	}
	if clang, cxx, ok := detectSystemClang(); ok {
		return clang, cxx, nil
	}
	return "", "", &CCompilerPromptError{Kind: PromptDownloadClang}
}

// resolveCCChoice applies a user's decision to pick a C compiler.
func resolveCCChoice(ctx context.Context, choice CCompilerChoice, options InstallOptions) (ccPath, cxxPath string, err error) {
	switch choice {
	case CCChoiceCancel:
		return "", "", errors.New("已取消安装：请先手动安装 C 编译器（GCC 或 Clang）后再安装该插件")
	case CCChoiceGCC:
		gcc, cxx, ver, ok := detectSystemGCC()
		if !ok {
			return "", "", fmt.Errorf("未找到系统 GCC（ASTRBOT_CC/CC 环境变量或 PATH 中的 gcc）")
		}
		logger.Info("Using system GCC for cgo plugin: %s (v%s)", gcc, ver)
		return gcc, cxx, nil
	case CCChoiceClang:
		if clang, cxx, ok := detectSystemClang(); ok {
			logger.Info("Using system Clang for cgo plugin: %s", clang)
			return clang, cxx, nil
		}
		return downloadAndSetupClang(ctx, options)
	case CCChoiceDownload:
		return downloadAndSetupClang(ctx, options)
	default:
		return "", "", fmt.Errorf("未知的 C 编译器选择: %q", choice)
	}
}

// detectSystemGCC resolves a usable system GCC following the documented
// priority: ASTRBOT_CC > CC env var > `gcc` on PATH. It also derives the CXX
// sibling (g++ / c++). Returns ok=false when none exists.
func detectSystemGCC() (cc, cxx, version string, ok bool) {
	candidates := []string{}
	if p := os.Getenv("ASTRBOT_CC"); p != "" {
		candidates = append(candidates, p)
	}
	if p := os.Getenv("CC"); p != "" {
		candidates = append(candidates, p)
	}
	if p, err := exec.LookPath("gcc"); err == nil {
		candidates = append(candidates, p)
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			cc = p
			cxx = siblingCompiler(p, "g++")
			if _, err := os.Stat(cxx); err != nil {
				cxx = p // fallback: use same binary for C++
			}
			version = compilerVersion(p)
			return cc, cxx, version, true
		}
	}
	return "", "", "", false
}

// detectSystemClang resolves a usable system Clang: ASTRBOT_CC/CC if it points
// at clang, else `clang` on PATH. Returns ok=false when none exists.
func detectSystemClang() (cc, cxx string, ok bool) {
	candidates := []string{}
	for _, env := range []string{"ASTRBOT_CC", "CC"} {
		if p := os.Getenv(env); p != "" {
			if strings.Contains(strings.ToLower(filepath.Base(p)), "clang") {
				candidates = append(candidates, p)
			}
		}
	}
	if p, err := exec.LookPath("clang"); err == nil {
		candidates = append(candidates, p)
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, siblingCompiler(p, "clang++"), true
		}
	}
	return "", "", false
}

// siblingCompiler returns the sibling C++ compiler next to cc (e.g. gcc → g++,
// clang → clang++), preserving a windows .exe suffix.
func siblingCompiler(cc, base string) string {
	dir := filepath.Dir(cc)
	name := base
	if strings.HasSuffix(strings.ToLower(filepath.Base(cc)), ".exe") {
		name += ".exe"
	}
	return filepath.Join(dir, name)
}

// compilerVersion returns the first line of `<cc> --version` trimmed.
func compilerVersion(cc string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, cc, "--version").Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if len(line) > 80 {
		line = line[:80]
	}
	return line
}

// clangRoot returns the per-OS private directory where the bundled Clang is
// extracted (e.g. ~/.local/share/astrbot-go/clang). Overridable with
// ASTRBOT_CLANG_BIN pointing directly at a clang executable.
func clangRoot() string {
	if p := os.Getenv("ASTRBOT_CLANG_BIN"); p != "" {
		return p
	}
	return filepath.Join(toolchainUserStateDir(), "clang")
}

// downloadAndSetupClang downloads the platform's prebuilt Clang and returns the
// CC/CXX paths, installing it under the private user dir. It falls back to a
// friendly error when the platform has no prebuilt archive (e.g. Termux).
// Progress (bytes) and stage text are reported through options.Progress/Stage
// so the WebUI can show a live download bar while Clang is being fetched.
// clangLockFile is the name of the marker file written while a Clang download/
// extract is in progress. If it is present on a later run, the previous attempt
// did not finish cleanly (crash / user cancel / kill), so the whole root dir is
// discarded and re-downloaded instead of trusting a half-extracted Clang.
const clangLockFile = ".install-lock"

func downloadAndSetupClang(ctx context.Context, options InstallOptions) (cc, cxx string, err error) {
	// Bundled/system clang already present?
	if clang, cxx, ok := detectSystemClang(); ok {
		return clang, cxx, nil
	}
	root := clangRoot()

	// A previously downloaded Clang (zig) at the private root — but only trust
	// it if no lock file is left behind from an interrupted install.
	if _, err := os.Stat(filepath.Join(root, clangLockFile)); err == nil {
		logger.Warn("Clang install was interrupted previously, removing %s and re-downloading", root)
		if err := os.RemoveAll(root); err != nil {
			return "", "", fmt.Errorf("清理未完成的 Clang 安装目录失败: %w", err)
		}
	}
	if cc, cxx, ok := zigCCFromRoot(root); ok {
		return cc, cxx, nil
	}

	info, err := zigArchiveInfoFor()
	if err != nil {
		return "", "", err
	}

	// Download cache lives next to the toolchain (NOT the volatile tmp dir),
	// so an interrupted download can be resumed via Range requests.
	cacheDir := filepath.Join(toolchainUserStateDir(), "clang-download")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", "", err
	}
	archivePath := filepath.Join(cacheDir, info.archive)

	hadPartial := false
	if fi, err := os.Stat(archivePath); err == nil && !fi.IsDir() && fi.Size() > 0 {
		hadPartial = true
	}

	if options.Stage != nil {
		options.Stage("下载 C 编译器 (zig)…")
	}
	if err := downloadClangArchive(ctx, info.archive, archivePath, options.Progress); err != nil {
		return "", "", err
	}
	if hadPartial {
		logger.Info("Clang (zig) archive %s resumed and completed", info.archive)
	}

	if options.Stage != nil {
		options.Stage("解压 C 编译器 (zig)…")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", "", err
	}
	// Mark the install as in progress: a leftover lock on a later run means the
	// extract never finished, so it is discarded and retried from scratch.
	lockPath := filepath.Join(root, clangLockFile)
	if err := os.WriteFile(lockPath, []byte(time.Now().Format(time.RFC3339)), 0o644); err != nil {
		return "", "", fmt.Errorf("创建 Clang 安装锁失败: %w", err)
	}
	if err := extractClangArchive(ctx, archivePath, root, info.triple); err != nil {
		_ = os.Remove(lockPath)
		return "", "", fmt.Errorf("解压 C 编译器失败: %w", err)
	}
	// Only remove the lock after a clean extract; then verify the zig binary.
	if cc, cxx, ok := zigCCFromRoot(root); !ok {
		_ = os.Remove(lockPath)
		return "", "", fmt.Errorf("解压后未找到 zig/clang 可执行文件")
	} else if err := os.Remove(lockPath); err != nil {
		return "", "", fmt.Errorf("清理 Clang 安装锁失败: %w", err)
	} else {
		return cc, cxx, nil
	}
}

// zigArchiveInfo describes the Zig distribution used as the bundled C compiler
// (zig cc is a clang-compatible C compiler, ~55MB vs the ~1GB LLVM bundle).
type zigArchiveInfo struct {
	archive string // download file name, e.g. zig-x86_64-linux-0.16.0.tar.xz
	triple  string // top-level extraction directory, e.g. zig-x86_64-linux-0.16.0
	kind    string // "tar.xz" | "zip"
}

func zigArchiveInfoFor() (zigArchiveInfo, error) {
	ver := zigVersion()
	switch runtime.GOOS {
	case "linux":
		switch runtime.GOARCH {
		case "amd64":
			return zigArchiveInfo{fmt.Sprintf("zig-x86_64-linux-%s.tar.xz", ver), "zig-x86_64-linux-" + ver, "tar.xz"}, nil
		case "arm64":
			return zigArchiveInfo{fmt.Sprintf("zig-aarch64-linux-%s.tar.xz", ver), "zig-aarch64-linux-" + ver, "tar.xz"}, nil
		}
	case "darwin":
		switch runtime.GOARCH {
		case "amd64":
			return zigArchiveInfo{fmt.Sprintf("zig-x86_64-macos-%s.tar.xz", ver), "zig-x86_64-macos-" + ver, "tar.xz"}, nil
		case "arm64":
			return zigArchiveInfo{fmt.Sprintf("zig-aarch64-macos-%s.tar.xz", ver), "zig-aarch64-macos-" + ver, "tar.xz"}, nil
		}
	case "windows":
		switch runtime.GOARCH {
		case "amd64":
			return zigArchiveInfo{fmt.Sprintf("zig-x86_64-windows-%s.zip", ver), "zig-x86_64-windows-" + ver, "zip"}, nil
		case "arm64":
			return zigArchiveInfo{fmt.Sprintf("zig-aarch64-windows-%s.zip", ver), "zig-aarch64-windows-" + ver, "zip"}, nil
		}
	}
	return zigArchiveInfo{}, zigUnsupportedHint()
}

func zigVersion() string {
	if v := os.Getenv("ASTRBOT_CLANG_VERSION"); v != "" {
		return v
	}
	return "0.16.0"
}

// zigCCFromRoot returns the CC/CXX paths for a previously-installed zig bundle
// at root, or ok=false when the zig binary is not present.
func zigCCFromRoot(root string) (cc, cxx string, ok bool) {
	bin := filepath.Join(root, "zig")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if info, err := os.Stat(bin); err == nil && !info.IsDir() {
		// go uses `zig cc` / `zig c++` as the C/C++ compilers.
		return bin + " cc", bin + " c++", true
	}
	return "", "", false
}

func zigUnsupportedHint() error {
	if runtime.GOOS == "android" {
		prefix := os.Getenv("PREFIX")
		if prefix == "" {
			prefix = "/data/data/com.termux/files/usr"
		}
		return fmt.Errorf("Termux (Android) 没有官方 Zig 包。请在 Termux 中执行：\n" +
			"  pkg update && pkg install clang\n" +
			"安装后即可自动检测到（或设置环境变量 ASTRBOT_CC=%s/bin/clang），然后重新安装插件。", prefix)
	}
	return fmt.Errorf("当前平台 %s/%s 没有可用的 C 编译器预编译包，请手动安装 Clang/GCC 后再安装插件",
		runtime.GOOS, runtime.GOARCH)
}

// downloadClangArchive downloads the Clang archive to dest, trying each mirror
// base in order and resuming an existing partial file via HTTP Range requests.
// A 10-minute per-request timeout keeps a stalled mirror from hanging forever.
func downloadClangArchive(ctx context.Context, archive, dest string, progress func(downloaded, total int64)) error {
	// Already fully cached?
	if info, err := os.Stat(dest); err == nil && !info.IsDir() && info.Size() > 0 {
		logger.Info("Clang archive already cached: %s", dest)
		return nil
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	var lastErr error
	for _, base := range zigMirrorBases() {
		url := base + "/" + archive
		if err := resumeDownload(ctx, client, url, dest, progress); err != nil {
			lastErr = err
			logger.Warn("Clang download from %s failed: %v", url, err)
			continue
		}
		logger.Info("Clang archive downloaded from %s", url)
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no mirror base configured")
	}
	return fmt.Errorf("下载 Clang 失败：%w。可手动安装 Clang/GCC 或设置 ASTRBOT_CLANG_BIN", lastErr)
}

// resumeDownload streams url into dest, resuming from the current file size via
// an HTTP Range header when the server supports it and the file is a valid
// prefix. It reports byte progress through progress.
func resumeDownload(ctx context.Context, client *http.Client, url, dest string, progress func(downloaded, total int64)) error {
	// Determine existing size for a resume offset.
	var offset int64
	if info, err := os.Stat(dest); err == nil && !info.IsDir() {
		offset = info.Size()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// Server ignored the Range header (or offset==0): start over.
		if offset > 0 {
			logger.Info("Resume not supported by %s, restarting download", url)
		}
		offset = 0
	case http.StatusPartialContent:
		// Resuming: verify the range actually starts where we expect.
		if offset > 0 {
			logger.Info("Resuming %s from byte %d", url, offset)
		}
	default:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	total := resp.ContentLength
	if total <= 0 {
		total = 0
	}
	// Open append-only so a resumed range extends the partial file.
	flag := os.O_CREATE | os.O_WRONLY
	if offset > 0 {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(dest, flag, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	buf := make([]byte, 256*1024)
	written := offset
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			written += int64(n)
			if progress != nil {
				progress(written, offset+total)
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// zigMirrorBases returns the ordered list of download bases tried for the zig
// archive. A user override via ASTRBOT_CLANG_MIRROR replaces the whole list.
func zigMirrorBases() []string {
	if p := os.Getenv("ASTRBOT_CLANG_MIRROR"); p != "" {
		return []string{strings.TrimRight(p, "/")}
	}
	return []string{
		"https://ziglang.org/download/" + zigVersion(),
	}
}

// extractClangArchive unpacks the downloaded zig archive into root, promoting
// the single top-level directory so the zig binary lands at root/zig. It uses
// mholt/archives for format identification + extraction (pure Go, cross
// platform: zip / tar.xz / tar.gz all handled uniformly). It checks ctx
// cancellation between members so an aborted install leaves the lock file
// behind (the next run discards root and retries).
func extractClangArchive(ctx context.Context, archive, root, triple string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	format, stream, err := archives.Identify(ctx, archive, f)
	if err != nil {
		f.Close()
		return fmt.Errorf("识别归档格式失败: %w", err)
	}
	ex, ok := format.(archives.Extractor)
	if !ok {
		f.Close()
		return fmt.Errorf("不支持的归档格式: %s", archive)
	}

	if err := os.MkdirAll(root, 0o755); err != nil {
		f.Close()
		return err
	}
	topSeen := false
	err = ex.Extract(ctx, stream, func(ctx context.Context, fi archives.FileInfo) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		// Strip the single top-level directory (e.g. "zig-x86_64-linux-0.16.0/")
		// so contents land directly under root (zig → root/zig).
		rel, ok := stripTopDir(fi.NameInArchive)
		if !ok {
			return nil
		}
		topSeen = true
		target, err := safeJoin(root, rel)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		src, err := fi.Open()
		if err != nil {
			return err
		}
		defer src.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, src)
		return err
	})
	f.Close()
	if err != nil {
		return err
	}
	if !topSeen {
		return fmt.Errorf("zig archive missing expected directory %q", triple)
	}
	return nil
}

// stripTopDir rewrites an archive member path, dropping the leading top-level
// directory (e.g. "zig-x86_64-linux-0.16.0/bin/zig" → "bin/zig").
func stripTopDir(rel string) (string, bool) {
	sep := "/"
	parts := strings.Split(rel, sep)
	for len(parts) > 0 && parts[0] == "" {
		parts = parts[1:]
	}
	if len(parts) <= 1 {
		return "", false
	}
	return strings.Join(parts[1:], sep), true
}

// toolchainUserStateDir mirrors the toolchain's private user dir (kept here to
// avoid an import cycle on the unexported userStateDir).
func toolchainUserStateDir() string {
	return toolchain.UserStateDir()
}

// exe returns name with a .exe suffix on Windows.
func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
