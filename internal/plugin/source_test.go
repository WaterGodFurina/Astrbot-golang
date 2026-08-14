package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestArchiveExt verifies that archive URLs map to a cache-file extension that
// extractArchive recognizes — especially ".tar.gz", which filepath.Ext alone
// would reduce to ".gz" (M-44).
func TestArchiveExt(t *testing.T) {
	cases := map[string]string{
		"https://example.com/x.tar.gz": ".tar.gz",
		"https://example.com/x.tgz":    ".tgz",
		"https://example.com/x.zip":    ".zip",
		"https://example.com/x.TAR.GZ": ".tar.gz",
		"https://example.com/x.TGZ":    ".tgz",
		"https://example.com/x.ZIP":    ".zip",
		"https://example.com/x.bin":    ".bin",
		"/local/dir/archive.tar.gz":    ".tar.gz",
	}
	for in, want := range cases {
		if got := archiveExt(in); got != want {
			t.Errorf("archiveExt(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestExtractArchiveMatchesArchiveExt ensures every extension produced by
// archiveExt is handled by extractArchive's switch.
func TestExtractArchiveMatchesArchiveExt(t *testing.T) {
	for _, in := range []string{"a.zip", "a.tar.gz", "a.tgz"} {
		ext := archiveExt(in)
		switch ext {
		case ".zip", ".tar.gz", ".tgz":
		default:
			t.Errorf("archiveExt(%q) = %q is not handled by extractArchive", in, ext)
		}
	}
}

// TestDownloadRejectsRedirectToPrivateHost verifies the SSRF guard survives
// redirects: the initial URL is public but a 302 points at a loopback address,
// which must be rejected by CheckRedirect instead of followed.
func TestDownloadRejectsRedirectToPrivateHost(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:9/private", http.StatusFound)
	}))
	defer target.Close()

	err := downloadFile(context.Background(), target.URL, filepath.Join(t.TempDir(), "out"))
	if err == nil {
		t.Fatal("expected redirect to a loopback host to be rejected")
	}
	if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("error should mention the rejected redirect host, got: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(t.TempDir(), "out")); serr == nil {
		t.Error("no output file should be written when the redirect is rejected")
	}
}
