package sources

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenAIEmbeddingSource(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	_ = os.Chdir(dir)
	t.Cleanup(func() { _ = os.Chdir(old) })

	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "text-embedding-3-small" {
			t.Errorf("unexpected model: %v", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"embedding":[0.1,0.2,0.3]},{"embedding":[0.4,0.5,0.6]}]}`)
	})

	src := NewOpenAIEmbeddingSource(map[string]interface{}{
		"embedding_api_base": srv.URL,
		"embedding_api_key":  "sk-test",
		"embedding_model":    "text-embedding-3-small",
	}, map[string]interface{}{})

	vec, err := src.GetEmbedding(context.Background(), "hello")
	if err != nil {
		t.Fatalf("get embedding: %v", err)
	}
	if len(vec) != 3 || vec[0] != 0.1 || vec[2] != 0.3 {
		t.Errorf("unexpected vector: %v", vec)
	}

	vecs, err := src.GetEmbeddings(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("get embeddings: %v", err)
	}
	if len(vecs) != 2 || len(vecs[1]) != 3 {
		t.Errorf("unexpected batch: %+v", vecs)
	}
	if src.GetDim() != 0 {
		t.Errorf("expected dim 0 when unset")
	}
	if src.Meta().ProviderType != provider.CapEmbedding {
		t.Errorf("capability not set: %v", src.Meta().ProviderType)
	}
}

func TestOpenAITTSSource(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	_ = os.Chdir(dir)
	t.Cleanup(func() { _ = os.Chdir(old) })

	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/speech" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["voice"] != "alloy" {
			t.Errorf("unexpected voice: %v", body["voice"])
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("RIFFfakewavdata"))
	})

	src := NewOpenAITTSSource(map[string]interface{}{
		"api_base": srv.URL,
		"key":      "sk-test",
		"model":    "tts-1",
	}, map[string]interface{}{})

	path, err := src.GetAudio(context.Background(), "你好")
	if err != nil {
		t.Fatalf("get audio: %v", err)
	}
	if path == "" {
		t.Fatalf("empty output path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !strings.HasPrefix(string(data), "RIFF") {
		t.Errorf("unexpected audio content: %q", data)
	}
	_ = os.Remove(path)
	if src.SupportStream() {
		t.Errorf("expected no streaming support")
	}
}

func TestOpenAIWhisperSourceLocalFile(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	_ = os.Chdir(dir)
	t.Cleanup(func() { _ = os.Chdir(old) })

	// Fake audio file + stub transcriptions endpoint.
	audio := filepath.Join(dir, "voice.wav")
	if err := os.WriteFile(audio, []byte("fakeaudio"), 0644); err != nil {
		t.Fatal(err)
	}

	var gotModel, gotFilename string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_ = r.ParseMultipartForm(1 << 20)
		gotModel = r.FormValue("model")
		if files := r.MultipartForm.File["file"]; len(files) > 0 {
			gotFilename = files[0].Filename
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":" 你好世界 "}`)
	})

	src := NewOpenAIWhisperSource(map[string]interface{}{
		"api_base": srv.URL,
		"key":      "sk-test",
		"model":    "whisper-1",
	}, map[string]interface{}{})

	text, err := src.GetText(context.Background(), audio)
	if err != nil {
		t.Fatalf("get text: %v", err)
	}
	if text != "你好世界" {
		t.Errorf("unexpected transcript: %q", text)
	}
	if gotModel != "whisper-1" {
		t.Errorf("unexpected model sent: %q", gotModel)
	}
	if gotFilename != "voice.wav" {
		t.Errorf("unexpected uploaded filename: %q", gotFilename)
	}
}

func TestOpenAIWhisperSourceRemoteURL(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	_ = os.Chdir(dir)
	t.Cleanup(func() { _ = os.Chdir(old) })

	audioSrv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("downloadedaudio"))
	})
	transcribeSrv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"hi"}`)
	})

	src := NewOpenAIWhisperSource(map[string]interface{}{
		"api_base": transcribeSrv.URL,
		"key":      "sk-test",
	}, map[string]interface{}{})

	text, err := src.GetText(context.Background(), audioSrv.URL+"/audio.wav")
	if err != nil {
		t.Fatalf("get text from url: %v", err)
	}
	if text != "hi" {
		t.Errorf("unexpected transcript: %q", text)
	}
}

func TestTEIRerankSource(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rerank" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["query"] != "cat" {
			t.Errorf("unexpected query: %v", body["query"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"index":1,"score":0.9},{"index":0,"score":0.2}]`)
	})

	src := NewTEIRerankSource(map[string]interface{}{
		"rerank_api_base": srv.URL,
	}, map[string]interface{}{})

	results, err := src.Rerank(context.Background(), "cat", []string{"dog post", "cat post"}, 0)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Index != 1 || results[0].RelevanceScore != 0.9 {
		t.Errorf("unexpected first result: %+v", results[0])
	}
	if src.Meta().ProviderType != provider.CapRerank {
		t.Errorf("capability not set: %v", src.Meta().ProviderType)
	}
}

func TestCreateProviderByType(t *testing.T) {
	// All four new provider types must be constructible through the registry.
	for _, typ := range []string{"openai_whisper", "openai_tts", "openai_embedding", "tei_rerank"} {
		p, err := provider.CreateProvider(typ, map[string]interface{}{
			"type": typ, "model": "m",
		}, map[string]interface{}{})
		if err != nil {
			t.Fatalf("create %s: %v", typ, err)
		}
		if p.Meta().Type != typ {
			t.Errorf("type mismatch: got %q want %q", p.Meta().Type, typ)
		}
	}
}

func TestProviderManagerCapabilities(t *testing.T) {
	pm := provider.NewProviderManager()
	emb := NewOpenAIEmbeddingSource(map[string]interface{}{"id": "emb", "type": "openai_embedding"}, nil)
	tts := NewOpenAITTSSource(map[string]interface{}{"id": "tts", "type": "openai_tts"}, nil)
	pm.Register("emb", emb)
	pm.Register("tts", tts)

	if pm.GetEmbeddingProvider() != emb {
		t.Errorf("embedding provider not found by fallback")
	}
	if pm.GetTTSProvider() != tts {
		t.Errorf("tts provider not found by fallback")
	}
	pm.SetDefaultEmbeddingProvider("emb")
	if pm.GetEmbeddingProvider() != emb {
		t.Errorf("embedding provider not found by default id")
	}
	if pm.GetRerankProvider() != nil {
		t.Errorf("expected no rerank provider")
	}
}
