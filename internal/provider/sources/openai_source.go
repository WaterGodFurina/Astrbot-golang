// Package sources implements concrete LLM provider sources.
// Ported from astrbot/core/provider/sources/
package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

// OpenAISource is an OpenAI-compatible chat provider.
// Ported from astrbot/core/provider/sources/openai_source.py
type OpenAISource struct {
	provider.BaseProvider
	apiBase string
	apiKey  string
	client  *http.Client
}

// NewOpenAISource creates an OpenAI provider.
func NewOpenAISource(config, settings map[string]interface{}) *OpenAISource {
	bp := provider.NewBaseProvider(config, settings)
	s := &OpenAISource{
		BaseProvider: bp,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
	s.apiBase, _ = config["api_base"].(string)
	if s.apiBase == "" {
		s.apiBase = "https://api.openai.com/v1"
	}
	keys := s.getKeys()
	if len(keys) > 0 {
		s.apiKey = keys[0]
	}
	return s
}

func (s *OpenAISource) getKeys() []string {
	keysRaw, _ := s.Config()["key"].([]interface{})
	if len(keysRaw) == 0 {
		return []string{""}
	}
	keys := make([]string, 0, len(keysRaw))
	for _, k := range keysRaw {
		if str, ok := k.(string); ok && str != "" {
			keys = append(keys, str)
		}
	}
	if len(keys) == 0 {
		keys = append(keys, "")
	}
	return keys
}

// GetCurrentKey returns the current API key.
func (s *OpenAISource) GetCurrentKey() string {
	return s.apiKey
}

// SetKey sets the API key.
func (s *OpenAISource) SetKey(key string) {
	s.apiKey = key
}

// GetModels returns available models.
func (s *OpenAISource) GetModels(ctx context.Context) ([]string, error) {
	url := fmt.Sprintf("%s/models", s.apiBase)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		models = append(models, m.ID)
	}
	return models, nil
}

// TextChat sends a non-streaming chat request.
func (s *OpenAISource) TextChat(ctx context.Context, req *provider.ProviderRequest) (*provider.LLMResponse, error) {
	body := s.buildRequestBody(req, false)
	resp, err := s.doRequest(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return &provider.LLMResponse{
			Role:           "err",
			CompletionText: fmt.Sprintf("API error %d: %s", resp.StatusCode, string(respBody)),
		}, nil
	}
	var result struct {
		Choices []struct {
			Message struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(result.Choices) == 0 {
		return &provider.LLMResponse{Role: "assistant", CompletionText: ""}, nil
	}
	choice := result.Choices[0]
	llmResp := &provider.LLMResponse{
		Role:           choice.Message.Role,
		CompletionText: choice.Message.Content,
		Usage: &provider.TokenUsage{
			InputOther: result.Usage.PromptTokens,
			Output:     result.Usage.CompletionTokens,
		},
	}
	for _, tc := range choice.Message.ToolCalls {
		llmResp.ToolsCallName = append(llmResp.ToolsCallName, tc.Function.Name)
		var argsMap map[string]interface{}
		if tc.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &argsMap)
		}
		if argsMap == nil {
			argsMap = make(map[string]interface{})
		}
		llmResp.ToolsCallArgs = append(llmResp.ToolsCallArgs, argsMap)
		llmResp.ToolsCallIDs = append(llmResp.ToolsCallIDs, tc.ID)
	}
	return llmResp, nil
}

// TextChatStream sends a streaming chat request.
func (s *OpenAISource) TextChatStream(ctx context.Context, req *provider.ProviderRequest) (<-chan *provider.LLMResponse, error) {
	body := s.buildRequestBody(req, true)
	resp, err := s.doRequest(ctx, body)
	if err != nil {
		return nil, err
	}
	ch := make(chan *provider.LLMResponse, 100)
	go func() {
		defer resp.Body.Close()
		defer close(ch)
		decoder := json.NewDecoder(resp.Body)
		for decoder.More() {
			var chunk struct {
				Choices []struct {
					Delta struct {
						Role    string `json:"role"`
						Content string `json:"content"`
					} `json:"delta"`
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
			}
			if err := decoder.Decode(&chunk); err != nil {
				if err == io.EOF {
					break
				}
				ch <- &provider.LLMResponse{Role: "err", CompletionText: err.Error()}
				return
			}
			if len(chunk.Choices) > 0 {
				choice := chunk.Choices[0]
				llmResp := &provider.LLMResponse{
					Role:           choice.Delta.Role,
					CompletionText: choice.Delta.Content,
				}
				if choice.FinishReason != "" {
					llmResp.Role = "assistant"
				}
				ch <- llmResp
			}
		}
	}()
	return ch, nil
}

// Test verifies the provider.
func (s *OpenAISource) Test(ctx context.Context) error {
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

func (s *OpenAISource) buildRequestBody(req *provider.ProviderRequest, stream bool) map[string]interface{} {
	body := map[string]interface{}{
		"model":  s.GetModel(),
		"stream": stream,
	}

	// Build messages
	messages := []map[string]interface{}{}
	if req.SystemPrompt != "" {
		messages = append(messages, map[string]interface{}{
			"role":    "system",
			"content": req.SystemPrompt,
		})
	}
	// Add context messages
	for _, msg := range req.Contexts {
		messages = append(messages, msg)
	}
	// Add current user message
	messages = append(messages, req.ToUserMessage())
	body["messages"] = messages

	// Add tools if present
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}

	return body
}

func (s *OpenAISource) doRequest(ctx context.Context, body map[string]interface{}) (*http.Response, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}
	url := fmt.Sprintf("%s/chat/completions", s.apiBase)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+s.apiKey)
	return s.client.Do(httpReq)
}
