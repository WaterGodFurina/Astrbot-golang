package pipeline

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestHTTPJSONRetryPreservesBody: 重试必须携带完整请求体。bug.md 3.2 曾用
// req.Clone（浅拷贝，Body 共享）导致 429/5xx 重试发送空 body，重试必然失败。
func TestHTTPJSONRetryPreservesBody(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	lastBody := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		attempts++
		lastBody = string(b)
		n := attempts
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	data, status, err := httpJSON(http.MethodPost, srv.URL, nil, map[string]interface{}{"query": "hello"})
	if err != nil {
		t.Fatalf("httpJSON: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	mu.Lock()
	n := attempts
	body := lastBody
	mu.Unlock()
	if n < 2 {
		t.Fatalf("expected at least one retry, got %d attempt(s)", n)
	}
	if !strings.Contains(body, `"query":"hello"`) {
		t.Fatalf("retry request body lost the payload: %q", body)
	}
	if !strings.Contains(string(data), `"ok":true`) {
		t.Fatalf("unexpected response: %s", data)
	}
}
