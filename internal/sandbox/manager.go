// Package sandbox implements the sandbox runtime interface for skill execution.
//
// The sandbox is an isolated execution environment (container or VM) where
// skill scripts run without access to the host filesystem. Skills discovered
// in the sandbox are cached locally via SkillManager.SetSandboxSkillsCache().
//
// In the Python version, this was handled by computer_tools/booters which
// executed code inside Docker containers. In Go, we provide a simplified
// interface that can be backed by either:
//   - Docker exec (via aiodocker equivalent)
//   - Local process isolation (restricted Go subprocess)
//   - Remote sandbox API (HTTP)
package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/skills"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var logger = log.GetDefault().WithComponent("Sandbox")

// BooterType identifies the sandbox backend.
type BooterType string

const (
	BooterLocal  BooterType = "local"
	BooterDocker BooterType = "docker"
	BooterRemote BooterType = "remote"
)

// Booter is the interface for sandbox execution backends.
type Booter interface {
	Type() BooterType
	Start(ctx context.Context) error
	Stop() error
	IsRunning() bool
	Exec(ctx context.Context, cmd string, args []string, workdir string) (stdout, stderr string, exitCode int, err error)
	// ListSkills discovers SKILL.md files inside the sandbox's workspace.
	ListSkills(ctx context.Context) ([]skills.SandboxCacheEntry, error)
	// ReadFile reads a file from the sandbox workspace.
	ReadFile(ctx context.Context, path string) (string, error)
	// WriteFile writes content to a file in the sandbox workspace.
	WriteFile(ctx context.Context, path, content string) error
}

// blockedCmdPatterns 对齐 Python booters/local.py 的 _BLOCKED_COMMAND_PATTERNS：
// 本地沙箱（无隔离）下的破坏性 shell 命令黑名单。命中即拒绝执行。
var blockedCmdPatterns = []string{
	" rm -rf ", " rm -fr ", " rm -r ", " mkfs", " dd if=",
	" shutdown", " reboot", " poweroff", " halt", " sudo ",
	":(){:|:&};:", " kill -9 ", " killall ",
}

// isSafeLocalCommand 检查 sh -c 形式的命令串是否命中破坏性命令黑名单
// （对齐 Python _is_safe_command 的包裹空格匹配）。
func isSafeLocalCommand(args []string) bool {
	if len(args) >= 2 && args[0] == "-c" {
		cmd := " " + strings.ToLower(strings.TrimSpace(args[1])) + " "
		for _, pat := range blockedCmdPatterns {
			if strings.Contains(cmd, pat) {
				return false
			}
		}
	}
	return true
}

// maxSandboxOutput 限制单条沙箱命令捕获的 stdout/stderr 总量（1MB，对齐
// pipeline 宿主 shell 工具 maxShellOutput），超限部分丢弃并附加截断标记，
// 防止 `yes` 一类高输出命令在超时窗口内把宿主内存灌满。
const maxSandboxOutput = 1 << 20

// cappedBuffer 是上限缓冲 Writer：达到 max 后丢弃后续写入（不报错，避免
// SIGPIPE 噪音）并标记截断，String() 在截断时附加提示。
type cappedBuffer struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	max  int
	trun bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() >= b.max {
		b.trun = true
		return len(p), nil
	}
	if b.buf.Len()+len(p) > b.max {
		_, _ = b.buf.Write(p[:b.max-b.buf.Len()])
		b.trun = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.buf.String()
	if b.trun {
		s += "\n[输出超过 1MB 已截断]"
	}
	return s
}

// stringBuffer 是 dockerRun 输出参数的抽象：strings.Builder 与 cappedBuffer
// 都满足（dockerOutput 等工具调用保持 strings.Builder，沙箱 Exec 用
// cappedBuffer 限流）。
type stringBuffer interface {
	Write(p []byte) (int, error)
	String() string
}

// LocalBooter executes commands as local subprocesses with restricted env,
// backing the sandbox's /workspace onto a host directory. This gives a working
// sandbox runtime without Docker: file operations are mapped into the host
// root so anything written via sandbox file tools can be read back, and vice
// versa.
//
// 注意：本地子进程没有 CPU/内存/磁盘等资源限制，也缺少强隔离，仅用于
// 开发/测试场景。不可信代码请使用 DockerBooter 或 RemoteBooter。
type LocalBooter struct {
	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	root    string // host directory backing the sandbox /workspace
}

