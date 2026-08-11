package plugin

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestReadPluginMetadata validates metadata.json parsing, required fields and
// the cgo default (absent/empty → false).
func TestReadPluginMetadata(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) {
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(`{"name":"echo","desc":"d","version":"1.0.0","cgo":true}`)
	meta, err := ReadPluginMetadata(dir)
	if err != nil {
		t.Fatalf("ReadPluginMetadata: %v", err)
	}
	if meta.Name != "echo" || meta.Description != "d" || meta.Version != "1.0.0" {
		t.Errorf("unexpected metadata: %+v", meta)
	}
	if !meta.RequiresCgo() {
		t.Error("cgo=true should require a C compiler")
	}

	// cgo absent → false
	write(`{"name":"echo"}`)
	meta, err = ReadPluginMetadata(dir)
	if err != nil {
		t.Fatalf("ReadPluginMetadata: %v", err)
	}
	if meta.RequiresCgo() {
		t.Error("absent cgo must default to false")
	}

	// missing file → descriptive error
	if err := os.Remove(filepath.Join(dir, "metadata.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPluginMetadata(dir); err == nil || !strings.Contains(err.Error(), "metadata.json") {
		t.Fatalf("expected missing metadata.json error, got %v", err)
	}

	// missing name → error
	write(`{"desc":"d"}`)
	if _, err := ReadPluginMetadata(dir); err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("expected missing name error, got %v", err)
	}
}

// TestEnsureMainGo guards the main.go requirement.
func TestEnsureMainGo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureMainGo(dir); err != nil {
		t.Errorf("ensureMainGo with main.go: %v", err)
	}
	empty := t.TempDir()
	if err := ensureMainGo(empty); err == nil || !strings.Contains(err.Error(), "main.go") {
		t.Fatalf("expected missing main.go error, got %v", err)
	}
}

// TestEnsureCCompilerPromptsWithoutChoice exercises the detection flow: with no
// user choice and a fresh environment the function must return a
// CCompilerPromptError (either choose_compiler when GCC is present, or
// download_clang when not) instead of silently succeeding.
func TestEnsureCCompilerPromptsWithoutChoice(t *testing.T) {
	_, _, err := ensureCCompiler(context.Background(), InstallOptions{})
	var promptErr *CCompilerPromptError
	if !errors.As(err, &promptErr) {
		t.Fatalf("expected *CCompilerPromptError, got %T: %v", err, err)
	}
	switch promptErr.Kind {
	case PromptChooseCompiler:
		if !promptErr.HasGCC || promptErr.GCCPath == "" {
			t.Errorf("choose_compiler prompt missing GCC info: %+v", promptErr)
		}
	case PromptDownloadClang:
		if promptErr.HasGCC {
			t.Errorf("download_clang prompt should not report GCC: %+v", promptErr)
		}
	default:
		t.Fatalf("unexpected prompt kind: %s", promptErr.Kind)
	}
}

// TestEnsureCCompilerGCCChoice forces the gcc choice; it only passes when a
// system GCC is actually present (CI may lack one, so it skips gracefully).
func TestEnsureCCompilerGCCChoice(t *testing.T) {
	gcc, _, _, ok := detectSystemGCC()
	if !ok {
		t.Skip("no system gcc on this host")
	}
	cc, cxx, err := ensureCCompiler(context.Background(), InstallOptions{CCChoice: string(CCChoiceGCC)})
	if err != nil {
		t.Fatalf("ensureCCompiler(gcc): %v", err)
	}
	if cc != gcc {
		t.Errorf("expected gcc path %q, got %q", gcc, cc)
	}
	if cxx == "" {
		t.Error("expected a CXX path")
	}
}

// TestEnsureCCompilerCancel returns a user-cancelled error.
func TestEnsureCCompilerCancel(t *testing.T) {
	_, _, err := ensureCCompiler(context.Background(), InstallOptions{CCChoice: string(CCChoiceCancel)})
	if err == nil || !strings.Contains(err.Error(), "取消") {
		t.Fatalf("expected cancel error, got %v", err)
	}
}

// TestZigCCFromRoot verifies the zig CC/CXX derivation ("zig cc"/"zig c++").
func TestZigCCFromRoot(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "zig")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cc, cxx, ok := zigCCFromRoot(dir)
	if !ok {
		t.Fatal("expected zigCCFromRoot to detect the zig binary")
	}
	if cc != bin+" cc" || cxx != bin+" c++" {
		t.Errorf("unexpected CC/CXX: %q / %q", cc, cxx)
	}
	if _, _, ok := zigCCFromRoot(t.TempDir()); ok {
		t.Error("empty root should not report a zig binary")
	}
}

