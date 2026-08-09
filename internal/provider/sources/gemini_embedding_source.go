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

// GeminiEmbeddingSource produces text embeddings via Google Gemini's
// embedContent / batchEmbedContents REST endpoints.
// Ported from astrbot/core/provider/sources/gemini_embedding_source.py
type GeminiEmbeddingSource struct {
	provider.BaseProvider
	apiBase string
	apiKey  string
	client  *http.Client
	dim     int
}

// NewGeminiEmbeddingSource creates a Gemini embedding provider.
func NewGeminiEmbeddingSource(config, settings map[string]interface{}) *GeminiEmbeddingSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &GeminiEmbeddingSource{
		BaseProvider: bp,
		client: &http.Client{
			Timeout: time.Duration(configInt(config, "timeout", 20)) * time.Second,
		},
	}
	s.apiBase = configString(config, "embedding_api_base", configString(config, "api_base", "https://generativelanguage.googleapis.com"))
	s.apiBase = strings.TrimSuffix(s.apiBase, "/")
	s.apiBase = strings.TrimSuffix(s.apiBase, "/v1beta")
	s.apiKey = configString(config, "embedding_api_key", configString(config, "key", ""))
	s.dim = configInt(config, "embedding_dimensions", 768)
	if m := configString(config, "embedding_model", configString(config, "model", "")); m != "" {
		s.SetModel(m)
	}
	if s.GetModel() == "" {
		s.SetModel("gemini-embedding-exp-03-07")
	}
	s.SetCapability(provider.CapEmbedding)
	return s
}

// GetEmbedding returns the embedding vector for a single text.
func (s *GeminiEmbeddingSource) GetEmbedding(ctx context.Context, text string) ([]float32, error) {
	model := s.GetModel()
	url := fmt.Sprintf("%s/v1beta/models/%s:embedContent?key=%s", s.apiBase, model, s.apiKey)
	body := map[string]interface{}{
		"model":   model,
		"content": map[string]interface{}{"parts": []map[string]interface{}{{"text": text}}},
	}
	if s.dim > 0 {
		body["outputDimensionality"] = s.dim
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("gemini embedding API error %d: %s", resp.StatusCode, string(data))
	}

	var result struct {
		Embedding struct {
			Values []float64 `json:"values"`
		} `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	vec := make([]float32, len(result.Embedding.Values))
	for i, v := range result.Embedding.Values {
		vec[i] = float32(v)
	}
	return vec, nil
}

// GetEmbeddings returns embedding vectors for multiple texts.
func (s *GeminiEmbeddingSource) GetEmbeddings(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	model := s.GetModel()
	url := fmt.Sprintf("%s/v1beta/models/%s:batchEmbedContents?key=%s", s.apiBase, model, s.apiKey)
	requests := make([]map[string]interface{}, 0, len(texts))
	for _, t := range texts {
		r := map[string]interface{}{
			"model":   model,
			"content": map[string]interface{}{"parts": []map[string]interface{}{{"text": t}}},
		}
		if s.dim > 0 {
			r["outputDimensionality"] = s.dim
		}
		requests = append(requests, r)
	}
	payload, err := json.Marshal(map[string]interface{}{"requests": requests})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("gemini embedding API error %d: %s", resp.StatusCode, string(data))
	}

	var result struct {
		Embeddings []struct {
			Values []float64 `json:"values"`
		} `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	vecs := make([][]float32, 0, len(result.Embeddings))
	for _, e := range result.Embeddings {
		v := make([]float32, len(e.Values))
		for i, f := range e.Values {
			v[i] = float32(f)
		}
		vecs = append(vecs, v)
	}
	return vecs, nil
}

// GetDim returns the configured embedding dimension (default 768).
func (s *GeminiEmbeddingSource) GetDim() int { return s.dim }

// Test verifies the provider by embedding a probe string.
func (s *GeminiEmbeddingSource) Test(ctx context.Context) error {
	_, err := s.GetEmbedding(ctx, "test")
	return err
}
