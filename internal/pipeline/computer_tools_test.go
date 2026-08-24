package pipeline

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// inTempDir changes into a fresh temp dir so workspaceRoot and the data/*
// roots resolve there without polluting the repo.
func inTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	return dir
}

func TestResolveLocalPathRelative(t *testing.T) {
	dir := inTempDir(t)
	umo := "test:conv"

	p, err := resolveLocalPath("notes.txt", umo, false)
	if err != nil {
		t.Fatalf("resolve relative: %v", err)
	}
	want := filepath.Join(dir, "data", "workspaces", "test_conv", "notes.txt")
	if p != want {
		t.Errorf("relative path resolved to %q, want %q", p, want)
	}
}

func TestResolveLocalPathAbsoluteWithinWorkspace(t *testing.T) {
	dir := inTempDir(t)
	umo := "test:conv"
	ws := filepath.Join(dir, "data", "workspaces", "test_conv")
	abs := filepath.Join(ws, "sub", "a.txt")

	p, err := resolveLocalPath(abs, umo, false)
	if err != nil {
		t.Fatalf("resolve absolute in workspace: %v", err)
	}
	if p != filepath.Clean(abs) {
		t.Errorf("absolute path resolved to %q, want %q", p, abs)
	}
}

func TestResolveLocalPathHomeAndDot(t *testing.T) {
	dir := inTempDir(t)
	umo := "t:c"

	// ~ expands to an absolute home path which (outside the workspace) must be
	// rejected, mirroring Python's expanduser + allowed-root check.
	if _, err := resolveLocalPath("~/file.txt", umo, false); err == nil {
		t.Errorf("expected rejection for ~/ outside allowed roots")
	}

	if _, err := resolveLocalPath("./x.txt", umo, false); err != nil {
		t.Errorf("resolve ./ failed: %v", err)
	}
	// A relative path keeps resolving under the workspace even with ./ or sub
	// dirs, never escaping it.
	p, err := resolveLocalPath("./sub/../x.txt", umo, false)
	if err != nil {
		t.Fatalf("resolve with . and ..: %v", err)
	}
	if p != filepath.Join(dir, "data", "workspaces", "t_c", "x.txt") {
		t.Errorf("unexpected cleaned path: %q", p)
	}
}

func TestResolveLocalPathRejectsOutside(t *testing.T) {
	inTempDir(t)
	umo := "t:c"

	if _, err := resolveLocalPath("../outside.txt", umo, false); err == nil {
		t.Errorf("expected rejection for ../ outside workspace")
	}
	if _, err := resolveLocalPath("/etc/passwd", umo, false); err == nil {
		t.Errorf("expected rejection for absolute path outside allowed roots")
	}
	if _, err := resolveLocalPath("../../../../tmp/evil.txt", umo, false); err == nil {
		t.Errorf("expected rejection for traversal path")
	}
	if _, err := resolveLocalPath("", umo, false); err == nil {
		t.Errorf("expected rejection for empty path")
	}
}

func TestResolveLocalPathWriteRoots(t *testing.T) {
	dir := inTempDir(t)
	umo := "t:c"
	skillsAbs := filepath.Join(dir, "data", "skills", "x.txt")

	// Writing into data/skills (read-only root) must be rejected.
	if _, err := resolveLocalPath(skillsAbs, umo, true); err == nil {
		t.Errorf("expected write rejection under data/skills")
	}
	// Reading from data/skills is allowed.
	if _, err := resolveLocalPath(skillsAbs, umo, false); err != nil {
		t.Errorf("expected read allowed under data/skills: %v", err)
	}
}

func TestResolveLocalPathRejectsSymlinkEscape(t *testing.T) {
	dir := inTempDir(t)
	umo := "t:c"
	ws := filepath.Join(dir, "data", "workspaces", "t_c")
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "link")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	// A lexical path under the workspace that traverses a symlink pointing
	// outside must be rejected, whether or not the target exists yet.
	if _, err := resolveLocalPath(filepath.Join(ws, "link", "secret.txt"), umo, false); err == nil {
		t.Errorf("expected rejection for non-existent symlink escape")
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveLocalPath(filepath.Join(ws, "link", "secret.txt"), umo, false); err == nil {
		t.Errorf("expected rejection for existing symlink escape")
	}

	// A plain path inside the workspace still resolves.
	if _, err := resolveLocalPath(filepath.Join(ws, "sub", "ok.txt"), umo, false); err != nil {
		t.Errorf("expected normal workspace path allowed: %v", err)
	}
}

