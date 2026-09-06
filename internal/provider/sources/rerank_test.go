package sources

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

func TestBailianRerankSource(t *testing.T) {
	var gotQuery string
	var gotInput bool
	var gotAuth string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		input, _ := body["input"].(map[string]interface{})
		gotInput = input != nil
		if gotInput {
			gotQuery, _ = input["query"].(string)
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":"200","output":{"results":[`+
			`{"index":1,"relevance_score":0.9},{"index":0,"relevance_score":0.3}]}}`)
	})

	src := NewBailianRerankSource(map[string]interface{}{
		"rerank_api_base": srv.URL,
		"rerank_api_key":  "sk-test",
	}, map[string]interface{}{})

	results, err := src.Rerank(context.Background(), "cat", []string{"dog post", "cat post"}, 0)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if !gotInput {
		t.Errorf("expected input-shaped payload")
	}
	if gotQuery != "cat" {
		t.Errorf("unexpected query: %q", gotQuery)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("unexpected auth: %q", gotAuth)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Index != 1 || results[0].RelevanceScore != 0.9 {
		t.Errorf("unexpected first result: %+v", results[0])
	}
	if results[1].RelevanceScore != 0.3 {
		t.Errorf("unexpected second result: %+v", results[1])
	}
	if src.Meta().ProviderType != provider.CapRerank {
		t.Errorf("capability not set: %v", src.Meta().ProviderType)
	}
}

func TestNvidiaRerankSource(t *testing.T) {
	var gotPath, gotAuth string
	var gotModel string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"rankings":[{"index":1,"logit":0.6},`+
			`{"index":2,"relevance_score":0.8}]}`)
	})

	src := NewNvidiaRerankSource(map[string]interface{}{
		"nvidia_rerank_api_base": srv.URL,
		"nvidia_rerank_api_key":  "nv-key",
	}, map[string]interface{}{})

	results, err := src.Rerank(context.Background(), "cat", []string{"a", "b", "c"}, 0)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if gotPath != "/nvidia/reranking" {
		t.Errorf("unexpected path: %q", gotPath)
	}
	if gotAuth != "Bearer nv-key" {
		t.Errorf("unexpected auth: %q", gotAuth)
	}
	if gotModel != "nvidia/llama-nemotron-rerank-vl-1b-v2" {
		t.Errorf("unexpected model: %q", gotModel)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Index != 2 || results[0].RelevanceScore != 0.8 {
		t.Errorf("unexpected first result: %+v", results[0])
	}
	if results[1].Index != 1 || results[1].RelevanceScore != 0.6 {
		t.Errorf("logit fallback not used: %+v", results[1])
	}
	if src.Meta().ProviderType != provider.CapRerank {
		t.Errorf("capability not set: %v", src.Meta().ProviderType)
	}
}

func TestVLLMRerankSource(t *testing.T) {
	var gotPath string
	var gotBody map[string]interface{}
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"index":0,"relevance_score":0.5},`+
			`{"index":1,"relevance_score":0.9}]}`)
	})

	src := NewVLLMRerankSource(map[string]interface{}{
		"rerank_api_base": srv.URL,
	}, map[string]interface{}{})

	results, err := src.Rerank(context.Background(), "cat", []string{"a", "b"}, 0)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if gotPath != "/v1/rerank" {
		t.Errorf("unexpected path: %q", gotPath)
	}
	if gotBody["model"] != "BAAI/bge-reranker-base" {
		t.Errorf("unexpected model: %v", gotBody["model"])
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

func TestXinferenceRerankSource(t *testing.T) {
	var gotPath string
	var gotBody map[string]interface{}
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"index":0,"relevance_score":0.1},`+
			`{"index":3,"relevance_score":0.99}]}`)
	})

	src := NewXinferenceRerankSource(map[string]interface{}{
		"rerank_api_base": srv.URL,
		"rerank_model":    "rerank-uid",
	}, map[string]interface{}{})

	results, err := src.Rerank(context.Background(), "cat", []string{"a", "b"}, 0)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if gotPath != "/v1/rerank" {
		t.Errorf("unexpected path: %q", gotPath)
	}
	if gotBody["model"] != "rerank-uid" {
		t.Errorf("unexpected model: %v", gotBody["model"])
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Index != 3 || results[0].RelevanceScore != 0.99 {
		t.Errorf("unexpected first result: %+v", results[0])
	}
	if src.Meta().ProviderType != provider.CapRerank {
		t.Errorf("capability not set: %v", src.Meta().ProviderType)
	}
}

func TestCreateRerankProviders(t *testing.T) {
	for _, typ := range []string{"bailian_rerank", "nvidia_rerank", "vllm_rerank", "xinference_rerank"} {
		p, err := provider.CreateProvider(typ, map[string]interface{}{
			"type": typ, "model": "m",
		}, map[string]interface{}{})
		if err != nil {
			t.Fatalf("create %s: %v", typ, err)
		}
		if p.Meta().Type != typ {
			t.Errorf("type mismatch: got %q want %q", p.Meta().Type, typ)
		}
		if p.Meta().ProviderType != provider.CapRerank {
			t.Errorf("%s: capability not set: %v", typ, p.Meta().ProviderType)
		}
	}
}
