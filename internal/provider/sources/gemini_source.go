// Package sources - Google Gemini chat provider.
// Ported from astrbot/core/provider/sources/gemini_source.py
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

// GeminiSource is a Google Gemini chat provider.
type GeminiSource struct {
	provider.BaseProvider
	apiBase string
	apiKey  string
	client  *http.Client
}

// NewGeminiSource creates a Gemini provider.
func NewGeminiSource(config, settings map[string]interface{}) *GeminiSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &GeminiSource{
		BaseProvider: bp,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
	s.apiBase, _ = config["api_base"].(string)
	if s.apiBase == "" {
		s.apiBase = "https://generativelanguage.googleapis.com/v1beta"
	}
	if key, ok := config["key"].(string); ok {
		s.apiKey = key
	}
	if keys, ok := config["key"].([]interface{}); ok && len(keys) > 0 {
		if k, ok := keys[0].(string); ok {
			s.apiKey = k
		}
	}
	return s
}

// doRequest sends an HTTP request with retry logic.
func (s *GeminiSource) doRequest(ctx context.Context, req *http.Request) (*http.Response, error) {
	cfg := RetryConfigFromSettings(s.Settings())
	return DoWithRetry(ctx, s.client, func() (*http.Request, error) {
		// Clone the request to get a fresh body for each retry attempt.
		clone := req.Clone(ctx)
		return clone, nil
	}, cfg, "Gemini")
}

// TextChat sends a non-streaming chat request.
func (s *GeminiSource) TextChat(ctx context.Context, req *provider.ProviderRequest) (*provider.LLMResponse, error) {
	model := s.GetModel()
	if model == "" {
		model = "gemini-pro"
	}
	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", s.apiBase, model, s.apiKey)
	body := s.buildRequestBody(req, false)
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	contents, _ := body["contents"].([]map[string]interface{})
	logger.Debug("LLM request: url=%s model=%s messages=%d", url, model, len(contents))
	resp, err := s.doRequest(ctx, httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return &provider.LLMResponse{
			Role:           "err",
			CompletionText: fmt.Sprintf("API error %d: %s", resp.StatusCode, string(respBody)),
		}, nil
	}
	var result struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	var text string
	for _, cand := range result.Candidates {
		for _, part := range cand.Content.Parts {
			text += part.Text
		}
	}
	logger.Debug("LLM response: text_len=%d", len(text))
	return &provider.LLMResponse{
		Role:           "assistant",
		CompletionText: text,
		Usage: &provider.TokenUsage{
			InputOther: result.UsageMetadata.PromptTokenCount,
			Output:     result.UsageMetadata.CandidatesTokenCount,
		},
	}, nil
}

// TextChatStream sends a streaming chat request.
func (s *GeminiSource) TextChatStream(ctx context.Context, req *provider.ProviderRequest) (<-chan *provider.LLMResponse, error) {
	model := s.GetModel()
	if model == "" {
		model = "gemini-pro"
	}
	url := fmt.Sprintf("%s/models/%s:streamGenerateContent?key=%s&alt=sse", s.apiBase, model, s.apiKey)
	body := s.buildRequestBody(req, true)
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	contents, _ := body["contents"].([]map[string]interface{})
	logger.Debug("LLM request: url=%s model=%s messages=%d", url, model, len(contents))
	resp, err := s.doRequest(ctx, httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}
	ch := make(chan *provider.LLMResponse, 100)
	go func() {
		defer resp.Body.Close()
		defer close(ch)
		var usage *provider.TokenUsage
		var content strings.Builder
		reader := newSSEReader(ctx, resp, func(data string) (stop bool) {
			var chunk struct {
				Candidates []struct {
					Content struct {
						Parts []struct {
							Text string `json:"text"`
						} `json:"parts"`
					} `json:"content"`
				} `json:"candidates"`
				UsageMetadata *struct {
					PromptTokenCount     int `json:"promptTokenCount"`
					CandidatesTokenCount int `json:"candidatesTokenCount"`
				} `json:"usageMetadata"`
			}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				return false
			}
			if chunk.UsageMetadata != nil {
				usage = &provider.TokenUsage{
					InputOther: chunk.UsageMetadata.PromptTokenCount,
					Output:     chunk.UsageMetadata.CandidatesTokenCount,
				}
			}
			for _, cand := range chunk.Candidates {
				for _, part := range cand.Content.Parts {
					if part.Text != "" {
						content.WriteString(part.Text)
						ch <- &provider.LLMResponse{
							Role:           "assistant",
							IsChunk:        true,
							CompletionText: part.Text,
						}
					}
				}
			}
			return false
		})
		_ = reader.scan()
		// Emit a final consolidated chunk so token usage reaches the pipeline.
		if usage != nil || content.Len() > 0 {
			ch <- &provider.LLMResponse{
				Role:           "assistant",
				CompletionText: content.String(),
				Usage:          usage,
			}
		}
		if usage != nil {
			logger.Debug("LLM stream done, usage=%v", usage)
		}
	}()
	logger.Debug("LLM stream started: model=%s", model)
	return ch, nil
}

// Test verifies the provider.
func (s *GeminiSource) Test(ctx context.Context) error {
	req := &provider.ProviderRequest{
		Prompt:    "REPLY `PONG` ONLY",
		SessionID: "test",
	}
	resp, err := s.TextChat(ctx, req)
	if err != nil {
		return err
	}
	if resp.Role == "err" {
		return fmt.Errorf("provider test failed: %s", resp.CompletionText)
	}
	return nil
}

func (s *GeminiSource) buildRequestBody(req *provider.ProviderRequest, stream bool) map[string]interface{} {
	contents := []map[string]interface{}{}
	for _, msg := range req.Contexts {
		role, _ := msg["role"].(string)
		geminiRole := "user"
		if role == "assistant" {
			geminiRole = "model"
		}
		content, _ := msg["content"].(string)
		contents = append(contents, map[string]interface{}{
			"role": geminiRole,
			"parts": []map[string]interface{}{
				{"text": content},
			},
		})
	}
	// Add current user message
	contents = append(contents, map[string]interface{}{
		"role": "user",
		"parts": []map[string]interface{}{
			{"text": req.Prompt},
		},
	})
	body := map[string]interface{}{
		"contents": contents,
	}
	if req.SystemPrompt != "" {
		body["systemInstruction"] = map[string]interface{}{
			"parts": []map[string]interface{}{
				{"text": req.SystemPrompt},
			},
		}
	}
	return body
}
