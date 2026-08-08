// Package sources - Anthropic (Claude) chat provider.
// Ported from astrbot/core/provider/sources/anthropic_source.py
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

// AnthropicSource is a Claude/Anthropic chat provider.
type AnthropicSource struct {
	provider.BaseProvider
	apiBase string
	apiKey  string
	client  *http.Client
}

// NewAnthropicSource creates an Anthropic provider.
func NewAnthropicSource(config, settings map[string]interface{}) *AnthropicSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &AnthropicSource{
		BaseProvider: bp,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
	s.apiBase, _ = config["api_base"].(string)
	if s.apiBase == "" {
		s.apiBase = "https://api.anthropic.com/v1"
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

// TextChat sends a non-streaming chat request.
func (s *AnthropicSource) TextChat(ctx context.Context, req *provider.ProviderRequest) (*provider.LLMResponse, error) {
	body := s.buildRequestBody(req, false)
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/messages", s.apiBase)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", s.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	resp, err := s.client.Do(httpReq)
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
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Role  string `json:"role"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	var text string
	for _, c := range result.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	return &provider.LLMResponse{
		Role:           "assistant",
		CompletionText: text,
		Usage: &provider.TokenUsage{
			InputOther: result.Usage.InputTokens,
			Output:     result.Usage.OutputTokens,
		},
	}, nil
}

// TextChatStream sends a streaming chat request.
func (s *AnthropicSource) TextChatStream(ctx context.Context, req *provider.ProviderRequest) (<-chan *provider.LLMResponse, error) {
	body := s.buildRequestBody(req, true)
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/messages", s.apiBase)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", s.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	resp, err := s.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	ch := make(chan *provider.LLMResponse, 100)
	go func() {
		defer resp.Body.Close()
		defer close(ch)
		decoder := json.NewDecoder(resp.Body)
		for decoder.More() {
			var event struct {
				Type  string `json:"type"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			if err := decoder.Decode(&event); err != nil {
				if err == io.EOF {
					break
				}
				return
			}
			if event.Type == "content_block_delta" && event.Delta.Text != "" {
				ch <- &provider.LLMResponse{
					Role:           "assistant",
					CompletionText: event.Delta.Text,
				}
			}
		}
	}()
	return ch, nil
}

// Test verifies the provider.
func (s *AnthropicSource) Test(ctx context.Context) error {
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

func (s *AnthropicSource) buildRequestBody(req *provider.ProviderRequest, stream bool) map[string]interface{} {
	body := map[string]interface{}{
		"model":      s.GetModel(),
		"max_tokens": 4096,
		"stream":     stream,
	}
	if req.SystemPrompt != "" {
		body["system"] = req.SystemPrompt
	}
	// Build messages (Anthropic uses user/assistant roles)
	messages := []map[string]interface{}{}
	for _, msg := range req.Contexts {
		role, _ := msg["role"].(string)
		if role == "system" {
			continue
		}
		messages = append(messages, msg)
	}
	messages = append(messages, req.ToUserMessage())
	body["messages"] = messages
	return body
}
