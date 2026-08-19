// Package sources - Kimi (Moonshot) coding chat provider.
// Ported from astrbot/core/provider/sources/kimi_code_source.py
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

const (
	kimiCodeAPIBase      = "https://api.kimi.com/coding"
	kimiCodeDefaultModel = "kimi-for-coding"
	kimiCodeUserAgent    = "claude-code/0.1.0"
)

// KimiCodeSource is a Kimi coding chat provider speaking the Anthropic
// Messages protocol with a spoofed Claude Code user agent.
type KimiCodeSource struct {
	provider.BaseProvider
	apiBase        string
	apiKey         string
	client         *http.Client
	streamClient   *http.Client
	customHeaders  map[string]string
	thinkingConfig map[string]interface{}
}

// NewKimiCodeSource creates a Kimi Code provider.
func NewKimiCodeSource(config, settings map[string]interface{}) *KimiCodeSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &KimiCodeSource{
		BaseProvider:  bp,
		client:        &http.Client{Timeout: 120 * time.Second},
		streamClient:  newStreamClient(),
		customHeaders: map[string]string{},
	}
	s.apiBase = configString(config, "api_base", kimiCodeAPIBase)
	if s.GetModel() == "" {
		s.SetModel(configString(config, "model", kimiCodeDefaultModel))
	}
	if keys, ok := config["key"].([]interface{}); ok {
		for _, k := range keys {
			if str, ok := k.(string); ok && str != "" {
				s.apiKey = str
				break
			}
		}
	}
	if s.apiKey == "" {
		if k, ok := config["key"].(string); ok {
			s.apiKey = k
		}
	}
	if ch, ok := config["custom_headers"].(map[string]interface{}); ok {
		for k, v := range ch {
			s.customHeaders[k] = fmt.Sprint(v)
		}
	}
	if strings.TrimSpace(s.customHeaders["User-Agent"]) == "" {
		s.customHeaders["User-Agent"] = kimiCodeUserAgent
	}
	if tc, ok := config["anth_thinking_config"].(map[string]interface{}); ok {
		s.thinkingConfig = tc
	}
	return s
}

// GetCurrentKey returns the current API key.
func (s *KimiCodeSource) GetCurrentKey() string {
	return s.apiKey
}

// SetKey sets the API key.
func (s *KimiCodeSource) SetKey(key string) {
	s.apiKey = key
}

// GetModels returns available models.
func (s *KimiCodeSource) GetModels(ctx context.Context) ([]string, error) {
	url := fmt.Sprintf("%s/v1/models", s.apiBase)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", s.apiKey)
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

// TextChat sends a non-streaming Anthropic-style Messages request.
func (s *KimiCodeSource) TextChat(ctx context.Context, req *provider.ProviderRequest) (*provider.LLMResponse, error) {
	body := s.buildRequestBody(req, false)
	resp, err := s.doRequest(ctx, body, s.client)
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
		ID      string `json:"id"`
		Content []struct {
			Type      string                 `json:"type"`
			Text      string                 `json:"text"`
			Thinking  string                 `json:"thinking"`
			Signature string                 `json:"signature"`
			ID        string                 `json:"id"`
			Name      string                 `json:"name"`
			Input     map[string]interface{} `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			OutputTokens         int `json:"output_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	llmResp := &provider.LLMResponse{
		Role:          "assistant",
		ID:            result.ID,
		ToolsCallArgs: []map[string]interface{}{},
		ToolsCallName: []string{},
		ToolsCallIDs:  []string{},
	}
	textParts := []string{}
	for _, block := range result.Content {
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
		case "thinking":
			llmResp.ReasoningContent = strings.TrimSpace(block.Thinking)
			llmResp.ReasoningSignature = block.Signature
		case "tool_use":
			if block.Input == nil {
				block.Input = map[string]interface{}{}
			}
			llmResp.ToolsCallName = append(llmResp.ToolsCallName, block.Name)
			llmResp.ToolsCallIDs = append(llmResp.ToolsCallIDs, block.ID)
			llmResp.ToolsCallArgs = append(llmResp.ToolsCallArgs, block.Input)
		}
	}
	llmResp.CompletionText = strings.TrimSpace(strings.Join(textParts, ""))
	if len(llmResp.ToolsCallArgs) > 0 {
		llmResp.Role = "tool"
	}
	llmResp.Usage = &provider.TokenUsage{
		InputOther:  result.Usage.InputTokens,
		InputCached: result.Usage.CacheReadInputTokens,
		Output:      result.Usage.OutputTokens,
	}
	logger.Debug("LLM response: text_len=%d", len(llmResp.CompletionText))
	return llmResp, nil
}

