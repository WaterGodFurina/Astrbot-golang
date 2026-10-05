package plugin

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/WaterGodFurina/Astrbot-golang/internal/toolchain"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

// sdkModulePath is the module path of the standalone plugin SDK that every
// plugin links against. Builds `replace` it to the local copy.
const sdkModulePath = "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"

// sdkModuleVersion 是宿主内置的插件 SDK 版本（与宿主 go.mod 的 require 一致，
// 发版时同步 bump）。发布版宿主进程的 CWD 下没有 go.mod，SDK 解析与下载在
// 找不到 go.mod 时以该常量兜底定位模块缓存，不再依赖进程工作目录。
const sdkModuleVersion = "v1.9.1"

// nativeEntryUnix 是 Native 构建时注入插件 package main 的生成入口
// （不改动插件作者源码）。宿主用 plugin.Open 加载 .so 后 Lookup 并调用它：
// 它把作者声明的包级变量 plugin（*sdk.Plugin）交给 native.Serve，后者跑
// OnLoad、绑定进程内 HostService，并返回 PluginService 供宿主【直接函数调用】。
//
// 注意：Native 插件作者必须用一个非 main 文件声明 `var plugin = &sdk.Plugin{...}`
// （main() 不执行、且这里按名引用它）。已在 README 记录。
const nativeEntryUnix = `package main

import native "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/native"

func AstrBotNativePlugin(pluginID string) (native.Plugin, error) {
	return native.Serve(plugin, pluginID)
}
`

// nativeEntryWindows 是 Windows Native 构建（-buildmode=c-shared）注入的入口。
// 宿主用 LoadLibrary 加载 .dll 后 GetProcAddress 调用 C 符号
// AstrBotPluginOpen/Call/Free/Close（见 SDK native/cabi_windows.go）。本文件只
// 负责把作者的包级 plugin 变量登记给 SDK（main() 在 c-shared 下不执行）。
//
// 注意：不声明 func main()——作者源码已有 main()（内容 sdk.Serve(plugin)），
// c-shared 下不执行但需存在，重复声明会 "main redeclared"。
const nativeEntryWindows = `package main

import native "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/native"

func init() { native.SetPlugin(plugin) }
`

// grpcEntry 是为普通（gRPC 子进程）Go 插件注入的空导入：它链接
// transport/grpc，使其 init() 注册 sdk.Serve 的真实实现与反向调用拨号器。
// Native 构建不注入本文件，因此 Native 插件不链接 grpc/go-plugin。
const grpcEntry = `package main

import _ "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/transport/grpc"
`

// Compiler builds plugin source into a platform-native executable using the
// bundled Go toolchain. It also performs static safety checks (import
// blacklist) and `go vet` before compiling.
type Compiler struct {
	tc *toolchain.Toolchain
	// GoProxy 是 Go 包仓库地址（GOPROXY），空则默认 goproxy.cn。
	GoProxy string
	// GoFlags 是附加到 go build 的额外参数（如 "-tags xxx"），空则无。
	GoFlags string
}

// defaultGoProxy 是未配置 goproxy 时使用的 Go 模块代理。
const defaultGoProxy = "https://goproxy.cn,direct"

// NewCompiler creates a compiler backed by the given toolchain.
func NewCompiler(tc *toolchain.Toolchain) *Compiler {
	return &Compiler{tc: tc, GoProxy: defaultGoProxy}
}

// SetGoConfig 注入插件编译的 Go 包仓库地址与额外构建参数（来自 config goproxy/goflags）。
func (c *Compiler) SetGoConfig(goproxy, goflags string) {
	if goproxy != "" {
		c.GoProxy = goproxy
	}
	c.GoFlags = goflags
}

// goproxyEnv 返回编译时使用的 GOPROXY。
func (c *Compiler) goproxyEnv() string {
	if c.GoProxy != "" {
		return c.GoProxy
	}
	return defaultGoProxy
}

// goflagsEnv 返回编译时使用的 GOFLAGS（附加用户 goflags）。
func (c *Compiler) goflagsEnv() string {
	base := "-mod=mod"
	if c.GoFlags != "" {
		return base + " " + c.GoFlags
	}
	return base
}

// validateModuleName rejects module names that could corrupt or inject into
// the generated go.mod: the name is written verbatim into the `module` line,
// so newlines/whitespace/control characters would let it smuggle arbitrary
// extra lines (e.g. a forged `require`).
func validateModuleName(moduleName string) error {
	if err := module.CheckPath(moduleName); err != nil {
		return fmt.Errorf("非法 module 名称 %q: %w", moduleName, err)
	}
	return nil
}