func TestExecuteGrepRestrictsToWorkspace(t *testing.T) {
	dir := inTempDir(t)
	umo := "t:c"

	if out := executeGrep("x", "/etc", "", 10, umo); !strings.Contains(out, "outside") {
		t.Errorf("expected absolute path outside workspace rejected, got: %q", out)
	}
	if out := executeGrep("x", "../../../etc", "", 10, umo); !strings.Contains(out, "outside") {
		t.Errorf("expected traversal path rejected, got: %q", out)
	}

	ws := filepath.Join(dir, "data", "workspaces", "t_c")
	if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("hello world\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if out := executeGrep("world", "", "", 10, umo); !strings.Contains(out, "notes.txt") {
		t.Errorf("expected grep within workspace to find match, got: %q", out)
	}
}

func TestSandboxResolvePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"foo.txt", "/workspace/foo.txt"},
		{"./foo.txt", "/workspace/./foo.txt"},
		{"/workspace/foo.txt", "/workspace/foo.txt"},
		{"/tmp/abs.txt", "/tmp/abs.txt"},
		{"sub/../a.txt", "/workspace/sub/../a.txt"},
		{"", ""},
		{"  spaced name.txt ", "/workspace/spaced name.txt"},
	}
	for _, c := range cases {
		if got := sandboxResolvePath(c.in); got != c.want {
			t.Errorf("sandboxResolvePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func registerTestShellSession(t *testing.T, id, owner string, stdin io.WriteCloser) {
	t.Helper()
	shellSessionsMu.Lock()
	shellSessions[id] = &shellSession{
		ID:    id,
		Owner: owner + "\x00" + owner,
		Stdin: stdin,
	}
	shellSessionsMu.Unlock()
	t.Cleanup(func() {
		shellSessionsMu.Lock()
		delete(shellSessions, id)
		shellSessionsMu.Unlock()
		if stdin != nil {
			_ = stdin.Close()
		}
	})
}

func TestShellSessionWriteOwnershipAndData(t *testing.T) {
	pr, pw := io.Pipe()
	registerTestShellSession(t, "write1", "alice", pw)

	if out := shellSessionWrite("write1", "hello", "bob", "bob", false); !strings.Contains(out, "does not belong") {
		t.Errorf("write from wrong owner not blocked: %q", out)
	}
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 16)
		n, err := pr.Read(buf)
		if err != nil {
			got <- "err: " + err.Error()
			return
		}
		got <- string(buf[:n])
	}()
	if out := shellSessionWrite("write1", "hello", "alice", "alice", false); !strings.Contains(out, "Written to session") {
		t.Errorf("write from owner failed: %q", out)
	}
	select {
	case data := <-got:
		if data != "hello" {
			t.Errorf("session received %q, want %q", data, "hello")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out reading from stdin pipe")
	}
	_ = pr.Close()
}

func TestShellSessionPollOwnership(t *testing.T) {
	registerTestShellSession(t, "poll1", "alice", nil)
	if out := shellSessionPoll("poll1", "bob", "bob"); !strings.Contains(out, "does not belong") {
		t.Errorf("poll from wrong owner not blocked: %q", out)
	}
	if out := shellSessionPoll("poll1", "alice", "alice"); strings.Contains(out, "does not belong") {
		t.Errorf("poll from owner incorrectly blocked: %q", out)
	}
}

func TestShellSessionSignalOwnership(t *testing.T) {
	registerTestShellSession(t, "sig1", "alice", nil)
	if out := shellSessionSignal("sig1", true, "bob", "bob"); !strings.Contains(out, "does not belong") {
		t.Errorf("signal from wrong owner not blocked: %q", out)
	}
	if out := shellSessionSignal("sig1", true, "alice", "alice"); !strings.Contains(out, "not running") {
		t.Errorf("signal from owner failed: %q", out)
	}
}

func TestBackgroundShellSessionStdinWrite(t *testing.T) {
	inTempDir(t)
	umo := "bg:test"
	out := executeLocalShell(umo, umo, "cat", true, 0)
	prefix := "session id: "
	idx := strings.Index(out, prefix)
	if idx < 0 {
		t.Fatalf("no session id in output: %q", out)
	}
	id := out[idx+len(prefix):]
	if end := strings.IndexByte(id, ')'); end >= 0 {
		id = id[:end]
	}

	shellSessionsMu.Lock()
	s := shellSessions[id]
	shellSessionsMu.Unlock()
	if s == nil || s.Stdin == nil {
		t.Fatalf("session %s not registered with a stdin pipe", id)
	}
	if r := shellSessionWrite(id, "x", "other:user", "other:user", false); !strings.Contains(r, "does not belong") {
		t.Fatalf("write from wrong owner not blocked: %q", r)
	}
	if r := shellSessionWrite(id, "hello world", umo, umo, false); !strings.Contains(r, "Written to session") {
		t.Fatalf("write to background session failed: %q", r)
	}

	deadline := time.Now().Add(3 * time.Second)
	ok := false
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if strings.Contains(shellSessionPoll(id, umo, umo), "hello world") {
			ok = true
			break
		}
	}
	shellSessionSignal(id, true, umo, umo)
	if !ok {
		t.Fatal("background session output did not contain written data")
	}
}
