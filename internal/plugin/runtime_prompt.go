package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/WaterGodFurina/Astrbot-golang/internal/pysdk"
	"github.com/WaterGodFurina/Astrbot-golang/internal/toolchain"
)

// RuntimePromptKind 区分需要下载 Go 工具链还是 CPython。
type RuntimePromptKind string

const (
	RuntimePromptGoSDK  RuntimePromptKind = "go_sdk"
	RuntimePromptPython RuntimePromptKind = "python"
)

// goSDKMirrors 是 Go 工具链官方归档的下载镜像列表（base URL 风格：archive
// 文件名直接拼在 base 之后）。构造 Go 下载确认弹窗时随 prompt 返回，前端
// 展示给用户选择加速镜像。
var goSDKMirrors = []string{
	"https://go.dev/dl",
	"https://golang.google.cn/dl",
	"https://mirrors.aliyun.com/golang",
}

// pythonMirrors 是 CPython（python-build-standalone）的下载镜像列表（前缀
// 风格：镜像前缀直接拼在官方 URL 之前）。构造 Python 下载确认弹窗时随 prompt
// 返回。
var pythonMirrors = []string{
	"https://github.com/astral-sh/python-build-standalone/releases/download",
	"https://gh-proxy.com/",
	"https://ghfast.top/",
	"https://ghproxy.net/",
}

// RuntimePromptError 由 InstallFromSource 返回，表示安装需要用户决定是否
// 自动下载运行时（Go/CPython）。dashboard 把它转为 web 提示 code，前端弹窗。
type RuntimePromptError struct {
	Kind    RuntimePromptKind `json:"kind"`
	Android bool              `json:"android"` // runtime.GOOS == "android"
	// Mirrors 是可供用户选择的下载镜像列表（Go 为 base URL 风格，CPython 为
	// 前缀风格）。用户选中后随 GoChoice/PythonChoice 一起回传。
	Mirrors []string `json:"mirrors"`
	// Message 是可选的定制提示文案（如"系统 Python 版本过低"场景）；空时用
	// Error() 的默认文案。
	Message string `json:"message,omitempty"`
	// Command 是 Android/Termux 下建议用户执行的补救命令（前端 code 块展示
	// 并提供复制按钮）。
	Command string `json:"command,omitempty"`
}

func (e *RuntimePromptError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Android {
		switch e.Kind {
		case RuntimePromptPython:
			// Android/Termux 上 Python 运行时准备失败的常见根因：宿主基础
			// 依赖里的 C 扩展包（grpcio/cryptography/pillow/psutil）在
			// Termux 无预编译 wheel、pip 本地编译失败——而非缺解释器本身。
			// 这 4 个包 Termux 官方仓库有预编译包；其余依赖均为纯 Python/
			// 有 wheel，pip 直接装无需 clang。
			e.Command = "pkg install python python-grpcio python-cryptography python-pillow python-psutil"
			return "无法准备 Python 插件运行环境：缺少 C 扩展依赖的预编译包"
		default:
			return "未检测到可用的 Go 工具链/SDK。请在 Termux 中执行 pkg install golang 后重试，或选择自动下载 Go 工具链"
		}
	}
	switch e.Kind {
	case RuntimePromptPython:
		return "无法准备 Python 运行时，需要确认是否自动下载 CPython"
	default:
		return "未检测到可用的 Go 工具链/SDK，需要确认是否自动下载 Go 工具链"
	}
}

// newRuntimePromptError 构造带默认镜像列表的下载确认提示，按 Kind 填
// Mirrors（Go/CPython 各自的预设镜像）。
func newRuntimePromptError(kind RuntimePromptKind, android bool) *RuntimePromptError {
	e := &RuntimePromptError{Kind: kind, Android: android}
	switch kind {
	case RuntimePromptPython:
		e.Mirrors = pythonMirrors
	default:
		e.Mirrors = goSDKMirrors
	}
	return e
}

// sdkDirProbe reports whether the plugin SDK can be resolved without
// provisioning the bundled toolchain (no download side effect). It prefers the
// already-available bundled toolchain's go (matching sdkDir's lookup:
// ASTRBOT_GO_SDK → go.mod replace → module cache), falling back to the system
// `go` on PATH when the bundled toolchain is not present.
func (c *Compiler) sdkDirProbe() bool {
	goBin := ""
	if c.tc != nil {
		if bin, err := c.tc.GoBin(); err == nil {
			goBin = bin
		}
	}
	_, err := findSDKDirWithGo(goBin)
	return err == nil
}