// NewLocalBooter creates a local sandbox booter.
func NewLocalBooter() *LocalBooter {
	return &LocalBooter{}
}

// SetRoot overrides the host directory that backs the sandbox /workspace.
func (b *LocalBooter) SetRoot(root string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.root = root
}

func (b *LocalBooter) Type() BooterType { return BooterLocal }

func (b *LocalBooter) Start(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running {
		return fmt.Errorf("local booter already running")
	}
	if b.root == "" {
		b.root = filepath.Join("data", "sandbox", "workspace")
	}
	if err := os.MkdirAll(b.root, 0o750); err != nil {
		return err
	}
	b.running = true
	logger.Debug("Local sandbox booter started (root=%s)", b.root)
	return nil
}

func (b *LocalBooter) Stop() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.running {
		return nil
	}
	b.running = false
	if b.cancel != nil {
		b.cancel()
	}
	logger.Debug("Local sandbox booter stopped")
	return nil
}

func (b *LocalBooter) IsRunning() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

// mapPath maps a sandbox path (/workspace/... or a relative path) onto the
// host backing directory. Absolute paths outside /workspace are shadowed under
// the root so the booter cannot touch the real host filesystem. 路径先做
// filepath.Clean，再拼接到 root 下并用 filepath.Rel 校验，防止 `../../x`
// 一类相对路径逃出沙盒根目录。
func (b *LocalBooter) mapPath(path string) (string, error) {
	raw := strings.TrimSpace(path)
	if raw == "" {
		return "", fmt.Errorf("沙盒路径为空")
	}
	p := filepath.Clean(filepath.FromSlash(raw))
	p = strings.TrimPrefix(p, SandboxWorkdir)
	p = strings.TrimPrefix(p, string(filepath.Separator))
	b.mu.Lock()
	root := b.root
	b.mu.Unlock()
	if root == "" {
		root = filepath.Join("data", "sandbox", "workspace")
	}
	joined := filepath.Join(root, p)
	rel, err := filepath.Rel(root, joined)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("沙盒路径越界：%q 逃出沙盒根目录 %s", path, root)
	}
	if err := verifyNoSymlinkEscape(root, joined); err != nil {
		return "", err
	}
	return joined, nil
}

// verifyNoSymlinkEscape 解析 p 最深一层已存在的祖先的真实路径，校验其仍在
// root 之内。词法检查挡不住沙盒内指向宿主目录的符号链接（如 /workspace/evil
// -> /etc），只有解析符号链接后才能确认真实落点没有逃出沙盒根目录。
func verifyNoSymlinkEscape(root, p string) error {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		// 根目录本身不存在，沙盒内不可能有符号链接，无从逃逸。
		return nil
	}
	cur := p
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			rel, rerr := filepath.Rel(resolvedRoot, resolved)
			if rerr != nil {
				return rerr
			}
			if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("沙盒路径 %q 经符号链接逃出沙盒根目录 %s", p, root)
			}
			return nil
		}
		// 目标尚未创建（写入新文件等）：逐级向上找已存在的祖先再解析。
		parent := filepath.Dir(cur)
		if parent == cur || parent == root {
			return nil
		}
		cur = parent
	}
}

