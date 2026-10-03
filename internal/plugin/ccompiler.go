package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/WaterGodFurina/Astrbot-golang/internal/toolchain"
	"github.com/mholt/archives"
)

// CCompilerPromptKind identifies which prompt the user must answer before a
// cgo-requiring plugin can be built.
type CCompilerPromptKind string

const (
	// PromptChooseGCC is shown when a system GCC exists: the user picks
	// GCC, auto-downloaded zig cc, or cancels.
	PromptChooseGCC CCompilerPromptKind = "choose_gcc"
	// PromptChooseClang is shown when a system Clang exists (no GCC): the user
	// picks system Clang, auto-downloaded zig cc, or cancels.
	PromptChooseClang CCompilerPromptKind = "choose_clang"
	// PromptDownloadZigCC is shown when no C compiler is present at all: the
	// user chooses whether to download zig cc or cancel.
	PromptDownloadZigCC CCompilerPromptKind = "download_zig_cc"
)

// CCompilerChoice is the user's answer to a CCompilerPromptError.
type CCompilerChoice string

const (
	CCChoiceGCC    CCompilerChoice = "gcc"    // use the system GCC (CC/CXX env)
	CCChoiceClang  CCompilerChoice = "clang"  // use the system Clang
	CCChoiceZigCC  CCompilerChoice = "zig_cc" // use/download the bundled zig cc
	CCChoiceCancel CCompilerChoice = "cancel" // abort installation
)

// CCompilerPromptError is returned by InstallFromSource when a plugin declares
// cgo but the host cannot (or the user has not decided to) pick a C compiler.
// The dashboard surfaces it to the WebUI so the user can review the options.
type CCompilerPromptError struct {
	// Kind is which dialog the frontend should render.
	Kind CCompilerPromptKind `json:"kind"`
	// HasGCC reports whether a usable system GCC was detected (only meaningful
	// for PromptChooseGCC).
	HasGCC bool `json:"has_gcc"`
	// GCCPath / GCCXXPath are the detected system compiler paths.
	GCCPath   string `json:"gcc_path,omitempty"`
	GCCXXPath string `json:"gcc_xx_path,omitempty"`
	// GCCVersion is a short version string of the detected GCC (for display).
	GCCVersion string `json:"gcc_version,omitempty"`
	// HasClang reports whether a usable system Clang was detected (only
	// meaningful for PromptChooseClang).
	HasClang bool `json:"has_clang"`
	// ClangPath / ClangXXPath are the detected system Clang paths.
	ClangPath   string `json:"clang_path,omitempty"`
	ClangXXPath string `json:"clang_xx_path,omitempty"`
	// ClangVersion is a short version string of the detected Clang.
	ClangVersion string `json:"clang_version,omitempty"`
	// Mirrors 是 zig cc 可选下载镜像基地址列表（"github 加速镜像选择"同款交互），
	// 前端弹窗展示让用户选择；选择经 install 的 zig_mirror 回传覆盖默认顺序。
	// 仅对 PromptDownloadZigCC（及选择"下载 zig cc"）有意义。
	Mirrors []string `json:"mirrors,omitempty"`
}

func (e *CCompilerPromptError) Error() string {
	switch e.Kind {
	case PromptChooseGCC:
		return "检测到系统已安装 GCC，需要选择使用 GCC 还是下载 zig cc"
	case PromptChooseClang:
		return "检测到系统已安装 Clang，需要选择使用 Clang 还是下载 zig cc"
	default:
		return "未检测到 C 编译器，需要确认是否自动下载并安装 zig cc"
	}
}