// Prepare ensures the plugin module builds against the local SDK: it writes a
// go.mod (when missing) or patches the existing one to require the SDK module
// and replace it with the local SDK directory.
func (c *Compiler) Prepare(srcDir, moduleName string) error {
	// moduleName 会原样写入 go.mod 的 module 行，先校验为合法 Go module
	// path，拒绝换行/空白/控制字符等注入（go.mod 注入）。
	if err := validateModuleName(moduleName); err != nil {
		return err
	}
	sdkDir, err := c.sdkDir()
	if err != nil {
		return fmt.Errorf("resolve SDK dir: %w", err)
	}

	modPath := filepath.Join(srcDir, "go.mod")
	data, err := os.ReadFile(modPath) // #nosec G304 -- 读取插件模块自身 go.mod（srcDir 已归一化）
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		data = []byte("module " + moduleName + "\n")
	}
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return fmt.Errorf("parse go.mod: %w", err)
	}
	if f.Module == nil {
		if err := f.AddModuleStmt(moduleName); err != nil {
			return err
		}
	}
	if f.Go == nil {
		_ = f.AddGoStmt("1.23")
	}
	hasRequire := false
	for _, r := range f.Require {
		if r.Mod.Path == sdkModulePath {
			hasRequire = true
			break
		}
	}
	if !hasRequire {
		if err := f.AddRequire(sdkModulePath, "v0.0.0"); err != nil {
			return fmt.Errorf("add require: %w", err)
		}
	}
	_ = f.DropReplace(sdkModulePath, "")
	if err := f.AddReplace(sdkModulePath, "", sdkDir, ""); err != nil {
		return fmt.Errorf("add replace: %w", err)
	}
	out, err := f.Format()
	if err != nil {
		return fmt.Errorf("format go.mod: %w", err)
	}
	return os.WriteFile(modPath, out, 0o644) // #nosec G306 -- go.mod 常规权限即可
}

// Tidy re-resolves the plugin module's dependency graph after Prepare replaced
// the SDK with the local copy. Without it the plugin keeps the indirect versions
// recorded in its own go.mod (e.g. protobuf v1.34.2 from an older SDK), while the
// host links the SDK's own versions (v1.36.11). Native plugin.Open requires both
// sides to share every linked package's exact version, so a stale indirect dep
// makes it fail with "plugin was built with a different version of package
// google.golang.org/protobuf/internal/pragma".
//
// Failure is non-fatal: a network hiccup leaves the module as-is and the build
// proceeds (matching the previous behavior) rather than blocking the install.
func (c *Compiler) Tidy(ctx context.Context, srcDir string) error {
	goBin, err := c.tc.Ensure()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, goBin, "mod", "tidy") // #nosec G204 -- 依赖对齐：args 固定; nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd.Dir = srcDir
	cmd.Env = c.tc.BuildEnv(map[string]string{"GOPROXY": c.goproxyEnv(), "GOFLAGS": c.goflagsEnv()})
	if out, err := cmd.CombinedOutput(); err != nil {
		logger.I18nWarn("插件 go mod tidy 失败（依赖版本可能落后于宿主 SDK）: %v\n%s", err, out)
		return nil
	}
	return nil
}

// Vet runs `go vet ./...` in the plugin module.
func (c *Compiler) Vet(ctx context.Context, srcDir string) error {
	goBin, err := c.tc.Ensure()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, goBin, "vet", "./...") // #nosec G204 -- 编译插件模块（核心），args 固定; nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd.Dir = srcDir
	cmd.Env = c.tc.BuildEnv(map[string]string{"GOPROXY": c.goproxyEnv(), "GOFLAGS": c.goflagsEnv()})
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go vet: %w\n%s", err, out)
	}
	return nil
}

// Build compiles the Go module at srcDir into outputPath. The environment is
// derived from the toolchain (isolated GOPATH; CGO_ENABLED from the host).
func (c *Compiler) Build(ctx context.Context, srcDir, outputPath string) error {
	return c.BuildWithProgressC(ctx, srcDir, outputPath, nil, "", "")
}

// BuildWithProgress is like Build but reports toolchain download progress via
// the callback when the bundled Go toolchain has to be provisioned first.
func (c *Compiler) BuildWithProgress(ctx context.Context, srcDir, outputPath string, progress toolchain.ProgressFunc) error {
	return c.BuildWithProgressC(ctx, srcDir, outputPath, progress, "", "")
}

