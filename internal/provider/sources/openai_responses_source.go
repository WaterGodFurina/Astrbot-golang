// Package sources - OpenAI stateless Responses API chat provider.
// Ported from astrbot/core/provider/sources/openai_responses_source.py
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

	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

// reasoningStateType mirrors ProviderOpenAIResponses._REASONING_STATE_TYPE.
const reasoningStateType = "openai_responses_reasoning"

// responsesUsage is the token accounting block of a Responses API response.
type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// responsesContentPart is one element of a message item's content list.
type responsesContentPart struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

// responsesOutputItem is a top-level output item of a Responses API response.
type responsesOutputItem struct {
	Type    string                 `json:"type"`
	ID      string                 `json:"id"`
	Role    string                 `json:"role"`
	Content []responsesContentPart `json:"content"`
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	CallID    string `json:"call_id"`
}

// responsesResponse is the Responses API response body.
type responsesResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	IncompleteDetails struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []responsesOutputItem `json:"output"`
	Usage  *responsesUsage       `json:"usage"`
}

// OpenAIResponsesSource is a stateless Responses API chat provider.
type OpenAIResponsesSource struct {
	provider.BaseProvider
	apiBase       string
	apiKey        string
	client        *http.Client
	customHeaders map[string]string
}

// NewOpenAIResponsesSource creates an OpenAI Responses API provider.
func NewOpenAIResponsesSource(config, settings map[string]interface{}) *OpenAIResponsesSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &OpenAIResponsesSource{
		BaseProvider:  bp,
		client:        &http.Client{Timeout: 120 * time.Second},
		customHeaders: map[string]string{},
	}
	s.apiBase = configString(config, "api_base", "https://api.openai.com/v1")
	keys := s.getKeys()
	if len(keys) > 0 {
		s.apiKey = keys[0]
	}
	if ch, ok := config["custom_headers"].(map[string]interface{}); ok {
		for k, v := range ch {
			s.customHeaders[k] = fmt.Sprint(v)
		}
	}
	return s
}

