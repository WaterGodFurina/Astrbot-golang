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

// XinferenceRerankSource reranks documents via a Xinference /v1/rerank
// endpoint.
// Ported from astrbot/core/provider/sources/xinference_rerank_source.py.
// The Python provider used the xinference_client SDK which posts to
// {base}/v1/rerank with {"model", "query", "documents", "top_n"} and parses
// response["results"]. The SDK's list_models/launch_model bootstrap is not
// ported: the configured model (rerank_model) is sent directly as the model
// UID, so the model must already be running on the server.
type XinferenceRerankSource struct {
	*provider.BaseProvider
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// NewXinferenceRerankSource creates a Xinference rerank provider.
func NewXinferenceRerankSource(config, settings map[string]interface{}) *XinferenceRerankSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &XinferenceRerankSource{
		BaseProvider: bp,
		model:        configString(config, "rerank_model", "BAAI/bge-reranker-base"),
		client: &http.Client{
			Timeout: time.Duration(configInt(config, "timeout", 20)) * time.Second,
		},
	}
	s.baseURL = strings.TrimSuffix(configString(config, "rerank_api_base", "http://127.0.0.1:8000"), "/")
	s.apiKey = configString(config, "rerank_api_key", "")
	s.SetModel(s.model)
	s.SetCapability(provider.CapRerank)
	return s
}

// Rerank reranks the documents and returns the top-N results ordered by score.
func (s *XinferenceRerankSource) Rerank(ctx context.Context, query string, documents []string, topN int) ([]*provider.RerankResult, error) {
	if strings.TrimSpace(query) == "" || len(documents) == 0 {
		return nil, nil
	}

	payload := map[string]interface{}{
		"model":     s.model,
		"query":     query,
		"documents": documents,
	}
	if topN > 0 {
		payload["top_n"] = topN
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/v1/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
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
		return nil, fmt.Errorf("rerank API error %d: %s", resp.StatusCode, string(data))
	}

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	resultsRaw, _ := data["results"].([]interface{})
	results := make([]*provider.RerankResult, 0, len(resultsRaw))
	for i, item := range resultsRaw {
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
		}
		results = append(results, &provider.RerankResult{Index: index, RelevanceScore: score})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].RelevanceScore > results[j].RelevanceScore })
	if topN > 0 && len(results) > topN {
		results = results[:topN]
	}
	return results, nil
}
