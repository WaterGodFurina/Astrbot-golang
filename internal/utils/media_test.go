package utils

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// serveLarge writes exactly n bytes and returns an httptest server.
func serveBytes(n int64, payload byte) *httptest.Server {
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = payload
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remaining := n
		for remaining > 0 {
			k := int64(len(chunk))
			if k > remaining {
				k = remaining
			}
			if _, err := w.Write(chunk[:k]); err != nil {
				return
			}
			remaining -= k
		}
	}))
}

// TestDownloadToBase64SizeLimit verifies oversized downloads are rejected.
func TestDownloadToBase64SizeLimit(t *testing.T) {
	srv := serveBytes(maxDownloadBytes+1, 'x')
	defer srv.Close()

	_, err := DownloadToBase64(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected size-limit error for oversized download")
	}
}

// TestDownloadFileSizeLimit verifies the file path also enforces the limit.
func TestDownloadFileSizeLimit(t *testing.T) {
	srv := serveBytes(maxDownloadBytes+1, 'y')
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "big.bin")
	err := DownloadFile(context.Background(), srv.URL, dest)
	if err == nil {
		t.Fatal("expected size-limit error for oversized download")
	}
}

// TestDownloadSmallBody verifies the happy path still works for both helpers.
func TestDownloadSmallBody(t *testing.T) {
	body := []byte("hello media")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	b64, err := DownloadToBase64(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("DownloadToBase64: %v", err)
	}
	if b64 != base64.StdEncoding.EncodeToString(body) {
		t.Errorf("DownloadToBase64 = %q, want %q", b64, base64.StdEncoding.EncodeToString(body))
	}

	dest := filepath.Join(t.TempDir(), "media.bin")
	if err := DownloadFile(context.Background(), srv.URL, dest); err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("DownloadFile wrote %q, want %q", got, body)
	}
}

// TestDownloadHTTPStatus verifies non-200 responses surface an error.
func TestDownloadHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := DownloadToBase64(context.Background(), srv.URL); err == nil {
		t.Error("expected error for HTTP 404")
	}
	if err := DownloadFile(context.Background(), srv.URL, filepath.Join(t.TempDir(), "x.bin")); err == nil {
		t.Error("expected error for HTTP 404")
	}
}
