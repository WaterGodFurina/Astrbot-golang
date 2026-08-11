package plugin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/AstrBotDevs/AstrBot/internal/toolchain"
	"golang.org/x/mod/modfile"
)

// sdkModulePath is the module path of the standalone plugin SDK that every
// plugin links against. Builds `replace` it to the local copy.
const sdkModulePath = "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"

// Compiler builds plugin source into a platform-native executable using the
// bundled Go toolchain. It also performs static safety checks (import
// blacklist) and `go vet` before compiling.
type Compiler struct {
	tc *toolchain.Toolchain
}

// NewCompiler creates a compiler backed by the given toolchain.
func NewCompiler(tc *toolchain.Toolchain) *Compiler {
	return &Compiler{tc: tc}
}

// Prepare ensures the plugin module builds against the local SDK: it writes a
// go.mod (when missing) or patches the existing one to require the SDK module
// and replace it with the local SDK directory.
func (c *Compiler) Prepare(srcDir, moduleName string) error {
	sdkDir, err := findSDKDir()
	if err != nil {
		return fmt.Errorf("resolve SDK dir: %w", err)
	}

	modPath := filepath.Join(srcDir, "go.mod")
	data, err := os.ReadFile(modPath)
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
	return os.WriteFile(modPath, out, 0o644)
}

// Vet runs `go vet ./...` in the plugin module.
func (c *Compiler) Vet(ctx context.Context, srcDir string) error {
	goBin, err := c.tc.Ensure()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, goBin, "vet", "./...")
	cmd.Dir = srcDir
	cmd.Env = c.tc.BuildEnv(map[string]string{"GOPROXY": "https://goproxy.cn,direct"})
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
	if err := os.MkdirAll(filepath.Dir(absOut), 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, goBin, "build", "-o", absOut, "-ldflags=-s -w", "./...")
	cmd.Dir = srcDir
	extra := map[string]string{
		"GOPROXY": "https://goproxy.cn,direct",
	}
	if cc != "" {
		extra["CC"] = cc
		extra["CXX"] = cxx
		extra["CGO_ENABLED"] = "1"
	}
	cmd.Env = c.tc.BuildEnv(extra)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build: %w\n%s", err, out)
	}
	return nil
}

// findSDKDir locates the local SDK module directory, resolving it from
// ASTRBOT_GO_SDK, the nearest go.mod `replace`, or the conventional sibling
// directory.
func findSDKDir() (string, error) {
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
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("no go.mod with the AstrBot SDK replace found (set %s)", "ASTRBOT_GO_SDK")
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
	return b.String()
}

// artifactName returns the compiled binary file name for the current platform.
func artifactName(id string) string {
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	return sanitizeID(id) + "-" + runtime.GOOS + "-" + runtime.GOARCH + ext
}
