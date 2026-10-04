package plugin

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/ulikunitz/xz"
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

// TestReadPluginMetadataYAML covers the Python ecosystem metadata.yaml
// convention (name/display_name/short_desc/desc/version/author/repo).
func TestReadPluginMetadataYAML(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) {
		if err := os.WriteFile(filepath.Join(dir, "metadata.yaml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(`name: meme_manager
display_name: 表情包管理器
short_desc: 集表情包管理、智能发送、语义检索与云端同步于一体
desc: >-
  为 AstrBot 提供 WebUI 表情包管理页面
help: 功能：1. AI自动发送表情包
version: 4.15.1
author: anka
repo: https://github.com/anka-afk/astrbot_plugin_meme_manager
tags:
  - 表情包
`)
	meta, err := ReadPluginMetadata(dir)
	if err != nil {
		t.Fatalf("ReadPluginMetadata(yaml): %v", err)
	}
	if meta.Name != "meme_manager" {
		t.Errorf("Name = %q", meta.Name)
	}
	if meta.DisplayName != "表情包管理器" || meta.ShortDesc == "" {
		t.Errorf("DisplayName/ShortDesc 解析失败: %+v", meta)
	}
	if meta.Version != "4.15.1" || meta.Author != "anka" {
		t.Errorf("Version/Author 解析失败: %+v", meta)
	}
	if meta.Repo != "https://github.com/anka-afk/astrbot_plugin_meme_manager" {
		t.Errorf("Repo = %q", meta.Repo)
	}
	if !strings.Contains(meta.Description, "WebUI 表情包管理页面") {
		t.Errorf("Description 解析失败: %q", meta.Description)
	}

	// 未写 language 不影响解析（语言完全由入口文件推断）
	write("name: x\nversion: 1.0.0\n")
	meta, err = ReadPluginMetadata(dir)
	if err != nil {
		t.Fatalf("ReadPluginMetadata(yaml): %v", err)
	}
	if meta.Name != "x" {
		t.Errorf("Name = %q", meta.Name)
	}

	// 缺失 name → 报错
	write("desc: no name\n")
	if _, err := ReadPluginMetadata(dir); err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("expected missing name error, got %v", err)
	}

	// 两者都缺 → 报错提示两种文件
	if err := os.Remove(filepath.Join(dir, "metadata.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPluginMetadata(dir); err == nil || !strings.Contains(err.Error(), "metadata.yaml") {
		t.Fatalf("expected missing metadata error, got %v", err)
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

// TestResolveLanguage verifies language detection is purely entry-file based
// (main.py / __init__.py → python, else go) and never consults metadata.
func TestResolveLanguage(t *testing.T) {
	newDir := func(files ...string) string {
		dir := t.TempDir()
		for _, f := range files {
			if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	cases := []struct {
		name string
		dir  string
		want string
	}{
		{"main.py 推断 python", newDir("main.py"), "python"},
		{"__init__.py 推断 python", newDir("__init__.py"), "python"},
		{"main.py + main.go 共存 → python", newDir("main.py", "main.go"), "python"},
		{"仅 main.go 默认 go", newDir("main.go"), "go"},
		{"空目录默认 go", t.TempDir(), "go"},
		{"无任何入口默认 go", newDir("README.md"), "go"},
	}
	for _, c := range cases {
		if got := ResolveLanguage(c.dir); got != c.want {
			t.Errorf("%s: ResolveLanguage = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestEnsureCCompilerPromptsWithoutChoice exercises the detection flow: with no
// user choice, a system zig cc is used directly; otherwise the function must
// return a CCompilerPromptError (choose_gcc / choose_clang / download_zig_cc)
// so the caller can ask the user instead of silently succeeding.
func TestEnsureCCompilerPromptsWithoutChoice(t *testing.T) {
	cc, cxx, err := ensureCCompiler(context.Background(), InstallOptions{GoChoice: "download"})
	if err == nil {
		// zig cc (bundled or on PATH) was detected and used directly.
		if cc == "" || cxx == "" {
			t.Fatalf("zig cc detected but empty CC/CXX: %q / %q", cc, cxx)
		}
		return
	}
	var promptErr *CCompilerPromptError
	if !errors.As(err, &promptErr) {
		t.Fatalf("expected *CCompilerPromptError, got %T: %v", err, err)
	}
	switch promptErr.Kind {
	case PromptChooseGCC:
		if !promptErr.HasGCC || promptErr.GCCPath == "" {
			t.Errorf("choose_gcc prompt missing GCC info: %+v", promptErr)
		}
	case PromptChooseClang:
		if !promptErr.HasClang || promptErr.ClangPath == "" {
			t.Errorf("choose_clang prompt missing Clang info: %+v", promptErr)
		}
	case PromptDownloadZigCC:
		if promptErr.HasGCC || promptErr.HasClang {
			t.Errorf("download_zig_cc prompt should not report a compiler: %+v", promptErr)
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
	// CC/CXX 必须以 "<zig> cc"/"<zig> c++" 开头，并附带目标三元组
	//（-target <triple>，缺失会导致 Native 插件 NEEDED=libc.so 加载失败）。
	wantSuffix := zigTargetFlag()
	wantCC := bin + " cc"
	if wantSuffix != "" {
		wantCC += " " + wantSuffix
	}
	wantCXX := bin + " c++"
	if wantSuffix != "" {
		wantCXX += " " + wantSuffix
	}
	if cc != wantCC || cxx != wantCXX {
		t.Errorf("unexpected CC/CXX: %q / %q (want %q / %q)", cc, cxx, wantCC, wantCXX)
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
func TestInstallCGoPluginPromptsForCompiler(t *testing.T) {
	m := newTestManager(t)
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
	_, err := m.InstallFromSource(ctx, "cgoplugin", src, InstallOptions{GoChoice: "download"})
	var riskErr *RiskError
	if !errors.As(err, &riskErr) {
		t.Fatalf("expected *RiskError for cgo plugin before IgnoreRisk, got %T: %v", err, err)
	}
	if m.Get("cgoplugin") != nil {
		t.Fatal("cgo plugin must not be installed before risks are confirmed")
	}

	// 用户确认风险（IgnoreRisk）后，进入 C 编译器选择流程。
	_, err = m.InstallFromSource(ctx, "cgoplugin", src, InstallOptions{IgnoreRisk: true, GoChoice: "download"})
	var promptErr *CCompilerPromptError
	if !errors.As(err, &promptErr) {
		t.Fatalf("expected *CCompilerPromptError for cgo plugin, got %T: %v", err, err)
	}
	if m.Get("cgoplugin") != nil {
		t.Fatal("cgo plugin must not be installed before a compiler is chosen")
	}

	// A cancelled choice terminates install with a clear error.
	_, err = m.InstallFromSource(ctx, "cgoplugin", src, InstallOptions{IgnoreRisk: true, CCChoice: string(CCChoiceCancel), GoChoice: "download"})
	if err == nil || !strings.Contains(err.Error(), "取消") {
		t.Fatalf("expected cancel error, got %v", err)
	}
}

// TestExtractZigCCArchiveSinglePass verifies the flat extraction strips the
// top-level triple/ prefix and writes straight to root (no temp-dir copy).
func TestExtractZigCCArchiveSinglePass(t *testing.T) {
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
	_ = zf.Close()

	if err := extractZigCCArchive(context.Background(), src, root, "triple"); err != nil {
		t.Fatalf("extractZigCCArchive: %v", err)
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

// TestExtractZigCCArchiveTarXz verifies .tar.xz extraction via mholt/archives.
func TestExtractZigCCArchiveTarXz(t *testing.T) {
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
	_ = f.Close()

	if out, err := exec.Command("xz", "-k", tarPath).CombinedOutput(); err != nil {
		t.Skipf("xz not available, skipping: %v %s", err, out)
	}
	if err := extractZigCCArchive(context.Background(), src, root, "triple"); err != nil {
		t.Fatalf("extractZigCCArchive tar.xz: %v", err)
	}
	for _, want := range []string{"bin/zig", "lib/libfoo.so"} {
		if _, err := os.Stat(filepath.Join(root, want)); err != nil {
			t.Errorf("missing %s: %v", want, err)
		}
	}
}

// TestZigCCLockFileDiscardsInterruptedInstall verifies that a leftover// .install-lock makes downloadAndSetupZigCC discard the cached root and
// re-download instead of trusting a half-extracted zig cc. It serves a fake zig
// archive from a local httptest server so the test needs no network.
func TestZigCCLockFileDiscardsInterruptedInstall(t *testing.T) {
	if _, _, ok := detectSystemClang(); ok {
		t.Skip("system clang present; download path not exercised")
	}
	old := os.Getenv("ASTRBOT_ZIGCC_BIN")
	oldMirror := os.Getenv("ASTRBOT_ZIGCC_MIRROR")
	t.Cleanup(func() {
		_ = os.Setenv("ASTRBOT_ZIGCC_BIN", old)
		_ = os.Setenv("ASTRBOT_ZIGCC_MIRROR", oldMirror)
	})
	root := t.TempDir()
	if err := os.Setenv("ASTRBOT_ZIGCC_BIN", root); err != nil {
		t.Fatal(err)
	}

	// A tiny fake zig distribution matching the platform archive format
	// (tar.xz on linux/macos, zip on windows) with a <triple>/zig entry.
	info, err := zigArchiveInfoFor()
	if err != nil {
		t.Skipf("no zig archive for this platform: %v", err)
	}
	fakeArchive := buildFakeZigArchive(t, info)

	// zigArchiveSHA256 引入后，默认版本归档下载后会先做 sha256 校验：mock 的
	// 假归档与官方 pin 值不符会被判"校验失败"而走不到 extract 路径。测试同包
	// 临时把 info.archive 的 pin 覆盖为假归档的真实 sha256（defer 恢复原值），
	// 让下载通过校验、真正进入下载→解压→清锁路径。
	zsum := sha256.Sum256(fakeArchive)
	oldSum, hadSum := zigArchiveSHA256[info.archive]
	zigArchiveSHA256[info.archive] = hex.EncodeToString(zsum[:])
	t.Cleanup(func() {
		if hadSum {
			zigArchiveSHA256[info.archive] = oldSum
		} else {
			delete(zigArchiveSHA256, info.archive)
		}
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fakeArchive)
	}))
	defer srv.Close()
	if err := os.Setenv("ASTRBOT_ZIGCC_MIRROR", srv.URL); err != nil {
		t.Fatal(err)
	}

	// Simulate a previous interrupted install: a stale zig binary (would be
	// trusted on a healthy cache) PLUS a leftover lock file.
	staleZig := filepath.Join(root, "zig")
	if err := os.WriteFile(staleZig, []byte("stale-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(root, zigCCLockFile)
	if err := os.WriteFile(lock, []byte("2026-01-01T00:00:00Z"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The stale root (with its lock) must be discarded before download starts,
	// then a fresh zig is downloaded and installed.
	cc, cxx, err := downloadAndSetupZigCC(context.Background(), InstallOptions{GoChoice: "download"})
	if err != nil {
		t.Fatalf("downloadAndSetupZigCC: %v", err)
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

// TestZigMirrorBases 验证 zig cc 下载源优先级：用户选择 > ASTRBOT_ZIGCC_MIRROR
// 环境变量 > 默认列表（华为云 → 官方）。
func TestZigMirrorBases(t *testing.T) {
	// 默认列表：首个为华为云加速，含官方 ziglang.org。
	def := defaultZigMirrorBases()
	if len(def) < 2 {
		t.Fatalf("defaultZigMirrorBases 至少应含加速+官方两项, got %v", def)
	}
	if !strings.Contains(def[0], "liujiacai.net") {
		t.Errorf("默认首选应为国内社区镜像, got %q", def[0])
	}
	if !strings.Contains(def[len(def)-1], "ziglang.org") {
		t.Errorf("默认末位应为官方 ziglang.org, got %q", def[len(def)-1])
	}

	// 用户显式选择覆盖默认。
	got := zigMirrorBases("https://mirror.example.com/zig/")
	if len(got) != 1 || got[0] != "https://mirror.example.com/zig" {
		t.Errorf("用户选择镜像应独占且去尾斜杠, got %v", got)
	}

	// 环境变量次之。
	t.Setenv("ASTRBOT_ZIGCC_MIRROR", "https://env.example.com/zig")
	got = zigMirrorBases("")
	if len(got) != 1 || got[0] != "https://env.example.com/zig" {
		t.Errorf("ASTRBOT_ZIGCC_MIRROR 应被采用, got %v", got)
	}

	// 都为空 → 默认列表。
	t.Setenv("ASTRBOT_ZIGCC_MIRROR", "")
	got = zigMirrorBases("")
	if len(got) != len(def) {
		t.Errorf("空选择应回退默认列表, got %v", got)
	}
}

// buildFakeZigArchive builds a minimal valid plugin archive in the platform's
// zig archive format (tar.xz / zip) containing a single "<triple>/zig" entry,
// so the download→extract path can be exercised without network access.
func buildFakeZigArchive(t *testing.T, info zigArchiveInfo) []byte {
	t.Helper()
	payload := []byte("#!/bin/sh\nexit 0\n")
	var buf bytes.Buffer
	switch info.kind {
	case "zip":
		zw := zip.NewWriter(&buf)
		w, err := zw.Create(info.triple + "/zig")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	default: // tar.xz
		xw, err := xz.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		tw := tar.NewWriter(xw)
		hdr := &tar.Header{Name: info.triple + "/zig", Mode: 0o755, Size: int64(len(payload))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := xw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

// TestZigCCExtractFailureKeepsLockAndClearsCache verifies that when the
// downloaded zig cc archive fails to extract (i.e. it is corrupt), the install
// lock is PRESERVED so the next run discards the half-extracted root, and the
// corrupt archive cache file is removed so it is not trusted forever (which
// would otherwise make cgo installs fail indefinitely).
func TestZigCCExtractFailureKeepsLockAndClearsCache(t *testing.T) {
	if _, _, ok := detectSystemClang(); ok {
		t.Skip("system clang present; download path not exercised")
	}
	info, err := zigArchiveInfoFor()
	if err != nil {
		t.Skipf("no zig archive for this platform: %v", err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ASTRBOT_ZIGCC_BIN", t.TempDir())
	// Serve a non-archive body so download succeeds but extraction fails.
	mockBody := []byte("this is definitely not a zip or tar archive")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(mockBody)
	}))
	defer srv.Close()
	t.Setenv("ASTRBOT_ZIGCC_MIRROR", srv.URL)

	// zigArchiveSHA256 引入后，默认版本归档会先做 sha256 校验：mock 的非归档
	// 内容与官方 pin 值不符会被判"校验失败"而走不到 extract 路径。测试同包
	// 临时把 info.archive 的 pin 覆盖为 mock 内容的真实 sha256（defer 恢复
	// 原值），让下载通过校验、真正进入 extract 失败路径。
	bodySum := sha256.Sum256(mockBody)
	oldSum, hadSum := zigArchiveSHA256[info.archive]
	zigArchiveSHA256[info.archive] = hex.EncodeToString(bodySum[:])
	t.Cleanup(func() {
		if hadSum {
			zigArchiveSHA256[info.archive] = oldSum
		} else {
			delete(zigArchiveSHA256, info.archive)
		}
	})

	_, _, err = downloadAndSetupZigCC(context.Background(), InstallOptions{GoChoice: "download"})
	if err == nil {
		t.Fatal("expected extract failure for a corrupt archive")
	}

	root := zigRoot()
	lock := filepath.Join(root, zigCCLockFile)
	if _, serr := os.Stat(lock); os.IsNotExist(serr) {
		t.Error("install lock must be preserved on extract failure so the next run discards the root")
	}
	cacheFile := filepath.Join(toolchainUserStateDir(), "zigcc-download", info.archive)
	if _, serr := os.Stat(cacheFile); !os.IsNotExist(serr) {
		t.Errorf("corrupt archive cache %s should have been removed, stat err=%v", cacheFile, serr)
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
	_ = zf.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := extractZigCCArchive(ctx, src, root, "triple"); err == nil {
		t.Fatal("expected cancellation error from extractZigCCArchive")
	}
}

// TestResumeDownload verifies resumeDownload appends to an existing partial file
// using an HTTP Range request and reports cumulative progress.
func TestResumeDownload(t *testing.T) {
	full := []byte("0123456789abcdefghij") // 20 bytes
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rng := r.Header.Get("Range"); rng != "" {
			var from int64
			_, _ = fmt.Sscanf(rng, "bytes=%d-", &from)
			if from >= int64(len(full)) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(full)-1, len(full)))
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(full[from:])
			return
		}
		_, _ = w.Write(full)
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