// TextChatStream sends a streaming Anthropic-style Messages request (SSE).
func (s *KimiCodeSource) TextChatStream(ctx context.Context, req *provider.ProviderRequest) (<-chan *provider.LLMResponse, error) {
	body := s.buildRequestBody(req, true)
	resp, err := s.doRequest(ctx, body, s.streamClient)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	ch := make(chan *provider.LLMResponse, 100)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		// Tool use inputs stream as partial JSON across content_block_delta
		// events, accumulated per content block index.
		type toolAcc struct {
			id, name, inputJSON string
		}
		toolBuf := map[int]*toolAcc{}
		var finalToolCalls []*toolAcc
		content := new(strings.Builder)
		reasoning := new(strings.Builder)
		usage := &provider.TokenUsage{}
		var responseID string

		reader := newSSEReader(ctx, resp, func(data string) (stop bool) {
			var event struct {
				Type    string `json:"type"`
				Index   int    `json:"index"`
				Message *struct {
					ID    string `json:"id"`
					Usage struct {
						InputTokens          int `json:"input_tokens"`
						OutputTokens         int `json:"output_tokens"`
						CacheReadInputTokens int `json:"cache_read_input_tokens"`
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
					Thinking    string `json:"thinking"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
				Usage *struct {
					InputTokens          int `json:"input_tokens"`
					OutputTokens         int `json:"output_tokens"`
					CacheReadInputTokens int `json:"cache_read_input_tokens"`
				} `json:"usage"`
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
				case "thinking_delta":
					if event.Delta.Thinking != "" {
						reasoning.WriteString(event.Delta.Thinking)
						ch <- &provider.LLMResponse{
							Role:             "assistant",
							IsChunk:          true,
							ReasoningContent: event.Delta.Thinking,
							ID:               responseID,
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
				if event.Usage != nil {
					usage.InputOther = event.Usage.InputTokens
					usage.InputCached = event.Usage.CacheReadInputTokens
					usage.Output = event.Usage.OutputTokens
				}
			case "message_stop":
				return true
			}
			return false
		})
		if err := reader.scan(); err != nil {
			logger.Warn("Kimi Code stream read error: %v", err)
			ch <- &provider.LLMResponse{Role: "err", CompletionText: fmt.Sprintf("Kimi Code stream read error: %v", err)}
			return
		}

		final := &provider.LLMResponse{
			Role:             "assistant",
			ID:               responseID,
			CompletionText:   content.String(),
			ReasoningContent: reasoning.String(),
			Usage:            usage,
			ToolsCallArgs:    []map[string]interface{}{},
			ToolsCallName:    []string{},
			ToolsCallIDs:     []string{},
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
		logger.Debug("LLM stream done, usage=%v", usage)
		ch <- final
	}()
	logger.Debug("LLM stream started: model=%s", s.GetModel())
	return ch, nil
}

// Test verifies the provider.
func (s *KimiCodeSource) Test(ctx context.Context) error {
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

func (s *KimiCodeSource) buildRequestBody(req *provider.ProviderRequest, stream bool) map[string]interface{} {
	body := map[string]interface{}{
		"model":      s.GetModel(),
		"max_tokens": 65536,
		"stream":     stream,
	}
	if req.SystemPrompt != "" {
		body["system"] = []map[string]interface{}{{"type": "text", "text": req.SystemPrompt}}
	}
	// Build messages (Kimi follows the Anthropic user/assistant convention).
	// Convert OpenAI-style tool_calls/tool history into Anthropic tool_use /
	// tool_result content blocks so tool-loop follow-ups stay valid.
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

	// Convert OpenAI function schema to Anthropic tool definitions.
	if len(req.Tools) > 0 {
		if tools := anthropicToolDefs(req.Tools); len(tools) > 0 {
			body["tools"] = tools
			body["tool_choice"] = map[string]interface{}{"type": "auto"}
		}
	}

	if extra, ok := s.Config()["custom_extra_body"].(map[string]interface{}); ok {
		for k, v := range extra {
			body[k] = v
		}
	}
	s.applyThinkingConfig(body)
	return body
}

// anthropicToolDefs flattens OpenAI function schemas into Anthropic tool
// definitions (name / description / input_schema).
func anthropicToolDefs(tools []map[string]interface{}) []map[string]interface{} {
	defs := []map[string]interface{}{}
	for _, t := range tools {
		fn, _ := t["function"].(map[string]interface{})
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		def := map[string]interface{}{"name": name}
		if desc, ok := fn["description"].(string); ok && desc != "" {
			def["description"] = desc
		}
		if params, ok := fn["parameters"].(map[string]interface{}); ok {
			def["input_schema"] = params
		}
		defs = append(defs, def)
	}
	return defs
}

// applyThinkingConfig injects the configured Anthropic thinking mode into the
// request body (adaptive or budget-based enabled).
func (s *KimiCodeSource) applyThinkingConfig(body map[string]interface{}) {
	if s.thinkingConfig == nil {
		return
	}
	thinkingType, _ := s.thinkingConfig["type"].(string)
	if thinkingType == "adaptive" {
		body["thinking"] = map[string]interface{}{"type": "adaptive"}
		if effort, _ := s.thinkingConfig["effort"].(string); effort != "" {
			body["output_config"] = map[string]interface{}{"effort": effort}
		}
	} else if thinkingType == "" {
		if budget, ok := s.thinkingConfig["budget"]; ok && budget != nil {
			body["thinking"] = map[string]interface{}{"budget_tokens": budget, "type": "enabled"}
		}
	}
}

func (s *KimiCodeSource) doRequest(ctx context.Context, body map[string]interface{}, client *http.Client) (*http.Response, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}
	url := fmt.Sprintf("%s/v1/messages", s.apiBase)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", s.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	for k, v := range s.customHeaders {
		httpReq.Header.Set(k, v)
	}
	msgs, _ := body["messages"].([]map[string]interface{})
	logger.Debug("LLM request: url=%s model=%s messages=%d", url, s.GetModel(), len(msgs))
	return client.Do(httpReq)
}