func (s *OpenAIResponsesSource) getKeys() []string {
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
func (s *OpenAIResponsesSource) GetCurrentKey() string {
	return s.apiKey
}

// SetKey sets the API key.
func (s *OpenAIResponsesSource) SetKey(key string) {
	s.apiKey = key
}

// GetModels returns available models.
func (s *OpenAIResponsesSource) GetModels(ctx context.Context) ([]string, error) {
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

// TextChat sends a non-streaming Responses API request.
func (s *OpenAIResponsesSource) TextChat(ctx context.Context, req *provider.ProviderRequest) (*provider.LLMResponse, error) {
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
	var result responsesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return s.parseResponse(&result, len(req.Tools) > 0)
}

// TextChatStream sends a streaming Responses API request (SSE events).
func (s *OpenAIResponsesSource) TextChatStream(ctx context.Context, req *provider.ProviderRequest) (<-chan *provider.LLMResponse, error) {
	body := s.buildRequestBody(req, true)
	resp, err := s.doRequest(ctx, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	ch := make(chan *provider.LLMResponse, 100)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		// Tool call arguments stream as partial JSON deltas keyed by item id.
		type callAcc struct {
			callID, name, args string
		}
		calls := map[string]*callAcc{}
		content := new(strings.Builder)
		reasoning := new(strings.Builder)
		var responseID string
		var final *provider.LLMResponse

		reader := newSSEReader(ctx, resp, func(data string) (stop bool) {
			var event struct {
				Type      string               `json:"type"`
				Delta     string               `json:"delta"`
				Arguments string               `json:"arguments"`
				ItemID    string               `json:"item_id"`
				Response  *responsesResponse   `json:"response"`
				Item      *responsesOutputItem `json:"item"`
				Code      string               `json:"code"`
				Message   string               `json:"message"`
			}
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				return false
			}
			if event.Response != nil && event.Response.ID != "" {
				responseID = event.Response.ID
			}
			switch event.Type {
			case "error":
				ch <- &provider.LLMResponse{
					Role:           "err",
					CompletionText: fmt.Sprintf("Responses API stream failed: %s: %s", event.Code, event.Message),
				}
				return true
			case "response.output_text.delta", "response.refusal.delta":
				if event.Delta != "" {
					content.WriteString(event.Delta)
					ch <- &provider.LLMResponse{
						Role:           "assistant",
						IsChunk:        true,
						CompletionText: event.Delta,
						ID:             responseID,
					}
				}
			case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
				if event.Delta != "" {
					reasoning.WriteString(event.Delta)
					ch <- &provider.LLMResponse{
						Role:             "assistant",
						IsChunk:          true,
						ReasoningContent: event.Delta,
						ID:               responseID,
					}
				}
			case "response.output_item.added", "response.output_item.done":
				if event.Item != nil && event.Item.Type == "function_call" {
					acc := calls[event.Item.ID]
					if acc == nil {
						acc = &callAcc{}
						calls[event.Item.ID] = acc
					}
					acc.callID = event.Item.CallID
					acc.name = event.Item.Name
					if event.Item.Arguments != "" {
						acc.args = event.Item.Arguments
					}
				}
			case "response.function_call_arguments.delta":
				if acc := calls[event.ItemID]; acc != nil && event.Delta != "" {
					acc.args += event.Delta
				}
			case "response.function_call_arguments.done":
				if acc := calls[event.ItemID]; acc != nil && event.Arguments != "" {
					acc.args = event.Arguments
				}
			case "response.completed", "response.incomplete", "response.failed":
				if event.Response != nil {
					if parsed, err := s.parseResponse(event.Response, len(req.Tools) > 0); err == nil {
						final = parsed
					}
				}
				return true
			}
			return false
		})
		_ = reader.scan()

		if final == nil {
			// Stream ended without a terminal event: consolidate accumulated state.
			final = &provider.LLMResponse{
				Role:             "assistant",
				ID:               responseID,
				CompletionText:   content.String(),
				ReasoningContent: reasoning.String(),
				ToolsCallArgs:    []map[string]interface{}{},
				ToolsCallName:    []string{},
				ToolsCallIDs:     []string{},
			}
			for _, acc := range calls {
				argsMap := map[string]interface{}{}
				if acc.args != "" {
					_ = json.Unmarshal([]byte(acc.args), &argsMap)
				}
				final.ToolsCallName = append(final.ToolsCallName, acc.name)
				final.ToolsCallIDs = append(final.ToolsCallIDs, acc.callID)
				final.ToolsCallArgs = append(final.ToolsCallArgs, argsMap)
			}
			if len(final.ToolsCallArgs) > 0 {
				final.Role = "tool"
			}
		} else {
			// Fill any gaps left by the terminal response with accumulated data.
			if final.CompletionText == "" {
				final.CompletionText = content.String()
			}
			if final.ReasoningContent == "" {
				final.ReasoningContent = reasoning.String()
			}
			if final.ID == "" {
				final.ID = responseID
			}
			if len(final.ToolsCallArgs) == 0 && len(calls) > 0 {
				for _, acc := range calls {
					argsMap := map[string]interface{}{}
					if acc.args != "" {
						_ = json.Unmarshal([]byte(acc.args), &argsMap)
					}
					final.ToolsCallName = append(final.ToolsCallName, acc.name)
					final.ToolsCallIDs = append(final.ToolsCallIDs, acc.callID)
					final.ToolsCallArgs = append(final.ToolsCallArgs, argsMap)
				}
				if len(final.ToolsCallArgs) > 0 {
					final.Role = "tool"
				}
			}
		}
		ch <- final
	}()
	return ch, nil
}

// Test verifies the provider.
func (s *OpenAIResponsesSource) Test(ctx context.Context) error {
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

// parseResponse normalizes a Responses API response into an LLMResponse.
func (s *OpenAIResponsesSource) parseResponse(resp *responsesResponse, mapFunctionCalls bool) (*provider.LLMResponse, error) {
	if resp.Status == "failed" {
		code := resp.Error.Code
		if code == "" {
			code = "unknown_error"
		}
		message := resp.Error.Message
		if message == "" {
			message = "Responses API request failed"
		}
		return nil, fmt.Errorf("Responses API request failed: %s: %s. response_id=%s", code, message, resp.ID)
	}
	if resp.IncompleteDetails.Reason == "content_filter" {
		return nil, fmt.Errorf("Responses API output was rejected by the provider content filter. response_id=%s", resp.ID)
	}

	llmResp := &provider.LLMResponse{
		Role:          "assistant",
		ID:            resp.ID,
		ToolsCallArgs: []map[string]interface{}{},
		ToolsCallName: []string{},
		ToolsCallIDs:  []string{},
	}
	textParts := []string{}
	reasoningParts := []string{}
	serializedReasoning := []map[string]interface{}{}

	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				switch part.Type {
				case "output_text":
					textParts = append(textParts, part.Text)
				case "refusal":
					textParts = append(textParts, part.Refusal)
				}
			}
		case "reasoning":
			itemReasoning := []string{}
			for _, part := range item.Content {
				if part.Type == "reasoning_text" && part.Text != "" {
					itemReasoning = append(itemReasoning, part.Text)
				}
			}
			if len(itemReasoning) == 0 {
				for _, summary := range item.Summary {
					if summary.Text != "" {
						itemReasoning = append(itemReasoning, summary.Text)
					}
				}
			}
			reasoningParts = append(reasoningParts, itemReasoning...)
			serialized := map[string]interface{}{
				"type":    item.Type,
				"content": item.Content,
			}
			if item.ID != "" {
				serialized["id"] = item.ID
			}
			if len(item.Summary) > 0 {
				serialized["summary"] = item.Summary
			}
			serializedReasoning = append(serializedReasoning, serialized)
		case "function_call":
			if !mapFunctionCalls {
				continue
			}
			argsMap := map[string]interface{}{}
			if item.Arguments != "" {
				if err := json.Unmarshal([]byte(item.Arguments), &argsMap); err != nil {
					argsMap = map[string]interface{}{}
				}
			}
			llmResp.ToolsCallArgs = append(llmResp.ToolsCallArgs, argsMap)
			llmResp.ToolsCallName = append(llmResp.ToolsCallName, item.Name)
			llmResp.ToolsCallIDs = append(llmResp.ToolsCallIDs, item.CallID)
		}
	}

	llmResp.CompletionText = strings.Join(textParts, "")
	if len(reasoningParts) > 0 {
		llmResp.ReasoningContent = strings.Join(reasoningParts, "\n")
	}
	if len(serializedReasoning) > 0 {
		if b, err := json.Marshal(map[string]interface{}{
			"type":  reasoningStateType,
			"items": serializedReasoning,
		}); err == nil {
			llmResp.ReasoningSignature = string(b)
		}
	}
	if len(llmResp.ToolsCallArgs) > 0 {
		llmResp.Role = "tool"
	}

	if resp.Usage != nil {
		cached := resp.Usage.InputTokensDetails.CachedTokens
		llmResp.Usage = &provider.TokenUsage{
			InputOther:  resp.Usage.InputTokens - cached,
			InputCached: cached,
			Output:      resp.Usage.OutputTokens,
		}
	} else {
		llmResp.Usage = &provider.TokenUsage{}
	}

	hasText := strings.TrimSpace(llmResp.CompletionText) != ""
	hasReasoning := strings.TrimSpace(llmResp.ReasoningContent) != ""
	if !hasText && !hasReasoning && len(llmResp.ToolsCallArgs) == 0 {
		return nil, fmt.Errorf("Responses API returned no usable output. response_id=%s, status=%s", resp.ID, resp.Status)
	}
	return llmResp, nil
}