// BuildWithProgressC is BuildWithProgress with an explicit C compiler override.
// cc/cxx are the CC/CXX compiler paths (e.g. from ensureCCompiler); when empty
// the toolchain decides CGO_ENABLED from the host environment (CGOEnabled()).
func (c *Compiler) BuildWithProgressC(ctx context.Context, srcDir, outputPath string, progress toolchain.ProgressFunc, cc, cxx string) error {
	return c.build(ctx, srcDir, outputPath, progress, cc, cxx, nil)
}

// BuildWithProgressOut is BuildWithProgressC plus a callback invoked with each
// line of `go build -v` output as it is produced (dependency downloads like
// "go: downloading github.com/mattn/go-sqlite3 v1.14.49" and the module path of
// each package being compiled), so the WebUI can surface live build progress.
func (c *Compiler) BuildWithProgressOut(ctx context.Context, srcDir, outputPath string, progress toolchain.ProgressFunc, cc, cxx string, outputCb func(line string)) error {
	return c.build(ctx, srcDir, outputPath, progress, cc, cxx, outputCb)
}

func (c *Compiler) build(ctx context.Context, srcDir, outputPath string, progress toolchain.ProgressFunc, cc, cxx string, outputCb func(line string)) error {
	goBin, err := c.tc.EnsureWithProgress(progress)
	if err != nil {
		return fmt.Errorf("ensure toolchain: %w", err)
	}
	// Resolve to an absolute path: go build resolves relative -o against the
	// working directory (srcDir), but the caller expects the artifact relative
	// to the host's data dir.
	absOut, err := filepath.Abs(outputPath)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}
	// #nosec G301 -- 插件构建产物目录（用户态）
	if err := os.MkdirAll(filepath.Dir(absOut), 0o755); err != nil {
		return err
	}
	// Link the gRPC transport so sdk.Serve (now a grpc-free hook) is populated
	// in subprocess plugins. Native builds inject native_entry.go instead and do
	// not link grpc/go-plugin.
	grpcEntryPath := filepath.Join(srcDir, "grpc_entry.go")
	if err := os.WriteFile(grpcEntryPath, []byte(grpcEntry), 0o644); err != nil { // #nosec G306 -- 生成文件，常规权限
		return fmt.Errorf("inject grpc entry: %w", err)
	}
	defer os.Remove(grpcEntryPath)

	args := []string{"build"}
	if outputCb != nil {
		args = append(args, "-v")
	}
	args = append(args, "-o", absOut, "-ldflags=-s -w", "./...")
	cmd := exec.CommandContext(ctx, goBin, args...) // #nosec G204 -- 编译插件模块（核心），args 由固定 flag 拼装; nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd.Dir = srcDir
	extra := map[string]string{
		"GOPROXY": c.goproxyEnv(),
		"GOFLAGS": c.goflagsEnv(),
	}
	if cc != "" {
		extra["CC"] = cc
		extra["CXX"] = cxx
		extra["CGO_ENABLED"] = "1"
	}
	cmd.Env = c.tc.BuildEnv(extra)

	if outputCb == nil {
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("go build: %w\n%s", err, out)
		}
		return nil
	}

	// Stream `go build -v` output line by line so the WebUI can show live
	// dependency downloads and the packages currently being compiled.
	// stdout and stderr must be buffered separately: cmd.Stdout is written by
	// exec's internal copy goroutine, while the scanner below reads stderr
	// through a TeeReader — sharing one buffer would race both writers.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("go build start: %w", err)
	}
	scanner := bufio.NewScanner(io.TeeReader(stderr, &stderrBuf))
	scanner.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)
	for scanner.Scan() {
		outputCb(scanner.Text())
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("go build: %w\n%s", err, stderrBuf.String())
	}
	return nil
}

// findSDKDir locates the local SDK module directory, resolving it from
// ASTRBOT_GO_SDK, the nearest go.mod `replace`, the module cache (from the
// go.mod `require` version), or the conventional sibling directory.
//
// It has no toolchain context (test-support helper, e.g. BuildTestPlugin), so
// the module-cache fallback uses the system `go` from PATH. Compilation paths
// go through findSDKDirWithGo instead, which uses the bundled toolchain.
func findSDKDir() (string, error) {
	return findSDKDirWithGo("")
}

