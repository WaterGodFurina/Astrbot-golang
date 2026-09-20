// Package sources - Ollama local LLM provider.
// Ported from astrbot/core/provider/sources/ollama_source.py
package sources

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// OllamaSource is an Ollama local LLM provider.
type OllamaSource struct {
	*provider.BaseProvider
	apiBase string
	client  *http.Client
	// streamClient 供 SSE 流式读取。
	streamClient *http.Client
	// disableThinking 对应 ollama_disable_thinking：为 true 时不下发思考
	// （原生 /api/chat 传 think=false）。
	disableThinking bool
}

// ollamaToolCall 为原生 /api/chat 的 message.tool_calls 元素。arguments 在
// 新版 Ollama 是 JSON 对象，旧版可能是 JSON 字符串，故用 interface{} 承接。
type ollamaToolCall struct {
	Function struct {
		Name      string      `json:"name"`
		Arguments interface{} `json:"arguments"`
	} `json:"function"`
}

// ollamaMessage 为原生 /api/chat 的 message 字段。
type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Thinking  string           `json:"thinking"`
	ToolCalls []ollamaToolCall `json:"tool_calls"`
}

// NewOllamaSource creates an Ollama provider.
func NewOllamaSource(config, settings map[string]interface{}) *OllamaSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &OllamaSource{
		BaseProvider: bp,
		client: &http.Client{
			Timeout: 300 * time.Second, // Ollama can be slow
		},
		streamClient: newStreamClient(),
	}
	s.apiBase, _ = config["api_base"].(string)
	if s.apiBase == "" {
		s.apiBase = "http://localhost:11434"
	}
	// ollama_disable_thinking（WebUI 模板已提供该键）：读取并生效，为 true 时
	// 关闭思考模式。
	s.disableThinking = configBool(config, "ollama_disable_thinking", false)
	return s
}

// GetModels 列出 Ollama 本地模型（GET /api/tags，取 models[].name）。Ollama
// 原生 API 的模型列表端点，与 dashboard provider_sources 的取法一致。
func (s *OllamaSource) GetModels(ctx context.Context) ([]string, error) {
	url := strings.TrimRight(s.apiBase, "/") + "/api/tags"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, truncate(string(body), 1024))
	}
	var result struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(result.Models))
	for _, m := range result.Models {
		if m.Name != "" {
			models = append(models, m.Name)
		}
	}
	return models, nil
}

// ollamaArguments 把原生 tool_calls 的 arguments（对象或 JSON 字符串）归一
// 为 map，供 LLMResponse.ToolsCallArgs 使用。
func ollamaArguments(v interface{}) map[string]interface{} {
	switch a := v.(type) {
	case map[string]interface{}:
		return a
	case string:
		out := map[string]interface{}{}
		if strings.TrimSpace(a) != "" {
			_ = json.Unmarshal([]byte(a), &out)
		}
		return out
	case nil:
		return map[string]interface{}{}
	default:
		out := map[string]interface{}{}
		if b, err := json.Marshal(a); err == nil {
			_ = json.Unmarshal(b, &out)
		}
		return out
	}
}

// doRequest sends an HTTP request with retry logic.
func (s *OllamaSource) doRequest(ctx context.Context, body map[string]interface{}, client *http.Client) (*http.Response, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}
	url := fmt.Sprintf("%s/api/chat", s.apiBase)
	msgs, _ := body["messages"].([]map[string]interface{})
	logger.Debug("LLM request: url=%s model=%s messages=%d", url, s.GetModel(), len(msgs))
	cfg := RetryConfigFromSettings(s.Settings())
	return DoWithRetry(ctx, client, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}, cfg, "Ollama")
}