func (s *OpenAIResponsesSource) buildRequestBody(req *provider.ProviderRequest, stream bool) map[string]interface{} {
	body := map[string]interface{}{
		"model":  s.GetModel(),
		"stream": stream,
		"store":  false,
	}
	if req.SystemPrompt != "" {
		body["instructions"] = req.SystemPrompt
	}

	// Convert the chat-format history (contexts + current user message) into
	// Responses API input items.
	messages := make([]map[string]interface{}, 0, len(req.Contexts)+1)
	messages = append(messages, req.Contexts...)
	messages = append(messages, req.ToUserMessage())
	body["input"] = s.convertMessagesToResponseInput(messages)

	// Tools use the OpenAI function schema, flattened to {"type":"function", ...}.
	if len(req.Tools) > 0 {
		tools := []map[string]interface{}{}
		for _, t := range req.Tools {
			if fn, ok := t["function"].(map[string]interface{}); ok {
				tool := map[string]interface{}{"type": "function"}
				for k, v := range fn {
					tool[k] = v
				}
				tools = append(tools, tool)
			} else {
				tools = append(tools, t)
			}
		}
		if len(tools) > 0 {
			body["tools"] = tools
			body["tool_choice"] = "auto"
		}
	}

	// Optional provider-config extra body fields.
	if extra, ok := s.Config()["custom_extra_body"].(map[string]interface{}); ok {
		for k, v := range extra {
			body[k] = v
		}
	}
	if _, has := body["max_output_tokens"]; !has {
		if mt, ok := s.Config()["max_tokens"]; ok {
			body["max_output_tokens"] = mt
		}
	}
	if _, has := body["reasoning"]; !has {
		if re, ok := s.Config()["reasoning_effort"]; ok {
			body["reasoning"] = map[string]interface{}{"effort": re}
		}
	}
	return body
}

