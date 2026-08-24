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
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// maxToolCallIndex caps the tool-call delta index so a malformed stream cannot
// trigger an out-of-range slice access or a huge allocation.
const maxToolCallIndex = 64

// OpenAISource is an OpenAI-compatible chat provider.
// Ported from astrbot/core/provider/sources/openai_source.py
type OpenAISource struct {
	*provider.BaseProvider
	apiBase      string
	apiKey       string
	client       *http.Client
	streamClient *http.Client
	// extraHeaders are additional HTTP headers applied to every request.
	extraHeaders map[string]string
	// postProcessBody mutates the chat request body right before it is sent.
	// Thin OpenAI-compatible subclasses use this to customize payloads.
	postProcessBody func(body map[string]interface{})
}

// NewOpenAISource creates an OpenAI provider.
func NewOpenAISource(config, settings map[string]interface{}) *OpenAISource {
	bp := provider.NewBaseProvider(config, settings)
	s := &OpenAISource{
		BaseProvider: bp,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
		streamClient: newStreamClient(),
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, truncate(string(body), 1024))
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
	resp, err := s.doRequest(ctx, body, s.client)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &provider.LLMResponse{
			Role:           "err",
			CompletionText: fmt.Sprintf("API error %d: %s", resp.StatusCode, truncate(string(respBody), 1024)),
		}, nil
	}
	var result struct {
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
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
		return nil, fmt.Errorf("openai completion has no usable output")
	}
	choice := result.Choices[0]
	llmResp := &provider.LLMResponse{
		Role:             choice.Message.Role,
		CompletionText:   choice.Message.Content,
		ReasoningContent: choice.Message.ReasoningContent,
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
	logger.Debug("LLM response: text_len=%d", len(llmResp.CompletionText))
	return llmResp, nil
}

