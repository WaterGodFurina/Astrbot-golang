// Package sources implements concrete LLM provider sources.
// Ported from astrbot/core/provider/sources/
package sources

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2" // nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used -- #nosec G404: 429 换 key 随机挑选非安全场景
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// maxToolCallIndex caps the tool-call delta index so a malformed stream cannot
// trigger an out-of-range slice access or a huge allocation.
const maxToolCallIndex = 64

// maxRecoveryAttempts 外层恢复循环的最大尝试次数（对齐 py text_chat 的
// max_retries = 10）。
const maxRecoveryAttempts = 10

// mimoReasoningModels MiMo 推理模型（对齐 py _finally_convert_payload 的
// mimo_reasoning_models）：要求 assistant 历史消息必须回传 reasoning_content，
// 缺失时 API 返回 400。
var mimoReasoningModels = map[string]bool{
	"mimo-v2.5-pro": true,
	"mimo-v2.5":     true,
	"mimo-v2-pro":   true,
	"mimo-v2-omni":  true,
	"mimo-v2-flash": true,
}

// OpenAISource is an OpenAI-compatible chat provider.
// Ported from astrbot/core/provider/sources/openai_source.py
type OpenAISource struct {
	*provider.BaseProvider
	apiBase string
	apiKey  string
	// apiKeys 保存全部 key（对齐 py __init__ 的 self.api_keys = get_keys()）。
	// 429 换 key 重试时按请求局部副本挑选，不改动本字段，避免并发数据竞争。
	apiKeys      []string
	client       *http.Client
	streamClient *http.Client
	// extraHeaders are additional HTTP headers applied to every request.
	extraHeaders map[string]string
	// imageModerationPatterns 图片内容审核错误关键词（对齐 py
	// image_moderation_error_patterns 配置：大小写不敏感子串匹配，非正则）。
	// 命中时移除图片重试。
	imageModerationPatterns []string
	// reasoningKey 响应中推理内容的字段名（对齐 py reasoning_key）：默认
	// "reasoning_content"，Groq/OpenRouter 覆写为 "reasoning"。
	reasoningKey string
	// postProcessBody mutates the chat request body right before it is sent.
	// Thin OpenAI-compatible subclasses use this to customize payloads.
	postProcessBody func(body map[string]interface{})
}

// NewOpenAISource creates an OpenAI provider.
func NewOpenAISource(config, settings map[string]interface{}) *OpenAISource {
	bp := provider.NewBaseProvider(config, settings)
	s := &OpenAISource{
		BaseProvider: bp,
		reasoningKey: "reasoning_content",
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
		streamClient: newStreamClient(),
	}
	// timeout 配置（对齐 py __init__：provider_config["timeout"]，默认 120，
	// 字符串形态强转 int）。仅作用于非流式客户端；流式客户端沿用独立的
	// 10 分钟整体超时（避免长回复被掐断）。
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
	// proxy 配置（对齐 py _create_http_client：带代理的 HTTP 客户端，
	// 流式/非流式共用同一代理出口）。
	if proxy, _ := config["proxy"].(string); proxy != "" {
		if u, err := url.Parse(proxy); err == nil && u.Scheme != "" {
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.Proxy = http.ProxyURL(u)
			s.client.Transport = tr
			s.streamClient.Transport = tr
		}
	}
	// custom_headers 配置（对齐 py：非空 dict 时生效，值 str() 强转）。
	if ch, ok := config["custom_headers"].(map[string]interface{}); ok && len(ch) > 0 {
		s.extraHeaders = make(map[string]string, len(ch))
		for k, v := range ch {
			s.extraHeaders[k] = fmt.Sprintf("%v", v)
		}
	}
	// image_moderation_error_patterns 配置（对齐 py
	// _get_image_moderation_error_patterns：兼容单字符串或字符串数组，
	// 去空白、忽略空项与非字符串项）。
	s.imageModerationPatterns = parseImageModerationPatterns(config["image_moderation_error_patterns"])
	s.apiBase, _ = config["api_base"].(string)
	if s.apiBase == "" {
		s.apiBase = "https://api.openai.com/v1"
	}
	keys := s.getKeys()
	if len(keys) > 0 {
		s.apiKey = keys[0]
	}
	s.apiKeys = keys
	return s
}

func (s *OpenAISource) getKeys() []string {
	// 构造期解析配置后缓存，后续调用返回同一份只读快照（对齐 py
	// get_keys 返回 self.api_keys；避免每次重新解析配置）。
	if len(s.apiKeys) > 0 {
		return s.apiKeys
	}
	cfg := s.Config()
	// 兼容 keyring 数组形态与单字符串形态（DashScope 等子类直接用字符串
	// key 构造 OpenAISource 字面量，此时 apiKeys 未缓存）。
	if single, ok := cfg["key"].(string); ok && single != "" {
		return []string{single}
	}
	keysRaw, _ := cfg["key"].([]interface{})
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
	// 对齐 py openai_source.get_models：按模型 ID 排序后返回。
	sort.Strings(models)
	return models, nil
}

