package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestLocalBooterExec(t *testing.T) {
	b := NewLocalBooter()
	b.SetRoot(t.TempDir())
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer b.Stop()

	if err := b.WriteFile(ctx, SandboxWorkdir+"/greeting.txt", "hi"); err != nil {
		t.Fatalf("write: %v", err)
	}
	stdout, stderr, code, err := b.Exec(ctx, "sh", []string{"-c", "cat greeting.txt"}, SandboxWorkdir)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if strings.TrimSpace(stdout) != "hi" {
		t.Errorf("exec stdout %q, want %q", stdout, "hi")
	}

	// Non-zero exit is surfaced, not treated as an error.
	stdout, _, code, err = b.Exec(ctx, "sh", []string{"-c", "exit 3"}, SandboxWorkdir)
	if err != nil {
		t.Fatalf("exec non-zero: %v", err)
	}
	if code != 3 {
		t.Errorf("expected exit code 3, got %d", code)
	}
}

func TestLocalBooterNotRunning(t *testing.T) {
	b := NewLocalBooter()
	if _, err := b.ReadFile(context.Background(), "x.txt"); err == nil {
		t.Errorf("expected error when not running")
	}
	if _, _, _, err := b.Exec(context.Background(), "sh", nil, SandboxWorkdir); err == nil {
		t.Errorf("expected exec error when not running")
	}
}

// TestLocalBooterSkills verifies SKILL.md discovery mirrors the sandbox layout.
func TestLocalBooterSkills(t *testing.T) {
	b := NewLocalBooter()
	b.SetRoot(t.TempDir())
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer b.Stop()

	skillDir := filepath.Join(b.root, "skills", "demo")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\ndescription: A demo skill\n---\nbody"), 0644); err != nil {
		t.Fatal(err)
	}
	entries, err := b.ListSkills(ctx)
	if err != nil {
		t.Fatalf("list skills: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "demo" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
	if entries[0].Description != "A demo skill" {
		t.Errorf("description not parsed: %q", entries[0].Description)
	}
}
