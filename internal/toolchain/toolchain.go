// Package toolchain manages the self-contained Go toolchain used to compile
// plugins on the user's machine.
//
// Strategy (in order of preference):
//  1. ASTRBOT_GO_BIN — explicit override (best for Termux/dev machines).
//  2. A bundled official Go distribution at the per-OS user directory
//     (downloaded and extracted on first use).
//  3. A `go` binary already on PATH (system-installed Go).
//  4. Download the official Go archive and extract it into the user dir.
//
// All artifacts live under the user's private directories, never in system
// locations, so no root is required.
package toolchain

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

var logger = log.GetDefault().WithComponent("Toolchain")

// DefaultGoVersion is the Go release bundled when none is pinned via
// ASTRBOT_GO_VERSION.
const DefaultGoVersion = "1.24.3"

// envOverrides the values honored by the toolchain.
const (
	EnvGoBin        = "ASTRBOT_GO_BIN"         // path to an existing `go` binary
	EnvGoVer        = "ASTRBOT_GO_VERSION"     // e.g. "1.24.3"
	EnvGoMirror     = "ASTRBOT_GO_MIRROR"      // base URL of an official Go archive mirror
	EnvGoSkipVerify = "ASTRBOT_GO_SKIP_VERIFY" // set to bypass the sha256 checksum gate
)

// Toolchain locates (and if needed provisions) a Go toolchain for building
// plugin subprocesses.
type Toolchain struct {
	// Version is the Go release to bundle, e.g. "1.24.3".
	Version string
}

// ProgressFunc receives download progress (bytes) during provisioning.
type ProgressFunc func(downloaded, total int64)

// New returns a Toolchain using the version from ASTRBOT_GO_VERSION or the
// default.
func New() *Toolchain {
	v := os.Getenv(EnvGoVer)
	if v == "" {
		v = DefaultGoVersion
	}
	return &Toolchain{Version: v}
}

// GoBin returns the resolved `go` binary path without provisioning, or an
// error if no usable toolchain could be found and none is cached.
func (tc *Toolchain) GoBin() (string, error) {
	if p := os.Getenv(EnvGoBin); p != "" {
		if tc.isExecutable(p) {
			logger.Info("Using Go toolchain from ASTRBOT_GO_BIN: %s", p)
			return p, nil
		}
		return "", fmt.Errorf("ASTRBOT_GO_BIN=%q does not point to an executable", p)
	}

	if tc.BundledRoot() != "" {
		if bin := filepath.Join(tc.BundledRoot(), "bin", exe("go")); tc.isExecutable(bin) {
			logger.Info("Using bundled Go toolchain: %s", bin)
			return bin, nil
		}
	}

	if p, err := exec.LookPath("go"); err == nil {
		logger.Info("Using system Go toolchain: %s", p)
		return p, nil
	}

	return "", fmt.Errorf("no Go toolchain found (set %s or run Ensure())", EnvGoBin)
}

// Ensure provisions the bundled toolchain (download + extract) if needed and
// returns the `go` binary path. It never overwrites an existing bundle.
func (tc *Toolchain) Ensure() (string, error) {
	return tc.EnsureWithProgress(nil)
}

// EnsureWithProgress is like Ensure but also reports download progress through
// the callback (the download is ~150-200MB on first run).
func (tc *Toolchain) EnsureWithProgress(progress ProgressFunc) (string, error) {
	if bin, err := tc.GoBin(); err == nil {
		return bin, nil
	}
	return tc.downloadAndExtract(progress)
}

// GOROOT returns the root of the bundled distribution, or "" if not present.
func (tc *Toolchain) GOROOT() string {
	root := tc.BundledRoot()
	if root == "" {
		return ""
	}
	return filepath.Join(root, "go")
}

// GOPATH returns the private module cache dir for plugin compilation.
func (tc *Toolchain) GOPATH() string {
	return filepath.Join(userStateDir(), "gopath")
}

// BaseDir returns the per-OS private directory that hosts the toolchain.
func (tc *Toolchain) BaseDir() string {
	return filepath.Join(userStateDir(), "toolchain")
}

// BundledRoot returns the directory the official archive is extracted into
// (<BaseDir>), or "" when the current platform cannot host a bundled Go.
func (tc *Toolchain) BundledRoot() string {
	if !supportsBundledGo() {
		return ""
	}
	return tc.BaseDir()
}

