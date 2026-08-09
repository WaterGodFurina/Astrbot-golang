package sources

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

func TestGeminiEmbeddingSource(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, ":batchEmbedContents") {
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			reqs, ok := body["requests"].([]interface{})
			if !ok || len(reqs) != 2 {
				t.Errorf("unexpected batch requests: %v", body["requests"])
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"embeddings":[{"values":[0.4,0.5,0.6]},{"values":[0.7,0.8,0.9]}]}`)
			return
		}
		if strings.Contains(r.URL.Path, ":embedContent") {
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["outputDimensionality"] != float64(3) {
				t.Errorf("unexpected outputDimensionality: %v", body["outputDimensionality"])
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"embedding":{"values":[0.1,0.2,0.3]}}`)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	})

	src := NewGeminiEmbeddingSource(map[string]interface{}{
		"embedding_api_base":   srv.URL,
		"embedding_api_key":    "test-key",
		"embedding_model":      "gemini-embedding-exp-03-07",
		"embedding_dimensions": 3,
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
	if src.GetDim() != 3 {
		t.Errorf("expected dim 3, got %d", src.GetDim())
	}
	if src.Meta().ProviderType != provider.CapEmbedding {
		t.Errorf("capability not set: %v", src.Meta().ProviderType)
	}
}

func TestNvidiaEmbeddingSource(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("unexpected auth header: %q", r.Header.Get("Authorization"))
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["input_type"] != "query" {
			t.Errorf("unexpected input_type: %v", body["input_type"])
		}
		if body["model"] != "nvidia/llama-nemotron-embed-1b-v2" {
			t.Errorf("unexpected model: %v", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"embedding":[0.1,0.2,0.3,0.4]},{"embedding":[0.5,0.6,0.7,0.8]}]}`)
	})

	src := NewNvidiaEmbeddingSource(map[string]interface{}{
		"embedding_api_base":   srv.URL,
		"embedding_api_key":    "sk-test",
		"embedding_model":      "nvidia/llama-nemotron-embed-1b-v2",
		"input_type":           "query",
		"embedding_dimensions": 4,
	}, map[string]interface{}{})

	vec, err := src.GetEmbedding(context.Background(), "hello")
	if err != nil {
		t.Fatalf("get embedding: %v", err)
	}
	if len(vec) != 4 || vec[0] != 0.1 || vec[3] != 0.4 {
		t.Errorf("unexpected vector: %v", vec)
	}

	vecs, err := src.GetEmbeddings(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("get embeddings: %v", err)
	}
	if len(vecs) != 2 || len(vecs[1]) != 4 {
		t.Errorf("unexpected batch: %+v", vecs)
	}
	if src.GetDim() != 4 {
		t.Errorf("expected dim 4, got %d", src.GetDim())
	}
	if src.Meta().ProviderType != provider.CapEmbedding {
		t.Errorf("capability not set: %v", src.Meta().ProviderType)
	}
}

func TestOllamaEmbeddingSource(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "nomic-embed-text" {
			t.Errorf("unexpected model: %v", body["model"])
		}
		if body["dimensions"] != float64(3) {
			t.Errorf("unexpected dimensions: %v", body["dimensions"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"embeddings":[[0.1,0.2,0.3],[0.4,0.5,0.6]]}`)
	})

	src := NewOllamaEmbeddingSource(map[string]interface{}{
		"embedding_api_base":   srv.URL,
		"embedding_model":      "nomic-embed-text",
		"embedding_dimensions": 3,
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
	if src.GetDim() != 3 {
		t.Errorf("expected dim 3, got %d", src.GetDim())
	}
	if src.Meta().ProviderType != provider.CapEmbedding {
		t.Errorf("capability not set: %v", src.Meta().ProviderType)
	}
}

func TestDashScopeEmbeddingSource(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/services/embeddings/text-embedding/text-embedding" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("unexpected auth header: %q", r.Header.Get("Authorization"))
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "text-embedding-v4" {
			t.Errorf("unexpected model: %v", body["model"])
		}
		if body["dimension"] != float64(3) {
			t.Errorf("unexpected dimension: %v", body["dimension"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":{"embeddings":[
			{"text_index":1,"embedding":[0.4,0.5,0.6]},
			{"text_index":0,"embedding":[0.1,0.2,0.3]}
		]}}`)
	})

	src := NewDashScopeEmbeddingSource(map[string]interface{}{
		"embedding_api_base":   srv.URL + "/api/v1",
		"embedding_api_key":    "sk-test",
		"embedding_model":      "text-embedding-v4",
		"embedding_dimensions": 3,
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
	if len(vecs) != 2 || len(vecs[0]) != 3 || vecs[0][0] != 0.1 || vecs[1][0] != 0.4 {
		t.Errorf("unexpected batch (order by text_index): %+v", vecs)
	}
	if src.GetDim() != 3 {
		t.Errorf("expected dim 3, got %d", src.GetDim())
	}
	if src.Meta().ProviderType != provider.CapEmbedding {
		t.Errorf("capability not set: %v", src.Meta().ProviderType)
	}
}

func TestCreateEmbeddingProvidersByType(t *testing.T) {
	for _, typ := range []string{"gemini_embedding", "nvidia_embedding", "ollama_embedding", "dashscope_embedding"} {
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
