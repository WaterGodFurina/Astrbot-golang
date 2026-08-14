package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