// ensureCCompiler resolves the C compiler to use for a cgo plugin.
//
// The flow (all prompts surface through WebUI/CLI via CCompilerPromptError):
//
//  1. If the user already answered (options.CCChoice), honor it:
//     - gcc     → return the detected system GCC/CC/CXX.
//     - clang   → return the detected system Clang.
//     - zig_cc  → use/download the bundled zig cc.
//     - cancel  → return a user-cancelled error.
//  2. Otherwise detect in priority order:
//     - zig cc (bundled download or `zig` on PATH) → used directly, no prompt.
//     - system GCC → return a PromptChooseGCC error.
//     - system Clang → return a PromptChooseClang error.
//     - neither → return a PromptDownloadZigCC error.
//
// It returns the compiler paths (ccPath/cxxPath) once the user choice is
// resolved, or an error (CCompilerPromptError for decisions still needed).
func ensureCCompiler(ctx context.Context, options InstallOptions) (ccPath, cxxPath string, err error) {
	choice := CCompilerChoice(strings.ToLower(strings.TrimSpace(options.CCChoice)))
	if choice != "" {
		return resolveCCChoice(ctx, choice, options)
	}

	// No explicit choice yet: detect what the host already has.
	// Priority: zig cc (bundled or on PATH) > GCC > Clang.
	if cc, cxx, ok := detectZigCC(); ok {
		return cc, cxx, nil
	}
	if gcc, cxx, ver, ok := detectSystemGCC(); ok {
		return "", "", &CCompilerPromptError{
			Kind:       PromptChooseGCC,
			HasGCC:     true,
			GCCPath:    gcc,
			GCCXXPath:  cxx,
			GCCVersion: ver,
		}
	}
	if clang, cxx, ok := detectSystemClang(); ok {
		return "", "", &CCompilerPromptError{
			Kind:         PromptChooseClang,
			HasClang:     true,
			ClangPath:    clang,
			ClangXXPath:  cxx,
			ClangVersion: compilerVersion(clang),
		}
	}
	return "", "", &CCompilerPromptError{Kind: PromptDownloadZigCC, Mirrors: defaultZigMirrorBases()}
}