func (b *LocalBooter) Exec(ctx context.Context, cmd string, args []string, workdir string) (string, string, int, error) {
	b.mu.Lock()
	running := b.running
	b.mu.Unlock()
	if !running {
		return "", "", -1, fmt.Errorf("local sandbox not running")
	}
	if strings.TrimSpace(workdir) == "" {
		workdir = SandboxWorkdir
	}
	dir, err := b.mapPath(workdir)
	if err != nil {
		return "", "", -1, err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", "", -1, err
	}
	// 本地沙箱无隔离：先做破坏性命令黑名单拦截（对齐 Python local.py 的
	// _is_safe_command），命中即拒绝，避免 LLM 工具调用 rm -rf/mkfs 等。
	if !isSafeLocalCommand(args) {
		return "", "", -1, fmt.Errorf("blocked unsafe shell command")
	}
	// #nosec G204 -- 本地沙箱执行核心：cmd/args 由 Booter 从沙箱操作（LLM 给定的工具调用）构造，
	// 环境变量已最小化（localBooterEnv 剔除敏感变量），工作目录限定在沙箱映射路径内。
	c := exec.CommandContext(ctx, cmd, args...) // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	c.Dir = dir
	// 显式设置最小化环境：只保留 PATH（嵌套子进程找命令）、HOME 与本地化变量。
	// 默认 exec 会继承宿主全部环境变量，沙箱内 `env` 可读到 ASTRBOT_*/API key
	// 等敏感变量，这里全部剔除。
	c.Env = localBooterEnv()
	// 输出有上限缓冲（maxSandboxOutput）：高输出命令只保留前 1MB。
	var stdout, stderr cappedBuffer
	stdout.max, stderr.max = maxSandboxOutput, maxSandboxOutput
	c.Stdout = &stdout
	c.Stderr = &stderr
	err = c.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return stdout.String(), stderr.String(), ee.ExitCode(), nil
	}
	return stdout.String(), stderr.String(), -1, err
}

// localBooterEnv returns the minimal environment for sandbox commands: PATH
// plus a few locale variables. Host secrets (ASTRBOT_*, API keys, tokens) are
// deliberately excluded so `env` inside the sandbox cannot leak them.
func localBooterEnv() []string {
	path := os.Getenv("PATH")
	if strings.TrimSpace(path) == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	env := []string{"PATH=" + path}
	for _, key := range []string{"HOME", "LANG", "LC_ALL", "LC_CTYPE", "TZ"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return env
}

func (b *LocalBooter) ListSkills(ctx context.Context) ([]skills.SandboxCacheEntry, error) {
	b.mu.Lock()
	running := b.running
	b.mu.Unlock()
	if !running {
		return nil, fmt.Errorf("local sandbox not running")
	}
	root, err := b.mapPath(SandboxWorkdir)
	if err != nil {
		return nil, err
	}
	skillsRoot := filepath.Join(root, "skills")
	var entries []skills.SandboxCacheEntry
	_ = filepath.WalkDir(skillsRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.EqualFold(d.Name(), "SKILL.md") {
			return nil
		}
		dir := filepath.Base(filepath.Dir(p))
		// #nosec G304 -- p comes from filepath.WalkDir over the sandbox skills
		// root (managed local directory), matching SKILL.md entries.
		content, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		entries = append(entries, skills.SandboxCacheEntry{
			Name:        dir,
			Description: skills.ParseFrontmatterDescription(string(content)),
			Path:        filepath.ToSlash(filepath.Join(SandboxWorkdir, "skills", dir, "SKILL.md")),
		})
		return nil
	})
	return entries, nil
}

func (b *LocalBooter) ReadFile(ctx context.Context, path string) (string, error) {
	b.mu.Lock()
	running := b.running
	b.mu.Unlock()
	if !running {
		return "", fmt.Errorf("local sandbox not running")
	}
	host, err := b.mapPath(path)
	if err != nil {
		return "", err
	}
	// #nosec G304 -- host is the result of mapPath, which confines reads to the
	// sandbox workspace root.
	data, err := os.ReadFile(host)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("file not found: %s", path)
		}
		return "", err
	}
	return string(data), nil
}

func (b *LocalBooter) WriteFile(ctx context.Context, path, content string) error {
	b.mu.Lock()
	running := b.running
	b.mu.Unlock()
	if !running {
		return fmt.Errorf("local sandbox not running")
	}
	host, err := b.mapPath(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(host), 0o750); err != nil {
		return err
	}
	return os.WriteFile(host, []byte(content), 0o600)
}

// SandboxWorkdir is the sandbox workspace directory used as the cwd for
// shell/python exec and the base for relative file paths.
const SandboxWorkdir = "/workspace"

