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

// OpenAIEmbeddingSource produces text embeddings via the OpenAI-compatible
// /embeddings endpoint.
// Ported from astrbot/core/provider/sources/openai_embedding_source.py
type OpenAIEmbeddingSource struct {
	provider.BaseProvider
	apiBase string
	apiKey  string
	client  *http.Client
	dim     int
}

// NewOpenAIEmbeddingSource creates an OpenAI embedding provider.
func NewOpenAIEmbeddingSource(config, settings map[string]interface{}) *OpenAIEmbeddingSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &OpenAIEmbeddingSource{
		BaseProvider: bp,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
	s.apiBase = configString(config, "embedding_api_base", configString(config, "api_base", "https://api.openai.com/v1"))
	s.apiBase = strings.TrimSuffix(s.apiBase, "/")
	s.apiKey = configString(config, "embedding_api_key", configString(config, "key", ""))
	s.dim = configInt(config, "embedding_dimensions", 0)
	if m := configString(config, "embedding_model", configString(config, "model", "")); m != "" {
		s.SetModel(m)
	}
	if s.GetModel() == "" {
		s.SetModel("text-embedding-3-small")
	}
	s.SetCapability(provider.CapEmbedding)
	return s
}

// GetEmbedding returns the embedding vector for a single text.
func (s *OpenAIEmbeddingSource) GetEmbedding(ctx context.Context, text string) ([]float32, error) {
	vecs, err := s.embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("embedding API returned no vectors")
	}
	return vecs[0], nil
}

// GetEmbeddings returns embedding vectors for multiple texts.
func (s *OpenAIEmbeddingSource) GetEmbeddings(ctx context.Context, texts []string) ([][]float32, error) {
	return s.embed(ctx, texts)
}

func (s *OpenAIEmbeddingSource) embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body := map[string]interface{}{
		"input": texts,
		"model": s.GetModel(),
	}
	if s.dim > 0 {
		body["dimensions"] = s.dim
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	url := s.apiBase + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("embedding API error %d: %s", resp.StatusCode, string(data))
	}

	var result struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	vecs := make([][]float32, 0, len(result.Data))
	for _, d := range result.Data {
		v := make([]float32, len(d.Embedding))
		for i, f := range d.Embedding {
			v[i] = float32(f)
		}
		vecs = append(vecs, v)
	}
	return vecs, nil
}

// GetDim returns the configured embedding dimension (0 = unknown).
func (s *OpenAIEmbeddingSource) GetDim() int { return s.dim }

// Test verifies the provider by listing models.
func (s *OpenAIEmbeddingSource) Test(ctx context.Context) error {
	url := s.apiBase + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("embedding API error %d", resp.StatusCode)
	}
	return nil
}