// findSDKDirWithGo is findSDKDir using an explicit go binary for the module
// cache lookup (the bundled toolchain, consistent with plugin builds). When
// goBin is empty the system `go` on PATH is used. The work directory is pinned
// to the nearest go.mod so resolution does not depend on the process CWD.
func findSDKDirWithGo(goBin string) (string, error) {
	if p := os.Getenv("ASTRBOT_GO_SDK"); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(filepath.Join(abs, "go.mod")); err == nil {
			return abs, nil
		}
		return "", fmt.Errorf("ASTRBOT_GO_SDK=%q has no go.mod", p)
	}

	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		// #nosec G304 -- 向上查找宿主 go.mod 定位 SDK
		if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
			if sdk := sdkReplaceFromGoMod(data); sdk != "" {
				p := sdk
				if !filepath.IsAbs(p) {
					p = filepath.Join(dir, p)
				}
				abs, err := filepath.Abs(p)
				if err != nil {
					return "", err
				}
				return abs, nil
			}
			// 无本地 replace 时（SDK 已作为依赖从 GitHub 拉取），从 go.mod
			// 的 require 版本在 GOMODCACHE 定位 SDK 源码目录。
			if version := sdkRequireFromGoMod(data); version != "" {
				if p, err := sdkInModCache(goBin, dir, version); err == nil {
					return p, nil
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// 兜底：发布版宿主无 go.mod（CWD 任意），用内置 SDK 版本直接查模块缓存。
	if p, err := sdkInModCache(goBin, "", sdkModuleVersion); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("no go.mod with the AstrBot SDK replace found (set %s)", "ASTRBOT_GO_SDK")
}

// sdkDir resolves the local SDK module directory using the bundled toolchain's
// go binary, falling back to the system `go` when the toolchain cannot be
// provisioned.
func (c *Compiler) sdkDir() (string, error) {
	if c.tc != nil {
		if goBin, err := c.tc.Ensure(); err == nil {
			return findSDKDirWithGo(goBin)
		}
	}
	return findSDKDirWithGo("")
}

// sdkRequireFromGoMod extracts the SDK require version from go.mod contents
// (single-line form: "github.com/WaterGodFurina/Astrbot-go-plugin-sdk vX.Y.Z").
func sdkRequireFromGoMod(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, sdkModulePath+" ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			return fields[1]
		}
	}
	return ""
}

// sdkInModCache resolves the SDK source directory in the Go module cache for a
// given require version. `go list -m` returns the actual on-disk directory
// (module cache escapes uppercase letters as !x, so manual path joining would
// break). goBin is the `go` binary to run (empty → the system `go` from PATH,
// only used by toolchain-less test helpers); workDir pins the command's working
// directory to the go.mod location instead of the process CWD.
func sdkInModCache(goBin, workDir, version string) (string, error) {
	// 主路径：按 GOMODCACHE 布局直接文件系统定位（不加载宿主依赖图、
	// 不依赖网络，Windows 网络差时 go list -m 加载完整模块图会失败）。
	if version != "" {
		if dir := sdkInModuleCacheDir(goBin, version); dir != "" {
			logger.Debug("SDK 解析：命中模块缓存目录 %s", dir)
			return dir, nil
		}
		logger.Debug("SDK 解析：模块缓存未命中（version=%s），回退 go list -m", version)
	}
	// 回退：go list -m 解析（完整依赖图，网络好时也能命中本地 replace 场景）。
	if goBin == "" {
		goBin = "go"
	}
	cmd := exec.Command(goBin, "list", "-m", "-f", "{{.Dir}}", sdkModulePath) // #nosec G204 -- 插件编译核心：`go list -m` 定位 SDK 模块缓存，参数全为固定常量，无注入面; nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	out, err := cmd.Output()
	if err != nil {
		// stderr 里通常是依赖图加载失败的根因（缺失模块 go.mod / 网络错误）。
		var stderr []byte
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = ee.Stderr
		}
		logger.Debug("SDK 解析：go list -m 失败（go=%s dir=%s）: %v %s", goBin, workDir, err, strings.TrimSpace(string(stderr)))
		return "", fmt.Errorf("locate SDK %s@%s in module cache: %w", sdkModulePath, version, err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", fmt.Errorf("cannot locate SDK source in module cache")
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return "", fmt.Errorf("SDK source %q missing go.mod: %w", dir, err)
	}
	return dir, nil
}