// DockerBooter executes commands inside a Docker container.
//
// Mirrors AstrBot's Docker sandbox logic (computer_tools/booters/boxlite.py
// and bay_manager.py): a long-lived sandbox container is created once and
// reused; shell/python/file operations run inside it via `docker exec`, and
// files are transferred via the Docker CLI.
type DockerBooter struct {
	mu          sync.Mutex
	running     bool
	containerID string
	name        string
	image       string

	// 资源/网络隔离参数（可经 ASTRBOT_SANDBOX_* 环境变量覆盖，默认值见
	// NewDockerBooter）：memory/cpus/pidsLimit 限制容器资源，network 默认
	// "none" 切断容器网络（设 "full" 等可放行），capDropAll 移除全部
	// Linux capabilities。
	memory     string
	cpus       string
	pidsLimit  string
	network    string
	capDropAll bool
}

// NewDockerBooter creates a Docker-based sandbox booter.
func NewDockerBooter(image string) *DockerBooter {
	if image == "" {
		image = "ubuntu:22.04"
	}
	return &DockerBooter{
		image:      image,
		name:       fmt.Sprintf("astrbot-sandbox-%d", time.Now().UnixNano()),
		memory:     sandboxEnv("ASTRBOT_SANDBOX_MEMORY", "512m"),
		cpus:       sandboxEnv("ASTRBOT_SANDBOX_CPUS", "1"),
		pidsLimit:  sandboxEnv("ASTRBOT_SANDBOX_PIDS_LIMIT", "64"),
		network:    sandboxEnv("ASTRBOT_SANDBOX_NETWORK", "none"),
		capDropAll: true,
	}
}

// sandboxEnv returns the value of an ASTRBOT_SANDBOX_* env override, or the
// fallback when unset/blank.
func sandboxEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func (b *DockerBooter) Type() BooterType { return BooterDocker }

func (b *DockerBooter) Start(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running {
		return nil
	}
	// Reuse an existing managed container if it is still running; a stopped
	// one (e.g. after a host reboot) is restarted, or discarded and rebuilt.
	if out, err := dockerOutput(ctx, "ps", "-aq", "--filter", "label=astrbot.sandbox=managed"); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			running, err := dockerOutput(ctx, "inspect", "-f", "{{.State.Running}}", line)
			if err != nil {
				continue
			}
			if strings.TrimSpace(running) == "true" {
				b.containerID = line
				b.running = true
				logger.Debug("Docker sandbox booter reusing container %s", line)
				return nil
			}
			_, startErr := dockerOutput(ctx, "start", line)
			if startErr == nil {
				b.containerID = line
				b.running = true
				logger.Debug("Docker sandbox booter restarted container %s", line)
				return nil
			}
			logger.I18nWarn("docker start 失败，移除容器 %s: %v", line, startErr)
			_, _ = dockerOutput(ctx, "rm", "-f", line)
		}
	}
	// Create a fresh container that idles until exec'd into. Resource limits,
	// capability drops and (by default) no network are applied so a runaway
	// skill cannot exhaust the host or reach internal services.
	args := []string{"run", "-d", "--name", b.name,
		"--label", "astrbot.sandbox=managed",
		"--workdir", "/workspace"}
	if b.memory != "" {
		args = append(args, "--memory", b.memory)
	}
	if b.cpus != "" {
		args = append(args, "--cpus", b.cpus)
	}
	if b.pidsLimit != "" {
		args = append(args, "--pids-limit", b.pidsLimit)
	}
	if b.network != "" {
		args = append(args, "--network", b.network)
	}
	if b.capDropAll {
		args = append(args, "--cap-drop", "ALL")
	}
	args = append(args, b.image, "tail", "-f", "/dev/null")
	if out, err := dockerOutput(ctx, args...); err != nil {
		logger.I18nWarn("docker run 失败: %v (%s)", err, strings.TrimSpace(out))
		return fmt.Errorf("start docker sandbox: %w", err)
	} else {
		b.containerID = strings.TrimSpace(out)
	}
	logger.Debug("Docker sandbox booter started (image=%s, container=%s)", b.image, b.containerID)
	b.running = true
	return nil
}

func (b *DockerBooter) Stop() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.running || b.containerID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = dockerOutput(ctx, "rm", "-f", b.containerID)
	b.running = false
	logger.Debug("Docker sandbox booter stopped (container=%s)", b.containerID)
	return nil
}

