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
	"strconv"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// 对齐 Python #9881：Gemini thinking level 允许集合与回退值。
var geminiThinkingLevels = []string{"MINIMAL", "LOW", "MEDIUM", "HIGH"}

// GeminiSource is a Google Gemini chat provider.
type GeminiSource struct {
	*provider.BaseProvider
	apiBase        string
	apiKey         string
	client         *http.Client
	streamClient   *http.Client
	thinkingConfig map[string]interface{}
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
	s.apiBase = strings.TrimSpace(s.apiBase)
	if s.apiBase == "" {
		s.apiBase = "https://generativelanguage.googleapis.com"
	}
	// api_base 归一（对齐 py __init__ rstrip("/") + google-genai 默认 /v1beta）：
	// 去掉尾斜杠（WebUI 模板默认值 ".../generativelanguage.googleapis.com/" 会
	// 让 "%s/models/..." 拼出双斜杠 URL），缺版本段时补 /v1beta。
	s.apiBase = strings.TrimRight(s.apiBase, "/")
	if !strings.HasSuffix(s.apiBase, "/v1beta") && !strings.HasSuffix(s.apiBase, "/v1") {
		s.apiBase += "/v1beta"
	}
	if key, ok := config["key"].(string); ok {
		s.apiKey = key
	}
	if keys, ok := config["key"].([]interface{}); ok && len(keys) > 0 {
		if k, ok := keys[0].(string); ok {
			s.apiKey = k
		}
	}
	if tc, ok := config["gm_thinking_config"].(map[string]interface{}); ok {
		s.thinkingConfig = tc
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
			PromptTokenCount        int `json:"promptTokenCount"`
			CandidatesTokenCount    int `json:"candidatesTokenCount"`
			TotalTokenCount         int `json:"totalTokenCount"`
			CachedContentTokenCount int `json:"cachedContentTokenCount"`
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
	// prompt_token_count 包含从缓存服务的 token，需减去 cached_content_token_count
	// 以避免重复计算缓存 input（对齐 Python #9881）。
	promptTokens := result.UsageMetadata.PromptTokenCount
	cached := result.UsageMetadata.CachedContentTokenCount
	llmResp := &provider.LLMResponse{
		Role:           "assistant",
		CompletionText: text,
		Usage: &provider.TokenUsage{
			InputOther:  promptTokens - cached,
			InputCached: cached,
			Output:      result.UsageMetadata.CandidatesTokenCount,
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
					PromptTokenCount        int `json:"promptTokenCount"`
					CandidatesTokenCount    int `json:"candidatesTokenCount"`
					CachedContentTokenCount int `json:"cachedContentTokenCount"`
				} `json:"usageMetadata"`
			}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				return false
			}
			if chunk.UsageMetadata != nil {
				// 对齐 Python #9881：减去 cached_content_token_count 避免重复计算缓存 input。
				pt := chunk.UsageMetadata.PromptTokenCount
				cc := chunk.UsageMetadata.CachedContentTokenCount
				usage = &provider.TokenUsage{
					InputOther:  pt - cc,
					InputCached: cc,
					Output:      chunk.UsageMetadata.CandidatesTokenCount,
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
		var parts []map[string]interface{}
		geminiRole := "user"
		switch role {
		case "assistant":
			geminiRole = "model"
			parts = geminiPartsFromContent(msg["content"])
			// 工具循环历史：tool_calls 转 functionCall parts（对齐 Python
			// gemini_source），否则第二轮请求起 functionCall 缺失，函数调用断裂。
			for _, tc := range toolCallsSlice(msg["tool_calls"]) {
				fn, _ := tc["function"].(map[string]interface{})
				if fn == nil {
					continue
				}
				name, _ := fn["name"].(string)
				if name == "" {
					continue
				}
				var args map[string]interface{}
				if argsRaw, ok := fn["arguments"].(string); ok && argsRaw != "" {
					_ = json.Unmarshal([]byte(argsRaw), &args)
				}
				if args == nil {
					args = map[string]interface{}{}
				}
				parts = append(parts, map[string]interface{}{
					"functionCall": map[string]interface{}{"name": name, "args": args},
				})
			}
		case "tool":
			// 工具结果 → user 角色的 functionResponse part（对齐 Python：
			// func_name 取 name 字段，缺省回退 tool_call_id）。
			funcName, _ := msg["name"].(string)
			if funcName == "" {
				funcName, _ = msg["tool_call_id"].(string)
			}
			parts = []map[string]interface{}{{
				"functionResponse": map[string]interface{}{
					"name": funcName,
					"response": map[string]interface{}{
						"name":    funcName,
						"content": msg["content"],
					},
				},
			}}
		default:
			// Convert array content blocks (text / image_url / audio_url) into
			// Gemini parts; string content becomes a single text part.
			parts = geminiPartsFromContent(msg["content"])
		}
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
				// OpenAI JSON Schema → Gemini Schema 转换（对齐 py ToolSet.google_schema
				// 的 convert_schema）：type 大写枚举、裁剪 allowlist 字段、type 列表取
				// 首个非 null、递归 properties/items。原样透传会因小写 type 与多余字段
				// 被 REST API 拒绝（400）。
				decl["parameters"] = geminiConvertSchema(params)
			}
			funcDecls = append(funcDecls, decl)
		}
		if len(funcDecls) > 0 {
			body["tools"] = []map[string]interface{}{
				{"functionDeclarations": funcDecls},
			}
		}
	}
	// 对齐 Python #9881/_prepare_query_config：按模型系列门控注入 thinking
	// （2.5 系列 thinkingBudget，3.x thinkingLevel）。
	s.applyThinkingConfig(body)
	return body
}