// BuildEnv returns the environment for compiling a plugin with the bundled
// (or system) toolchain. extra entries override defaults.
//
// GOROOT is intentionally NOT set: since Go 1.20 the go command infers its
// GOROOT from the executable's location, which covers both the bundled
// toolchain (<BaseDir>/go/bin/go) and a system-installed go.
func (tc *Toolchain) BuildEnv(extra map[string]string) []string {
	cgo := "0"
	if CGOEnabled() {
		cgo = "1"
	}
	env := []string{
		"GO111MODULE=on",
		"GOFLAGS=-mod=mod",
		"CGO_ENABLED=" + cgo,
		"GOPATH=" + tc.GOPATH(),
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return append(os.Environ(), env...)
}

// CGOEnabled reports whether plugin builds may use cgo. cgo is only usable
// when a C compiler exists on the host (the bundled Go toolchain does not ship
// one). Override with ASTRBOT_PLUGIN_CGO=0/1 to force.
func CGOEnabled() bool {
	if v := os.Getenv("ASTRBOT_PLUGIN_CGO"); v != "" {
		return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "on")
	}
	for _, c := range []string{"cc", "gcc", "clang"} {
		if _, err := exec.LookPath(c); err == nil {
			return true
		}
	}
	return false
}

// downloadAndExtract provisions the official Go distribution under BaseDir.
func (tc *Toolchain) downloadAndExtract(progress ProgressFunc) (string, error) {
	if !supportsBundledGo() {
		return "", fmt.Errorf("%s", tc.unsupportedHint())
	}

	base := tc.BaseDir()
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", fmt.Errorf("create toolchain dir: %w", err)
	}

	archive := archiveName(tc.Version)
	dest := filepath.Join(base, archive)
	url := downloadURL(archive)

	logger.Info("Downloading Go toolchain %s from %s", archive, url)
	if err := downloadFile(url, dest, progress); err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	// 校验失败即中止安装（供应链防护）：删除已下载文件并报错，避免被篡改的
	// 工具链被解压使用。verifyChecksum 在设置 ASTRBOT_GO_SKIP_VERIFY 时直接
	// 返回 nil，显式跳过校验的调用方不受影响。
	if err := tc.verifyChecksum(dest, archive); err != nil {
		_ = os.Remove(dest)
		return "", fmt.Errorf("checksum verification failed for %s (set %s to bypass): %w", archive, EnvGoSkipVerify, err)
	}

	logger.Info("Extracting Go toolchain %s", archive)
	if err := extractArchive(dest, base); err != nil {
		return "", fmt.Errorf("extract %s: %w", archive, err)
	}
	_ = os.Remove(dest)

	bin := filepath.Join(base, "go", "bin", exe("go"))
	if !tc.isExecutable(bin) {
		return "", fmt.Errorf("extracted toolchain missing go binary: %s", bin)
	}
	return bin, nil
}

// unsupportedHint returns a friendly, platform-aware message when no bundled
// Go archive exists for the current platform (e.g. Termux/Android).
func (tc *Toolchain) unsupportedHint() string {
	if runtime.GOOS == "android" {
		prefix := os.Getenv("PREFIX")
		if prefix == "" {
			prefix = "/data/data/com.termux/files/usr"
		}
		return "Termux (Android) 没有官方 Go 工具链可用。请在 Termux 中执行：\n" +
			"  pkg update && pkg install golang\n" +
			"安装后让 `go` 出现在 PATH 中，或设置环境变量：\n" +
			fmt.Sprintf("  export ASTRBOT_GO_BIN=%s/bin/go\n", prefix) +
			"然后重启 AstrBot。"
	}
	return fmt.Sprintf("bundled Go is not supported on %s/%s (set %s to an existing go binary)",
		runtime.GOOS, runtime.GOARCH, EnvGoBin)
}

// verifyChecksum fetches "<file>.sha256" from the mirror and compares it.
// Returns an error on any mismatch/failure. When ASTRBOT_GO_SKIP_VERIFY is set
// the check is skipped entirely (caller's explicit opt-out).
func (tc *Toolchain) verifyChecksum(dest, archive string) error {
	if os.Getenv(EnvGoSkipVerify) != "" {
		return nil
	}
	sumURL := checksumURL(archive)
	resp, err := http.Get(sumURL)
	if err != nil {
		return fmt.Errorf("fetch checksum: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("checksum endpoint returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Errorf("read checksum: %w", err)
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return fmt.Errorf("unexpected checksum body: %q", body)
	}
	want := strings.TrimSpace(fields[0])

	f, err := os.Open(dest) // #nosec G304 -- dest is the just-downloaded archive under the private BaseDir
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
		return fmt.Errorf("sha256 mismatch: got %s want %s", got, want)
	}
	logger.Info("Checksum verified for %s", archive)
	return nil
}

