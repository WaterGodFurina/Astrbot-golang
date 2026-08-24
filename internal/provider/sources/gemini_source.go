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
	*provider.BaseProvider
	apiBase      string
	apiKey       string
	client       *http.Client
	streamClient *http.Client
}

// NewGeminiSource creates a Gemini provider.
func NewGeminiSource(config, settings map[string]interface{}) *GeminiSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &GeminiSource{
		BaseProvider: bp,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
		streamClient: newStreamClient(),
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

// doRequest sends an HTTP request with retry logic. jsonBody is re-serialized
// into a fresh bytes.Reader per retry attempt so retries carry a full body.
func (s *GeminiSource) doRequest(ctx context.Context, client *http.Client, url string, jsonBody []byte) (*http.Response, error) {
	cfg := RetryConfigFromSettings(s.Settings())
	return DoWithRetry(ctx, client, func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("x-goog-api-key", s.apiKey)
		return httpReq, nil
	}, cfg, "Gemini")
}

// TextChat sends a non-streaming chat request.
func (s *GeminiSource) TextChat(ctx context.Context, req *provider.ProviderRequest) (*provider.LLMResponse, error) {
	model := s.GetModel()
	if model == "" {
		model = "gemini-pro"
	}
	if !geminiModelPathSafe(model) {
		return nil, fmt.Errorf("invalid gemini model name: %q", model)
	}
	url := fmt.Sprintf("%s/models/%s:generateContent", s.apiBase, model)
	body := s.buildRequestBody(req, false)
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	contents, _ := body["contents"].([]map[string]interface{})
	logger.Debug("LLM request: url=%s model=%s messages=%d", stripURLQuery(url), model, len(contents))
	resp, err := s.doRequest(ctx, s.client, url, jsonBody)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &provider.LLMResponse{
			Role:           "err",
			CompletionText: fmt.Sprintf("API error %d: %s", resp.StatusCode, truncate(string(respBody), 1024)),
		}, nil
	}
	var result struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string `json:"text"`
					FunctionCall *struct {
						Name string                 `json:"name"`
						Args map[string]interface{} `json:"args"`
					} `json:"functionCall"`
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
	var toolCalls []struct {
		name string
		args map[string]interface{}
	}
	for _, cand := range result.Candidates {
		for _, part := range cand.Content.Parts {
			if part.FunctionCall != nil {
				args := part.FunctionCall.Args
				if args == nil {
					args = map[string]interface{}{}
				}
				toolCalls = append(toolCalls, struct {
					name string
					args map[string]interface{}
				}{name: part.FunctionCall.Name, args: args})
				continue
			}
			text += part.Text
		}
	}
	logger.Debug("LLM response: text_len=%d", len(text))
	llmResp := &provider.LLMResponse{
		Role:           "assistant",
		CompletionText: text,
		Usage: &provider.TokenUsage{
			InputOther: result.UsageMetadata.PromptTokenCount,
			Output:     result.UsageMetadata.CandidatesTokenCount,
		},
	}
	for _, tc := range toolCalls {
		llmResp.ToolsCallName = append(llmResp.ToolsCallName, tc.name)
		llmResp.ToolsCallArgs = append(llmResp.ToolsCallArgs, tc.args)
		llmResp.ToolsCallIDs = append(llmResp.ToolsCallIDs, tc.name) // Gemini 无 call id，用 name 占位
	}
	if len(llmResp.ToolsCallArgs) > 0 {
		llmResp.Role = "tool"
	}
	return llmResp, nil
}