// gemini25ThinkingModels 为仅支持 thinkingBudget、不支持 thinkingLevel 的
// Gemini 2.5 系列白名单（对齐 py _prepare_query_config 中对 2.5 系列的显式
// 枚举）。若对这些模型误发 thinkingLevel，REST API 直接 400。
var gemini25ThinkingModels = map[string]bool{
	"gemini-2.5-pro":                                     true,
	"gemini-2.5-pro-preview":                             true,
	"gemini-2.5-flash":                                   true,
	"gemini-2.5-flash-preview":                           true,
	"gemini-2.5-flash-lite":                              true,
	"gemini-2.5-flash-lite-preview":                      true,
	"gemini-robotics-er-1.5-preview":                     true,
	"gemini-live-2.5-flash-preview-native-audio-09-2025": true,
}

// applyThinkingConfig 按模型门控注入 Gemini thinking 配置（对齐 py
// _prepare_query_config）：
//   - 2.5 系列（白名单）：只下发 thinkingBudget（REST 字段 thinkingBudget），
//     配置缺失时默认 0；
//   - gemini-3-*/gemini-3.*：下发 thinkingLevel（REST 字段 thinkingLevel），
//     允许集合 {MINIMAL,LOW,MEDIUM,HIGH}，gemini-3.7 仅 {LOW,MEDIUM,HIGH} 并
//     回退 MEDIUM，其余回退 HIGH；
//   - 其他模型：不下发。
func (s *GeminiSource) applyThinkingConfig(body map[string]interface{}) {
	modelName := s.GetModel()
	if gemini25ThinkingModels[modelName] {
		budget := 0
		if s.thinkingConfig != nil {
			if v, ok := s.thinkingConfig["budget"]; ok && v != nil {
				budget = intFromAny(v, 0)
			}
		}
		body["thinkingConfig"] = map[string]interface{}{"thinkingBudget": budget}
		return
	}
	if !strings.HasPrefix(modelName, "gemini-3-") && !strings.HasPrefix(modelName, "gemini-3.") {
		return
	}
	// 3.x 默认 HIGH（对齐 py thinking_level 默认值）。
	level := "HIGH"
	if s.thinkingConfig != nil {
		if v, ok := s.thinkingConfig["level"].(string); ok && v != "" {
			level = v
		}
	}
	level = strings.ToUpper(level)
	allowedLevels := geminiThinkingLevels
	fallbackLevel := "HIGH"
	if strings.HasPrefix(modelName, "gemini-3.7") {
		allowedLevels = []string{"LOW", "MEDIUM", "HIGH"}
		fallbackLevel = "MEDIUM"
	}
	valid := false
	for _, l := range allowedLevels {
		if level == l {
			valid = true
			break
		}
	}
	if !valid {
		logger.Warn("Invalid thinking level %s for %s, using %s", level, modelName, fallbackLevel)
		level = fallbackLevel
	}
	body["thinkingConfig"] = map[string]interface{}{"thinkingLevel": level}
}

