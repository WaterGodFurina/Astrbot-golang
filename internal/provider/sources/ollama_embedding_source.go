package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

// OllamaEmbeddingSource produces text embeddings via Ollama's /api/embed
// endpoint.
// Ported from astrbot/core/provider/sources/ollama_embedding_source.py
type OllamaEmbeddingSource struct {
	provider.BaseProvider
	baseURL string
	model   string
	client  *http.Client
	dim     int
}

// NewOllamaEmbeddingSource creates an Ollama embedding provider.
func NewOllamaEmbeddingSource(config, settings map[string]interface{}) *OllamaEmbeddingSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &OllamaEmbeddingSource{
		BaseProvider: bp,
		client: &http.Client{
			Timeout: time.Duration(configInt(config, "timeout", 60)) * time.Second,
		},
	}
	s.baseURL = configString(config, "embedding_api_base", "http://localhost:11434")
	s.baseURL = strings.TrimSuffix(s.baseURL, "/")
	s.baseURL = strings.TrimSuffix(s.baseURL, "/api/embed")
	s.model = configString(config, "embedding_model", "nomic-embed-text")
	s.dim = configInt(config, "embedding_dimensions", 0)
	s.SetModel(s.model)
	s.SetCapability(provider.CapEmbedding)
	return s
}

// GetEmbedding returns the embedding vector for a single text.
func (s *OllamaEmbeddingSource) GetEmbedding(ctx context.Context, text string) ([]float32, error) {
	vecs, err := s.embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("ollama embedding API returned no vectors")
	}
	return vecs[0], nil
}

// GetEmbeddings returns embedding vectors for multiple texts.
func (s *OllamaEmbeddingSource) GetEmbeddings(ctx context.Context, texts []string) ([][]float32, error) {
	return s.embed(ctx, texts)
}

func (s *OllamaEmbeddingSource) embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	payload := map[string]interface{}{
		"model": s.model,
		"input": texts,
	}
	if s.dim > 0 {
		payload["dimensions"] = s.dim
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	url := s.baseURL + "/api/embed"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("ollama embedding API error %d: %s", resp.StatusCode, string(data))
	}

	var result struct {
		Embeddings [][]float64 `json:"embeddings"`
		Data       [][]float64 `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	raw := result.Embeddings
	if raw == nil {
		raw = result.Data
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("ollama embedding API returned no embeddings")
	}
	vecs := make([][]float32, 0, len(raw))
	for _, v := range raw {
		vec := make([]float32, len(v))
		for i, f := range v {
			vec[i] = float32(f)
		}
		vecs = append(vecs, vec)
	}
	return vecs, nil
}

// GetDim returns the configured embedding dimension (0 = unknown).
func (s *OllamaEmbeddingSource) GetDim() int { return s.dim }

// Test verifies the provider by embedding a probe string.
func (s *OllamaEmbeddingSource) Test(ctx context.Context) error {
	_, err := s.GetEmbedding(ctx, "test")
	return err
}