// TextChatStream sends a streaming chat request.
func (s *GeminiSource) TextChatStream(ctx context.Context, req *provider.ProviderRequest) (<-chan *provider.LLMResponse, error) {
	model := s.GetModel()
	if model == "" {
		model = "gemini-pro"
	}
	if !geminiModelPathSafe(model) {
		return nil, fmt.Errorf("invalid gemini model name: %q", model)
	}
	url := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse", s.apiBase, model)
	body := s.buildRequestBody(req, true)
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	contents, _ := body["contents"].([]map[string]interface{})
	logger.Debug("LLM request: url=%s model=%s messages=%d", stripURLQuery(url), model, len(contents))
	resp, err := s.doRequest(ctx, s.streamClient, url, jsonBody)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, truncate(string(respBody), 1024))
	}
	ch := make(chan *provider.LLMResponse, 100)
	go func() {
		defer resp.Body.Close()
		defer close(ch)
		var usage *provider.TokenUsage
		var content strings.Builder
		var toolCalls []struct {
			name string
			args map[string]interface{}
		}
		reader := newSSEReader(ctx, resp, func(data string) (stop bool) {
			var chunk struct {
				Candidates []struct {
					Content struct {
						Parts []struct {
							Text         string `json:"text"`
							FunctionCall *struct {
								Name string                 `json:"name"`
								Args map[string]interface{} `json:"args"`
							} `json:"functionCall"`
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
					if part.FunctionCall != nil {
						args := part.FunctionCall.Args
						if args == nil {
							args = map[string]interface{}{}
						}
						toolCalls = append(toolCalls, struct {
							name string
							args map[string]interface{}
						}{name: part.FunctionCall.Name, args: args})
						continue
					}
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
		if err := reader.scan(); err != nil {
			logger.Warn("Gemini stream read error: %v", err)
			ch <- &provider.LLMResponse{Role: "err", CompletionText: fmt.Sprintf("Gemini stream read error: %v", err)}
			return
		}
		// Emit a final consolidated chunk so token usage reaches the pipeline.
		if usage != nil || content.Len() > 0 || len(toolCalls) > 0 {
			final := &provider.LLMResponse{
				Role:           "assistant",
				CompletionText: content.String(),
				Usage:          usage,
			}
			for _, tc := range toolCalls {
				final.ToolsCallName = append(final.ToolsCallName, tc.name)
				final.ToolsCallArgs = append(final.ToolsCallArgs, tc.args)
				final.ToolsCallIDs = append(final.ToolsCallIDs, tc.name) // Gemini 无 call id，用 name 占位
			}
			if len(final.ToolsCallArgs) > 0 {
				final.Role = "tool"
			}
			ch <- final
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
		// Convert array content blocks (text / image_url / audio_url) into
		// Gemini parts; string content becomes a single text part.
		parts := geminiPartsFromContent(msg["content"])
		if len(parts) == 0 {
			continue
		}
		contents = append(contents, map[string]interface{}{
			"role":  geminiRole,
			"parts": parts,
		})
	}
	// Add current user message with its inline media parts.
	parts := geminiPartsFromContent(req.Prompt)
	for _, imgURL := range req.ImageURLs {
		if part := imageToGeminiPart(imgURL); part != nil {
			parts = append(parts, part)
		}
	}
	for _, audioURL := range req.AudioURLs {
		if part := geminiMediaPart(audioURL); part != nil {
			parts = append(parts, part)
		}
	}
	contents = append(contents, map[string]interface{}{
		"role":  "user",
		"parts": parts,
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
	// Convert OpenAI function schemas to Gemini functionDeclarations.
	if len(req.Tools) > 0 {
		funcDecls := []map[string]interface{}{}
		for _, t := range req.Tools {
			fn, _ := t["function"].(map[string]interface{})
			if fn == nil {
				continue
			}
			name, _ := fn["name"].(string)
			if name == "" {
				continue
			}
			decl := map[string]interface{}{"name": name}
			if desc, ok := fn["description"].(string); ok && desc != "" {
				decl["description"] = desc
			}
			if params, ok := fn["parameters"].(map[string]interface{}); ok {
				decl["parameters"] = params
			}
			funcDecls = append(funcDecls, decl)
		}
		if len(funcDecls) > 0 {
			body["tools"] = []map[string]interface{}{
				{"functionDeclarations": funcDecls},
			}
		}
	}
	return body
}