// sdkInModuleCacheDir 直接按 GOMODCACHE 目录布局定位 SDK 源码目录
// （$GOMODCACHE/<module-path>@<version>，大写字母按 Go 规范转 !x! 编码）。
// go env GOMODCACHE 只读本地配置，不加载模块图、不联网。
func sdkInModuleCacheDir(goBin, version string) string {
	if goBin == "" {
		goBin = "go"
	}
	out, err := exec.Command(goBin, "env", "GOMODCACHE").Output() // #nosec G204 -- 固定参数，无注入面
	if err != nil {
		logger.Debug("SDK 解析：go env GOMODCACHE 失败（go=%s）: %v", goBin, err)
		return ""
	}
	modcache := strings.TrimSpace(string(out))
	if modcache == "" {
		logger.Debug("SDK 解析：go env GOMODCACHE 返回空（go=%s）", goBin)
		return ""
	}
	dir := filepath.Join(modcache, moduleCachePath(sdkModulePath, version))
	logger.Debug("SDK 解析：检查模块缓存 %s", dir)
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		return dir
	} else {
		logger.Debug("SDK 解析：%s 不存在（%v）", dir, err)
	}
	return ""
}

// moduleCachePath 把模块路径+版本转为 GOMODCACHE 目录名：大写字母前缀
// "!" 并转小写（如 WaterGodFurina → !water!god!furina）。
func moduleCachePath(modPath, version string) string {
	var sb strings.Builder
	for _, r := range modPath {
		if r >= 'A' && r <= 'Z' {
			sb.WriteByte('!')
			sb.WriteRune(r - 'A' + 'a')
		} else {
			sb.WriteRune(r)
		}
	}
	return sb.String() + "@" + version
}

// sdkReplaceFromGoMod extracts the SDK replace target from go.mod contents
// (handles both "replace mod => path" and block form).
func sdkReplaceFromGoMod(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, sdkModulePath) || !strings.Contains(line, "=>") {
			continue
		}
		return strings.TrimSpace(strings.SplitN(line, "=>", 2)[1])
	}
	return ""
}

// moduleNameFromID sanitizes a plugin id into a valid Go module path.
func moduleNameFromID(id string) string {
	return "example.com/astrbot-plugin/" + sanitizeID(strings.ToLower(id))
}

// sanitizeID makes an id safe for use in file names.
// SanitizeIDLocal 导出 sanitizeID 供 dashboard 配置文件路径构造使用
// （插件数据目录名 = stable id 的同一净化规则）。
func SanitizeIDLocal(id string) string {
	return sanitizeID(id)
}

func sanitizeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "." || s == ".." {
		return "plugin"
	}
	return s
}

// artifactName returns the compiled binary file name for the current platform.
func artifactName(id string) string {
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	return sanitizeID(id) + "-" + runtime.GOOS + "-" + runtime.GOARCH + ext
}

// nativeArtifactName returns the Native shared-library file name for the
// current platform: "<id>-<GOOS>-<GOARCH>.so" (Unix) / ".dll" (Windows).
func nativeArtifactName(id string) string {
	base := sanitizeID(id) + "-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		return base + ".dll"
	}
	return base + ".so"
}

