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
	"net/url"
	"strconv"
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
	// extraHeaders 为 custom_headers 配置，注入每个请求（对齐 py default_headers）。
	extraHeaders map[string]string
	// thinkingConfig 为 anth_thinking_config（对齐 py self.thinking_config）。
	thinkingConfig map[string]interface{}
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
	s.apiBase = strings.TrimSpace(s.apiBase)
	if s.apiBase == "" {
		s.apiBase = "https://api.anthropic.com"
	}
	// 对齐 Python #9802：api_base trim 空格，空则默认 https://api.anthropic.com；
	// rstrip("/")；removesuffix("/v1")——用户配了 /v1 也剥掉，Go 侧统一拼 /v1/messages。
	s.apiBase = strings.TrimSuffix(strings.TrimRight(s.apiBase, "/"), "/v1")
	// timeout 配置（对齐 py __init__：provider_config["timeout"]，默认 120 秒，
	// 字符串形态强转 int）。仅作用于非流式客户端；流式沿用 10 分钟整体超时。
	switch v := config["timeout"].(type) {
	case int:
		if v > 0 {
			s.client.Timeout = time.Duration(v) * time.Second
		}
	case float64:
		if v > 0 {
			s.client.Timeout = time.Duration(v) * time.Second
		}
	case string:
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			s.client.Timeout = time.Duration(n) * time.Second
		}
	}
	// proxy 配置（对齐 py _create_http_client：配置 proxy 时走代理，流式/非流式共用）。
	if proxy, _ := config["proxy"].(string); proxy != "" {
		if u, err := url.Parse(proxy); err == nil && u.Scheme != "" {
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.Proxy = http.ProxyURL(u)
			s.client.Transport = tr
			s.streamClient.Transport = tr
		}
	}
	// custom_headers（对齐 py _resolve_custom_headers：非空 dict 生效，值 str 强转）。
	if ch, ok := config["custom_headers"].(map[string]interface{}); ok && len(ch) > 0 {
		s.extraHeaders = make(map[string]string, len(ch))
		for k, v := range ch {
			s.extraHeaders[k] = fmt.Sprint(v)
		}
	}
	// anth_thinking_config（对齐 py self.thinking_config）。
	if tc, ok := config["anth_thinking_config"].(map[string]interface{}); ok {
		s.thinkingConfig = tc
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
	url := fmt.Sprintf("%s/v1/messages", s.apiBase)
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
		// custom_headers 注入（对齐 py AsyncAnthropic(default_headers=...)）。
		for k, v := range s.extraHeaders {
			httpReq.Header.Set(k, v)
		}
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
			Type      string                 `json:"type"`
			Text      string                 `json:"text"`
			Thinking  string                 `json:"thinking"`
			Signature string                 `json:"signature"`
			ID        string                 `json:"id"`
			Name      string                 `json:"name"`
			Input     map[string]interface{} `json:"input"`
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
	// Anthropic 的 input_tokens 排除了 cache 读写，需把 cache_creation_input_tokens
	// 加回 input_other 以保持总 input 与上下文占用统计准确（对齐 Python #9880）。
	llmResp := &provider.LLMResponse{
		Role:          "assistant",
		ToolsCallArgs: []map[string]interface{}{},
		ToolsCallName: []string{},
		ToolsCallIDs:  []string{},
		Usage: &provider.TokenUsage{
			InputOther:  result.Usage.InputTokens + result.Usage.CacheCreationInputTokens,
			InputCached: result.Usage.CacheReadInputTokens,
			Output:      result.Usage.OutputTokens,
		},
	}
	var text strings.Builder
	for _, c := range result.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
		case "thinking":
			// 思考块回填 reasoning_content + signature（对齐 py _query）。
			llmResp.ReasoningContent = strings.TrimSpace(c.Thinking)
			llmResp.ReasoningSignature = c.Signature
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
		reasoning := new(strings.Builder)
		reasoningSignature := ""
		usage := &provider.TokenUsage{}
		var responseID string
		sentFinal := false

		// buildFinal 聚合文本/思考/工具调用/用量为最终响应, 在 message_stop
		// 分支或流干净结束时发送。
		buildFinal := func() *provider.LLMResponse {
			final := &provider.LLMResponse{
				Role:               "assistant",
				ID:                 responseID,
				CompletionText:     content.String(),
				ReasoningContent:   reasoning.String(),
				ReasoningSignature: reasoningSignature,
				Usage:              usage,
				ToolsCallArgs:      []map[string]interface{}{},
				ToolsCallName:      []string{},
				ToolsCallIDs:       []string{},
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
					Thinking    string `json:"thinking"`
					Signature   string `json:"signature"`
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
					// 对齐 Python #9880：input_tokens 不含 cache 读写，需加回 cache_creation。
					usage.InputOther = event.Message.Usage.InputTokens + event.Message.Usage.CacheCreationInputTokens
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
					// 思考增量：累积并作为 reasoning 片段下发（对齐 py
					// _query_stream 的 thinking_delta 分支）。
					if event.Delta.Thinking != "" {
						reasoning.WriteString(event.Delta.Thinking)
						ch <- &provider.LLMResponse{
							Role:             "assistant",
							IsChunk:          true,
							ReasoningContent: event.Delta.Thinking,
							ID:               responseID,
						}
					}
				case "signature_delta":
					// thinking 签名只保留最后一个（对齐 py reasoning_signature）。
					reasoningSignature = event.Delta.Signature
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
					// 对齐 Python #9880：若 message_delta 携带 cache_creation_input_tokens 则累加。
					if event.Usage.CacheCreationInputTokens > 0 {
						usage.InputOther += event.Usage.CacheCreationInputTokens
					}
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
	// max_tokens 默认 65536（对齐 py _query/_query_stream 的 65536）；用户显式
	// 配置 max_tokens 时覆盖，否则长回复会被 4096 截断。
	maxTokens := 65536
	if v := configInt(s.Config(), "max_tokens", 0); v > 0 {
		maxTokens = v
	}
	body := map[string]interface{}{
		"model":      s.GetModel(),
		"max_tokens": maxTokens,
		"stream":     stream,
	}
	if req.SystemPrompt != "" {
		// system 以文本块列表下发（对齐 py text_chat：system 为 [{"type":"text",...}]），
		// 这样 cache_control 断点才能挂到最后一个 block 上。
		body["system"] = []map[string]interface{}{
			{"type": "text", "text": req.SystemPrompt},
		}
	}
	// Convert OpenAI-style contexts (tool_calls / tool / image_url) into the
	// Anthropic Messages protocol; plain user/assistant text passes through.
	// Sanitize 合并相邻同角色消息并清理孤儿工具块：并行多工具的多个
	// tool_result 必须合并在同一条 user 消息里，否则 API 返回 400。
	messages := []map[string]interface{}{}
	for _, msg := range req.Contexts {
		role, _ := msg["role"].(string)
		if role == "system" {
			continue
		}
		messages = append(messages, anthropicMessage(msg))
	}
	messages = append(messages, anthropicMessage(req.ToUserMessage()))
	body["messages"] = anthropicSanitizeMessages(messages)

	// Convert OpenAI function schemas to Anthropic tool definitions.
	if len(req.Tools) > 0 {
		if tools := anthropicToolDefs(req.Tools); len(tools) > 0 {
			body["tools"] = tools
			// tool_choice 配置化（对齐 py _normalize_tool_choice）：优先取
			// 请求级 ToolChoice（对应 py text_chat 参数），其次 provider 配置
			// tool_choice，缺省 "auto"。
			toolChoice := req.ToolChoice
			if toolChoice == nil {
				toolChoice = s.Config()["tool_choice"]
			}
			if toolChoice == nil {
				toolChoice = "auto"
			}
			body["tool_choice"] = anthropicNormalizeToolChoice(toolChoice)
		}
	}
	// cache_control 断点（对齐 py _apply_explicit_prompt_cache_breakpoints）。
	s.applyExplicitPromptCacheBreakpoints(body)
	// thinking 配置（对齐 py _apply_thinking_config）。
	s.applyThinkingConfig(body)
	// custom_extra_body 最后合并（对齐 py extra_body 在 SDK 调用时注入，用户
	// 配置可覆盖上面的默认字段）。
	if extra, ok := s.Config()["custom_extra_body"].(map[string]interface{}); ok {
		for k, v := range extra {
			body[k] = v
		}
	}
	return body
}

// anthropicNormalizeToolChoice 将 tool_choice 归一化为 Anthropic API 格式
// （对齐 py anthropic_source._normalize_tool_choice）。
func anthropicNormalizeToolChoice(toolChoice interface{}) map[string]interface{} {
	if tc, ok := toolChoice.(map[string]interface{}); ok {
		return tc
	}
	tc, _ := toolChoice.(string)
	switch tc {
	case "required":
		// 兼容 OpenAI 命名：required → any
		return map[string]interface{}{"type": "any"}
	case "auto", "any", "none":
		return map[string]interface{}{"type": tc}
	case "tool":
		// {"type":"tool"} 必须配合 name 字段指定具体工具名，纯字符串无法指定，
		// 回退 auto（对齐 py）。
		logger.Warn("tool_choice='tool' 无法指定工具名，已回退为 'auto'")
		return map[string]interface{}{"type": "auto"}
	default:
		if tc != "" {
			logger.Warn("未知的 tool_choice 值: %s，已回退为 'auto'", tc)
		}
		return map[string]interface{}{"type": "auto"}
	}
}

// applyExplicitPromptCacheBreakpoints 给 system 列表最后一个 block 打上
// ephemeral 缓存断点（对齐 py _apply_explicit_prompt_cache_breakpoints）。
func (s *AnthropicSource) applyExplicitPromptCacheBreakpoints(body map[string]interface{}) {
	systemBlocks, ok := body["system"].([]map[string]interface{})
	if !ok || len(systemBlocks) == 0 {
		return
	}
	lastBlock := systemBlocks[len(systemBlocks)-1]
	if _, has := lastBlock["cache_control"]; !has {
		lastBlock["cache_control"] = map[string]interface{}{"type": "ephemeral"}
	}
}

// applyThinkingConfig 注入 Anthropic thinking 配置（对齐 py _apply_thinking_config）：
// adaptive 模式下发 {"type":"adaptive"} 并可带 output_config.effort；否则在配置了
// 非空 budget 时下发 {"type":"enabled","budget_tokens":budget}。
func (s *AnthropicSource) applyThinkingConfig(body map[string]interface{}) {
	if s.thinkingConfig == nil {
		return
	}
	thinkingType, _ := s.thinkingConfig["type"].(string)
	if thinkingType == "adaptive" {
		body["thinking"] = map[string]interface{}{"type": "adaptive"}
		effort, _ := s.thinkingConfig["effort"].(string)
		outputCfg, _ := body["output_config"].(map[string]interface{})
		if outputCfg == nil {
			outputCfg = map[string]interface{}{}
		}
		if effort != "" {
			outputCfg["effort"] = effort
		}
		if len(outputCfg) > 0 {
			body["output_config"] = outputCfg
		}
		return
	}
	if thinkingType != "" {
		return
	}
	if budget, ok := s.thinkingConfig["budget"]; ok && configTruthy(budget) {
		body["thinking"] = map[string]interface{}{
			"budget_tokens": budget,
			"type":          "enabled",
		}
	}
}

// configTruthy 复刻 Python 的真值判断（用于 budget 等可能为 0/空值的配置）。
func configTruthy(v interface{}) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case int:
		return t != 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	case string:
		return strings.TrimSpace(t) != "" && strings.TrimSpace(t) != "0"
	default:
		return false
	}
}
