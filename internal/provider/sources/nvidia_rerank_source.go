package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// NvidiaRerankSource reranks documents via the NVIDIA NIM retrieval /reranking
// endpoint.
// Ported from astrbot/core/provider/sources/nvidia_rerank_source.py.
type NvidiaRerankSource struct {
	*provider.BaseProvider
	baseURL       string
	apiKey        string
	model         string
	modelEndpoint string
	truncate      string
	client        *http.Client
}

// NewNvidiaRerankSource creates an NVIDIA rerank provider.
func NewNvidiaRerankSource(config, settings map[string]interface{}) *NvidiaRerankSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &NvidiaRerankSource{
		BaseProvider:  bp,
		model:         configString(config, "nvidia_rerank_model", "nv-rerank-qa-mistral-4b:1"),
		modelEndpoint: configString(config, "nvidia_rerank_model_endpoint", "/reranking"),
		truncate:      configString(config, "nvidia_rerank_truncate", ""),
		client: &http.Client{
			Timeout: time.Duration(configInt(config, "timeout", 20)) * time.Second,
		},
	}
	s.baseURL = strings.TrimSuffix(configString(config, "nvidia_rerank_api_base",
		"https://ai.api.nvidia.com/v1/retrieval"), "/")
	s.apiKey = configString(config, "nvidia_rerank_api_key", "")
	s.SetModel(s.model)
	s.SetCapability(provider.CapRerank)
	return s
}

// getEndpoint builds the full API URL following NVIDIA's per-model URL rules:
// model names without "/" map to the "nvidia" path segment, otherwise dots are
// replaced with underscores.
func (s *NvidiaRerankSource) getEndpoint() string {
	modelPath := "nvidia"
	if strings.Contains(s.model, "/") {
		modelPath = strings.ReplaceAll(strings.Trim(s.model, "/"), ".", "_")
	}
	endpoint := strings.TrimPrefix(s.modelEndpoint, "/")
	return fmt.Sprintf("%s/%s/%s", s.baseURL, modelPath, endpoint)
}

// buildPayload mirrors the Python _build_payload.
func (s *NvidiaRerankSource) buildPayload(query string, documents []string) map[string]interface{} {
	passages := make([]map[string]string, 0, len(documents))
	for _, d := range documents {
		passages = append(passages, map[string]string{"text": d})
	}
	payload := map[string]interface{}{
		"model":    s.model,
		"query":    map[string]string{"text": query},
		"passages": passages,
	}
	if s.truncate != "" {
		payload["truncate"] = s.truncate
	}
	return payload
}

// Rerank reranks the documents and returns the top-N results ordered by score.
func (s *NvidiaRerankSource) Rerank(ctx context.Context, query string, documents []string, topN int) ([]*provider.RerankResult, error) {
	if strings.TrimSpace(query) == "" || len(documents) == 0 {
		return nil, nil
	}

	payload := s.buildPayload(query, documents)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.getEndpoint(), bytes.NewReader(body))
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
		var errData map[string]interface{}
		_ = json.NewDecoder(resp.Body).Decode(&errData)
		detail := "Unknown Error"
		if errData != nil {
			if d, ok := errData["detail"].(string); ok && d != "" {
				detail = d
			} else if m, ok := errData["message"].(string); ok && m != "" {
				detail = m
			}
		}
		return nil, fmt.Errorf("HTTP %d - %s", resp.StatusCode, detail)
	}

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	rankings, _ := data["rankings"].([]interface{})
	results := make([]*provider.RerankResult, 0, len(rankings))
	for i, item := range rankings {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		index := i
		if v, ok := m["index"].(float64); ok {
			index = int(v)
		}
		score := 0.0
		if v, ok := m["relevance_score"].(float64); ok {
			score = v
		} else if v, ok := m["logit"].(float64); ok {
			score = v
		}
		results = append(results, &provider.RerankResult{Index: index, RelevanceScore: score})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].RelevanceScore > results[j].RelevanceScore })
	if topN > 0 && len(results) > topN {
		results = results[:topN]
	}
	return results, nil
}

// Test verifies the provider by issuing a minimal rerank request.
func (s *NvidiaRerankSource) Test(ctx context.Context) error {
	_, err := s.Rerank(ctx, "test", []string{"测试文档"}, 1)
	return err
}