// ensureGoInstallReady runs before the Go plugin build: it probes whether the
// Go toolchain and the plugin SDK are usable, and when either is missing
// surfaces a RuntimePromptError (or honors the user's GoChoice). It returns
// nil once the toolchain + SDK are guaranteed resolvable for the upcoming
// Prepare.
func (m *SubprocessManager) ensureGoInstallReady(ctx context.Context, opts InstallOptions) error {
	toolchainOK := false
	if m.toolchain != nil {
		if _, err := m.toolchain.GoBin(); err == nil {
			toolchainOK = true
		}
	}
	// 无 bundled 工具链时（测试/未配置 bundled 的环境），系统 go 也算可用——
	// 编译器 Prepare/Build 本就支持系统 go。
	if !toolchainOK {
		if _, err := exec.LookPath("go"); err == nil {
			toolchainOK = true
		}
	}
	sdkOK := m.compiler != nil && m.compiler.sdkDirProbe()
	if toolchainOK && sdkOK {
		return nil
	}

	switch strings.ToLower(strings.TrimSpace(opts.GoChoice)) {
	case "":
		return newRuntimePromptError(RuntimePromptGoSDK, runtime.GOOS == "android")
	case "cancel":
		if runtime.GOOS == "android" {
			return errors.New("已取消安装：请先在 Termux 中执行 pkg install golang 安装 Go，然后重新安装该插件")
		}
		return errors.New("已取消安装：请先手动安装 Go 工具链（并确保插件 SDK 可解析，可设置 ASTRBOT_GO_SDK）后再安装该插件")
	case "download":
		// 用户选择了下载镜像 → 注入 toolchain 包级覆盖（downloadURL/
		// checksumURL 优先使用它），随后的 EnsureWithProgress 下载即走该镜像。
		if opts.GoMirror != "" {
			toolchain.SetGoMirror(opts.GoMirror)
		}
		return m.downloadGoSDK(ctx, opts)
	default:
		return fmt.Errorf("未知的 Go 工具链选择: %q", opts.GoChoice)
	}
}