func (tc *Toolchain) isExecutable(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// downloadURL builds the mirror-aware URL for an official archive file.
func downloadURL(archive string) string {
	base := os.Getenv(EnvGoMirror)
	if base == "" {
		base = "https://go.dev/dl"
	}
	return strings.TrimRight(base, "/") + "/" + archive
}

// checksumURL builds the URL of the sha256 file. go.dev/dl only serves the
// archives (redirecting to dl.google.com); the .sha256 files live on
// dl.google.com directly.
func checksumURL(archive string) string {
	base := os.Getenv(EnvGoMirror)
	if base == "" {
		base = "https://dl.google.com/go"
	}
	return strings.TrimRight(base, "/") + "/" + archive + ".sha256"
}

// archiveName returns the official distribution archive file name for the
// current platform, e.g. "go1.24.3.linux-amd64.tar.gz".
func archiveName(version string) string {
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("go%s.%s-%s.%s", version, runtime.GOOS, runtime.GOARCH, ext)
}

// supportsBundledGo reports whether the official Go archives cover this
// platform. Termux (android) and exotic GOOS/GOARCH pairs are not covered.
func supportsBundledGo() bool {
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
	default:
		return false
	}
	switch runtime.GOARCH {
	case "amd64", "arm64", "386", "arm":
		return true
	default:
		return false
	}
}

// userStateDir returns the per-OS private data dir for AstrBot Go state.
//
//	Windows: %LOCALAPPDATA%\AstrBot-Go
//	macOS:   ~/Library/Application Support/AstrBot-Go
//	other:   ~/.local/share/astrbot-go  (Linux / Termux)
func userStateDir() string {
	return UserStateDir()
}

// UserStateDir is the exported per-OS private data dir for AstrBot Go state
// (same layout as userStateDir). Exposed so other packages (e.g. the plugin
// C-compiler provisioning) can place sibling artifacts next to the toolchain.
func UserStateDir() string {
	switch runtime.GOOS {
	case "windows":
		if base := os.Getenv("LOCALAPPDATA"); base != "" {
			return filepath.Join(base, "AstrBot-Go")
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "AstrBot-Go")
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "astrbot-go")
	}
	return "."
}

// downloadFile streams a URL to dest with a progress timeout, logging and
// reporting download progress every ~10%.
func downloadFile(url, dest string, progress ProgressFunc) error {
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dest) // #nosec G304 -- dest is the private BaseDir archive path constructed from tc.Version
	if err != nil {
		return err
	}
	defer f.Close()
	return copyWithProgress(f, resp.Body, resp.ContentLength, progress)
}

// copyWithProgress copies r to w, logging percentage milestones and invoking
// the progress callback (when set).
func copyWithProgress(w io.Writer, r io.Reader, total int64, progress ProgressFunc) error {
	buf := make([]byte, 64*1024)
	var written int64
	lastPct := -1
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			written += int64(n)
			if total > 0 {
				pct := int(written * 100 / total)
				if pct != lastPct && pct%10 == 0 {
					lastPct = pct
					logger.Info("Downloading Go toolchain: %d%% (%s / %s)",
						pct, humanSize(written), humanSize(total))
				}
			}
			if progress != nil {
				progress(written, total)
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

// humanSize formats a byte count as a human-readable size (e.g. "123.4 MiB").
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

// extractArchive unpacks a Go official archive (tar.gz or zip) into dir,
// preserving the leading "go/" component so GOROOT becomes dir/go.
func extractArchive(src, dir string) error {
	if strings.HasSuffix(src, ".zip") {
		return extractZip(src, dir)
	}
	return extractTarGz(src, dir)
}

func extractTarGz(src, dir string) error {
	f, err := os.Open(src) // #nosec G304 -- src is the verified archive under the private BaseDir
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
		target, err := safeJoin(dir, hdr.Name)
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
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(uint32(hdr.Mode&0o755))) // #nosec G304 -- target validated by safeJoin
			if err != nil {
				return err
			}
			// #nosec decompression_bomb -- Go SDK 工具链归档来自受信任的固定下载源（golang.org dl / 镜像，
			// 版本固定且下载时经 checksum 校验），safeJoin 已防穿越。
			if _, err := io.Copy(out, tr); err != nil { // nosemgrep: go.lang.security.decompression_bomb.potential-dos-via-decompression-bomb
				_ = out.Close()
				return err
			}
			_ = out.Close()
		}
	}
}

func extractZip(src, dir string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		target, err := safeJoin(dir, zf.Name)
		if err != nil {
			return err
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(target) // #nosec G304 -- target validated by safeJoin
		if err != nil {
			_ = rc.Close()
			return err
		}
		// #nosec decompression_bomb -- Go SDK 工具链归档来自受信任的固定下载源（版本固定，下载时校验），safeJoin 已防穿越
		if _, err := io.Copy(out, rc); err != nil { // nosemgrep: go.lang.security.decompression_bomb.potential-dos-via-decompression-bomb
			_ = out.Close()
			_ = rc.Close()
			return err
		}
		_ = out.Close()
		_ = rc.Close()
	}
	return nil
}

// safeJoin joins base+rel and rejects paths escaping base.
func safeJoin(base, rel string) (string, error) {
	if rel == "" || rel == "." {
		return base, nil
	}
	target := filepath.Join(base, rel)
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	relPath, err := filepath.Rel(absBase, absTarget)
	if err != nil || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry escapes destination: %s", rel)
	}
	return target, nil
}

func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