// resolveCCChoice applies a user's decision to pick a C compiler.
func resolveCCChoice(ctx context.Context, choice CCompilerChoice, options InstallOptions) (ccPath, cxxPath string, err error) {
	switch choice {
	case CCChoiceCancel:
		return "", "", errors.New("已取消安装：请先手动安装 C 编译器（GCC/Clang 或下载 zig cc）后再安装该插件")
	case CCChoiceGCC:
		gcc, cxx, ver, ok := detectSystemGCC()
		if !ok {
			return "", "", fmt.Errorf("未找到系统 GCC（ASTRBOT_CC/CC 环境变量或 PATH 中的 gcc）")
		}
		logger.I18nInfo("为 cgo 插件使用系统 GCC: %s (v%s)", gcc, ver)
		return gcc, cxx, nil
	case CCChoiceClang:
		clang, cxx, ok := detectSystemClang()
		if !ok {
			return "", "", fmt.Errorf("未找到系统 Clang（ASTRBOT_CC/CC 环境变量或 PATH 中的 clang）")
		}
		logger.I18nInfo("为 cgo 插件使用系统 Clang: %s", clang)
		return clang, cxx, nil
	case CCChoiceZigCC:
		if cc, cxx, ok := detectZigCC(); ok {
			logger.I18nInfo("为 cgo 插件使用 zig cc: %s", cc)
			return cc, cxx, nil
		}
		return downloadAndSetupZigCC(ctx, options)
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
		if info, err := os.Stat(p); err == nil && !info.IsDir() { // #nosec G703 -- read-only stat on locally resolved compiler path
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
		if info, err := os.Stat(p); err == nil && !info.IsDir() { // #nosec G703 -- read-only stat on locally resolved compiler path
			return p, siblingCompiler(p, "clang++"), true
		}
	}
	return "", "", false
}

// detectZigCC resolves zig cc (used as the C compiler), whether from the
// bundled download (zigCCFromRoot) or a system `zig` on PATH. Returns
// ok=false when none exists.
func detectZigCC() (cc, cxx string, ok bool) {
	if cc, cxx, ok := zigCCFromRoot(zigRoot()); ok {
		return cc, cxx, true
	}
	if p, err := exec.LookPath("zig"); err == nil {
		if info, serr := os.Stat(p); serr == nil && !info.IsDir() { // #nosec G703 -- read-only stat on locally resolved compiler path
			return p + " cc", p + " c++", true
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
	out, err := exec.CommandContext(ctx, cc, "--version").Output() // #nosec G204 -- 插件编译核心：探测随附/系统 C 编译器版本，cc 由 zigRoot/detectSystemGCC 解析得出，参数固定为 "--version"; nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if len(line) > 80 {
		line = line[:80]
	}
	return line
}

// zigRoot returns the per-OS private directory where the bundled zig cc is
// extracted (e.g. ~/.local/share/astrbot-go/zigcc). Overridable with
// ASTRBOT_ZIGCC_BIN pointing directly at a zig executable.
func zigRoot() string {
	if p := os.Getenv("ASTRBOT_ZIGCC_BIN"); p != "" {
		return p
	}
	return filepath.Join(toolchainUserStateDir(), "zigcc")
}

// downloadAndSetupZigCC downloads the platform's prebuilt zig cc and returns
// the CC/CXX paths, installing it under the private user dir. It falls back to
// a friendly error when the platform has no prebuilt archive (e.g. Termux).
// Progress (bytes) and stage text are reported through options.Progress/Stage
// so the WebUI can show a live download bar while zig cc is being fetched.
// zigCCLockFile is the name of the marker file written while a zig cc download/
// extract is in progress. If it is present on a later run, the previous attempt
// did not finish cleanly (crash / user cancel / kill), so the whole root dir is
// discarded and re-downloaded instead of trusting a half-extracted zig cc.
const zigCCLockFile = ".install-lock"

func downloadAndSetupZigCC(ctx context.Context, options InstallOptions) (cc, cxx string, err error) {
	// 已下载的 zig cc 已存在？直接用（不重新下载）。
	root := zigRoot()
	if cc, cxx, ok := zigCCFromRoot(root); ok {
		return cc, cxx, nil
	}
	// 系统 Clang 已存在？直接用（无需下载 zig cc）。
	if clang, cxx, ok := detectSystemClang(); ok {
		return clang, cxx, nil
	}

	// A previously downloaded zig cc at the private root — but only trust
	// it if no lock file is left behind from an interrupted install.
	if _, err := os.Stat(filepath.Join(root, zigCCLockFile)); err == nil {
		logger.I18nWarn("zig cc 安装此前被中断，正在删除 %s 并重新下载", root)
		if err := os.RemoveAll(root); err != nil {
			return "", "", fmt.Errorf("清理未完成的 zig cc 安装目录失败: %w", err)
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
	cacheDir := filepath.Join(toolchainUserStateDir(), "zigcc-download")
	// #nosec G301 -- 工具链缓存目录（用户态）
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
	if err := downloadZigCCArchive(ctx, info.archive, archivePath, options.ZigCCMirror, options.Progress); err != nil {
		return "", "", err
	}
	if hadPartial {
		logger.I18nInfo("zig cc 压缩包 %s 已断点续传并完成", info.archive)
	}

	if options.Stage != nil {
		options.Stage("解压 C 编译器 (zig)…")
	}
	// #nosec G301 -- 工具链安装目录（用户态）
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", "", err
	}
	// Mark the install as in progress: a leftover lock on a later run means the
	// extract never finished, so it is discarded and retried from scratch.
	lockPath := filepath.Join(root, zigCCLockFile)
	// #nosec G306 -- 安装锁仅记录时间戳，无需收紧权限
	if err := os.WriteFile(lockPath, []byte(time.Now().Format(time.RFC3339)), 0o644); err != nil {
		return "", "", fmt.Errorf("创建 zig cc 安装锁失败: %w", err)
	}
	if err := extractZigCCArchive(ctx, archivePath, root, info.triple); err != nil {
		// 保留安装锁：取消/中断或解压错误时，下次运行会丢弃 root 重来，而不是
		// 信任半成品 zig。归档本身损坏（非用户取消）时同时清除缓存文件，避免
		// 损坏归档被"已缓存"逻辑长期信任导致 cgo 安装持续失败。
		if ctx.Err() == nil {
			_ = os.Remove(archivePath)
		}
		return "", "", fmt.Errorf("解压 C 编译器失败: %w", err)
	}
	// Only remove the lock after a clean extract; then verify the zig binary.
	if cc, cxx, ok := zigCCFromRoot(root); !ok {
		// 解压完成但布局不对（归档损坏/与平台不匹配）：同样保留锁并清除缓存，
		// 避免下次继续信任这个无法使用的归档。
		_ = os.Remove(archivePath)
		return "", "", fmt.Errorf("解压后未找到 zig 可执行文件")
	} else if err := os.Remove(lockPath); err != nil {
		return "", "", fmt.Errorf("清理 zig cc 安装锁失败: %w", err)
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
	if v := os.Getenv("ASTRBOT_ZIGCC_VERSION"); v != "" {
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
		return fmt.Errorf("termux (Android) 没有官方 Zig 包。请在 Termux 中执行：\n"+
			"  pkg update && pkg install clang\n"+
			"安装后即可自动检测到（或设置环境变量 ASTRBOT_CC=%s/bin/clang），然后重新安装插件。", prefix)
	}
	return fmt.Errorf("当前平台 %s/%s 没有可用的 C 编译器预编译包，请手动安装 GCC/Clang 后再安装插件",
		runtime.GOOS, runtime.GOARCH)
}

// zigArchiveSHA256 是默认 zig 版本（0.16.0）各平台归档的 sha256 校验值
// （来源 https://ziglang.org/download/index.json），下载后校验再解压，防镜像
// 被劫持时执行被篡改的编译器。自定义版本/镜像（ASTRBOT_ZIGCC_VERSION /
// ASTRBOT_ZIGCC_MIRROR）的归档不在表内时跳过校验（best effort）。
var zigArchiveSHA256 = map[string]string{
	"zig-x86_64-linux-0.16.0.tar.xz":  "70e49664a74374b48b51e6f3fdfbf437f6395d42509050588bd49abe52ba3d00",
	"zig-aarch64-linux-0.16.0.tar.xz": "ea4b09bfb22ec6f6c6ceac57ab63efb6b46e17ab08d21f69f3a48b38e1534f17",
	"zig-x86_64-macos-0.16.0.tar.xz":  "0387557ed1877bc6a2e1802c8391953baddba76081876301c522f52977b52ba7",
	"zig-aarch64-macos-0.16.0.tar.xz": "b23d70deaa879b5c2d486ed3316f7eaa53e84acf6fc9cc747de152450d401489",
	"zig-x86_64-windows-0.16.0.zip":   "68659eb5f1e4eb1437a722f1dd889c5a322c9954607f5edcf337bc3684a75a7e",
	"zig-aarch64-windows-0.16.0.zip":  "aee38316ee4111717900f45dd3130145c39289e105541d737eb8c5ed653c78ef",
}

// downloadZigCCArchive downloads the zig cc archive to dest, trying each mirror
// base in order and resuming an existing partial file via HTTP Range requests.
// A 10-minute per-request timeout keeps a stalled mirror from hanging forever.
func downloadZigCCArchive(ctx context.Context, archive, dest, mirror string, progress func(downloaded, total int64)) error {
	// Already fully cached? 缓存命中同样必须过 sha256（复用 pin 表）：仅 stat
	// 大小会被预放的伪造归档绕过 pin 表，直接解压执行被篡改的编译器。
	if info, err := os.Stat(dest); err == nil && !info.IsDir() && info.Size() > 0 {
		if sum, ok := zigArchiveSHA256[archive]; ok {
			if verr := verifySHA256(dest, sum); verr == nil {
				logger.I18nInfo("zig cc 压缩包已缓存且 sha256 校验通过: %s", dest)
				return nil
			} else {
				logger.I18nWarn("缓存的 zig cc 归档 sha256 校验失败，删除后重新下载: %v", verr)
				_ = os.Remove(dest)
			}
		} else {
			// 自定义版本/镜像不在 pin 表内（best effort）：保持原缓存行为。
			logger.I18nInfo("zig cc 压缩包已缓存: %s", dest)
			return nil
		}
	}
	client := &http.Client{
		Timeout:       30 * time.Minute,
		CheckRedirect: safeRedirect,
		// 与 source.go 下载路径一致：pin 单次 DNS 解析结果，封死
		// DNS-rebinding TOCTOU（safeRedirect 只复检主机名）。
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: pinnedDialContext()},
	}
	var lastErr error
	for _, base := range zigMirrorBases(mirror) {
		url := base + "/" + archive
		if err := resumeDownload(ctx, client, url, dest, progress); err != nil {
			lastErr = err
			logger.I18nWarn("从 %s 下载 zig cc 失败: %v", url, err)
			continue
		}
		// 完整性校验：默认版本归档在表内，校验失败即删除并换下一个镜像。
		if sum, ok := zigArchiveSHA256[archive]; ok {
			if err := verifySHA256(dest, sum); err != nil {
				lastErr = fmt.Errorf("zig cc 归档 sha256 校验失败: %w", err)
				_ = os.Remove(dest)
				logger.I18nWarn("从 %s 下载的 zig cc 归档校验失败，已删除: %v", url, err)
				continue
			}
		}
		logger.I18nInfo("zig cc 压缩包已从 %s 下载", url)
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no mirror base configured")
	}
	return fmt.Errorf("下载 zig cc 失败：%w。可手动安装 GCC/Clang 或设置 ASTRBOT_ZIGCC_BIN", lastErr)
}

// verifySHA256 校验文件的 sha256 与期望值一致（十六进制，大小写不敏感）。
func verifySHA256(path, want string) error {
	f, err := os.Open(path) // #nosec G304 -- 已下载的工具链归档路径
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 不匹配: 期望 %s 实际 %s", want, got)
	}
	return nil
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
			logger.I18nInfo("%s 不支持断点续传，重新开始下载", url)
		}
		offset = 0
	case http.StatusPartialContent:
		// Resuming: verify the range actually starts where we expect.
		if offset > 0 {
			logger.I18nInfo("续传 %s，从第 %d 字节开始", url, offset)
		}
	default:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	total := resp.ContentLength
	if total <= 0 {
		total = 0
	}
	// 大小上限：zig 压缩包实际 ~60MB，300MB 上限仅防恶意镜像无限流量
	//（与 source.go 下载路径的 maxDownloadSize 对应）。
	const maxZigDownload int64 = 300 << 20
	if offset+total > maxZigDownload {
		return fmt.Errorf("download %s exceeds size limit (%d bytes)", url, maxZigDownload)
	}
	// Open append-only so a resumed range extends the partial file.
	flag := os.O_CREATE | os.O_WRONLY
	if offset > 0 {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(dest, flag, 0o644) // #nosec G302,G304 -- dest 为工具链下载缓存路径，0o644 供同用户进程读取
	if err != nil {
		return err
	}
	defer f.Close()

	buf := make([]byte, 256*1024)
	written := offset
	limited := io.LimitReader(resp.Body, maxZigDownload-offset+1)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, rerr := limited.Read(buf)
		if n > 0 {
			written += int64(n)
			if written > maxZigDownload {
				return fmt.Errorf("download %s exceeds size limit (%d bytes)", url, maxZigDownload)
			}
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
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

// defaultZigMirrorBases 返回 zig cc 的默认下载镜像基地址（用户可选列表）。
// 已验证可达：华为云（国内加速，200）+ 官方 ziglang.org。其余（日本
// zig.worker.green 已无 DNS、俄罗斯 mirror.mephi.ru 不可达、GitHub 加速前缀
// 不代理 ziglang.org）不加入，避免每个死源超时拖慢下载。
func defaultZigMirrorBases() []string {
	return []string{
		"https://mirrors.huaweicloud.com/zig/download/" + zigVersion(),
		"https://ziglang.org/download/" + zigVersion(),
	}
}

// zigMirrorBases returns the ordered list of download bases tried for the zig
// archive. 优先级：本次用户选择的镜像（zig_mirror）→ ASTRBOT_ZIGCC_MIRROR
// 环境变量覆盖 → 默认列表。
func zigMirrorBases(mirror string) []string {
	if m := strings.TrimSpace(mirror); m != "" {
		return []string{strings.TrimRight(m, "/")}
	}
	if p := os.Getenv("ASTRBOT_ZIGCC_MIRROR"); p != "" {
		return []string{strings.TrimRight(p, "/")}
	}
	return defaultZigMirrorBases()
}

// extractZigCCArchive unpacks the downloaded zig archive into root, promoting
// the single top-level directory so the zig binary lands at root/zig. It uses
// mholt/archives for format identification + extraction (pure Go, cross
// platform: zip / tar.xz / tar.gz all handled uniformly). It checks ctx
// cancellation between members so an aborted install leaves the lock file
// behind (the next run discards root and retries).
func extractZigCCArchive(ctx context.Context, archive, root, triple string) error {
	f, err := os.Open(archive) // #nosec G304 -- 已下载的工具链归档路径
	if err != nil {
		return err
	}
	format, stream, err := archives.Identify(ctx, archive, f)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("识别归档格式失败: %w", err)
	}
	ex, ok := format.(archives.Extractor)
	if !ok {
		_ = f.Close()
		return fmt.Errorf("不支持的归档格式: %s", archive)
	}

	// #nosec G301 -- 工具链解压目录（用户态）
	if err := os.MkdirAll(root, 0o755); err != nil {
		_ = f.Close()
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
			return os.MkdirAll(target, 0o755) // #nosec G301 -- target 经 safeJoin 校验
		}
		// #nosec G301 -- target 经 safeJoin 校验
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		src, err := fi.Open()
		if err != nil {
			return err
		}
		defer src.Close()
		// 按归档 entry 的权限位落盘，保留可执行位；否则 Linux/macOS 上 zig 无
		// 执行位，cgo 构建报 permission denied。Windows 上 Chmod 为 no-op。
		// zip 归档可能不带 unix 权限位（Perm 为 0），此时回退到 0o755。
		perm := fi.Mode().Perm()
		if perm == 0 {
			perm = 0o755
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm) // #nosec G304 -- target 经 safeJoin 校验防穿越
		if err != nil {
			return err
		}
		_, err = io.Copy(out, src)
		cerr := out.Close()
		if err != nil {
			return err
		}
		return cerr
	})
	_ = f.Close()
	if err != nil {
		return err
	}
	if !topSeen {
		return fmt.Errorf("zig archive missing expected directory %q", triple)
	}
	// 解压后定位 zig 可执行文件并校验/补救执行位：官方归档为 root/zig，
	// 兼容 root/bin/zig 等布局。umask 或归档缺权限位时补齐，避免 cgo 构建
	// 阶段才报 permission denied。找不到仅告警（自定义镜像布局可能不同，
	// 后续会在构建时给出更明确的错误）。Windows 无执行位概念，仅跳过 chmod。
	candidates := []string{"zig", "zig.exe", filepath.Join("bin", "zig"), filepath.Join("bin", "zig.exe")}
	found := false
	for _, cand := range candidates {
		zigBin := filepath.Join(root, cand)
		info, statErr := os.Stat(zigBin)
		if statErr != nil || info.IsDir() {
			continue
		}
		found = true
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
			if err := os.Chmod(zigBin, info.Mode().Perm()|0o755); err != nil {
				return fmt.Errorf("修复 zig 可执行权限失败: %w", err)
			}
			logger.Warn("zig 缺少可执行权限，已自动修复: %s", zigBin)
		}
		break
	}
	if !found {
		logger.Warn("解压后未找到 zig 可执行文件（root=%s），跳过执行位校验", root)
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