// TestZigArchiveInfo ensures every supported platform maps to a zig archive.
func TestZigArchiveInfo(t *testing.T) {
	info, err := zigArchiveInfoFor()
	if err != nil {
		// Unsupported platforms (e.g. Termux) return a friendly hint.
		if !strings.Contains(err.Error(), "C 编译器") && !strings.Contains(err.Error(), "Clang") && !strings.Contains(err.Error(), "clang") {
			t.Fatalf("unexpected zig archive error: %v", err)
		}
		return
	}
	switch runtime.GOOS {
	case "windows":
		if info.kind != "zip" {
			t.Errorf("windows should use a zip, got %q", info.kind)
		}
	default:
		if info.kind != "tar.xz" {
			t.Errorf("unix should use tar.xz, got %q", info.kind)
		}
	}
	if info.archive == "" || info.triple == "" {
		t.Errorf("empty archive info: %+v", info)
	}
}

// TestInstallCGoPluginPromptsForCompiler verifies that installing a plugin
// whose metadata.json declares cgo=true surfaces a CCompilerPromptError (and
// does not attempt to build) until the user picks a compiler.
func TestInstallCGoPluginPromptsForCompiler(t *testing.T) {	m := newTestManager(t)
	ctx := context.Background()

	src := t.TempDir()
	main := `package main

import (
	sdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
)

// #cgo CFLAGS: -I.
func main() { sdk.Serve(&sdk.Plugin{Name: "cgoplugin"}) }
`
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "metadata.json"), []byte(`{"name":"cgoplugin","version":"1.0.0","cgo":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// #cgo CFLAGS 触发静态扫描风险门（编译期可执行任意命令）：未确认风险时
	// 返回 *RiskError，即使插件声明了 cgo。
	_, err := m.InstallFromSource(ctx, "cgoplugin", src, InstallOptions{})
	var riskErr *RiskError
	if !errors.As(err, &riskErr) {
		t.Fatalf("expected *RiskError for cgo plugin before IgnoreRisk, got %T: %v", err, err)
	}
	if m.Get("cgoplugin") != nil {
		t.Fatal("cgo plugin must not be installed before risks are confirmed")
	}

	// 用户确认风险（IgnoreRisk）后，进入 C 编译器选择流程。
	_, err = m.InstallFromSource(ctx, "cgoplugin", src, InstallOptions{IgnoreRisk: true})
	var promptErr *CCompilerPromptError
	if !errors.As(err, &promptErr) {
		t.Fatalf("expected *CCompilerPromptError for cgo plugin, got %T: %v", err, err)
	}
	if m.Get("cgoplugin") != nil {
		t.Fatal("cgo plugin must not be installed before a compiler is chosen")
	}

	// A cancelled choice terminates install with a clear error.
	_, err = m.InstallFromSource(ctx, "cgoplugin", src, InstallOptions{IgnoreRisk: true, CCChoice: string(CCChoiceCancel)})
	if err == nil || !strings.Contains(err.Error(), "取消") {
		t.Fatalf("expected cancel error, got %v", err)
	}
}

// TestExtractClangArchiveSinglePass verifies the flat extraction strips the
// top-level triple/ prefix and writes straight to root (no temp-dir copy).
func TestExtractClangArchiveSinglePass(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "zig.zip")
	root := filepath.Join(dir, "out")

	// Build a zip with a single top-level "triple/" dir containing files.
	zf, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	add := func(name, content string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if content != "" {
			if _, err := w.Write([]byte(content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	add("triple/bin/zig", "#!/bin/sh\n")
	add("triple/bin/zig2", "#!/bin/sh\n")
	add("triple/lib/libfoo.so", "MZ")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zf.Close()

	if err := extractClangArchive(context.Background(), src, root, "triple"); err != nil {
		t.Fatalf("extractClangArchive: %v", err)
	}
	for _, want := range []string{"bin/zig", "bin/zig2", "lib/libfoo.so"} {
		if _, err := os.Stat(filepath.Join(root, want)); err != nil {
			t.Errorf("missing %s: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "triple")); err == nil {
		t.Error("top-level triple dir should be stripped, not kept")
	}
}

// TestExtractClangArchiveTarXz verifies .tar.xz extraction via mholt/archives.
func TestExtractClangArchiveTarXz(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "zig.tar.xz")
	root := filepath.Join(dir, "out")

	// Build a tar, xz-compress it (stdlib has no xz writer, so shell out to xz).
	tarPath := filepath.Join(dir, "zig.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	add := func(name, content string) {
		flag := byte(tar.TypeReg)
		if content == "" {
			flag = tar.TypeDir
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: flag}); err != nil {
			t.Fatal(err)
		}
		if content != "" {
			if _, err := tw.Write([]byte(content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	add("triple/", "")
	add("triple/bin/zig", "#!/bin/sh\n")
	add("triple/lib/libfoo.so", "MZ")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if out, err := exec.Command("xz", "-k", tarPath).CombinedOutput(); err != nil {
		t.Skipf("xz not available, skipping: %v %s", err, out)
	}
	if err := extractClangArchive(context.Background(), src, root, "triple"); err != nil {
		t.Fatalf("extractClangArchive tar.xz: %v", err)
	}
	for _, want := range []string{"bin/zig", "lib/libfoo.so"} {
		if _, err := os.Stat(filepath.Join(root, want)); err != nil {
			t.Errorf("missing %s: %v", want, err)
		}
	}
}

func f2(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
// TestClangLockFileDiscardsInterruptedInstall verifies that a leftover// .install-lock makes downloadAndSetupClang discard the cached root and
// re-download instead of trusting a half-extracted Clang. It serves a fake zig
// archive from a local httptest server so the test needs no network.
func TestClangLockFileDiscardsInterruptedInstall(t *testing.T) {
	old := os.Getenv("ASTRBOT_CLANG_BIN")
	oldMirror := os.Getenv("ASTRBOT_CLANG_MIRROR")
	t.Cleanup(func() {
		os.Setenv("ASTRBOT_CLANG_BIN", old)
		os.Setenv("ASTRBOT_CLANG_MIRROR", oldMirror)
	})
	root := t.TempDir()
	if err := os.Setenv("ASTRBOT_CLANG_BIN", root); err != nil {
		t.Fatal(err)
	}

	// A tiny fake zig distribution: a zip with <triple>/zig.
	var fakeZip bytes.Buffer
	zw := zip.NewWriter(&fakeZip)
	w, _ := zw.Create("zig-x86_64-linux-0.16.0/zig")
	w.Write([]byte("#!/bin/sh\nexit 0\n"))
	zw.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fakeZip.Bytes())
	}))
	defer srv.Close()
	if err := os.Setenv("ASTRBOT_CLANG_MIRROR", srv.URL); err != nil {
		t.Fatal(err)
	}

	// Simulate a previous interrupted install: a stale zig binary (would be
	// trusted on a healthy cache) PLUS a leftover lock file.
	staleZig := filepath.Join(root, "zig")
	if err := os.WriteFile(staleZig, []byte("stale-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(root, clangLockFile)
	if err := os.WriteFile(lock, []byte("2026-01-01T00:00:00Z"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The stale root (with its lock) must be discarded before download starts,
	// then a fresh zig is downloaded and installed.
	cc, cxx, err := downloadAndSetupClang(context.Background(), InstallOptions{})
	if err != nil {
		t.Fatalf("downloadAndSetupClang: %v", err)
	}
	if _, serr := os.Stat(lock); !os.IsNotExist(serr) {
		t.Errorf("lock file should have been removed after a clean install")
	}
	if data, _ := os.ReadFile(staleZig); string(data) == "stale-binary" {
		t.Error("stale zig root should have been discarded and replaced")
	}
	if !strings.Contains(cc, "zig") || !strings.Contains(cxx, "zig") {
		t.Errorf("expected zig-based CC/CXX, got %q / %q", cc, cxx)
	}
}

// TestExtractAbortsOnCancel verifies that a cancelled context aborts extraction
// (leaving the caller's lock in place for a later retry).
func TestExtractAbortsOnCancel(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "zig.zip")
	root := filepath.Join(dir, "out")

	zf, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	add := func(name, content string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if content != "" {
			if _, err := w.Write([]byte(content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	add("triple/bin/zig", "#!/bin/sh\n")
	add("triple/lib/libfoo.so", "MZ")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zf.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := extractClangArchive(ctx, src, root, "triple"); err == nil {
		t.Fatal("expected cancellation error from extractClangArchive")
	}
}

// TestResumeDownload verifies resumeDownload appends to an existing partial file
// using an HTTP Range request and reports cumulative progress.
func TestResumeDownload(t *testing.T) {
	full := []byte("0123456789abcdefghij") // 20 bytes
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rng := r.Header.Get("Range"); rng != "" {
			var from int64
			fmt.Sscanf(rng, "bytes=%d-", &from)
			if from >= int64(len(full)) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(full)-1, len(full)))
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusPartialContent)
			w.Write(full[from:])
			return
		}
		w.Write(full)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 10 * time.Second}
	dest := filepath.Join(t.TempDir(), "archive.bin")

	// Pre-write a partial file (first 8 bytes) to exercise resume.
	if err := os.WriteFile(dest, full[:8], 0o644); err != nil {
		t.Fatal(err)
	}
	var got progressRecorder
	if err := resumeDownload(context.Background(), client, srv.URL, dest, got.record); err != nil {
		t.Fatalf("resumeDownload: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(full) {
		t.Fatalf("resumed file mismatch: got %q want %q", data, full)
	}
	if got.last != int64(len(full)) {
		t.Errorf("expected final progress %d, got %d", len(full), got.last)
	}
}

type progressRecorder struct {
	last int64
}

func (p *progressRecorder) record(downloaded, total int64) {
	p.last = downloaded
}
