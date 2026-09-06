package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/sandbox"
	"github.com/WaterGodFurina/Astrbot-golang/internal/skills"
)

type stageFakeBooter struct {
	mu    sync.Mutex
	files map[string]string
	fail  bool
}

func (f *stageFakeBooter) Type() sandbox.BooterType        { return sandbox.BooterRemote }
func (f *stageFakeBooter) Start(ctx context.Context) error { return nil }
func (f *stageFakeBooter) Stop() error                     { return nil }
func (f *stageFakeBooter) IsRunning() bool                 { return true }
func (f *stageFakeBooter) ListSkills(context.Context) ([]skills.SandboxCacheEntry, error) {
	return nil, nil
}
func (f *stageFakeBooter) Exec(context.Context, string, []string, string) (string, string, int, error) {
	return "", "", 0, nil
}
func (f *stageFakeBooter) ReadFile(context.Context, string) (string, error) { return "", nil }
func (f *stageFakeBooter) WriteFile(_ context.Context, path, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return os.ErrPermission
	}
	f.files[path] = content
	return nil
}

func TestStageFileIntoSandbox(t *testing.T) {
	dir := t.TempDir()
	host := filepath.Join(dir, "fileseg_bug.md_1789")
	if err := os.WriteFile(host, []byte("# hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	fb := &stageFakeBooter{files: map[string]string{}}
	mgr := sandbox.NewManager(nil)
	mgr.SetBooterFactory(func() sandbox.Booter { return fb })
	s := &ProcessStage{sandboxMgr: mgr}

	got := s.stageFileIntoSandbox(context.Background(), "g:1", host, true)
	want := sandboxWorkdir + "/fileseg_bug.md_1789"
	if got != want {
		t.Fatalf("staged path = %q, want %q", got, want)
	}
	if content := fb.files[want]; content != "# hello" {
		t.Fatalf("sandbox content = %q, want %q", content, "# hello")
	}

	if s.stageFileIntoSandbox(context.Background(), "g:1", host, false) != "" {
		t.Fatal("non-sandbox mode must skip staging")
	}
	if (&ProcessStage{}).stageFileIntoSandbox(context.Background(), "g:1", host, true) != "" {
		t.Fatal("nil sandbox manager must skip staging")
	}
	fb.fail = true
	if s.stageFileIntoSandbox(context.Background(), "g:1", host, true) != "" {
		t.Fatal("write failure must fall back to empty")
	}
	if s.stageFileIntoSandbox(context.Background(), "g:1", filepath.Join(dir, "missing"), true) != "" {
		t.Fatal("missing host file must fall back to empty")
	}
}