// downloadGoSDK provisions the bundled Go toolchain (if needed) and fetches
// the plugin SDK into the Go module cache so the upcoming Prepare resolves it.
// The SDK download targets the default module cache (no GOPATH override), the
// same location findSDKDirWithGo reads.
func (m *SubprocessManager) downloadGoSDK(ctx context.Context, opts InstallOptions) error {
	if m.compiler == nil {
		return errors.New("Go 插件安装不可用（编译配置未初始化）")
	}
	var goBin string
	if m.toolchain != nil {
		if opts.Stage != nil {
			opts.Stage("下载 Go 工具链…")
		}
		b, err := m.toolchain.EnsureWithProgress(opts.Progress)
		if err != nil {
			return err
		}
		goBin = b
	} else {
		// 无 bundled 工具链：用系统 go 拉 SDK（测试/无 bundled 环境）。
		b, err := exec.LookPath("go")
		if err != nil {
			return errors.New("Go 插件安装不可用（无 bundled 工具链且 PATH 无 go）")
		}
		goBin = b
	}
	// SDK 已可 resolve（ASTRBOT_GO_SDK / 本地 replace / 模块缓存）→ 无需下载。
	logger.Debug("SDK 解析：使用 go=%s 探测 SDK（版本来自宿主 go.mod）", goBin)
	if _, err := findSDKDirWithGo(goBin); err == nil {
		logger.Debug("SDK 解析：SDK 已可 resolve，跳过下载")
		return nil
	} else {
		logger.Debug("SDK 解析：SDK 未 resolve: %v，准备下载", err)
	}
	if opts.Stage != nil {
		opts.Stage("下载插件 SDK…")
	}
	// 把 SDK 拉进 go 模块缓存（与 findSDKDirWithGo 的模块缓存分支一致），
	// 使后续 Prepare 的 sdkDir() 命中。版本优先取宿主 go.mod 的 require。
	version := ""
	if dir, err := os.Getwd(); err == nil {
		if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil { // #nosec G304 -- 读取宿主自身 go.mod 定位 SDK 版本
			version = sdkRequireFromGoMod(data)
		}
	}
	target := sdkModulePath
	if version != "" {
		target += "@" + version
	} else {
		target += "@latest"
	}
	tmp, err := os.MkdirTemp("", "astrbot-go-sdk-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	// `go mod download module@version` 也要求位于模块上下文中。
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module astrbot-sdk-download\n\ngo 1.23\n"), 0o644); err != nil { // #nosec G306 -- 临时模块文件，常规权限即可
		return err
	}
	logger.Debug("SDK 解析：执行 go mod download %s（GOPROXY=%s）", target, m.compiler.goproxyEnv())
	cmd := exec.CommandContext(ctx, goBin, "mod", "download", target) // #nosec G204 -- 下载固定的插件 SDK 模块（target 由固定 module path + 宿主 go.mod 版本拼装）; nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd.Dir = tmp
	cmd.Env = append(os.Environ(),
		"GO111MODULE=on",
		"GOPROXY="+m.compiler.goproxyEnv(),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		logger.Debug("SDK 解析：go mod download 失败: %v\n%s", err, strings.TrimSpace(string(out)))
		return fmt.Errorf("下载插件 SDK 失败: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	logger.Debug("SDK 解析：go mod download 成功（%s）", target)
	// 校验下载后 SDK 确实可 resolve，否则后续 Prepare 仍会失败（给出明确错误
	// 而非笼统的 "no go.mod with the AstrBot SDK replace found"）。
	if _, err := findSDKDirWithGo(goBin); err != nil {
		logger.Debug("SDK 解析：下载后仍无法 resolve: %v", err)
		return fmt.Errorf("插件 SDK 下载后仍无法解析: %w", err)
	}
	logger.Debug("SDK 解析：SDK 下载后可 resolve")
	return nil
}

// pythonRuntimeForInstall resolves the Python runtime during plugin install.
// Unlike pythonRuntime (used for runtime loads), it surfaces a
// RuntimePromptError when the runtime cannot be prepared and the user has not
// yet decided to auto-download CPython. "download" proceeds through the normal
// prepare path (EnsurePythonBin/EnsureVenv auto-download CPython when the host
// has no interpreter).
func (m *SubprocessManager) pythonRuntimeForInstall(opts InstallOptions) (*pysdk.RuntimeEnv, error) {
	choice := strings.ToLower(strings.TrimSpace(opts.PythonChoice))
	switch choice {
	case "cancel":
		if runtime.GOOS == "android" {
			return nil, errors.New("已取消安装：请先在 Termux 中执行 pkg install python python-grpcio python-cryptography python-pillow python-psutil 后重试")
		}
		return nil, errors.New("已取消安装：请先手动安装 Python（或允许自动下载 CPython）后再安装该插件")
	case "":
		// 未决定且 PATH 上存在版本过低的系统 Python（<3.10）→ 先提示用户，
		// 说明将自动下载 CPython 3.12。
		if tooLow := pysdk.DetectTooLowPython(); tooLow != "" {
			return nil, &RuntimePromptError{
				Kind:    RuntimePromptPython,
				Android: runtime.GOOS == "android",
				Mirrors: pythonMirrors,
				Message: fmt.Sprintf("检测到系统 Python %s 版本过低，将自动下载 CPython 3.12", tooLow),
			}
		}
		// 未决定且主机没有可用解释器（需自动下载 CPython）→ 先提示用户。
		if pysdk.DiscoverPythonBin() == "" {
			return nil, newRuntimePromptError(RuntimePromptPython, runtime.GOOS == "android")
		}
	case "download":
		// 走现有自动下载 CPython 路径（EnsurePythonBin/EnsureVenv）。
	default:
		return nil, fmt.Errorf("未知的 Python 运行时选择: %q", opts.PythonChoice)
	}
	// 用户选择了下载镜像 → 注入 pysdk 包级覆盖（pyDownloadURL 优先使用它），
	// 随后的 EnsurePythonBin 下载即走该镜像前缀。安装流程串行（startInstanceMu），
	// 下次安装会覆盖，无需复位。
	if opts.PythonMirror != "" {
		pysdk.SetPythonMirror(opts.PythonMirror)
	}
	env, err := m.pythonRuntimeWithStage(opts.Stage)
	if err != nil {
		// 未决定时准备仍失败（宿主基础依赖装不上等）→ 转为下载确认提示。
		if choice == "" && errors.Is(err, pysdk.ErrRuntimeUnavailable) {
			return nil, newRuntimePromptError(RuntimePromptPython, runtime.GOOS == "android")
		}
		return nil, err
	}
	return env, nil
}
