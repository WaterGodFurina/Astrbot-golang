package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// OpenAIEmbeddingSource produces text embeddings via the OpenAI-compatible
// /embeddings endpoint.
// Ported from astrbot/core/provider/sources/openai_embedding_source.py
type OpenAIEmbeddingSource struct {
	*provider.BaseProvider
	apiBase string
	apiKey  string
	client  *http.Client
	dim     int
	// dimensionsMode 控制是否发送 dimensions 参数（对齐 py
	// embedding_dimensions_mode：auto/always/never，默认 auto）。
	dimensionsMode string
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
	s.apiKey = configKey(config, "embedding_api_key")
	if s.apiKey == "" {
		s.apiKey = configKey(config, "key")
	}
	s.dim = configInt(config, "embedding_dimensions", 0)
	// 对齐 py embedding_dimensions_mode：非 auto/always/never 时告警并回退 auto。
	s.dimensionsMode = configString(config, "embedding_dimensions_mode", "auto")
	switch s.dimensionsMode {
	case "auto", "always", "never":
	default:
		logger.Warn("未知的 embedding_dimensions_mode: %q，回退 auto", s.dimensionsMode)
		s.dimensionsMode = "auto"
	}
	if m := configString(config, "embedding_model", configString(config, "model", "")); m != "" {
		s.SetModel(m)
	}
	if s.GetModel() == "" {
		s.SetModel("text-embedding-3-small")
	}
	s.SetCapability(provider.CapEmbedding)
	return s
}

// shouldSendDimensions 判定当前端点/模型是否应携带 dimensions 参数
// （对齐 py _embedding_kwargs 的 auto 门控：仅官方 OpenAI text-embedding-3
// 与硅基流动 qwen 等明确支持的组合；always/never 直接决定）。
func (s *OpenAIEmbeddingSource) shouldSendDimensions() bool {
	switch s.dimensionsMode {
	case "always":
		return true
	case "never":
		return false
	}
	u, err := url.Parse(s.apiBase)
	if err != nil || u.Scheme != "https" {
		return false
	}
	model := strings.ToLower(s.GetModel())
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	host := u.Hostname()
	path := strings.TrimSuffix(u.Path, "/")
	if host == "api.openai.com" && path == "/v1" && strings.HasPrefix(model, "text-embedding-3") {
		return true
	}
	if host == "api.siliconflow.cn" && strings.HasPrefix(model, "qwen") {
		return true
	}
	return false
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
	if s.dim > 0 && s.shouldSendDimensions() {
		body["dimensions"] = s.dim
	}
	payloadBytes, _ := json.Marshal(body)

	url := s.apiBase + "/embeddings"
	cfg := RetryConfigFromSettings(s.Settings())
	resp, err := DoWithRetry(ctx, s.client, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payloadBytes))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
		return req, nil
	}, cfg, "Embedding")
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
