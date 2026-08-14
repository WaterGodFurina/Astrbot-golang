package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestWebhookDispatch: a registered webhook uuid receives the request.
func TestWebhookDispatch(t *testing.T) {
	s := &Server{}
	got := ""
	s.RegisterWebhook("lark-test", func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 64)
		n, _ := r.Body.Read(b)
		got = strings.TrimSpace(string(b[:n]))
		w.WriteHeader(http.StatusOK)
	})
	body := `{"type":"url_verification","challenge":"c123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/platforms/lark-test", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleWebhooks(w, req, []string{"platforms", "lark-test"})
	if got == "" {
		t.Error("registered handler must receive the request body")
	}
	if !strings.Contains(got, "c123") {
		t.Errorf("body mismatch: %q", got)
	}
}

// TestWebhookUnknownUUID: unknown uuid returns 404.
func TestWebhookUnknownUUID(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/platforms/nonexistent", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	s.handleWebhooks(w, req, []string{"platforms", "nonexistent"})
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown uuid must 404, got %d", w.Code)
	}
}

// TestWebhookList: listing returns registered uuids.
func TestWebhookList(t *testing.T) {
	s := &Server{}
	s.RegisterWebhook("a", func(w http.ResponseWriter, r *http.Request) {})
	s.RegisterWebhook("b", func(w http.ResponseWriter, r *http.Request) {})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/webhooks", nil)
	w := httptest.NewRecorder()
	s.handleWebhooks(w, req, []string{})
	var resp struct {
		Data struct {
			Platforms []string `json:"platforms"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data.Platforms) != 2 {
		t.Errorf("expected 2 uuids, got %v", resp.Data.Platforms)
	}
	s.ClearWebhooks()
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/webhooks", nil)
	w2 := httptest.NewRecorder()
	s.handleWebhooks(w2, req2, []string{})
	if !strings.Contains(w2.Body.String(), "\"platforms\":[]") {
		t.Errorf("after ClearWebhooks list must be empty: %s", w2.Body.String())
	}
}

// TestWebhookCallbackUnauthenticated: 统一 webhook 回调路径（带具体 uuid）无需
// dashboard token 即可经 apiHandler 全局鉴权门到达注册的处理器（H-15）。
func TestWebhookCallbackUnauthenticated(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "cmd_config.json"))
	defer s.Stop()

	got := ""
	s.RegisterWebhook("cb-uuid", func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/platforms/cb-uuid", strings.NewReader(`{"challenge":"x"}`))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook callback without token: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	if got != "/api/v1/webhooks/platforms/cb-uuid" {
		t.Fatalf("registered handler did not receive the callback, path=%q", got)
	}
}

// TestWebhookListRequiresAuth: uuid 列表枚举路径不得对未认证请求放行；带
// 合法 token 的枚举仍可用。
func TestWebhookListRequiresAuth(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "cmd_config.json"))
	defer s.Stop()
	s.RegisterWebhook("a", func(w http.ResponseWriter, r *http.Request) {})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/webhooks", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("webhook uuid list without token: want 401, got %d (%s)", w.Code, w.Body.String())
	}

	req = authedRequest(t, s, http.MethodGet, "/api/v1/webhooks", nil)
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook uuid list with token: want 200, got %d (%s)", w.Code, w.Body.String())
	}
}

// TestWebhookPlatformsPathRequiresAuth: 无 uuid 的 platforms 段（webhooks 枚举
// 别名）同样要求认证。
func TestWebhookPlatformsPathRequiresAuth(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "cmd_config.json"))
	defer s.Stop()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/webhooks/platforms", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("webhooks/platforms without token: want 401, got %d (%s)", w.Code, w.Body.String())
	}
}