// TextChatStream sends a streaming chat request (SSE).
func (s *OpenAISource) TextChatStream(ctx context.Context, req *provider.ProviderRequest) (<-chan *provider.LLMResponse, error) {
	body := s.buildRequestBody(req, true)
	resp, err := s.doRequest(ctx, body, s.streamClient)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, truncate(string(respBody), 1024))
	}

	ch := make(chan *provider.LLMResponse, 100)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		// Tool call fragments arrive as partial JSON across many chunks, so we
		// accumulate per-index fragments and emit the consolidated calls once
		// the stream finishes.
		type toolAcc struct {
			id, name, args string
		}
		var toolCalls []toolAcc
		content := new(strings.Builder)
		reasoning := new(strings.Builder)
		var usage *provider.TokenUsage
		var finishReason string

		reader := newSSEReader(ctx, resp, func(data string) (stop bool) {
			var chunk struct {
				Choices []struct {
					Delta struct {
						Role      string `json:"role"`
						Content   string `json:"content"`
						Reasoning string `json:"reasoning_content"`
						ToolCalls []struct {
							Index    int    `json:"index"`
							ID       string `json:"id"`
							Function struct {
								Name      string `json:"name"`
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
				Usage *struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
					TotalTokens      int `json:"total_tokens"`
				} `json:"usage"`
				Error *struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				return false
			}
			if chunk.Error != nil {
				msg := chunk.Error.Message
				if msg == "" {
					msg = chunk.Error.Type
				}
				ch <- &provider.LLMResponse{Role: "err", CompletionText: "API stream error: " + msg}
				return true
			}
			if chunk.Usage != nil {
				usage = &provider.TokenUsage{
					InputOther: chunk.Usage.PromptTokens,
					Output:     chunk.Usage.CompletionTokens,
				}
			}
			for _, choice := range chunk.Choices {
				if choice.FinishReason != "" {
					finishReason = choice.FinishReason
				}
				if choice.Delta.Content != "" {
					content.WriteString(choice.Delta.Content)
					ch <- &provider.LLMResponse{
						Role:           "assistant",
						IsChunk:        true,
						CompletionText: choice.Delta.Content,
					}
				}
				if choice.Delta.Reasoning != "" {
					reasoning.WriteString(choice.Delta.Reasoning)
				}
				for _, tc := range choice.Delta.ToolCalls {
					if tc.Index < 0 || tc.Index >= maxToolCallIndex {
						logger.Warn("OpenAI stream: skipping tool call fragment with invalid index %d", tc.Index)
						continue
					}
					for len(toolCalls) <= tc.Index {
						toolCalls = append(toolCalls, toolAcc{})
					}
					if tc.ID != "" {
						toolCalls[tc.Index].id = tc.ID
					}
					if tc.Function.Name != "" {
						toolCalls[tc.Index].name = tc.Function.Name
					}
					toolCalls[tc.Index].args += tc.Function.Arguments
				}
			}
			return false
		})
		if err := reader.scan(); err != nil {
			logger.Warn("OpenAI stream read error: %v", err)
			ch <- &provider.LLMResponse{Role: "err", CompletionText: fmt.Sprintf("OpenAI stream read error: %v", err)}
			return
		}

		final := &provider.LLMResponse{Role: "assistant", CompletionText: content.String(), ReasoningContent: reasoning.String(), Usage: usage}
		if finishReason == "length" || finishReason == "content_filter" {
			logger.Warn("OpenAI stream finished early: %s", finishReason)
			final.CompletionText += fmt.Sprintf("\n[response truncated: %s]", finishReason)
		}
		if len(toolCalls) > 0 {
			for _, tc := range toolCalls {
				final.ToolsCallName = append(final.ToolsCallName, tc.name)
				final.ToolsCallIDs = append(final.ToolsCallIDs, tc.id)
				argsMap := map[string]interface{}{}
				if tc.args != "" {
					_ = json.Unmarshal([]byte(tc.args), &argsMap)
				}
				final.ToolsCallArgs = append(final.ToolsCallArgs, argsMap)
			}
		}
		if usage != nil {
			logger.Debug("LLM stream done, usage=%v", usage)
		}
		ch <- final
	}()
	logger.Debug("LLM stream started: model=%s", s.GetModel())
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

// sanitizeContexts filters and normalizes assistant messages before send.
// Strict APIs (Moonshot, DeepSeek Reasoner) reject assistant messages lacking
// both content and tool_calls; context truncation/compression also leaves
// orphaned tool messages behind. Ported from _sanitize_assistant_messages in
// openai_source.py.
func sanitizeContexts(msgs []map[string]interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(msgs))
	pendingToolIDs := map[string]bool{}
	for _, msg := range msgs {
		role, _ := msg["role"].(string)
		if role == "assistant" {
			content, _ := msg["content"].(string)
			hasToolCalls := len(toolCallsSlice(msg["tool_calls"])) > 0
			if content == "" && !hasToolCalls {
				continue // 空 assistant 垃圾消息，丢弃
			}
			copied := cloneMap(msg)
			if content == "" && hasToolCalls {
				copied["content"] = nil
			}
			pendingToolIDs = map[string]bool{}
			for _, tc := range toolCallsSlice(copied["tool_calls"]) {
				if id, ok := tc["id"].(string); ok {
					pendingToolIDs[id] = true
				}
			}
			out = append(out, copied)
			continue
		}
		if role == "tool" {
			id, _ := msg["tool_call_id"].(string)
			if pendingToolIDs[id] {
				delete(pendingToolIDs, id)
				out = append(out, msg)
			}
			continue // 孤儿 tool 消息，丢弃
		}
		pendingToolIDs = map[string]bool{}
		out = append(out, msg)
	}
	return out
}

func (s *OpenAISource) buildRequestBody(req *provider.ProviderRequest, stream bool) map[string]interface{} {
	body := map[string]interface{}{
		"model":  s.GetModel(),
		"stream": stream,
	}
	if stream {
		// Ask the server to include usage in the final streamed chunk so token
		// statistics are available for streaming calls too.
		body["stream_options"] = map[string]interface{}{"include_usage": true}
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
	messages = append(messages, sanitizeContexts(req.Contexts)...)
	// Add current user message
	messages = append(messages, req.ToUserMessage())
	body["messages"] = messages

	// Add tools if present
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}

	// Let thin OpenAI-compatible subclasses customize the payload before send.
	if s.postProcessBody != nil {
		s.postProcessBody(body)
	}

	return body
}

func (s *OpenAISource) doRequest(ctx context.Context, body map[string]interface{}, client *http.Client) (*http.Response, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}
	url := fmt.Sprintf("%s/chat/completions", s.apiBase)
	msgs, _ := body["messages"].([]map[string]interface{})
	logger.Debug("LLM request: url=%s model=%s messages=%d", url, s.GetModel(), len(msgs))
	cfg := RetryConfigFromSettings(s.Settings())
	return DoWithRetry(ctx, client, func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+s.apiKey)
		for k, v := range s.extraHeaders {
			httpReq.Header.Set(k, v)
		}
		return httpReq, nil
	}, cfg, "OpenAI")
}
