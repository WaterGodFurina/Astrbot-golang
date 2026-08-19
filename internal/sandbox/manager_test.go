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

	// Build /workspace/evil -> host /etc (outside the sandbox root).
	evil := filepath.Join(root, "evil")
	if err := os.MkdirAll(evil, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(evil, "link")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	// Reading through the symlink must fail.
	if _, err := b.ReadFile(ctx, "evil/link/passwd"); err == nil {
		t.Fatal("reading /etc/passwd through an in-sandbox symlink must be rejected")
	}
	// Writing through the symlink must fail too.
	if err := b.WriteFile(ctx, "evil/link/pwned.txt", "x"); err == nil {
		t.Fatal("writing outside the sandbox through a symlink must be rejected")
	}
	// A symlink whose target stays inside the root is fine.
	inside := filepath.Join(root, "inside.txt")
	if err := os.WriteFile(inside, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../inside.txt", filepath.Join(evil, "ok")); err != nil {
		t.Fatal(err)
	}
	if got, err := b.ReadFile(ctx, "evil/ok"); err != nil || got != "ok" {
		t.Fatalf("in-root symlink should be readable: err=%v got=%q", err, got)
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
	_, _, code, err = b.Exec(ctx, "sh", []string{"-c", "exit 3"}, SandboxWorkdir)
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

// withFakeDocker installs a fake `docker` CLI (via ASTRBOT_DOCKER_BIN) that
// simulates a managed sandbox container and records every invoked command to a
// log file, so Start's reuse logic can be asserted without a docker daemon.
func withFakeDocker(t *testing.T, fn func(t *testing.T, logFile string)) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "docker")
	logFile := filepath.Join(dir, "docker.log")
	content := `#!/bin/sh
if [ -n "$FAKE_LOG" ]; then
  printf '%s\n' "$*" >> "$FAKE_LOG"
fi
case "$1" in
  ps) echo "abc123" ;;
  inspect) echo "${FAKE_INSPECT:-false}" ;;
  start)
    if [ -n "$FAKE_FAIL_START" ]; then
      echo "docker: error: cannot start container" >&2
      exit 1
    fi
    ;;
  rm) exit 0 ;;
  run) echo "def456" ;;
esac
`
	if err := os.WriteFile(script, []byte(content), 0755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("ASTRBOT_DOCKER_BIN", script)
	t.Setenv("FAKE_LOG", logFile)
	fn(t, logFile)
}

func fakeDockerCommands(t *testing.T, logFile string) []string {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read fake docker log: %v", err)
	}
	var cmds []string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			cmds = append(cmds, strings.Fields(l)[0])
		}
	}
	return cmds
}

func TestDockerBooterReuseRunningContainer(t *testing.T) {
	withFakeDocker(t, func(t *testing.T, logFile string) {
		t.Setenv("FAKE_INSPECT", "true")
		b := NewDockerBooter("ubuntu:22.04")
		if err := b.Start(context.Background()); err != nil {
			t.Fatalf("start: %v", err)
		}
		if !b.IsRunning() {
			t.Fatal("expected running")
		}
		if b.containerID != "abc123" {
			t.Fatalf("containerID = %q, want abc123", b.containerID)
		}
		cmds := fakeDockerCommands(t, logFile)
		if containsString(cmds, "start") || containsString(cmds, "run") {
			t.Errorf("running container should be reused directly, got %v", cmds)
		}
	})
}

func TestDockerBooterRestartsStoppedContainer(t *testing.T) {
	withFakeDocker(t, func(t *testing.T, logFile string) {
		t.Setenv("FAKE_INSPECT", "false")
		b := NewDockerBooter("ubuntu:22.04")
		if err := b.Start(context.Background()); err != nil {
			t.Fatalf("start: %v", err)
		}
		if !b.IsRunning() {
			t.Fatal("expected running")
		}
		if b.containerID != "abc123" {
			t.Fatalf("containerID = %q, want abc123", b.containerID)
		}
		cmds := fakeDockerCommands(t, logFile)
		if !containsString(cmds, "start") {
			t.Errorf("stopped container should be restarted, got %v", cmds)
		}
		if containsString(cmds, "run") {
			t.Errorf("restarted container must not be rebuilt, got %v", cmds)
		}
	})
}

func TestDockerBooterRebuildsUnstartableContainer(t *testing.T) {
	withFakeDocker(t, func(t *testing.T, logFile string) {
		t.Setenv("FAKE_INSPECT", "false")
		t.Setenv("FAKE_FAIL_START", "1")
		b := NewDockerBooter("ubuntu:22.04")
		if err := b.Start(context.Background()); err != nil {
			t.Fatalf("start: %v", err)
		}
		if !b.IsRunning() {
			t.Fatal("expected running")
		}
		if b.containerID != "def456" {
			t.Fatalf("containerID = %q, want def456", b.containerID)
		}
		cmds := fakeDockerCommands(t, logFile)
		if !containsString(cmds, "rm") || !containsString(cmds, "run") {
			t.Errorf("unstartable container should be removed and rebuilt, got %v", cmds)
		}
	})
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
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

// TestDockerBooterReadFileUsesExitCode verifies the file-not-found decision
// is based on the docker exec exit code, not on stdout content: a file whose
// content equals the old sentinel string must still be returned verbatim.
func TestDockerBooterReadFileUsesExitCode(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "docker")

	// The fake docker evaluates the `sh -c` command in its own CWD (dir), so
	// files created here are readable via `cat 'name'`.
	if err := os.WriteFile(filepath.Join(dir, "sentinel.txt"), []byte("__NO_SUCH_FILE__"), 0644); err != nil {
		t.Fatal(err)
	}
	content := `#!/bin/sh
cd "$(dirname "$0")"
prev=""
for a in "$@"; do
  if [ "$prev" = "-c" ]; then cmd="$a"; fi
  prev="$a"
done
eval "$cmd"
`
	if err := os.WriteFile(script, []byte(content), 0755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("ASTRBOT_DOCKER_BIN", script)

	b := NewDockerBooter("ubuntu:22.04")
	b.containerID = "c1"
	b.running = true

	// A file whose content is exactly the old sentinel string must be returned
	// verbatim (the old code would have reported "file not found").
	got, err := b.ReadFile(context.Background(), "sentinel.txt")
	if err != nil {
		t.Fatalf("ReadFile sentinel-content file: %v", err)
	}
	if got != "__NO_SUCH_FILE__" {
		t.Errorf("ReadFile = %q, want %q (content must not be confused with the sentinel)", got, "__NO_SUCH_FILE__")
	}

	// A genuinely missing file must surface an error via the exit code.
	if _, err := b.ReadFile(context.Background(), "missing.txt"); err == nil {
		t.Error("expected file-not-found error for missing file")
	}
}
