package t2i

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestRenderFallsBackToDefault verifies the fallback template lookup is
// performed under the lock (regression for the RUnlock-then-bare-read race).
func TestRenderFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "default.html"), []byte("DEFAULT {{.Content}}"), 0644); err != nil {
		t.Fatal(err)
	}
	r := NewRenderer(dir)
	out, err := r.Render("hi", "missing")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "DEFAULT hi" {
		t.Errorf("Render output = %q, want %q", out, "DEFAULT hi")
	}
}

// TestRenderNoTemplate verifies a renderer with no templates at all errors out
// instead of panicking.
func TestRenderNoTemplate(t *testing.T) {
	r := NewRenderer(t.TempDir())
	if _, err := r.Render("hi", "default"); err == nil {
		t.Error("expected error when no template is available")
	}
}

// TestWriteCacheFileAtomic verifies concurrent writers never leave a torn
// cache file: the final content is exactly one of the fully-written payloads.
func TestWriteCacheFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.bin")

	payloads := [][]byte{
		bytes.Repeat([]byte{'a'}, 1<<20),
		bytes.Repeat([]byte{'b'}, 1<<20),
		bytes.Repeat([]byte{'c'}, 1<<20),
	}
	var wg sync.WaitGroup
	for _, p := range payloads {
		wg.Add(1)
		go func(data []byte) {
			defer wg.Done()
			_ = writeCacheFile(path, data)
		}(p)
	}
	wg.Wait()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	valid := false
	for _, p := range payloads {
		if bytes.Equal(got, p) {
			valid = true
			break
		}
	}
	if !valid {
		t.Error("cache file content is torn (mix of concurrent writes)")
	}
	// No temp files should remain.
	matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(matches) != 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}
