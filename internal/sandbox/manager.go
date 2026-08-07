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
	"context"
	"fmt"
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

// LocalBooter executes commands as local subprocesses with restricted env.
// This is the simplest backend — no container isolation, but restricted PATH.
type LocalBooter struct {
	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
}

// NewLocalBooter creates a local sandbox booter.
func NewLocalBooter() *LocalBooter {
	return &LocalBooter{}
}

func (b *LocalBooter) Type() BooterType { return BooterLocal }

func (b *LocalBooter) Start(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running {
		return fmt.Errorf("local booter already running")
	}
	b.running = true
	logger.Info("Local sandbox booter started")
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

func (b *LocalBooter) Exec(ctx context.Context, cmd string, args []string, workdir string) (string, string, int, error) {
	// In production, this would exec a subprocess with restricted env/PATH.
	// For now, return a placeholder.
	return "", "", 0, fmt.Errorf("local booter exec not yet implemented")
}

func (b *LocalBooter) ListSkills(ctx context.Context) ([]skills.SandboxCacheEntry, error) {
	// In production, this would scan /workspace/skills/ inside the sandbox.
	return nil, nil
}

func (b *LocalBooter) ReadFile(ctx context.Context, path string) (string, error) {
	return "", fmt.Errorf("local booter ReadFile not yet implemented")
}

func (b *LocalBooter) WriteFile(ctx context.Context, path, content string) error {
	return fmt.Errorf("local booter WriteFile not yet implemented")
}

// DockerBooter executes commands inside a Docker container.
type DockerBooter struct {
	mu         sync.Mutex
	running    bool
	containerID string
	image      string
	cancel     context.CancelFunc
}

// NewDockerBooter creates a Docker-based sandbox booter.
func NewDockerBooter(image string) *DockerBooter {
	if image == "" {
		image = "ubuntu:22.04"
	}
	return &DockerBooter{image: image}
}

func (b *DockerBooter) Type() BooterType { return BooterDocker }

func (b *DockerBooter) Start(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running {
		return fmt.Errorf("docker booter already running")
	}
	// In production: docker run -d --rm <image> sleep infinity
	logger.Info("Docker sandbox booter started (image=%s)", b.image)
	b.running = true
	return nil
}

func (b *DockerBooter) Stop() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.running {
		return nil
	}
	// In production: docker stop <containerID>
	b.running = false
	if b.cancel != nil {
		b.cancel()
	}
	logger.Info("Docker sandbox booter stopped")
	return nil
}

func (b *DockerBooter) IsRunning() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

func (b *DockerBooter) Exec(ctx context.Context, cmd string, args []string, workdir string) (string, string, int, error) {
	// In production: docker exec <containerID> <cmd> <args...>
	return "", "", 0, fmt.Errorf("docker booter exec not yet implemented")
}

func (b *DockerBooter) ListSkills(ctx context.Context) ([]skills.SandboxCacheEntry, error) {
	// In production: docker exec <containerID> find /workspace/skills -name SKILL.md
	return nil, nil
}

func (b *DockerBooter) ReadFile(ctx context.Context, path string) (string, error) {
	return "", fmt.Errorf("docker booter ReadFile not yet implemented")
}

func (b *DockerBooter) WriteFile(ctx context.Context, path, content string) error {
	return fmt.Errorf("docker booter WriteFile not yet implemented")
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
