package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

const qwen3RerankModel = "qwen3-rerank"

// bailianCompatibleAPIPathSuffixes are the DashScope OpenAI-compatible rerank
// endpoint suffixes detected to switch request/response payload shapes.
var bailianCompatibleAPIPathSuffixes = []string{
	"/compatible-api/v1/reranks",
	"/compatible-mode/v1/reranks",
}

// BailianRerankSource reranks documents via the Aliyun Bailian (DashScope)
// text-rerank service.
// Ported from astrbot/core/provider/sources/bailian_rerank_source.py.
// NOTE: the Python implementation only uses the non-streaming JSON endpoint,
// so no SSE streaming is needed here.
type BailianRerankSource struct {
	provider.BaseProvider
	baseURL         string
	apiKey          string
	model           string
	returnDocuments bool
	instruct        string
	client          *http.Client
}

// NewBailianRerankSource creates a Bailian rerank provider.
func NewBailianRerankSource(config, settings map[string]interface{}) *BailianRerankSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &BailianRerankSource{
		BaseProvider: bp,
		model:        configString(config, "rerank_model", qwen3RerankModel),
		instruct:     configString(config, "instruct", ""),
		client: &http.Client{
			Timeout: time.Duration(configInt(config, "timeout", 30)) * time.Second,
		},
	}
	if v, ok := config["return_documents"].(bool); ok {
		s.returnDocuments = v
	}
	s.baseURL = configString(config, "rerank_api_base",
		"https://dashscope.aliyuncs.com/api/v1/services/rerank/text-rerank/text-rerank")
	s.apiKey = configString(config, "rerank_api_key", os.Getenv("DASHSCOPE_API_KEY"))
	s.SetModel(s.model)
	s.SetCapability(provider.CapRerank)
	return s
}

// usesCompatibleAPI reports whether the base URL points at a DashScope
// OpenAI-compatible rerank endpoint.
func (s *BailianRerankSource) usesCompatibleAPI() bool {
	u, err := url.Parse(s.baseURL)
	if err != nil {
		return false
	}
	path := strings.TrimSuffix(u.Path, "/")
	for _, suffix := range bailianCompatibleAPIPathSuffixes {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

// buildPayload mirrors the Python _build_payload: qwen3-rerank on the
// compatible API uses a flat OpenAI-style body, everything else uses the
// DashScope input/parameters shape.
func (s *BailianRerankSource) buildPayload(query string, documents []string, topN int) map[string]interface{} {
	normalizedTopN := 0
	if topN > 0 {
		normalizedTopN = topN
	}
	model := strings.ToLower(strings.TrimSpace(s.model))
	isCompatible := s.usesCompatibleAPI()

	if model == qwen3RerankModel && isCompatible {
		payload := map[string]interface{}{
			"model":     s.model,
			"query":     query,
			"documents": documents,
		}
		if normalizedTopN > 0 {
			payload["top_n"] = normalizedTopN
		}
		if s.instruct != "" {
			payload["instruct"] = s.instruct
		}
		return payload
	}

	params := make(map[string]interface{})
	if normalizedTopN > 0 {
		params["top_n"] = normalizedTopN
	}
	if s.returnDocuments {
		params["return_documents"] = true
	}
	if s.instruct != "" && model == qwen3RerankModel {
		params["instruct"] = s.instruct
	}
	base := map[string]interface{}{
		"model": s.model,
		"input": map[string]interface{}{
			"query":     query,
			"documents": documents,
		},
	}
	if len(params) > 0 {
		base["parameters"] = params
	}
	return base
}

// parseResults mirrors the Python _parse_results for both API shapes.
func (s *BailianRerankSource) parseResults(data map[string]interface{}) ([]*provider.RerankResult, error) {
	var results []interface{}
	if s.usesCompatibleAPI() {
		if v, ok := data["code"]; ok {
			switch c := v.(type) {
			case string:
				if c != "" {
					return nil, fmt.Errorf("bailian API error: %s - %v", c, data["message"])
				}
			case float64:
				if c != 0 {
					return nil, fmt.Errorf("bailian API error: %v - %v", c, data["message"])
				}
			}
		}
		results, _ = data["results"].([]interface{})
	} else {
		code, _ := data["code"].(string)
		if code == "" {
			code = "200"
		}
		if code != "200" {
			return nil, fmt.Errorf("bailian API error: %s - %v", code, data["message"])
		}
		output, _ := data["output"].(map[string]interface{})
		results, _ = output["results"].([]interface{})
	}

	out := make([]*provider.RerankResult, 0, len(results))
	for i, item := range results {
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
		out = append(out, &provider.RerankResult{Index: index, RelevanceScore: score})
	}
	return out, nil
}

// Rerank reranks the documents and returns the top-N results ordered by score.
func (s *BailianRerankSource) Rerank(ctx context.Context, query string, documents []string, topN int) ([]*provider.RerankResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if len(documents) == 0 {
		return nil, nil
	}
	if len(documents) > 500 {
		documents = documents[:500]
	}
	if s.apiKey == "" {
		return nil, fmt.Errorf("bailian API key is empty")
	}

	payload := s.buildPayload(query, documents, topN)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL, bytes.NewReader(body))
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
		return nil, fmt.Errorf("bailian rerank API error %d: %s", resp.StatusCode, string(data))
	}

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	results, err := s.parseResults(data)
	if err != nil {
		return nil, err
	}
	sort.Slice(results, func(i, j int) bool { return results[i].RelevanceScore > results[j].RelevanceScore })
	if topN > 0 && len(results) > topN {
		results = results[:topN]
	}
	return results, nil
}
