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
	if goBin == "" {
		goBin = "go"
	}
	cmd := exec.Command(goBin, "list", "-m", "-f", "{{.Dir}}", sdkModulePath) // #nosec G204 -- 插件编译核心：`go list -m` 定位 SDK 模块缓存，参数全为固定常量，无注入面; nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	out, err := cmd.Output()
	if err != nil {
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