// BuildNative compiles a plugin module into a Native shared library that the
// host loads in-process. Platform transport differs but the source shape is the
// same (`var plugin` + main()):
//
//   - Unix: -buildmode=plugin, entry exports AstrBotNativePlugin; host uses
//     plugin.Open + direct Go calls (zero serialization).
//   - Windows: -buildmode=c-shared, entry registers the plugin via
//     native.SetPlugin and the SDK exports the C-handle bridge
//     (AstrBotPluginOpen/Call/Free/Close); host uses LoadLibrary + sdkv1
//     protobuf bytes (one marshal round trip).
//
// CGO must be enabled; cc/cxx come from ensureCCompiler. The injected entry file
// is removed after the build so the plugin source directory stays untouched.
// 两平台都不导入 transport/grpc，故 Native 插件不链接 grpc/go-plugin。
func (c *Compiler) BuildNative(ctx context.Context, srcDir, outputPath string, progress toolchain.ProgressFunc, cc, cxx string, outputCb func(line string)) error {
	entry := nativeEntryUnix
	if runtime.GOOS == "windows" {
		entry = nativeEntryWindows
	}
	entryPath := filepath.Join(srcDir, "native_entry.go")
	if err := os.WriteFile(entryPath, []byte(entry), 0o644); err != nil { // #nosec G306 -- 注入到插件 srcDir 的生成文件，常规权限
		return fmt.Errorf("inject native entry: %w", err)
	}
	defer os.Remove(entryPath)

	goBin, err := c.tc.EnsureWithProgress(progress)
	if err != nil {
		return fmt.Errorf("ensure toolchain: %w", err)
	}
	absOut, err := filepath.Abs(outputPath)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absOut), 0o755); err != nil { // #nosec G301 -- 插件构建产物目录（用户态）
		return err
	}

	buildmode := "plugin"
	if runtime.GOOS == "windows" {
		buildmode = "c-shared"
	}
	args := []string{"build", "-buildmode=" + buildmode}
	if outputCb != nil {
		args = append(args, "-v")
	}
	args = append(args, "-o", absOut, "-ldflags=-s -w", "./...")
	cmd := exec.CommandContext(ctx, goBin, args...) // #nosec G204 -- 编译插件模块（核心），args 由固定 flag 拼装
	cmd.Dir = srcDir
	extra := map[string]string{
		"GOPROXY":     c.goproxyEnv(),
		"GOFLAGS":     c.goflagsEnv(),
		"CGO_ENABLED": "1",
	}
	if cc != "" {
		extra["CC"] = cc
		extra["CXX"] = cxx
	}
	cmd.Env = c.tc.BuildEnv(extra)

	if outputCb == nil {
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("go build native: %w\n%s", err, out)
		}
		return nil
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("go build native start: %w", err)
	}
	scanner := bufio.NewScanner(io.TeeReader(stderr, &stderrBuf))
	scanner.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)
	for scanner.Scan() {
		outputCb(scanner.Text())
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("go build native: %w\n%s", err, stderrBuf.String())
	}
	if os.Getenv("ASTRBOT_DEBUG_NATIVE_BUILDID") != "" {
		logger.I18nInfo("native buildid 诊断：%s", c.pragmaBuildID(ctx, srcDir, extra))
		if data, rerr := os.ReadFile(filepath.Join(srcDir, "go.mod")); rerr == nil { // #nosec G304 -- 诊断
			logger.I18nInfo("native 诊断 go.mod:\n%s", data)
		}
		if mods := c.listModules(ctx, srcDir, extra); mods != "" {
			logger.I18nInfo("native 诊断 modules:\n%s", mods)
		}
	}
	return nil
}

// listModules 是临时诊断：dump 插件构建环境解析出的关键模块版本。
func (c *Compiler) listModules(ctx context.Context, srcDir string, extra map[string]string) string {
	goBin, err := c.tc.Ensure()
	if err != nil {
		return "ensure: " + err.Error()
	}
	cmd := exec.CommandContext(ctx, goBin, "list", "-m", "-f", "{{.Path}} {{.Version}}", "google.golang.org/protobuf", "google.golang.org/grpc", "google.golang.org/genproto/googleapis/rpc", "github.com/WaterGodFurina/Astrbot-go-plugin-sdk") // #nosec G204 -- 诊断：args 固定
	cmd.Dir = srcDir
	cmd.Env = c.tc.BuildEnv(extra)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)) + " err=" + fmt.Sprint(err)
}

// pragmaBuildID 是临时诊断：解析插件构建环境（隔离 GOPATH、插件 go.mod）下
// google.golang.org/protobuf/internal/pragma 的包 build ID，用于与宿主对比
// 定位 plugin.Open "different version of package" 根因。仅当
// ASTRBOT_DEBUG_NATIVE_BUILDID 设置时由 BuildNative 调用。
func (c *Compiler) pragmaBuildID(ctx context.Context, srcDir string, extra map[string]string) string {
	goBin, err := c.tc.Ensure()
	if err != nil {
		return "ensure: " + err.Error()
	}
	cmd := exec.CommandContext(ctx, goBin, "list", "-export", "-f", "{{.Export}}", "google.golang.org/protobuf/internal/pragma") // #nosec G204 -- 诊断：args 固定
	cmd.Dir = srcDir
	cmd.Env = c.tc.BuildEnv(extra)
	out, err := cmd.Output()
	if err != nil {
		return "list: " + err.Error()
	}
	exp := strings.TrimSpace(string(out))
	cmd2 := exec.CommandContext(ctx, "go", "tool", "buildid", exp) // #nosec G204 -- 诊断：参数为上一步输出路径
	bid, err := cmd2.Output()
	if err != nil {
		return "buildid(" + exp + "): " + err.Error()
	}
	return exp + " -> " + strings.TrimSpace(string(bid))
}