// TextChat sends a non-streaming chat request.
func (s *OllamaSource) TextChat(ctx context.Context, req *provider.ProviderRequest) (*provider.LLMResponse, error) {
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
		Message         ollamaMessage `json:"message"`
		PromptEvalCount int           `json:"prompt_eval_count"`
		EvalCount       int           `json:"eval_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	llmResp := &provider.LLMResponse{
		Role:             result.Message.Role,
		CompletionText:   result.Message.Content,
		ReasoningContent: result.Message.Thinking,
		ToolsCallArgs:    []map[string]interface{}{},
		ToolsCallName:    []string{},
		ToolsCallIDs:     []string{},
		Usage: &provider.TokenUsage{
			InputOther: result.PromptEvalCount,
			Output:     result.EvalCount,
		},
	}
	// 原生 /api/chat 的 message.tool_calls 需回填为 LLMResponse 的工具调用，
	// 否则 Ollama 的函数调用永不触发（REST tool_calls 为 {"function":{...}}，
	// 无 id/type 字段）。
	for _, tc := range result.Message.ToolCalls {
		llmResp.ToolsCallName = append(llmResp.ToolsCallName, tc.Function.Name)
		llmResp.ToolsCallArgs = append(llmResp.ToolsCallArgs, ollamaArguments(tc.Function.Arguments))
		// 原生协议不返回 call id，用函数名占位，保证后续 tool 结果能回填。
		llmResp.ToolsCallIDs = append(llmResp.ToolsCallIDs, tc.Function.Name)
	}
	if len(llmResp.ToolsCallArgs) > 0 {
		llmResp.Role = "tool"
	}
	logger.Debug("LLM response: text_len=%d tools=%d", len(llmResp.CompletionText), len(llmResp.ToolsCallArgs))
	return llmResp, nil
}

// TextChatStream sends a streaming chat request.
func (s *OllamaSource) TextChatStream(ctx context.Context, req *provider.ProviderRequest) (<-chan *provider.LLMResponse, error) {
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
		defer resp.Body.Close()
		defer close(ch)
		decoder := json.NewDecoder(resp.Body)
		var usage *provider.TokenUsage
		var content strings.Builder
		var reasoning strings.Builder
		var finalToolCalls []ollamaToolCall
		sawDone := false

		// buildFinal 聚合文本/思考/工具调用/用量为最终响应。
		buildFinal := func() *provider.LLMResponse {
			final := &provider.LLMResponse{
				Role:             "assistant",
				CompletionText:   content.String(),
				ReasoningContent: reasoning.String(),
				Usage:            usage,
				ToolsCallArgs:    []map[string]interface{}{},
				ToolsCallName:    []string{},
				ToolsCallIDs:     []string{},
			}
			for _, tc := range finalToolCalls {
				final.ToolsCallName = append(final.ToolsCallName, tc.Function.Name)
				final.ToolsCallArgs = append(final.ToolsCallArgs, ollamaArguments(tc.Function.Arguments))
				final.ToolsCallIDs = append(final.ToolsCallIDs, tc.Function.Name)
			}
			if len(final.ToolsCallArgs) > 0 {
				final.Role = "tool"
			}
			return final
		}

		for decoder.More() {
			var chunk struct {
				Message         ollamaMessage `json:"message"`
				PromptEvalCount int           `json:"prompt_eval_count"`
				EvalCount       int           `json:"eval_count"`
				Done            bool          `json:"done"`
			}
			if err := decoder.Decode(&chunk); err != nil {
				if err == io.EOF {
					break
				}
				ch <- &provider.LLMResponse{Role: "err", CompletionText: fmt.Sprintf("Ollama stream decode error: %v", err)}
				return
			}
			if chunk.Message.Content != "" {
				content.WriteString(chunk.Message.Content)
				ch <- &provider.LLMResponse{
					Role:           chunk.Message.Role,
					IsChunk:        true,
					CompletionText: chunk.Message.Content,
				}
			}
			// 思考增量（原生 message.thinking）作为 reasoning 片段下发并累积。
			if chunk.Message.Thinking != "" {
				reasoning.WriteString(chunk.Message.Thinking)
				ch <- &provider.LLMResponse{
					Role:             chunk.Message.Role,
					IsChunk:          true,
					ReasoningContent: chunk.Message.Thinking,
				}
			}
			// 工具调用通常在末尾块给出；累积后在最终响应回填。
			if len(chunk.Message.ToolCalls) > 0 {
				finalToolCalls = append(finalToolCalls, chunk.Message.ToolCalls...)
			}
			if chunk.PromptEvalCount > 0 || chunk.EvalCount > 0 {
				usage = &provider.TokenUsage{
					InputOther: chunk.PromptEvalCount,
					Output:     chunk.EvalCount,
				}
			}
			if chunk.Done {
				sawDone = true
				ch <- buildFinal()
			}
		}
		// 流结束但未收到 done 块: 补发最终聚合块, 保证 usage/工具调用不丢失。
		if !sawDone {
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
func (s *OllamaSource) Test(ctx context.Context) error {
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

func (s *OllamaSource) buildRequestBody(req *provider.ProviderRequest, stream bool) map[string]interface{} {
	messages := []map[string]interface{}{}
	if req.SystemPrompt != "" {
		messages = append(messages, map[string]interface{}{
			"role":    "system",
			"content": req.SystemPrompt,
		})
	}
	messages = append(messages, req.Contexts...)
	// 图片: Ollama 原生协议要求 messages[].images 为 base64 数组（不带 data: 前缀）。
	var images []string
	for _, img := range req.ImageURLs {
		if data, _, err := fetchMediaData(img); err == nil {
			images = append(images, base64.StdEncoding.EncodeToString(data))
		} else {
			logger.Warn("Ollama: 加载图片 %q 失败: %v", img, err)
		}
	}
	userMsg := map[string]interface{}{"role": "user", "content": req.Prompt}
	if len(images) > 0 {
		userMsg["images"] = images
	}
	messages = append(messages, userMsg)
	body := map[string]interface{}{
		"model":    s.GetModel(),
		"messages": messages,
		"stream":   stream,
	}
	// 工具: 透传 OpenAI function schema（Ollama 的 tools 字段与 OpenAI 结构一致）。
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}
	// ollama_disable_thinking 生效：为 true 时显式传 think=false 关闭思考模式；
	// 未开启时不下发该字段，交由 Ollama 按模型默认决定。
	if s.disableThinking {
		body["think"] = false
	}
	return body
}
