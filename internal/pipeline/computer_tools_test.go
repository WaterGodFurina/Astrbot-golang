package pipeline

import (
	"os"
	"path/filepath"
	"testing"
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
