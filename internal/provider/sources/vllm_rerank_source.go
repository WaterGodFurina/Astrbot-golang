package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// VLLMRerankSource reranks documents via a vLLM /rerank endpoint.
// Ported from astrbot/core/provider/sources/vllm_rerank_source.py.
type VLLMRerankSource struct {
	*provider.BaseProvider
	baseURL   string
	apiSuffix string
	authKey   string
	model     string
	client    *http.Client
}

// NewVLLMRerankSource creates a vLLM rerank provider.
func NewVLLMRerankSource(config, settings map[string]interface{}) *VLLMRerankSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &VLLMRerankSource{
		BaseProvider: bp,
		model:        configString(config, "rerank_model", "BAAI/bge-reranker-base"),
		client: &http.Client{
			Timeout: time.Duration(configInt(config, "timeout", 20)) * time.Second,
		},
	}
	s.baseURL = strings.TrimSuffix(configString(config, "rerank_api_base", "http://127.0.0.1:8000"), "/")
	s.apiSuffix = configString(config, "rerank_api_suffix", "/v1/rerank")
	if s.apiSuffix == "" {
		s.apiSuffix = "/v1/rerank"
	}
	if !strings.HasPrefix(s.apiSuffix, "/") {
		s.apiSuffix = "/" + s.apiSuffix
	}
	s.authKey = configString(config, "rerank_api_key", "")
	s.SetModel(s.model)
	s.SetCapability(provider.CapRerank)
	return s
}

// Rerank reranks the documents and returns the top-N results ordered by score.
func (s *VLLMRerankSource) Rerank(ctx context.Context, query string, documents []string, topN int) ([]*provider.RerankResult, error) {
	if len(documents) == 0 {
		return nil, nil
	}

	payload := map[string]interface{}{
		"query":     query,
		"documents": documents,
		"model":     s.model,
	}
	if topN > 0 {
		payload["top_n"] = topN
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+s.apiSuffix, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.authKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.authKey)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("rerank API error %d: %s", resp.StatusCode, string(data))
	}

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	resultsRaw, ok := data["results"].([]interface{})
	if !ok || len(resultsRaw) == 0 {
		return nil, fmt.Errorf("rerank API response must contain a non-empty 'results' list")
	}

	results := make([]*provider.RerankResult, 0, len(resultsRaw))
	for _, item := range resultsRaw {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("rerank API returned invalid result data")
		}
		idx, okIndex := m["index"].(float64)
		score, okScore := m["relevance_score"].(float64)
		if !okIndex || !okScore {
			return nil, fmt.Errorf("rerank API returned invalid result data")
		}
		results = append(results, &provider.RerankResult{Index: int(idx), RelevanceScore: score})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].RelevanceScore > results[j].RelevanceScore })
	if topN > 0 && len(results) > topN {
		results = results[:topN]
	}
	return results, nil
}

// Test verifies the provider by issuing a minimal rerank request.
func (s *VLLMRerankSource) Test(ctx context.Context) error {
	_, err := s.Rerank(ctx, "test", []string{"测试文档"}, 1)
	return err
}