// TextChat sends a non-streaming chat request.
//
// 外层恢复循环（对齐 py text_chat 的 for retry_cnt in range(max_retries) +
// _handle_api_error）：429 限流 → 换用其他 key 重试；context-length 超限 →
// 弹最早两条非 system 记录重试；模型不支持 VLM/图片被审核/附件无效 → 去图片
// 重试；不支持 function calling → 去 tools 重试。
//
// key 轮换发生在本次请求局部：currentKey / availableKeys 由 newRecoveryState
// 初始化，每次请求都传 st.currentKey 给 doRequestWithKey，绝不改写 s.apiKey，
// 因此同一 OpenAISource 被多会话并发调用时无数据竞争。
func (s *OpenAISource) TextChat(ctx context.Context, req *provider.ProviderRequest) (*provider.LLMResponse, error) {
	st := newRecoveryState(s, req)
	body := s.buildRequestBody(req, false, st)
	for attempt := 0; attempt < maxRecoveryAttempts; attempt++ {
		resp, err := s.doRequestWithKey(ctx, body, s.client, st.currentKey)
		if err != nil {
			// retry429 对同一 key 重试耗尽后以 error 形式返回；若属 429 则换
			// key 再试（对齐 py：_query 抛出的异常进 _handle_api_error）。
			if s.handleRecoverableError(err.Error(), st) {
				body = s.buildRequestBody(req, false, st)
				continue
			}
			return nil, err
		}
		if resp.StatusCode == 200 {
			// py 中解析失败（含空输出）直接抛出，不做恢复
			llmResp, err := s.parseChatCompletion(resp.Body, len(req.Tools) > 0 && !st.toolsDisabled)
			_ = resp.Body.Close()
			return llmResp, err
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		_ = resp.Body.Close()
		errText := fmt.Sprintf("API error %d: %s", resp.StatusCode, truncate(string(respBody), 1024))
		if !s.handleRecoverableError(errText, st) {
			return &provider.LLMResponse{Role: "err", CompletionText: errText}, nil
		}
		// 恢复动作修改了 contexts/tools/currentKey，重建 payload 再重试
		body = s.buildRequestBody(req, false, st)
	}
	return &provider.LLMResponse{
		Role:           "err",
		CompletionText: fmt.Sprintf("API 调用失败，重试 %d 次仍然失败。", maxRecoveryAttempts),
	}, nil
}

// TextChatStream sends a streaming chat request (SSE).
// 外层恢复循环与 TextChat 一致（对齐 py text_chat_stream 的重试结构）。
func (s *OpenAISource) TextChatStream(ctx context.Context, req *provider.ProviderRequest) (<-chan *provider.LLMResponse, error) {
	st := newRecoveryState(s, req)
	body := s.buildRequestBody(req, true, st)
	var resp *http.Response
	for attempt := 0; attempt < maxRecoveryAttempts; attempt++ {
		r, err := s.doRequestWithKey(ctx, body, s.streamClient, st.currentKey)
		if err != nil {
			if s.handleRecoverableError(err.Error(), st) {
				body = s.buildRequestBody(req, true, st)
				continue
			}
			return nil, err
		}
		if r.StatusCode == 200 {
			resp = r
			break
		}
		respBody, _ := io.ReadAll(io.LimitReader(r.Body, 2048))
		_ = r.Body.Close()
		errText := fmt.Sprintf("API error %d: %s", r.StatusCode, truncate(string(respBody), 1024))
		if !s.handleRecoverableError(errText, st) {
			return nil, fmt.Errorf("%s", errText)
		}
		// 恢复动作修改了 contexts/tools/currentKey，重建 payload 再重试
		body = s.buildRequestBody(req, true, st)
	}
	if resp == nil {
		return nil, fmt.Errorf("API 调用失败，重试 %d 次仍然失败。", maxRecoveryAttempts)
	}

	ch := make(chan *provider.LLMResponse, 100)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		// 工具调用分片按 index 分桶累加，流结束后合并产出（对齐 py 的
		// ChatCompletionStreamState 语义）。
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
						// ReasoningAlt 是 Groq/OpenRouter 的推理字段名
						// （对齐 py reasoning_key="reasoning"）。
						ReasoningAlt string `json:"reasoning"`
						ToolCalls    []struct {
							// index 缺省时为 0，需用原始 JSON 判断是否真的缺省
							// （Gemini 兼容层/部分代理不回传 index，对齐 py #6661
							// 修复：缺 index 时按分片序号补齐）。
							Index    *int   `json:"index"`
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
					PromptTokens        int `json:"prompt_tokens"`
					CompletionTokens    int `json:"completion_tokens"`
					TotalTokens         int `json:"total_tokens"`
					PromptTokensDetails *struct {
						CachedTokens int `json:"cached_tokens"`
					} `json:"prompt_tokens_details"`
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
				// 对齐 py _extract_usage：prompt_tokens 扣除 cached_tokens 单独
				// 记账（input_other = prompt - cached，input_cached = cached）。
				cached := 0
				if chunk.Usage.PromptTokensDetails != nil {
					cached = chunk.Usage.PromptTokensDetails.CachedTokens
				}
				usage = &provider.TokenUsage{
					InputOther:  chunk.Usage.PromptTokens - cached,
					InputCached: cached,
					Output:      chunk.Usage.CompletionTokens,
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
				// 流式 reasoning 增量独立成 chunk 产出（display_reasoning_text
				// 依赖该事件形态，对齐 py _query_stream 的 yield 行为）。
				deltaReasoning := s.reasoningFromMessage(choice.Delta.Reasoning, choice.Delta.ReasoningAlt)
				if deltaReasoning != "" {
					reasoning.WriteString(deltaReasoning)
					ch <- &provider.LLMResponse{
						Role:             "assistant",
						IsChunk:          true,
						ReasoningContent: deltaReasoning,
					}
				}
				for i, tc := range choice.Delta.ToolCalls {
					// index 缺省时按分片在本次 delta 内的序号补齐（py #6661），
					// 显式回传的 index 照用。
					idx := 0
					if tc.Index != nil {
						idx = *tc.Index
					} else if len(choice.Delta.ToolCalls) > 0 {
						idx = i
					}
					if idx < 0 || idx >= maxToolCallIndex {
						logger.Warn("OpenAI stream: skipping tool call fragment with invalid index %d", idx)
						continue
					}
					for len(toolCalls) <= idx {
						toolCalls = append(toolCalls, toolAcc{})
					}
					if tc.ID != "" {
						toolCalls[idx].id = tc.ID
					}
					if tc.Function.Name != "" {
						toolCalls[idx].name = tc.Function.Name
					}
					toolCalls[idx].args += tc.Function.Arguments
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

// recoveryState 携带外层恢复循环的可变状态（对齐 py text_chat 循环里的
// payloads / context_query / image_fallback_used / func_tool 形参）。
type recoveryState struct {
	contexts          []map[string]interface{}
	imageFallbackUsed bool // 图片降级只允许做一次（py image_fallback_used）
	toolsDisabled     bool // function calling 降级后不再携带 tools（py func_tool=None）
	hasUserImage      bool // 当前用户消息是否带图（决定 VLM 降级是否有意义）
	dropUserImages    bool // 降级后重建用户消息时剔除图片块
	// 429 换 key 重试（对齐 py text_chat 的 chosen_key / available_api_keys）。
	// 两者均为本次请求的局部状态，跨并发请求不共享，故无需加锁；s.apiKeys /
	// s.apiKey 在重试期间只读，不被改写。
	currentKey    string
	availableKeys []string
}

// newRecoveryState 初始化恢复循环状态。key 轮换语义对齐 py：
//   - available_api_keys = self.api_keys.copy()
//   - chosen_key = random.choice(available_api_keys)
//
// 这里以当前 key 为起始 chosen_key，availableKeys 为其余 key 的副本；
// 首次 429 时再从中随机挑一个，效果与 py 一致（py 首次 429 会先把 chosen_key
// 从 available 移除，再去掉后剩余集合中 random.choice）。
func newRecoveryState(s *OpenAISource, req *provider.ProviderRequest) *recoveryState {
	all := s.getKeys()
	current := s.GetCurrentKey()
	if current == "" && len(all) > 0 {
		current = all[0]
	}
	return &recoveryState{
		contexts:      deepCopyContexts(req.Contexts),
		hasUserImage:  userRequestHasImage(req),
		currentKey:    current,
		availableKeys: keysExcluding(all, current),
	}
}

// keysExcluding 返回 keys 中去掉 exclude 后的新副本（不修改入参）。
func keysExcluding(keys []string, exclude string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if k != exclude {
			out = append(out, k)
		}
	}
	return out
}

// keyPreview 截断 key 用于日志（对齐 py chosen_key[:12]）。
func keyPreview(key string) string {
	if len(key) > 12 {
		return key[:12]
	}
	return key
}

// think 标签抽取（对齐 py _parse_openai_completion 的 reasoning_pattern）：
// 部分供应商在正文里包裹 <think>...</think> 表示推理内容。
var thinkTagRe = regexp.MustCompile(`(?s)<think>(.*?)</think>`)

var orphanThinkTailRe = regexp.MustCompile(`</think>\s*$`)

// deepCopyContexts 深拷贝上下文（对齐 py _prepare_chat_payload 的
// copy.deepcopy），恢复循环的就地修改不污染调用方历史。
func deepCopyContexts(msgs []map[string]interface{}) []map[string]interface{} {
	if msgs == nil {
		return nil
	}
	out := make([]map[string]interface{}, len(msgs))
	for i, m := range msgs {
		if m == nil {
			continue
		}
		if copied, ok := deepCopyValue(m).(map[string]interface{}); ok {
			out[i] = copied
		}
	}
	return out
}

// deepCopyValue 递归深拷贝 Go 原生结构，类型保持不变：map[string]interface{}、
// []map[string]interface{}、[]interface{} 递归复制，标量（string/number/bool/nil
// 等）直接返回。对齐 py copy.deepcopy 的语义，不改变容器类型。
func deepCopyValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, val := range t {
			out[k] = deepCopyValue(val)
		}
		return out
	case []map[string]interface{}:
		out := make([]map[string]interface{}, len(t))
		for i, val := range t {
			if val == nil {
				continue
			}
			out[i] = deepCopyValue(val).(map[string]interface{})
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, val := range t {
			out[i] = deepCopyValue(val)
		}
		return out
	default:
		return v
	}
}

// popOldestRecords 弹出前两条非 system 记录（对齐 py Provider.pop_record）。
func popOldestRecords(msgs []map[string]interface{}) []map[string]interface{} {
	popped := 0
	out := make([]map[string]interface{}, 0, len(msgs))
	for _, msg := range msgs {
		if role, _ := msg["role"].(string); role != "system" && popped < 2 {
			popped++
			continue
		}
		out = append(out, msg)
	}
	return out
}

// messagesContainImage 判断上下文是否含图片/音频块（对齐 py
// _context_contains_image：content 为 list 时检查 image_url / audio_url）。
func messagesContainImage(msgs []map[string]interface{}) bool {
	for _, msg := range msgs {
		var blocks []map[string]interface{}
		switch c := msg["content"].(type) {
		case []map[string]interface{}:
			blocks = c
		case []interface{}:
			blocks = contentAsBlocks(c)
		default:
			continue
		}
		for _, b := range blocks {
			if t, _ := b["type"].(string); t == "image_url" || t == "audio_url" {
				return true
			}
		}
	}
	return false
}

// userRequestHasImage 判断本次请求的用户消息是否携带图片。
func userRequestHasImage(req *provider.ProviderRequest) bool {
	if len(req.ImageURLs) > 0 {
		return true
	}
	for _, part := range req.ExtraUserContentParts {
		if t, _ := part["type"].(string); t == "image_url" {
			return true
		}
	}
	return false
}

// removeImagesFromContext 从上下文删除所有图片块（对齐 py
// _remove_image_from_context）；整条只剩图片时以 [图片] 占位保留文本语义。
func removeImagesFromContext(msgs []map[string]interface{}) {
	for _, msg := range msgs {
		var blocks []map[string]interface{}
		switch c := msg["content"].(type) {
		case []map[string]interface{}:
			blocks = c
		case []interface{}:
			blocks = contentAsBlocks(c)
		default:
			continue // 字符串等形态不含图片，原样保留
		}
		out := make([]map[string]interface{}, 0, len(blocks))
		for _, b := range blocks {
			if t, _ := b["type"].(string); t == "image_url" {
				continue
			}
			out = append(out, b)
		}
		if len(out) == 0 {
			// 用户只发了图片
			out = append(out, map[string]interface{}{"type": "text", "text": "[图片]"})
		}
		msg["content"] = out
	}
}

// stripUserImages 从当前用户消息剔除图片块（py 中当前消息本就是
// context_query 的一部分，去图降级同样作用其上；Go 侧两者分开组装，
// 故单独处理）。
func stripUserImages(msg map[string]interface{}) {
	var blocks []map[string]interface{}
	switch c := msg["content"].(type) {
	case []map[string]interface{}:
		blocks = c
	case []interface{}:
		blocks = contentAsBlocks(c)
	default:
		return
	}
	out := make([]map[string]interface{}, 0, len(blocks))
	for _, b := range blocks {
		if t, _ := b["type"].(string); t == "image_url" {
			continue
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		out = append(out, map[string]interface{}{"type": "text", "text": "[图片]"})
	}
	msg["content"] = out
}

// parseImageModerationPatterns 解析 image_moderation_error_patterns 配置
// （对齐 py _get_image_moderation_error_patterns：兼容单字符串或字符串
// 数组，去首尾空白、忽略空项与非字符串项）。
func parseImageModerationPatterns(raw interface{}) []string {
	var items []interface{}
	switch v := raw.(type) {
	case string:
		items = []interface{}{v}
	case []interface{}:
		items = v
	case []string:
		items = make([]interface{}, len(v))
		for i, s := range v {
			items[i] = s
		}
	default:
		return nil
	}
	patterns := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			patterns = append(patterns, s)
		}
	}
	return patterns
}

// isContentModeratedUploadError 判定错误是否为图片内容审核拦截（对齐 py
// _is_content_moderated_upload_error）：未配置任何 pattern 时恒为 false；
// 否则对错误文本做大小写不敏感子串匹配（非正则），命中任一即 true。
func (s *OpenAISource) isContentModeratedUploadError(errText string) bool {
	if len(s.imageModerationPatterns) == 0 {
		return false
	}
	lower := strings.ToLower(errText)
	for _, p := range s.imageModerationPatterns {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// handleRecoverableError 按错误文本判定是否可恢复，并就地执行恢复动作
// （对齐 py _handle_api_error，按 py 的分支顺序）：429 换 key → context-length
// 弹记录 → VLM/审核/附件降级 → 去 tools。返回 false 表示不可恢复。
//
// 429 分支：从 availableKeys 中移除 currentKey，若仍有剩余则随机换 key 并
// return true（对齐 py _handle_api_error 的 429 分支）；否则 return false。
// key 只写入本次请求的 recoveryState，不触碰 s.apiKey。
func (s *OpenAISource) handleRecoverableError(errText string, st *recoveryState) bool {
	lower := strings.ToLower(errText)
	if strings.Contains(errText, "429") {
		logger.Warn("API 调用过于频繁，尝试使用其他 Key 重试。当前 Key: %s", keyPreview(st.currentKey))
		remaining := keysExcluding(st.availableKeys, st.currentKey)
		if len(remaining) == 0 {
			return false
		}
		// 对齐 py _handle_api_error：非最后一次换 key 前等待 1s，避免瞬时空转。
		time.Sleep(time.Second)
		st.availableKeys = remaining
		st.currentKey = remaining[rand.IntN(len(remaining))]
		return true
	}
	if strings.Contains(lower, "maximum context length") || strings.Contains(lower, "context length") {
		logger.Warn("上下文长度超过限制。尝试弹出最早的记录然后重试。当前记录条数: %d", len(st.contexts))
		st.contexts = popOldestRecords(st.contexts)
		return true
	}
	if strings.Contains(lower, "the model is not a vlm") { // siliconcloud
		if st.imageFallbackUsed || (!messagesContainImage(st.contexts) && !st.hasUserImage) {
			return false
		}
		logger.Warn("检测到图片请求失败（model_not_vlm），已移除图片并重试（保留文本内容）。")
		removeImagesFromContext(st.contexts)
		st.dropUserImages = true
		st.imageFallbackUsed = true
		return true
	}
	// image_moderation_error_patterns 配置命中：图片被内容审核拦截
	// （对齐 py 的 image_content_moderated 分支，位置在 VLM 之后、
	// invalid_attachment 之前）。
	if s.isContentModeratedUploadError(errText) {
		if st.imageFallbackUsed || (!messagesContainImage(st.contexts) && !st.hasUserImage) {
			return false
		}
		logger.Warn("检测到图片内容审核拦截（image_content_moderated），已移除图片并重试（保留文本内容）。")
		removeImagesFromContext(st.contexts)
		st.dropUserImages = true
		st.imageFallbackUsed = true
		return true
	}
	if strings.Contains(lower, "invalid_attachment") ||
		(strings.Contains(lower, "download attachment") && strings.Contains(lower, "404")) {
		if st.imageFallbackUsed || (!messagesContainImage(st.contexts) && !st.hasUserImage) {
			return false
		}
		logger.Warn("检测到图片请求失败（invalid_attachment），已移除图片并重试。")
		removeImagesFromContext(st.contexts)
		st.dropUserImages = true
		st.imageFallbackUsed = true
		return true
	}
	// openai/ollama/gemini openai/siliconcloud 的错误提示与 code 不统一，
	// 只能通过字符串匹配（对齐 py 注释）。
	if strings.Contains(lower, "function calling is not enabled") ||
		(strings.Contains(lower, "tool") && strings.Contains(lower, "support")) ||
		(strings.Contains(lower, "function") && strings.Contains(lower, "support")) {
		logger.Warn("%s 不支持函数工具调用，已自动去除，不影响使用。如需永久关闭，可前往 WebUI 中关闭工具调用。", s.GetModel())
		st.toolsDisabled = true
		return true
	}
	return false
}

// reasoningFromMessage 按配置的 reasoningKey 取响应中的推理内容（对齐 py
// reasoning_key：默认 "reasoning_content"，Groq/OpenRouter 为 "reasoning"）。
// 优先返回配置字段，缺省时回退另一字段，兼容代理混用两种命名。
func (s *OpenAISource) reasoningFromMessage(reasoningContent, reasoning string) string {
	if s.reasoningKey == "reasoning" {
		if reasoning != "" {
			return reasoning
		}
		return reasoningContent
	}
	if reasoningContent != "" {
		return reasoningContent
	}
	return reasoning
}

// parseChatCompletion 解析非流式响应（对齐 py _parse_openai_completion）：
// <think> 标签抽取为 reasoning、list 形态 content 归一化、usage 扣除
// cached_tokens；withTools 为 false 时不解析 tool_calls（py tools=None 语义）。
func (s *OpenAISource) parseChatCompletion(r io.Reader, withTools bool) (*provider.LLMResponse, error) {
	var result struct {
		Choices []struct {
			Message struct {
				Role             string          `json:"role"`
				Content          json.RawMessage `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				// Reasoning 是 Groq/OpenRouter 的推理字段名（py reasoning_key）。
				Reasoning string `json:"reasoning"`
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
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(r).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(result.Choices) == 0 {
		return nil, fmt.Errorf("openai completion has no usable output")
	}
	choice := result.Choices[0]
	completionText := normalizeContentText(choice.Message.Content)
	var reasoningContent string
	// <think> 标签抽取为 reasoning_content（对齐 py）
	if matches := thinkTagRe.FindAllStringSubmatch(completionText, -1); len(matches) > 0 {
		parts := make([]string, 0, len(matches))
		for _, m := range matches {
			parts = append(parts, strings.TrimSpace(m[1]))
		}
		reasoningContent = strings.Join(parts, "\n")
		completionText = strings.TrimSpace(thinkTagRe.ReplaceAllString(completionText, ""))
	}
	// 清理部分模型泄漏的孤立 </think> 尾标签
	completionText = strings.TrimSpace(orphanThinkTailRe.ReplaceAllString(completionText, ""))
	// API 显式回传的 reasoning_content 优先级更高（对齐 py）
	if rc := s.reasoningFromMessage(choice.Message.ReasoningContent, choice.Message.Reasoning); rc != "" {
		reasoningContent = rc
	}
	cached := 0
	if result.Usage.PromptTokensDetails != nil {
		cached = result.Usage.PromptTokensDetails.CachedTokens
	}
	llmResp := &provider.LLMResponse{
		Role:             choice.Message.Role,
		CompletionText:   completionText,
		ReasoningContent: reasoningContent,
		Usage: &provider.TokenUsage{
			InputOther:  result.Usage.PromptTokens - cached,
			InputCached: cached,
			Output:      result.Usage.CompletionTokens,
		},
	}
	if withTools {
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
	}
	logger.Debug("LLM response: text_len=%d", len(llmResp.CompletionText))
	return llmResp, nil
}

// normalizeContentText 归一化 API 返回的 message.content（对齐 py
// _normalize_content）：dict 取 text 字段；list 形态（[{'type':'text',...}]）
// 拼接 text 部件、无 text 部件时返回 Python repr；字符串本身是 JSON 编码的
// content-part 数组时先 json 解析、失败再按 Python 字面量解析（单引号兜底）。
func normalizeContentText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return ""
		}
		return normalizeStringContent(s)
	case '{':
		var v interface{}
		if err := json.Unmarshal(trimmed, &v); err != nil {
			return ""
		}
		return normalizeContentValue(v)
	case '[':
		var v interface{}
		if err := json.Unmarshal(trimmed, &v); err == nil {
			return normalizeContentValue(v)
		}
		if lit, ok := parsePythonLiteral(string(trimmed)); ok {
			return normalizeContentValue(lit)
		}
		return ""
	default:
		// 其他类型（数字/布尔等）的兜底：py 为 str(raw_content)
		var v interface{}
		if err := json.Unmarshal(trimmed, &v); err != nil || v == nil {
			return ""
		}
		return pythonTextValue(v)
	}
}

// normalizeContentValue 处理已解析的 content 值（对齐 py _normalize_content
// 的 dict / list 分支）。
func normalizeContentValue(v interface{}) string {
	switch t := v.(type) {
	case map[string]interface{}:
		if tv, ok := t["text"]; ok {
			if tv == nil {
				return ""
			}
			return pythonTextValue(tv)
		}
		logger.Warn("Unexpected dict format content: %v", t)
		return ""
	case []interface{}:
		hasText := false
		for _, p := range t {
			if m, ok := p.(map[string]interface{}); ok {
				if typ, _ := m["type"].(string); typ == "text" {
					hasText = true
					break
				}
			}
		}
		if hasText {
			var b strings.Builder
			for _, p := range t {
				if m, ok := p.(map[string]interface{}); ok {
					if typ, _ := m["type"].(string); typ == "text" {
						if tv, exists := m["text"]; exists && tv != nil {
							b.WriteString(pythonTextValue(tv))
						}
					}
				}
			}
			return b.String()
		}
		// 非 content-part 格式，返回 Python 风格的 str(list)
		return pythonRepr(t)
	default:
		if v == nil {
			return ""
		}
		return pythonRepr(v)
	}
}

// normalizeStringContent 处理字符串形态 content（对齐 py _normalize_content 的
// str 分支）：若整串看起来是 JSON 数组（[ ... ] 且 < 8192 字符）则尝试解析——
// 先标准 JSON，失败后用 Python 字面量解析兜底单引号；解析为 list 且含 text
// 部件时拼接，否则原样返回字符串。
func normalizeStringContent(s string) string {
	content := strings.TrimSpace(s)
	check := strings.TrimSpace(s)
	if strings.HasPrefix(check, "[") && strings.HasSuffix(check, "]") && len(check) < 8192 {
		var parsed interface{}
		if err := json.Unmarshal([]byte(check), &parsed); err != nil {
			parsed, _ = parsePythonLiteral(check)
		}
		if list, ok := parsed.([]interface{}); ok {
			hasText := false
			for _, p := range list {
				if m, ok := p.(map[string]interface{}); ok {
					if typ, _ := m["type"].(string); typ == "text" {
						hasText = true
						break
					}
				}
			}
			if hasText {
				var b strings.Builder
				for _, p := range list {
					if m, ok := p.(map[string]interface{}); ok {
						if typ, _ := m["type"].(string); typ == "text" {
							if tv, exists := m["text"]; exists && tv != nil {
								b.WriteString(pythonTextValue(tv))
							}
						}
					}
				}
				return b.String()
			}
		}
	}
	return content
}

// pythonTextValue 模拟 Python str(v)（用于 text 字段的强制字符串化）。
func pythonTextValue(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	default:
		return pythonRepr(v)
	}
}

// pythonRepr 生成近似 Python repr 的字符串：字符串单引号、True/False/None、
// dict {'k': v}、list [a, b]。dict 的键序在 Go map 中不可保留，这里排序后
// 输出以保证确定性（py dict 保留插入序，属已知的轻微偏差）。
func pythonRepr(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return pythonQuoteString(t)
	case bool:
		if t {
			return "True"
		}
		return "False"
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return pythonFloatRepr(t)
	case float32:
		return pythonFloatRepr(float64(t))
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(pythonQuoteString(k))
			b.WriteString(": ")
			b.WriteString(pythonRepr(t[k]))
		}
		b.WriteByte('}')
		return b.String()
	case []interface{}:
		var b strings.Builder
		b.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(pythonRepr(item))
		}
		b.WriteByte(']')
		return b.String()
	case []map[string]interface{}:
		var b strings.Builder
		b.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(pythonRepr(item))
		}
		b.WriteByte(']')
		return b.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

// pythonQuoteString 按 Python repr 的规则用单引号包裹并转义（至少覆盖单引号、
// 反斜杠、换行、回车、制表符）。
func pythonQuoteString(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// pythonFloatRepr 输出 Python repr 风格的浮点（整数浮点补 .0）。
func pythonFloatRepr(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// parsePythonLiteral 解析 ast.literal_eval 的子集，足以覆盖 content-part 数组
// 场景：单/双引号字符串（含常见转义）、None/True/False、整数/浮点、list、dict。
// 整串必须被完整消费；失败返回 (nil, false)。
func parsePythonLiteral(s string) (interface{}, bool) {
	p := &pyLiteralParser{src: s}
	p.skipSpace()
	v, ok := p.parseValue()
	if !ok {
		return nil, false
	}
	p.skipSpace()
	if p.pos != len(p.src) {
		return nil, false
	}
	return v, true
}

type pyLiteralParser struct {
	src string
	pos int
}

func (p *pyLiteralParser) skipSpace() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			p.pos++
		default:
			return
		}
	}
}

func (p *pyLiteralParser) parseValue() (interface{}, bool) {
	if p.pos >= len(p.src) {
		return nil, false
	}
	c := p.src[p.pos]
	switch {
	case c == '\'' || c == '"':
		return p.parseString()
	case c == '[':
		return p.parseList()
	case c == '{':
		return p.parseDict()
	case c == '-' || c == '+' || c == '.' || (c >= '0' && c <= '9'):
		return p.parseNumber()
	case p.hasKeyword("True"):
		p.pos += 4
		return true, true
	case p.hasKeyword("False"):
		p.pos += 5
		return false, true
	case p.hasKeyword("None"):
		p.pos += 4
		return nil, true
	}
	return nil, false
}

func (p *pyLiteralParser) hasKeyword(kw string) bool {
	if !strings.HasPrefix(p.src[p.pos:], kw) {
		return false
	}
	end := p.pos + len(kw)
	if end < len(p.src) {
		c := p.src[end]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func (p *pyLiteralParser) parseList() (interface{}, bool) {
	p.pos++ // consume '['
	out := []interface{}{}
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == ']' {
		p.pos++
		return out, true
	}
	for {
		p.skipSpace()
		v, ok := p.parseValue()
		if !ok {
			return nil, false
		}
		out = append(out, v)
		p.skipSpace()
		if p.pos >= len(p.src) {
			return nil, false
		}
		switch p.src[p.pos] {
		case ',':
			p.pos++
			p.skipSpace()
			if p.pos < len(p.src) && p.src[p.pos] == ']' {
				p.pos++
				return out, true
			}
		case ']':
			p.pos++
			return out, true
		default:
			return nil, false
		}
	}
}

func (p *pyLiteralParser) parseDict() (interface{}, bool) {
	p.pos++ // consume '{'
	out := map[string]interface{}{}
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == '}' {
		p.pos++
		return out, true
	}
	for {
		p.skipSpace()
		key, ok := p.parseValue()
		if !ok {
			return nil, false
		}
		ks, ok := key.(string)
		if !ok {
			return nil, false
		}
		p.skipSpace()
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return nil, false
		}
		p.pos++
		p.skipSpace()
		val, ok := p.parseValue()
		if !ok {
			return nil, false
		}
		out[ks] = val
		p.skipSpace()
		if p.pos >= len(p.src) {
			return nil, false
		}
		switch p.src[p.pos] {
		case ',':
			p.pos++
			p.skipSpace()
			if p.pos < len(p.src) && p.src[p.pos] == '}' {
				p.pos++
				return out, true
			}
		case '}':
			p.pos++
			return out, true
		default:
			return nil, false
		}
	}
}

func (p *pyLiteralParser) parseNumber() (interface{}, bool) {
	start := p.pos
	if p.src[p.pos] == '+' || p.src[p.pos] == '-' {
		p.pos++
	}
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		isNumeric := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') ||
			(c >= 'A' && c <= 'F') || c == '.' || c == 'e' || c == 'E' ||
			c == '+' || c == '-' || c == 'x' || c == 'X'
		if !isNumeric {
			break
		}
		p.pos++
	}
	tok := p.src[start:p.pos]
	if tok == "" || tok == "+" || tok == "-" || tok == "." {
		return nil, false
	}
	if i, err := strconv.ParseInt(tok, 0, 64); err == nil {
		return int64(i), true
	}
	if u, err := strconv.ParseUint(tok, 0, 64); err == nil {
		return int64(u), true
	}
	if f, err := strconv.ParseFloat(tok, 64); err == nil {
		return f, true
	}
	return nil, false
}

func (p *pyLiteralParser) parseString() (interface{}, bool) {
	quote := p.src[p.pos]
	p.pos++
	var b strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '\\' {
			p.pos++
			if p.pos >= len(p.src) {
				return nil, false
			}
			switch e := p.src[p.pos]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '\\':
				b.WriteByte('\\')
			case '\'':
				b.WriteByte('\'')
			case '"':
				b.WriteByte('"')
			case '0':
				b.WriteByte(0)
			case 'x':
				if p.pos+2 >= len(p.src) {
					return nil, false
				}
				n, err := strconv.ParseUint(p.src[p.pos+1:p.pos+3], 16, 8)
				if err != nil {
					return nil, false
				}
				b.WriteByte(byte(n))
				p.pos += 2
			case 'u':
				if p.pos+4 >= len(p.src) {
					return nil, false
				}
				n, err := strconv.ParseUint(p.src[p.pos+1:p.pos+5], 16, 32)
				if err != nil {
					return nil, false
				}
				b.WriteRune(rune(n))
				p.pos += 4
			default:
				b.WriteByte(e)
			}
			p.pos++
			continue
		}
		if c == quote {
			p.pos++
			return b.String(), true
		}
		b.WriteByte(c)
		p.pos++
	}
	return nil, false
}

// sanitizeContexts filters and normalizes assistant messages before send.
// Strict APIs (Moonshot, DeepSeek Reasoner) reject assistant messages lacking
// both content and tool_calls; context truncation/compression also leaves
// orphaned tool messages behind. Ported from _sanitize_assistant_messages in
// openai_source.py.
//
// 对齐 py 语义：
//   - assistant 的 list 形态 content（含 think 部件）整条保留，think 部件的
//     抽取交给 finallyConvertPayload（py 中是先后两个独立步骤）；
//   - content 为空但有 reasoning_content 的 assistant 消息保留（推理模型
//     需要回传 reasoning 历史），content 置空串占位满足严格 API 校验。
func sanitizeContexts(msgs []map[string]interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(msgs))
	pendingToolIDs := map[string]bool{}
	for _, msg := range msgs {
		role, _ := msg["role"].(string)
		if role == "assistant" {
			contentEmpty := contentIsEmpty(msg["content"])
			hasToolCalls := len(toolCallsSlice(msg["tool_calls"])) > 0
			hasReasoning := false
			if rc, _ := msg["reasoning_content"].(string); rc != "" {
				hasReasoning = true
			}
			if contentEmpty && !hasToolCalls && !hasReasoning {
				continue // content/tool_calls/reasoning 三者全空，真垃圾消息，丢弃
			}
			copied := cloneMap(msg)
			if contentEmpty && hasToolCalls {
				copied["content"] = nil // 有 tool_calls，按 OpenAI 规范
			} else if contentEmpty && hasReasoning {
				copied["content"] = "" // 空 content 占位，满足严格 API 校验
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

// contentIsEmpty 判断消息 content 是否为空（对齐 py _is_empty：None / "" /
// [] 均视作空；非空 list 形态（含 think 部件）视为有内容）。
func contentIsEmpty(content interface{}) bool {
	switch c := content.(type) {
	case nil:
		return true
	case string:
		return c == ""
	case []map[string]interface{}:
		return len(c) == 0
	case []interface{}:
		return len(c) == 0
	default:
		return false
	}
}

// imageRefToDataURL resolves a user-supplied image reference into an inline
// base64 data URL the remote API can decode. Mirrors astrbot-py's
// openai_source._image_ref_to_data_url (safe mode): already-inline data URIs
// pass through unchanged; local file path / file:// / http(s) references are
// fetched via fetchMediaData. Returns "" when the reference cannot be
// resolved — the caller drops the part (image ignored) instead of failing.
func imageRefToDataURL(ref string) string {
	trimmed := strings.TrimSpace(ref)
	if strings.HasPrefix(trimmed, "data:") {
		return trimmed
	}
	data, mediaType, err := fetchMediaData(trimmed)
	if err != nil {
		logger.Warn("OpenAI: 加载图片 %q 失败，忽略该图片: %v", trimmed, err)
		return ""
	}
	if mediaType == "" {
		mediaType = "image/jpeg"
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// resolveOpenAIImageParts converts every image_url reference in the assembled
// messages into an inline base64 data URL (astrbot-py parity). A local
// file:// / path reference would otherwise be sent verbatim to the remote
// OpenAI-compatible API, which cannot read host-local files (GLM/Zhipu 看不了
// 图片). Unresolvable parts are dropped so a stale path never breaks the chat
// request.
func resolveOpenAIImageParts(messages []map[string]interface{}) {
	for _, msg := range messages {
		content, ok := msg["content"].([]map[string]interface{})
		if !ok {
			continue
		}
		out := content[:0]
		for _, part := range content {
			if typ, _ := part["type"].(string); typ == "image_url" {
				if img, ok := part["image_url"].(map[string]interface{}); ok {
					if u, _ := img["url"].(string); u != "" {
						if resolved := imageRefToDataURL(u); resolved != "" {
							img["url"] = resolved
							out = append(out, part)
							continue
						}
					}
				}
				logger.Warn("OpenAI: 图片引用无效或无法解析，忽略该图片块")
				continue
			}
			out = append(out, part)
		}
		msg["content"] = out
	}
}

// buildRequestBody 组装请求 payload。st 携带恢复循环状态（去图降级、
// tools 降级），非恢复路径传 st 即可（TextChat/TextChatStream 均已接入）。
func (s *OpenAISource) buildRequestBody(req *provider.ProviderRequest, stream bool, st *recoveryState) map[string]interface{} {
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
	messages = append(messages, st.contexts...)
	// Add current user message
	userMsg := req.ToUserMessage()
	// VLM 降级：用户消息同样剔除图片（py 中当前消息本就是 context_query
	// 的一部分，去图降级天然作用其上；Go 侧分开组装故单独处理）
	if st.dropUserImages {
		stripUserImages(userMsg)
	}
	messages = append(messages, userMsg)
	// 本地/相对 image_url 引用统一转 inline data URI（GLM/Zhipu 等远端 API
	// 读不到宿主本地路径）
	resolveOpenAIImageParts(messages)

	// py 顺序：_prepare_chat_payload 末尾先跑 _finally_convert_payload，
	// _query 里最后跑 _sanitize_assistant_messages。think 抽取可能把
	// content 变空，随后由 sanitize 补占位，顺序不能反。
	s.finallyConvertMessages(messages)
	messages = sanitizeContexts(messages)
	body["messages"] = messages

	// Add tools if present（function calling 降级后不再携带，对齐 py
	// payloads.pop("tools")）
	if len(req.Tools) > 0 && !st.toolsDisabled {
		body["tools"] = req.Tools
	}

	// custom_extra_body 配置合并（对齐 py _query：extra_body.update(
	// custom_extra_body)，浅合并、配置值覆盖同名默认字段）
	if extra, ok := s.Config()["custom_extra_body"].(map[string]interface{}); ok {
		for k, v := range extra {
			body[k] = v
		}
	}

	// Let thin OpenAI-compatible subclasses customize the payload before send.
	if s.postProcessBody != nil {
		s.postProcessBody(body)
	}

	return body
}

// finallyConvertMessages 请求前对 messages 做最终转换（对齐 py
// _finally_convert_payload）：
//   - assistant list 形态 content 中的 think 部件抽出为 reasoning_content，
//     其余部件保留；全为 think 时 content 置 nil（Grok 等拒绝空数组）；
//   - deepseek v4 / MiMo 推理模型：assistant 历史消息缺 reasoning_content
//     时补空串（缺失时 API 返回 400）；
//   - gemini 兼容层：tool 消息 content 若非合法 JSON，包成 {"result": ...}。
func (s *OpenAISource) finallyConvertMessages(messages []map[string]interface{}) {
	model := strings.ToLower(s.GetModel())
	isGemini := strings.Contains(model, "gemini")
	// deepseek-chat / deepseek-reasoner 现已指向 V4 模型（官方说明）
	isDeepseekV4Reasoning := strings.Contains(model, "deepseek-v4") ||
		strings.Contains(s.apiBase, "api.deepseek.com")
	isMimoReasoning := mimoReasoningModels[model]

	msgs := messages
	for _, message := range msgs {
		role, _ := message["role"].(string)
		if role != "assistant" && role != "tool" {
			continue
		}
		if role == "assistant" {
			// content 为 list 形态才做 think 抽取（py isinstance(list)）；
			// 内存形态是 []map，外部插件/JSON 入参可能给 []interface{}，
			// 两种都接受（与 deepCopyContexts 无关，后者保持类型不变）。
			var blocks []map[string]interface{}
			switch c := message["content"].(type) {
			case []map[string]interface{}:
				blocks = c
			case []interface{}:
				blocks = contentAsBlocks(c)
			default:
				blocks = nil
			}
			if blocks != nil {
				var reasoning strings.Builder
				reasoningPresent := false
				newContent := make([]map[string]interface{}, 0, len(blocks))
				for _, part := range blocks {
					if t, _ := part["type"].(string); t == "think" {
						reasoningPresent = true
						if th, ok := part["think"].(string); ok {
							reasoning.WriteString(th)
						}
						continue
					}
					newContent = append(newContent, part)
				}
				// 部分供应商（Grok 等）拒绝空 content 数组，全为 think 时置 nil
				if reasoningPresent {
					if len(newContent) > 0 {
						message["content"] = newContent
					} else {
						message["content"] = nil
					}
					message["reasoning_content"] = reasoning.String()
				}
			}
			if (isDeepseekV4Reasoning || isMimoReasoning) && message["reasoning_content"] == nil {
				// 推理模型要求 assistant 历史消息回传 reasoning_content 字段，
				// 即使推理内容为空
				message["reasoning_content"] = ""
			}
			continue
		}
		// Gemini 的 function_response 要求 JSON 对象，纯文本会触发
		// 400 Invalid argument，需包一层 JSON（对齐 py）
		if isGemini {
			if content, ok := message["content"].(string); ok {
				var js interface{}
				if err := json.Unmarshal([]byte(content), &js); err != nil {
					wrapped, _ := json.Marshal(map[string]interface{}{"result": content})
					message["content"] = string(wrapped)
				}
			}
		}
	}
}

// doRequest 保留原签名（走 s.apiKey），兼容 GetCurrentKey/SetKey/GetModels 等
// 既有调用点以及 OpenAI 兼容子类。
func (s *OpenAISource) doRequest(ctx context.Context, body map[string]interface{}, client *http.Client) (*http.Response, error) {
	return s.doRequestWithKey(ctx, body, client, s.apiKey)
}

// doRequestWithKey 用指定 key 发送请求（对齐 py 请求前 self.client.api_key =
// chosen_key）。key 通过闭包固化到本次请求，DoWithRetry / retry429 每次重建
// 请求都用同一个 key，Authorization 头不再读取 s.apiKey，避免 429 换 key
// 时与其他并发请求争用共享字段。
func (s *OpenAISource) doRequestWithKey(ctx context.Context, body map[string]interface{}, client *http.Client, key string) (*http.Response, error) {
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
		httpReq.Header.Set("Authorization", "Bearer "+key)
		for k, v := range s.extraHeaders {
			httpReq.Header.Set(k, v)
		}
		return httpReq, nil
	}, cfg, "OpenAI")
}
