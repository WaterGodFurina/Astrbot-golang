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

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// NvidiaEmbeddingSource produces text embeddings via NVIDIA's OpenAI
// compatible /embeddings endpoint.
// Ported from astrbot/core/provider/sources/nvidia_embedding_source.py
type NvidiaEmbeddingSource struct {
	*provider.BaseProvider
	apiBase   string
	apiKey    string
	model     string
	inputType string
	client    *http.Client
	dim       int
}

// NewNvidiaEmbeddingSource creates an NVIDIA embedding provider.
func NewNvidiaEmbeddingSource(config, settings map[string]interface{}) *NvidiaEmbeddingSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &NvidiaEmbeddingSource{
		BaseProvider: bp,
		client: &http.Client{
			Timeout: time.Duration(configInt(config, "timeout", 20)) * time.Second,
		},
	}
	s.apiBase = configString(config, "embedding_api_base", "https://integrate.api.nvidia.com/v1")
	s.apiBase = strings.TrimSuffix(s.apiBase, "/")
	s.apiBase = strings.TrimSuffix(s.apiBase, "/embeddings")
	s.apiKey = configString(config, "embedding_api_key", "")
	s.model = configString(config, "embedding_model", "nvidia/nemotron-3-embed-1b")
	s.inputType = configString(config, "input_type", "")
	s.dim = configInt(config, "embedding_dimensions", 0)
	s.SetModel(s.model)
	s.SetCapability(provider.CapEmbedding)
	return s
}

// GetEmbedding returns the embedding vector for a single text.
func (s *NvidiaEmbeddingSource) GetEmbedding(ctx context.Context, text string) ([]float32, error) {
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
func (s *NvidiaEmbeddingSource) GetEmbeddings(ctx context.Context, texts []string) ([][]float32, error) {
	return s.embed(ctx, texts)
}

func (s *NvidiaEmbeddingSource) embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	payload := map[string]interface{}{
		"input":           texts,
		"model":           s.model,
		"encoding_format": "float",
	}
	if s.inputType != "" {
		payload["input_type"] = s.inputType
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	url := s.apiBase + "/embeddings"
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
		return nil, fmt.Errorf("nvidia embedding API error %d: %s", resp.StatusCode, string(data))
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
func (s *NvidiaEmbeddingSource) GetDim() int { return s.dim }

// Test verifies the provider by embedding a probe string.
func (s *NvidiaEmbeddingSource) Test(ctx context.Context) error {
	_, err := s.GetEmbedding(ctx, "test")
	return err
}
