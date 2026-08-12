package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// DashScopeEmbeddingSource produces text embeddings via the Aliyun Bailian
// (DashScope) native text-embedding endpoint.
// Ported from astrbot/core/provider/sources/dashscope_embedding_source.py
type DashScopeEmbeddingSource struct {
	provider.BaseProvider
	apiBase string
	apiKey  string
	model   string
	client  *http.Client
	dim     int
}

// NewDashScopeEmbeddingSource creates a DashScope embedding provider.
func NewDashScopeEmbeddingSource(config, settings map[string]interface{}) *DashScopeEmbeddingSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &DashScopeEmbeddingSource{
		BaseProvider: bp,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
	s.apiKey = configString(config, "embedding_api_key", os.Getenv("DASHSCOPE_API_KEY"))
	s.apiBase = configString(config, "embedding_api_base", "https://dashscope.aliyuncs.com/api/v1")
	s.apiBase = strings.TrimSuffix(s.apiBase, "/")
	s.model = configString(config, "embedding_model", "text-embedding-v4")
	s.dim = configInt(config, "embedding_dimensions", 0)
	s.SetModel(s.model)
	s.SetCapability(provider.CapEmbedding)
	return s
}

// GetEmbedding returns the embedding vector for a single text.
func (s *DashScopeEmbeddingSource) GetEmbedding(ctx context.Context, text string) ([]float32, error) {
	vecs, err := s.embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("dashscope embedding API returned no vectors")
	}
	return vecs[0], nil
}

// GetEmbeddings returns embedding vectors for multiple texts.
func (s *DashScopeEmbeddingSource) GetEmbeddings(ctx context.Context, texts []string) ([][]float32, error) {
	return s.embed(ctx, texts)
}

func (s *DashScopeEmbeddingSource) embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	payload := map[string]interface{}{
		"model": s.model,
		"input": texts,
	}
	if s.dim > 0 {
		payload["dimension"] = s.dim
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	url := s.apiBase + "/services/embeddings/text-embedding/text-embedding"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if s.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("dashscope embedding API error %d: %s", resp.StatusCode, string(data))
	}

	var result struct {
		Output struct {
			Embeddings []dashScopeEmbeddingItem `json:"embeddings"`
		} `json:"output"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if len(result.Output.Embeddings) == 0 {
		return nil, fmt.Errorf("dashscope embedding API returned no embeddings")
	}
	items := result.Output.Embeddings
	sort.SliceStable(items, func(i, j int) bool {
		return dashScopeItemIndex(items[i]) < dashScopeItemIndex(items[j])
	})
	vecs := make([][]float32, 0, len(items))
	for _, it := range items {
		v := make([]float32, len(it.Embedding))
		for i, f := range it.Embedding {
			v[i] = float32(f)
		}
		vecs = append(vecs, v)
	}
	return vecs, nil
}

// dashScopeEmbeddingItem is one entry of output.embeddings.
type dashScopeEmbeddingItem struct {
	TextIndex *int      `json:"text_index"`
	Index     *int      `json:"index"`
	Embedding []float64 `json:"embedding"`
}

// dashScopeItemIndex resolves the sort key of an embedding item, preferring
// text_index (text models) and falling back to index (multimodal models).
func dashScopeItemIndex(it dashScopeEmbeddingItem) int {
	if it.TextIndex != nil {
		return *it.TextIndex
	}
	if it.Index != nil {
		return *it.Index
	}
	return 0
}

// GetDim returns the configured embedding dimension (0 = unknown).
func (s *DashScopeEmbeddingSource) GetDim() int { return s.dim }

// Test verifies the provider by embedding a probe string.
func (s *DashScopeEmbeddingSource) Test(ctx context.Context) error {
	_, err := s.GetEmbedding(ctx, "test")
	return err
}
