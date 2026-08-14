package sources

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// oversizedAudioServer streams more than maxAudioBytes so the download limit
// logic must abort instead of buffering the whole body (L-46.2e).
func oversizedAudioServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 1<<20)
		total := maxAudioBytes + 1<<20
		for written := 0; written < total; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
}

func TestWhisperFetchAudioRejectsOversized(t *testing.T) {
	srv := oversizedAudioServer()
	defer srv.Close()

	src := NewOpenAIWhisperSource(map[string]interface{}{
		"api_base": srv.URL,
		"key":      "sk-test",
	}, map[string]interface{}{})
	path, cleanup, err := src.fetchAudio(context.Background(), srv.URL)
	if err == nil {
		cleanup()
		t.Fatalf("expected oversized audio to be rejected, got path %s", path)
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestMimoFetchAudioRejectsOversized(t *testing.T) {
	srv := oversizedAudioServer()
	defer srv.Close()

	client := &http.Client{}
	path, cleanup, err := mimoFetchAudio(context.Background(), client, srv.URL)
	if err == nil {
		cleanup()
		t.Fatalf("expected oversized audio to be rejected, got path %s", path)
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("unexpected error: %v", err)
	}
}
