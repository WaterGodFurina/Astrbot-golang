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
	apiBase      string
	client       *http.Client
	streamClient *http.Client
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
	return s
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
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		PromptEvalCount int `json:"prompt_eval_count"`
		EvalCount       int `json:"eval_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	logger.Debug("LLM response: text_len=%d", len(result.Message.Content))
	return &provider.LLMResponse{
		Role:           result.Message.Role,
		CompletionText: result.Message.Content,
		Usage: &provider.TokenUsage{
			InputOther: result.PromptEvalCount,
			Output:     result.EvalCount,
		},
	}, nil
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
		sawDone := false
		for decoder.More() {
			var chunk struct {
				Message struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
				PromptEvalCount int  `json:"prompt_eval_count"`
				EvalCount       int  `json:"eval_count"`
				Done            bool `json:"done"`
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
			if chunk.PromptEvalCount > 0 || chunk.EvalCount > 0 {
				usage = &provider.TokenUsage{
					InputOther: chunk.PromptEvalCount,
					Output:     chunk.EvalCount,
				}
			}
			if chunk.Done {
				// Final chunk: emit consolidated response with usage.
				sawDone = true
				ch <- &provider.LLMResponse{
					Role:           "assistant",
					CompletionText: content.String(),
					Usage:          usage,
				}
			}
		}
		// 流结束但未收到 done 块: 补发最终聚合块, 保证 usage 不丢失。
		if !sawDone {
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
	return body
}
