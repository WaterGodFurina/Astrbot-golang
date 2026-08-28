package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/skills"
)

func TestLocalBooterFileRoundTrip(t *testing.T) {
	b := NewLocalBooter()
	b.SetRoot(t.TempDir())
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer b.Stop()

	// Write a relative path -> must land under /workspace (the root).
	if err := b.WriteFile(ctx, "notes.txt", "hello sandbox"); err != nil {
		t.Fatalf("write relative: %v", err)
	}
	// The same file must be readable via the absolute sandbox path.
	got, err := b.ReadFile(ctx, SandboxWorkdir+"/notes.txt")
	if err != nil {
		t.Fatalf("read absolute: %v", err)
	}
	if got != "hello sandbox" {
		t.Errorf("read back %q, want %q", got, "hello sandbox")
	}
	// And via the relative path.
	if got, err := b.ReadFile(ctx, "notes.txt"); err != nil || got != "hello sandbox" {
		t.Errorf("read relative failed: err=%v got=%q", err, got)
	}

	// Writing an absolute /workspace path is mapped too.
	if err := b.WriteFile(ctx, SandboxWorkdir+"/sub/a.txt", "nested"); err != nil {
		t.Fatalf("write nested: %v", err)
	}
	if got, err := b.ReadFile(ctx, "sub/a.txt"); err != nil || got != "nested" {
		t.Errorf("read nested failed: err=%v got=%q", err, got)
	}

	// Host filesystem must not be reachable: /etc/hostname is shadowed under root.
	if _, err := b.ReadFile(ctx, "/etc/hostname"); err == nil {
		t.Errorf("expected error reading host /etc/hostname via sandbox")
	}
}

// TestLocalBooterSymlinkEscapeRejected: a symlink inside the sandbox root that
// points outside must not smuggle host file access (bug.md 3.3).
func TestLocalBooterSymlinkEscapeRejected(t *testing.T) {
	root := t.TempDir()
	b := NewLocalBooter()
	b.SetRoot(root)
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer b.Stop()
	if err := os.Symlink("/etc", filepath.Join(root, "evil")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := b.ReadFile(ctx, "evil/passwd"); err == nil {
		t.Errorf("symlink escape must be rejected")
	}
}

// fakeBooter is a scriptable Booter used to test the per-session Manager.
type fakeBooter struct {
	id      string
	running bool
	started bool
	stopped bool
	mu      sync.Mutex
}

func (f *fakeBooter) Type() BooterType                   { return BooterRemote }
func (f *fakeBooter) IsRunning() bool                    { f.mu.Lock(); defer f.mu.Unlock(); return f.running }
func (f *fakeBooter) Stop() error                        { f.mu.Lock(); defer f.mu.Unlock(); f.stopped = true; f.running = false; return nil }
func (f *fakeBooter) ListSkills(ctx context.Context) ([]skills.SandboxCacheEntry, error) {
	return nil, nil
}
func (f *fakeBooter) Exec(ctx context.Context, cmd string, args []string, workdir string) (string, string, int, error) {
	return "out", "", 0, nil
}
func (f *fakeBooter) ReadFile(ctx context.Context, path string) (string, error) { return "data", nil }
func (f *fakeBooter) WriteFile(ctx context.Context, path, content string) error { return nil }
func (f *fakeBooter) Start(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
	f.running = true
	return nil
}

// TestManagerPerSessionSandbox: 每个会话（群/私聊）独立沙盒；同一会话复用；
// 沙盒失效（模拟 Bay "Sandbox not found" 后 booter 被重置）时，操作后丢弃该
// 会话，下一次 EnsureSession 自动拉取新沙盒（对齐 astrbot 行为，无主动健康检测）。
func TestManagerPerSessionSandbox(t *testing.T) {
	var booters []*fakeBooter
	m := NewManager(nil)
	m.SetBooterFactory(func() Booter {
		f := &fakeBooter{id: "b" + string(rune('a'+len(booters)))}
		booters = append(booters, f)
		return f
	})
	ctx := context.Background()

	// 两个不同会话 → 两个独立沙盒实例。
	s1, err := m.EnsureSession(ctx, "g:1")
	if err != nil {
		t.Fatalf("EnsureSession g:1: %v", err)
	}
	s2, err := m.EnsureSession(ctx, "p:2")
	if err != nil {
		t.Fatalf("EnsureSession p:2: %v", err)
	}
	if s1 == s2 {
		t.Fatal("different sessions must get different sandbox booters")
	}
	if len(booters) != 2 {
		t.Fatalf("expected 2 booter instances, got %d", len(booters))
	}

	// 同一会话再次获取 → 复用（不新建）。
	_, err = m.EnsureSession(ctx, "g:1")
	if err != nil {
		t.Fatalf("EnsureSession g:1 again: %v", err)
	}
	if len(booters) != 2 {
		t.Fatalf("reusing a session must not create a new booter, got %d instances", len(booters))
	}

	// 模拟 g:1 的沙盒被 Bay 回收：booter 自我重置 running=false（等价于
	// markDeadIfNeeded 命中 "Sandbox not found"）。下一次 Exec 应丢弃会话，
	// 再 EnsureSession 时自动拉取新沙盒。
	booters[0].mu.Lock()
	booters[0].running = false
	booters[0].mu.Unlock()

	_, _, _, err = m.Exec(ctx, "g:1", "sh", []string{"-c", "echo hi"}, SandboxWorkdir)
	if err == nil {
		t.Fatal("Exec on a dead sandbox must fail")
	}
	// 会话已被丢弃，下一次 EnsureSession 拉取新沙盒。
	if _, err := m.EnsureSession(ctx, "g:1"); err != nil {
		t.Fatalf("EnsureSession g:1 after dead: %v", err)
	}
	if len(booters) != 3 {
		t.Fatalf("dead sandbox must auto-pull a new booter, expected 3 instances, got %d", len(booters))
	}
}