func (b *DockerBooter) IsRunning() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

func (b *DockerBooter) Exec(ctx context.Context, cmd string, args []string, workdir string) (string, string, int, error) {
	b.mu.Lock()
	cid := b.containerID
	b.mu.Unlock()
	if cid == "" {
		return "", "", -1, fmt.Errorf("docker sandbox not running")
	}
	dockerArgs := []string{"exec"}
	if workdir != "" {
		dockerArgs = append(dockerArgs, "-w", workdir)
	}
	dockerArgs = append(dockerArgs, cid)
	if cmd == "sh" || cmd == "/bin/sh" {
		// Pass a single command string so pipes/redirection work. Callers may
		// pass the command either as ["-c", "cmd"] or as ["cmd"].
		cmdStart := 0
		if len(args) > 0 && args[0] == "-c" {
			cmdStart = 1
		}
		joined := strings.Join(args[cmdStart:], " ")
		dockerArgs = append(dockerArgs, "sh", "-c", joined)
	} else {
		dockerArgs = append(dockerArgs, cmd)
		dockerArgs = append(dockerArgs, args...)
	}
	var stdout, stderr cappedBuffer
	stdout.max, stderr.max = maxSandboxOutput, maxSandboxOutput
	code, err := dockerRun(ctx, dockerArgs, nil, &stdout, &stderr)
	return stdout.String(), stderr.String(), code, err
}

func (b *DockerBooter) ListSkills(ctx context.Context) ([]skills.SandboxCacheEntry, error) {
	b.mu.Lock()
	cid := b.containerID
	b.mu.Unlock()
	if cid == "" {
		return nil, fmt.Errorf("docker sandbox not running")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := dockerOutput(ctx, "exec", cid, "sh", "-c",
		"find /workspace/skills -name SKILL.md -o -name skill.md 2>/dev/null")
	if err != nil {
		return nil, err
	}
	var entries []skills.SandboxCacheEntry
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		dir := filepath.Dir(line)
		name := filepath.Base(dir)
		desc := ""
		if content, err := b.ReadFile(ctx, line); err == nil {
			desc = skills.ParseFrontmatterDescription(content)
		}
		entries = append(entries, skills.SandboxCacheEntry{
			Name:        name,
			Description: desc,
			Path:        strings.ReplaceAll(line, "\\", "/"),
		})
	}
	return entries, nil
}

func (b *DockerBooter) ReadFile(ctx context.Context, path string) (string, error) {
	b.mu.Lock()
	cid := b.containerID
	b.mu.Unlock()
	if cid == "" {
		return "", fmt.Errorf("docker sandbox not running")
	}
	// Use the exit code to detect a missing file instead of grepping stdout
	// for a sentinel string (which a file's own content could spoof).
	var stdout, stderr strings.Builder
	code, err := dockerRun(ctx, []string{"exec", "-w", SandboxWorkdir, cid, "sh", "-c", "cat '" + strings.ReplaceAll(path, "'", "'\\''") + "' 2>/dev/null"}, nil, &stdout, &stderr)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("file not found: %s", path)
	}
	return stdout.String(), nil
}

func (b *DockerBooter) WriteFile(ctx context.Context, path, content string) error {
	b.mu.Lock()
	cid := b.containerID
	b.mu.Unlock()
	if cid == "" {
		return fmt.Errorf("docker sandbox not running")
	}
	// mkdir -p parent, then cat > file via stdin.
	parent := filepath.Dir(path)
	if _, err := dockerOutput(ctx, "exec", "-w", SandboxWorkdir, cid, "sh", "-c", "mkdir -p '"+strings.ReplaceAll(parent, "'", "'\\''")+"'"); err != nil {
		return err
	}
	var stdout, stderr strings.Builder
	code, err := dockerRun(ctx, []string{"exec", "-i", "-w", SandboxWorkdir, cid, "sh", "-c", "cat > '" + strings.ReplaceAll(path, "'", "'\\''") + "'"}, strings.NewReader(content), &stdout, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("write file failed (exit %d): %s", code, stderr.String())
	}
	return nil
}

