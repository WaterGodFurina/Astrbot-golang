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

// TEIRerankSource reranks documents against a query via a HuggingFace
// Text Embeddings Inference (TEI) compatible /rerank endpoint.
// Ported from astrbot/core/provider/sources/tei_rerank_source.py
type TEIRerankSource struct {
	*provider.BaseProvider
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewTEIRerankSource creates a TEI rerank provider.
func NewTEIRerankSource(config, settings map[string]interface{}) *TEIRerankSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &TEIRerankSource{
		BaseProvider: bp,
		client: &http.Client{
			Timeout: time.Duration(configInt(config, "timeout", 20)) * time.Second,
		},
	}
	s.baseURL = configString(config, "rerank_api_base", "http://127.0.0.1:8080")
	s.baseURL = strings.TrimSuffix(s.baseURL, "/")
	s.apiKey = configString(config, "rerank_api_key", "")
	if m := configString(config, "model", ""); m != "" {
		s.SetModel(m)
	}
	s.SetCapability(provider.CapRerank)
	return s
}

// Rerank reranks the documents and returns the top-N results ordered by score.
func (s *TEIRerankSource) Rerank(ctx context.Context, query string, documents []string, topN int) ([]*provider.RerankResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("query is empty")
	}
	if len(documents) == 0 {
		return nil, nil
	}
	payload := map[string]interface{}{
		"query": query,
		"texts": documents,
	}
	if topN > 0 {
		payload["top_n"] = topN
	}
	bodyBytes, _ := json.Marshal(payload)

	url := s.baseURL + "/rerank"
	cfg := RetryConfigFromSettings(s.Settings())
	resp, err := DoWithRetry(ctx, s.client, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if s.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+s.apiKey)
		}
		return req, nil
	}, cfg, "Rerank-TEI")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("rerank API error %d: %s", resp.StatusCode, string(data))
	}

	var ranked []struct {
		Index int     `json:"index"`
		Score float64 `json:"score"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ranked); err != nil {
		return nil, err
	}
	results := make([]*provider.RerankResult, 0, len(ranked))
	for _, r := range ranked {
		results = append(results, &provider.RerankResult{
			Index:          r.Index,
			RelevanceScore: r.Score,
		})
	}
	return results, nil
}

// Test verifies the provider is reachable.
func (s *TEIRerankSource) Test(ctx context.Context) error {
	url := s.baseURL + "/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rerank health check failed: HTTP %d", resp.StatusCode)
	}
	return nil
}
