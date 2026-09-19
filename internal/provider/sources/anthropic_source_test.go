package sources

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// TestAnthropicBuildRequestBody verifies tools are sent, OpenAI tool-loop
// history is converted to Anthropic tool_use/tool_result, and max_tokens is
// read from config (M-37).
func TestAnthropicBuildRequestBody(t *testing.T) {
	s := NewAnthropicSource(map[string]interface{}{
		"key":        "k",
		"model":      "claude-3-7-sonnet",
		"max_tokens": 8192,
	}, nil)
	req := &provider.ProviderRequest{
		Prompt: "continue",
		Tools: []map[string]interface{}{
			{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "lookup",
					"description": "look things up",
					"parameters":  map[string]interface{}{"type": "object", "properties": map[string]interface{}{"q": map[string]interface{}{"type": "string"}}},
				},
			},
		},
		Contexts: []map[string]interface{}{
			{"role": "system", "content": "sys"},
			{
				"role":    "assistant",
				"content": "i'll call",
				"tool_calls": []interface{}{
					map[string]interface{}{
						"id": "call_9", "type": "function",
						"function": map[string]interface{}{"name": "lookup", "arguments": `{"q":"x"}`},
					},
				},
			},
			{"role": "tool", "tool_call_id": "call_9", "content": "result-9"},
		},
	}
	body := s.buildRequestBody(req, false)
	if body["max_tokens"] != 8192 {
		t.Fatalf("max_tokens = %v, want 8192", body["max_tokens"])
	}
	if body["stream"] != false {
		t.Fatalf("stream = %v", body["stream"])
	}
	tools := body["tools"].([]map[string]interface{})
	if len(tools) != 1 || tools[0]["name"] != "lookup" {
		t.Fatalf("unexpected tools: %v", tools)
	}
	msgs := body["messages"].([]map[string]interface{})
	// 对齐 py _merge_consecutive_anthropic_messages：连续的 user 消息
	//（tool_result 与最终 prompt）会合并为一条，tool_result 块前移。
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (tool_use + merged user), got %d", len(msgs))
	}
	// assistant message carries a tool_use content block
	asst := msgs[0]["content"].([]map[string]interface{})
	if len(asst) != 2 {
		t.Fatalf("expected text + tool_use blocks, got %d", len(asst))
	}
	tu := asst[1]
	if tu["type"] != "tool_use" || tu["id"] != "call_9" || tu["name"] != "lookup" {
		t.Fatalf("unexpected tool_use block: %v", tu)
	}
	// tool message becomes a user/tool_result block（与最终 user prompt 合并）
	trMsg := msgs[1]
	if trMsg["role"] != "user" {
		t.Fatalf("tool message role = %v", trMsg["role"])
	}
	trContent := trMsg["content"].([]map[string]interface{})
	if len(trContent) < 1 {
		t.Fatalf("merged user message should keep tool_result + prompt, got %d blocks", len(trContent))
	}
	tr := trContent[0]
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "call_9" || tr["content"] != "result-9" {
		t.Fatalf("unexpected tool_result block: %v", tr)
	}
	// 最终 user prompt 合并进同一条消息的后续块。
	if len(trContent) < 2 {
		t.Fatalf("expected merged user prompt block, got %v", trContent)
	}
	if txt, _ := trContent[1]["text"].(string); trContent[1]["type"] != "text" || txt != "continue" {
		t.Fatalf("unexpected merged user prompt block: %v", trContent[1])
	}
}

// TestAnthropicStreamUsageAndTools verifies the streaming path reads input
// tokens from message_start (message_delta only carries output_tokens, M-40a)
// and accumulates tool_use blocks into the final chunk (M-37b).
func TestAnthropicStreamUsageAndTools(t *testing.T) {
	events := []string{
		`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":123,"output_tokens":4}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_1","name":"lookup"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"x\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":77}}`,
		`{"type":"message_stop"}`,
	}
	var sse strings.Builder
	for _, e := range events {
		fmt.Fprintf(&sse, "data: %s\n\n", e)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse.String())
	}))
	defer srv.Close()

	s := NewAnthropicSource(map[string]interface{}{
		"key":      "k",
		"model":    "claude-3-7-sonnet",
		"api_base": srv.URL,
	}, nil)
	req := &provider.ProviderRequest{
		Prompt: "hi",
		Tools: []map[string]interface{}{
			{"type": "function", "function": map[string]interface{}{
				"name":       "lookup",
				"parameters": map[string]interface{}{"type": "object"},
			}},
		},
	}
	ch, err := s.TextChatStream(context.Background(), req)
	if err != nil {
		t.Fatalf("TextChatStream: %v", err)
	}
	var last *provider.LLMResponse
	for r := range ch {
		last = r
	}
	if last == nil {
		t.Fatal("no response received")
	}
	if last.Usage == nil {
		t.Fatal("expected usage")
	}
	if last.Usage.InputOther != 123 {
		t.Fatalf("input tokens = %d, want 123 (from message_start)", last.Usage.InputOther)
	}
	if last.Usage.Output != 77 {
		t.Fatalf("output tokens = %d, want 77", last.Usage.Output)
	}
	if len(last.ToolsCallName) != 1 || last.ToolsCallName[0] != "lookup" {
		t.Fatalf("unexpected tool names: %v", last.ToolsCallName)
	}
	if len(last.ToolsCallIDs) != 1 || last.ToolsCallIDs[0] != "tu_1" {
		t.Fatalf("unexpected tool ids: %v", last.ToolsCallIDs)
	}
	if len(last.ToolsCallArgs) != 1 || last.ToolsCallArgs[0]["q"] != "x" {
		t.Fatalf("unexpected tool args: %v", last.ToolsCallArgs)
	}
	if last.Role != "tool" {
		t.Fatalf("role = %v, want tool", last.Role)
	}
}