// convertMessagesToResponseInput converts OpenAI chat-format messages into
// Responses API input items, preserving function call ids and serialized
// reasoning output so the history can be replayed statelessly.
func (s *OpenAIResponsesSource) convertMessagesToResponseInput(messages []map[string]interface{}) []map[string]interface{} {
	responseInput := []map[string]interface{}{}
	for _, message := range messages {
		role, _ := message["role"].(string)
		if role == "tool" {
			toolCallID, _ := message["tool_call_id"].(string)
			if toolCallID == "" {
				continue
			}
			output := message["content"]
			outputStr := ""
			switch v := output.(type) {
			case string:
				outputStr = v
			default:
				if output != nil {
					if b, err := json.Marshal(output); err == nil {
						outputStr = string(b)
					}
				}
			}
			responseInput = append(responseInput, map[string]interface{}{
				"type":    "function_call_output",
				"call_id": toolCallID,
				"output":  outputStr,
			})
			continue
		}
		if role != "user" && role != "assistant" && role != "system" && role != "developer" {
			continue
		}

		content := message["content"]
		var convertedContent interface{}
		var reasoningItems []map[string]interface{}

		switch v := content.(type) {
		case string:
			convertedContent = v
		case []interface{}:
			contentParts := []map[string]interface{}{}
			assistantText := []string{}
			for _, p := range v {
				part, ok := p.(map[string]interface{})
				if !ok {
					continue
				}
				partType, _ := part["type"].(string)
				switch partType {
				case "think":
					restored := s.restoreReasoningItems(part)
					if len(restored) > 0 {
						reasoningItems = append(reasoningItems, restored...)
					} else if s.isDeepseek() {
						if think, ok := part["think"].(string); ok && think != "" {
							reasoningItems = append(reasoningItems, map[string]interface{}{
								"type": "reasoning",
								"content": []map[string]interface{}{
									{"type": "reasoning_text", "text": think},
								},
								"summary": []interface{}{},
							})
						}
					}
					continue
				case "text":
					text := fmt.Sprint(part["text"])
					if role == "assistant" {
						assistantText = append(assistantText, text)
					} else {
						contentParts = append(contentParts, map[string]interface{}{"type": "input_text", "text": text})
					}
					continue
				case "image_url":
					if role == "assistant" {
						continue
					}
					imageData, _ := part["image_url"].(map[string]interface{})
					imageURL, _ := imageData["url"].(string)
					if imageURL == "" {
						continue
					}
					detail := "auto"
					if d, ok := imageData["detail"].(string); ok && (d == "low" || d == "high" || d == "auto") {
						detail = d
					}
					contentParts = append(contentParts, map[string]interface{}{
						"type":      "input_image",
						"detail":    detail,
						"image_url": imageURL,
					})
					continue
				case "audio_url", "input_audio":
					if role == "assistant" {
						assistantText = append(assistantText, "[Audio]")
					} else {
						contentParts = append(contentParts, map[string]interface{}{"type": "input_text", "text": "[Audio]"})
					}
				}
			}
			if role == "assistant" {
				convertedContent = strings.Join(assistantText, "")
			} else if len(contentParts) > 0 {
				convertedContent = contentParts
			}
		default:
			if content != nil {
				convertedContent = fmt.Sprint(content)
			}
		}

		responseInput = append(responseInput, reasoningItems...)
		if convertedContent != nil {
			switch cc := convertedContent.(type) {
			case string:
				if cc != "" {
					responseInput = append(responseInput, map[string]interface{}{
						"type": "message", "role": role, "content": cc,
					})
				}
			case []map[string]interface{}:
				if len(cc) > 0 {
					responseInput = append(responseInput, map[string]interface{}{
						"type": "message", "role": role, "content": cc,
					})
				}
			}
		}

		if role == "assistant" {
			toolCalls, _ := message["tool_calls"].([]interface{})
			for _, tc := range toolCalls {
				toolCall, ok := tc.(map[string]interface{})
				if !ok {
					continue
				}
				function, _ := toolCall["function"].(map[string]interface{})
				callID, _ := toolCall["id"].(string)
				if function == nil || callID == "" {
					continue
				}
				arguments := "{}"
				if a, ok := function["arguments"].(string); ok {
					arguments = a
				} else if function["arguments"] != nil {
					if b, err := json.Marshal(function["arguments"]); err == nil {
						arguments = string(b)
					}
				}
				responseInput = append(responseInput, map[string]interface{}{
					"type":      "function_call",
					"call_id":   callID,
					"name":      fmt.Sprint(function["name"]),
					"arguments": arguments,
				})
			}
		}
	}
	return responseInput
}