// intFromAny 尽力把配置值转成 int（兼容 JSON 解码出的 float64 与字符串）。
func intFromAny(v interface{}, fallback int) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return parsed
		}
	}
	return fallback
}

// geminiSchemaSupportFields 为 Gemini Schema 允许保留的字段（对齐 py
// convert_schema 的 support_fields）。
var geminiSchemaSupportFields = []string{
	"title", "description", "enum", "minimum", "maximum",
	"maxItems", "minItems", "nullable", "required",
}

// geminiSupportedFormats 为各基础类型允许的 format 值（对齐 py supported_formats）。
var geminiSupportedFormats = map[string]map[string]bool{
	"string":  {"enum": true, "date-time": true},
	"integer": {"int32": true, "int64": true},
	"number":  {"float": true, "double": true},
}

// geminiSupportedTypes 为 Gemini Schema 支持的 JSON Schema 基础类型。
var geminiSupportedTypes = map[string]bool{
	"string": true, "number": true, "integer": true,
	"boolean": true, "array": true, "object": true, "null": true,
}

// geminiConvertSchema 将 OpenAI/JSON Schema 递归转换为 Gemini REST Schema
// （对齐 py ToolSet.google_schema 内嵌的 convert_schema）：
//   - anyOf：整体返回 {"anyOf": [递归转换...]}；
//   - type 为列表（如 ["string","null"]）时取首个非 null，缺省 "string"；
//   - type 归一为 Gemini REST 期望的大写枚举（STRING/OBJECT/...），不支持的
//     类型回退 NULL；
//   - 仅保留 support_fields 白名单字段，format 需在对应类型允许集合内；
//   - 递归转换 properties（剔除 default/additionalProperties），array 的 items
//     缺失时补 {"type":"STRING"}。
func geminiConvertSchema(schema map[string]interface{}) map[string]interface{} {
	if anyOf, ok := schema["anyOf"].([]interface{}); ok {
		out := make([]interface{}, 0, len(anyOf))
		for _, s := range anyOf {
			if sm, ok := s.(map[string]interface{}); ok {
				out = append(out, geminiConvertSchema(sm))
			}
		}
		return map[string]interface{}{"anyOf": out}
	}

	result := map[string]interface{}{}
	targetType := ""
	switch t := schema["type"].(type) {
	case string:
		targetType = t
	case []interface{}:
		for _, item := range t {
			if s, ok := item.(string); ok && s != "null" {
				targetType = s
				break
			}
		}
		if targetType == "" {
			targetType = "string"
		}
	}

	if geminiSupportedTypes[targetType] {
		result["type"] = strings.ToUpper(targetType)
		if format, ok := schema["format"].(string); ok && geminiSupportedFormats[targetType][format] {
			result["format"] = format
		}
	} else {
		result["type"] = "NULL"
	}

	for _, k := range geminiSchemaSupportFields {
		if v, ok := schema[k]; ok {
			result[k] = v
		}
	}

	if props, ok := schema["properties"].(map[string]interface{}); ok {
		properties := map[string]interface{}{}
		for key, value := range props {
			vm, ok := value.(map[string]interface{})
			if !ok {
				continue
			}
			converted := geminiConvertSchema(vm)
			delete(converted, "default")
			delete(converted, "additionalProperties")
			properties[key] = converted
		}
		if len(properties) > 0 {
			result["properties"] = properties
		}
	}

	if targetType == "array" {
		if items, ok := schema["items"].(map[string]interface{}); ok {
			result["items"] = geminiConvertSchema(items)
		} else {
			result["items"] = map[string]interface{}{"type": "STRING"}
		}
	}

	return result
}
