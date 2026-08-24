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
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// AnthropicSource is a Claude/Anthropic chat provider.
type AnthropicSource struct {
	*provider.BaseProvider
	apiBase      string
	apiKey       string
	client       *http.Client
	streamClient *http.Client
}

// NewAnthropicSource creates an Anthropic provider.
func NewAnthropicSource(config, settings map[string]interface{}) *AnthropicSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &AnthropicSource{
		BaseProvider: bp,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
		streamClient: newStreamClient(),
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

// doRequest sends an HTTP request with retry logic.
func (s *AnthropicSource) doRequest(ctx context.Context, body map[string]interface{}, client *http.Client) (*http.Response, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}
	url := fmt.Sprintf("%s/messages", s.apiBase)
	msgs, _ := body["messages"].([]map[string]interface{})
	logger.Debug("LLM request: url=%s model=%s messages=%d", url, s.GetModel(), len(msgs))
	cfg := RetryConfigFromSettings(s.Settings())
	return DoWithRetry(ctx, client, func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("x-api-key", s.apiKey)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
		return httpReq, nil
	}, cfg, "Anthropic")
}

// TextChat sends a non-streaming chat request.
func (s *AnthropicSource) TextChat(ctx context.Context, req *provider.ProviderRequest) (*provider.LLMResponse, error) {
	body := s.buildRequestBody(req, false)
	resp, err := s.doRequest(ctx, body, s.client)
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
		Content []struct {
			Type  string                 `json:"type"`
			Text  string                 `json:"text"`
			ID    string                 `json:"id"`
			Name  string                 `json:"name"`
			Input map[string]interface{} `json:"input"`
		} `json:"content"`
		Role  string `json:"role"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	llmResp := &provider.LLMResponse{
		Role:          "assistant",
		ToolsCallArgs: []map[string]interface{}{},
		ToolsCallName: []string{},
		ToolsCallIDs:  []string{},
		Usage: &provider.TokenUsage{
			InputOther:  result.Usage.InputTokens,
			InputCached: result.Usage.CacheReadInputTokens,
			Output:      result.Usage.OutputTokens,
		},
	}
	var text strings.Builder
	for _, c := range result.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
		case "tool_use":
			if c.Input == nil {
				c.Input = map[string]interface{}{}
			}
			llmResp.ToolsCallName = append(llmResp.ToolsCallName, c.Name)
			llmResp.ToolsCallIDs = append(llmResp.ToolsCallIDs, c.ID)
			llmResp.ToolsCallArgs = append(llmResp.ToolsCallArgs, c.Input)
		}
	}
	llmResp.CompletionText = text.String()
	if len(llmResp.ToolsCallArgs) > 0 {
		llmResp.Role = "tool"
	}
	logger.Debug("LLM response: text_len=%d", len(llmResp.CompletionText))
	return llmResp, nil
}

// TextChatStream sends a streaming chat request (SSE).
func (s *AnthropicSource) TextChatStream(ctx context.Context, req *provider.ProviderRequest) (<-chan *provider.LLMResponse, error) {
	body := s.buildRequestBody(req, true)
	resp, err := s.doRequest(ctx, body, s.streamClient)
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
		defer close(ch)
		defer resp.Body.Close()

		type toolAcc struct {
			id, name, inputJSON string
		}
		toolBuf := map[int]*toolAcc{}
		var finalToolCalls []*toolAcc
		content := new(strings.Builder)
		usage := &provider.TokenUsage{}
		var responseID string
		sentFinal := false

		// buildFinal 聚合文本/工具调用/用量为最终响应, 在 message_stop
		// 分支或流干净结束时发送。
		buildFinal := func() *provider.LLMResponse {
			final := &provider.LLMResponse{
				Role:           "assistant",
				ID:             responseID,
				CompletionText: content.String(),
				Usage:          usage,
				ToolsCallArgs:  []map[string]interface{}{},
				ToolsCallName:  []string{},
				ToolsCallIDs:   []string{},
			}
			for _, tc := range finalToolCalls {
				argsMap := map[string]interface{}{}
				if tc.inputJSON != "" {
					_ = json.Unmarshal([]byte(tc.inputJSON), &argsMap)
				}
				final.ToolsCallName = append(final.ToolsCallName, tc.name)
				final.ToolsCallIDs = append(final.ToolsCallIDs, tc.id)
				final.ToolsCallArgs = append(final.ToolsCallArgs, argsMap)
			}
			if len(final.ToolsCallArgs) > 0 {
				final.Role = "tool"
			}
			return final
		}

		reader := newSSEReader(ctx, resp, func(data string) (stop bool) {
			var event struct {
				Type    string `json:"type"`
				Index   int    `json:"index"`
				Message *struct {
					ID    string `json:"id"`
					Usage struct {
						InputTokens              int `json:"input_tokens"`
						OutputTokens             int `json:"output_tokens"`
						CacheReadInputTokens     int `json:"cache_read_input_tokens"`
						CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
				ContentBlock *struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
				Delta *struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
				Usage *struct {
					InputTokens              int `json:"input_tokens"`
					OutputTokens             int `json:"output_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
				Error *struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				return false
			}
			switch event.Type {
			case "message_start":
				if event.Message != nil {
					responseID = event.Message.ID
					usage.InputOther = event.Message.Usage.InputTokens
					usage.InputCached = event.Message.Usage.CacheReadInputTokens
					usage.Output = event.Message.Usage.OutputTokens
				}
			case "content_block_start":
				if event.ContentBlock != nil && event.ContentBlock.Type == "tool_use" {
					toolBuf[event.Index] = &toolAcc{
						id:   event.ContentBlock.ID,
						name: event.ContentBlock.Name,
					}
				}
			case "content_block_delta":
				if event.Delta == nil {
					return false
				}
				switch event.Delta.Type {
				case "text_delta":
					if event.Delta.Text != "" {
						content.WriteString(event.Delta.Text)
						ch <- &provider.LLMResponse{
							Role:           "assistant",
							IsChunk:        true,
							CompletionText: event.Delta.Text,
							ID:             responseID,
						}
					}
				case "input_json_delta":
					if acc := toolBuf[event.Index]; acc != nil {
						acc.inputJSON += event.Delta.PartialJSON
					}
				}
			case "content_block_stop":
				if acc := toolBuf[event.Index]; acc != nil {
					finalToolCalls = append(finalToolCalls, acc)
					delete(toolBuf, event.Index)
				}
case "message_delta":
			// message_delta 的 usage 只携带 output_tokens（对齐 Python
			// _update_usage：input 字段缺失时保留 message_start 的值）。
			if event.Usage != nil {
				usage.Output = event.Usage.OutputTokens
			}
			case "message_stop":
				ch <- buildFinal()
				sentFinal = true
				return true
			case "error":
				msg := "Anthropic stream error"
				if event.Error != nil && event.Error.Message != "" {
					msg = event.Error.Message
				}
				ch <- &provider.LLMResponse{Role: "err", CompletionText: msg}
				sentFinal = true
				return true
			}
			return false
		})
		if err := reader.scan(); err != nil {
			logger.Warn("Anthropic stream read error: %v", err)
			ch <- &provider.LLMResponse{Role: "err", CompletionText: fmt.Sprintf("Anthropic stream read error: %v", err)}
			return
		}
		// 对端干净 FIN 但未收到 message_stop: 补发最终聚合响应,
		// 保证工具调用与 token 统计不会丢失。
		if !sentFinal {
			ch <- buildFinal()
		}
		if usage != nil {
			logger.Debug("LLM stream done, usage=%v", usage)
		}
	}()
	logger.Debug("LLM stream started: model=%s", s.GetModel())
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
	maxTokens := 4096
	if v := configInt(s.Config(), "max_tokens", 0); v > 0 {
		maxTokens = v
	}
	body := map[string]interface{}{
		"model":      s.GetModel(),
		"max_tokens": maxTokens,
		"stream":     stream,
	}
	if req.SystemPrompt != "" {
		body["system"] = req.SystemPrompt
	}
	// Convert OpenAI-style contexts (tool_calls / tool / image_url) into the
	// Anthropic Messages protocol; plain user/assistant text passes through.
	messages := []map[string]interface{}{}
	for _, msg := range req.Contexts {
		role, _ := msg["role"].(string)
		if role == "system" {
			continue
		}
		messages = append(messages, anthropicMessage(msg))
	}
	messages = append(messages, anthropicMessage(req.ToUserMessage()))
	body["messages"] = messages

	// Convert OpenAI function schemas to Anthropic tool definitions.
	if len(req.Tools) > 0 {
		if tools := anthropicToolDefs(req.Tools); len(tools) > 0 {
			body["tools"] = tools
			body["tool_choice"] = map[string]interface{}{"type": "auto"}
		}
	}
	return body
}
