package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newBayServer spins up a fake Bay (Shipyard Neo) API.
func newBayServer(t *testing.T) *httptest.Server {
	t.Helper()
	var statusCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"sb-1","status":"starting","profile":"python-default","capabilities":["python","shell","filesystem"]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-1":
			n := atomic.AddInt32(&statusCalls, 1)
			status := "starting"
			if n >= 2 {
				status = "ready"
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"sb-1","status":%q,"profile":"python-default"}`, status)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes/sb-1/shell/exec":
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["command"] != "echo hi" || body["cwd"] != "." {
				t.Errorf("unexpected shell exec body: %v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"output":"hi\n","exit_code":0,"success":true}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes/sb-1/python/exec":
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if !strings.Contains(body["code"].(string), "print") {
				t.Errorf("unexpected python body: %v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"output":"py-out\n","success":true}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-1/filesystem/files":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"content":"file-content"}`)
		case r.Method == http.MethodPut && r.URL.Path == "/v1/sandboxes/sb-1/filesystem/files":
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["path"] != "out.txt" || body["content"] != "data" {
				t.Errorf("unexpected write body: %v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		default:
			http.Error(w, "not found: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestShipyardNeoBooterStartExecFile(t *testing.T) {
	srv := newBayServer(t)
	b := NewShipyardNeoBooter(srv.URL, "test-token", "", 0)
	ctx := context.Background()

	if err := b.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer b.Stop()
	if !b.IsRunning() {
		t.Fatalf("expected running")
	}

	// Shell exec via sh -c.
	stdout, stderr, code, err := b.Exec(ctx, "sh", []string{"-c", "echo hi"}, SandboxWorkdir)
	if err != nil || code != 0 || stdout != "hi\n" {
		t.Errorf("shell exec: err=%v code=%d stdout=%q stderr=%q", err, code, stdout, stderr)
	}

	// Python exec.
	stdout, _, code, err = b.Exec(ctx, "python3", []string{"-c", "print(1)"}, SandboxWorkdir)
	if err != nil || code != 0 || stdout != "py-out\n" {
		t.Errorf("python exec: err=%v code=%d stdout=%q", err, code, stdout)
	}

	// File read / write (relative paths resolve under /workspace on Bay).
	content, err := b.ReadFile(ctx, "notes.txt")
	if err != nil || content != "file-content" {
		t.Errorf("read file: err=%v content=%q", err, content)
	}
	if err := b.WriteFile(ctx, "out.txt", "data"); err != nil {
		t.Errorf("write file: %v", err)
	}
}

func TestShipyardNeoBooterMissingConfig(t *testing.T) {
	ctx := context.Background()
	b := NewShipyardNeoBooter("", "token", "", 0)
	if err := b.Start(ctx); err == nil {
		t.Errorf("expected error for empty endpoint")
	}
	if _, err := b.ReadFile(ctx, "x"); err == nil {
		t.Errorf("expected error when not running")
	}
	if _, _, _, err := b.Exec(ctx, "sh", nil, SandboxWorkdir); err == nil {
		t.Errorf("expected exec error when not running")
	}
}

func TestNormalizeNeoCwd(t *testing.T) {
	cases := map[string]string{
		"/workspace":     ".",
		"/workspace/":    ".",
		"/workspace/a":   "a",
		"/workspace/a/b": "a/b",
		"a":              "a",
		"":               ".",
	}
	for in, want := range cases {
		if got := normalizeNeoCwd(in); got != want {
			t.Errorf("normalizeNeoCwd(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestShipyardNeoExecClientNoOverallTimeout verifies the exec client has no
// overall timeout (command timeout is carried in the request body / ctx), while
// the short client keeps its 60s bound for create/poll/file requests (M-60).
func TestShipyardNeoExecClientNoOverallTimeout(t *testing.T) {
	b := NewShipyardNeoBooter("http://example.com", "token", "", 0)
	if b.client.Timeout != 60*time.Second {
		t.Fatalf("short client timeout = %v, want 60s", b.client.Timeout)
	}
	if b.execClient.Timeout != 0 {
		t.Fatalf("exec client must have no overall timeout, got %v", b.execClient.Timeout)
	}
}
