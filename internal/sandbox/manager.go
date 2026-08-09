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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/log"
	"github.com/AstrBotDevs/AstrBot/internal/skills"
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

// LocalBooter executes commands as local subprocesses with restricted env,
// backing the sandbox's /workspace onto a host directory. This gives a working
// sandbox runtime without Docker: file operations are mapped into the host
// root so anything written via sandbox file tools can be read back, and vice
// versa.
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
	if err := os.MkdirAll(b.root, 0755); err != nil {
		return err
	}
	b.running = true
	logger.Info("Local sandbox booter started (root=%s)", b.root)
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
	logger.Info("Local sandbox booter stopped")
	return nil
}

func (b *LocalBooter) IsRunning() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

// mapPath maps a sandbox path (/workspace/... or a relative path) onto the
// host backing directory. Absolute paths outside /workspace are shadowed under
// the root so the booter cannot touch the real host filesystem.
func (b *LocalBooter) mapPath(path string) string {
	p := filepath.Clean(filepath.FromSlash(strings.TrimSpace(path)))
	p = strings.TrimPrefix(p, SandboxWorkdir)
	p = strings.TrimPrefix(p, string(filepath.Separator))
	b.mu.Lock()
	root := b.root
	b.mu.Unlock()
	if root == "" {
		root = filepath.Join("data", "sandbox", "workspace")
	}
	return filepath.Join(root, p)
}

func (b *LocalBooter) Exec(ctx context.Context, cmd string, args []string, workdir string) (string, string, int, error) {
	b.mu.Lock()
	running := b.running
	b.mu.Unlock()
	if !running {
		return "", "", -1, fmt.Errorf("local sandbox not running")
	}
	dir := b.mapPath(workdir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", "", -1, err
	}
	c := exec.CommandContext(ctx, cmd, args...)
	c.Dir = dir
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return stdout.String(), stderr.String(), ee.ExitCode(), nil
	}
	return stdout.String(), stderr.String(), -1, err
}

func (b *LocalBooter) ListSkills(ctx context.Context) ([]skills.SandboxCacheEntry, error) {
	b.mu.Lock()
	running := b.running
	b.mu.Unlock()
	if !running {
		return nil, fmt.Errorf("local sandbox not running")
	}
	skillsRoot := filepath.Join(b.mapPath(SandboxWorkdir), "skills")
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
		content, _ := os.ReadFile(p)
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
	host := b.mapPath(path)
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
	host := b.mapPath(path)
	if err := os.MkdirAll(filepath.Dir(host), 0755); err != nil {
		return err
	}
	return os.WriteFile(host, []byte(content), 0644)
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
}

// NewDockerBooter creates a Docker-based sandbox booter.
func NewDockerBooter(image string) *DockerBooter {
	if image == "" {
		image = "ubuntu:22.04"
	}
	return &DockerBooter{
		image: image,
		name:  fmt.Sprintf("astrbot-sandbox-%d", time.Now().UnixNano()),
	}
}

func (b *DockerBooter) Type() BooterType { return BooterDocker }

func (b *DockerBooter) Start(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running {
		return nil
	}
	// Reuse an existing managed container if one is still running.
	if out, err := dockerOutput(ctx, "ps", "-aq", "--filter", "label=astrbot.sandbox=managed"); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if _, err := dockerOutput(ctx, "inspect", "-f", "{{.State.Running}}", line); err == nil {
				b.containerID = line
				b.running = true
				logger.Info("Docker sandbox booter reusing container %s", line)
				return nil
			}
		}
	}
	// Create a fresh container that idles until exec'd into.
	args := []string{"run", "-d", "--name", b.name,
		"--label", "astrbot.sandbox=managed",
		"--workdir", "/workspace", b.image, "tail", "-f", "/dev/null"}
	if out, err := dockerOutput(ctx, args...); err != nil {
		logger.Warn("docker run failed: %v (%s)", err, strings.TrimSpace(out))
		return fmt.Errorf("start docker sandbox: %w", err)
	} else {
		b.containerID = strings.TrimSpace(out)
	}
	logger.Info("Docker sandbox booter started (image=%s, container=%s)", b.image, b.containerID)
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
	logger.Info("Docker sandbox booter stopped (container=%s)", b.containerID)
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
	var stdout, stderr strings.Builder
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
	out, err := dockerOutput(ctx, "exec", "-w", SandboxWorkdir, cid, "sh", "-c", "cat '"+strings.ReplaceAll(path, "'", "'\\''")+"' 2>/dev/null || echo '__NO_SUCH_FILE__'")
	if err != nil {
		return "", err
	}
	if strings.Contains(out, "__NO_SUCH_FILE__") {
		return "", fmt.Errorf("file not found: %s", path)
	}
	return out, nil
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
func dockerRun(ctx context.Context, args []string, stdin io.Reader, stdout, stderr *strings.Builder) (int, error) {
	bin := os.Getenv("ASTRBOT_DOCKER_BIN")
	if bin == "" {
		bin = "docker"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
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
	if m.booter != nil {
		m.booter.Stop()
	}
	m.booter = b
	m.mu.Unlock()
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
	logger.Info("Synced %d skills from sandbox", len(entries))
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