// dockerOutput runs a docker command and returns stdout as a string.
func dockerOutput(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr strings.Builder
	code, err := dockerRun(ctx, args, nil, &stdout, &stderr)
	if err != nil {
		return stdout.String(), err
	}
	if code != 0 {
		return stdout.String(), fmt.Errorf("docker %v failed (exit %d): %s", args, code, stderr.String())
	}
	return stdout.String(), nil
}

// dockerRun executes the `docker` CLI, optionally feeding stdin, and captures
// stdout/stderr. Returns the process exit code.
func dockerRun(ctx context.Context, args []string, stdin io.Reader, stdout, stderr stringBuffer) (int, error) {
	bin := os.Getenv("ASTRBOT_DOCKER_BIN")
	if bin == "" {
		bin = "docker"
	}
	// #nosec G204 -- docker CLI invocation is the sandbox's core purpose; the
	// sandbox workspaces are isolated containers, and args are constructed by
	// the Booter from sandbox operations.
	cmd := exec.CommandContext(ctx, bin, args...) // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	if stdin != nil {
		cmd.Stdin = stdin
	}
	if stdout != nil {
		cmd.Stdout = stdout
	}
	if stderr != nil {
		cmd.Stderr = stderr
	}
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// Manager manages the sandbox lifecycle and skill synchronization.
type Manager struct {
	mu       sync.RWMutex
	booter   Booter
	skillMgr *skills.SkillManager
}

// NewManager creates a sandbox manager.
func NewManager(skillMgr *skills.SkillManager) *Manager {
	return &Manager{skillMgr: skillMgr}
}

// SetBooter switches the sandbox backend.
func (m *Manager) SetBooter(b Booter) {
	m.mu.Lock()
	old := m.booter
	m.booter = b
	m.mu.Unlock()
	// 旧 booter 的 Stop 可能阻塞较久（docker rm 最长 30s），必须在锁外执行，
	// 否则期间所有 Exec/读写操作都会被写锁卡住。
	if old != nil {
		if err := old.Stop(); err != nil {
			logger.Error("停止旧 sandbox booter 失败: %v", err)
		}
	}
}

// Start launches the sandbox.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.booter == nil {
		return fmt.Errorf("no booter configured")
	}
	return m.booter.Start(ctx)
}

// Stop shuts down the sandbox.
func (m *Manager) Stop() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.booter == nil {
		return nil
	}
	return m.booter.Stop()
}

// SyncSkills discovers skills from the sandbox and updates the local cache.
func (m *Manager) SyncSkills(ctx context.Context) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.booter == nil || !m.booter.IsRunning() {
		return fmt.Errorf("sandbox not running")
	}
	entries, err := m.booter.ListSkills(ctx)
	if err != nil {
		return fmt.Errorf("list sandbox skills: %w", err)
	}
	if m.skillMgr != nil {
		m.skillMgr.SetSandboxSkillsCache(entries)
	}
	logger.Debug("Synced %d skills from sandbox", len(entries))
	return nil
}

// Exec runs a command in the sandbox.
func (m *Manager) Exec(ctx context.Context, cmd string, args []string, workdir string) (string, string, int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.booter == nil || !m.booter.IsRunning() {
		return "", "", -1, fmt.Errorf("sandbox not running")
	}
	startTime := time.Now()
	stdout, stderr, code, err := m.booter.Exec(ctx, cmd, args, workdir)
	elapsed := time.Since(startTime)
	logger.Debug("Exec %s %v (exit=%d, %v)", cmd, args, code, elapsed)
	return stdout, stderr, code, err
}

// ReadFile reads a file from the sandbox workspace.
func (m *Manager) ReadFile(ctx context.Context, path string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.booter == nil || !m.booter.IsRunning() {
		return "", fmt.Errorf("sandbox not running")
	}
	return m.booter.ReadFile(ctx, path)
}

// WriteFile writes content to a file in the sandbox workspace.
func (m *Manager) WriteFile(ctx context.Context, path, content string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.booter == nil || !m.booter.IsRunning() {
		return fmt.Errorf("sandbox not running")
	}
	return m.booter.WriteFile(ctx, path, content)
}

// IsRunning returns whether the sandbox is active.
func (m *Manager) IsRunning() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.booter != nil && m.booter.IsRunning()
}
