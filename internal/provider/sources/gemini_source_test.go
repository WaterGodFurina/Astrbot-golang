package sources

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// TestGeminiRetryRebuildsBody verifies retries re-serialize the JSON body into
// a fresh reader every attempt (M-41). The old req.Clone shared a consumed
// bytes.Reader, so the second attempt would send an empty body.
func TestGeminiRetryRebuildsBody(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		if err := json.Unmarshal(body, &m); err != nil {
			t.Errorf("attempt %d: invalid json body %q", atomic.LoadInt32(&calls), body)
		}
		if _, ok := m["contents"]; !ok {
			t.Errorf("attempt %d: body missing contents: %q", atomic.LoadInt32(&calls), body)
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`)
	}))
	defer srv.Close()

	s := NewGeminiSource(map[string]interface{}{
		"key":      "k",
		"model":    "gemini-2.0-flash",
		"api_base": srv.URL,
	}, nil)
	resp, err := s.TextChat(context.Background(), &provider.ProviderRequest{Prompt: "hi", SessionID: "s"})
	if err != nil {
		t.Fatalf("TextChat: %v", err)
	}
	if resp.CompletionText != "ok" {
		t.Fatalf("completion = %q, want ok", resp.CompletionText)
	}
	if n := atomic.LoadInt32(&calls); n < 2 {
		t.Fatalf("expected retry, server calls = %d", n)
	}
}