// restoreReasoningItems deserializes the encrypted reasoning state embedded in
// a "think" content part back into Responses API reasoning items.
func (s *OpenAIResponsesSource) restoreReasoningItems(part map[string]interface{}) []map[string]interface{} {
	serialized, _ := part["encrypted"].(string)
	if serialized == "" {
		return nil
	}
	var state map[string]interface{}
	if err := json.Unmarshal([]byte(serialized), &state); err != nil {
		return nil
	}
	if st, _ := state["type"].(string); st != reasoningStateType {
		return nil
	}
	items, _ := state["items"].([]interface{})
	restored := []map[string]interface{}{}
	for _, item := range items {
		if m, ok := item.(map[string]interface{}); ok {
			restored = append(restored, m)
		}
	}
	return restored
}

func (s *OpenAIResponsesSource) isDeepseek() bool {
	if prov, _ := s.Config()["provider"].(string); prov == "deepseek" {
		return true
	}
	return strings.Contains(s.apiBase, "api.deepseek.com")
}

func (s *OpenAIResponsesSource) doRequest(ctx context.Context, body map[string]interface{}) (*http.Response, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}
	url := fmt.Sprintf("%s/responses", s.apiBase)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+s.apiKey)
	for k, v := range s.customHeaders {
		httpReq.Header.Set(k, v)
	}
	return s.client.Do(httpReq)
}
