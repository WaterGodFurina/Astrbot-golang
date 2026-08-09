package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

func TestOpenAIResponsesSourceTextChat(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "gpt-5" {
			t.Errorf("unexpected model: %v", body["model"])
		}
		if body["store"] != false {
			t.Errorf("expected store=false, got %v", body["store"])
		}
		if body["instructions"] != "Be concise" {
			t.Errorf("unexpected instructions: %v", body["instructions"])
		}
		if body["tool_choice"] != "auto" {
			t.Errorf("unexpected tool_choice: %v", body["tool_choice"])
		}
		input, _ := body["input"].([]interface{})
		if len(input) != 2 {
			t.Fatalf("expected 2 input items, got %d: %v", len(input), input)
		}
		tools, _ := body["tools"].([]interface{})
		if len(tools) != 1 {
			t.Fatalf("expected 1 tool, got %d", len(tools))
		}
		if tool, ok := tools[0].(map[string]interface{}); ok {
			if tool["type"] != "function" || tool["name"] != "get_weather" {
				t.Errorf("unexpected tool def: %v", tool)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"The weather in Paris is 18C."}]},{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}"}],"usage":{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":2}}}`)
	})

	src := NewOpenAIResponsesSource(map[string]interface{}{
		"api_base": srv.URL,
		"key":      "sk-test",
		"model":    "gpt-5",
	}, map[string]interface{}{})

	req := &provider.ProviderRequest{
		Prompt:       "What's the weather?",
		SessionID:    "test",
		SystemPrompt: "Be concise",
		Contexts: []map[string]interface{}{
			{"role": "user", "content": "Hi"},
		},
		Tools: []map[string]interface{}{
			{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "get_weather",
					"description": "Get weather for a city",
					"parameters":  map[string]interface{}{"type": "object"},
				},
			},
		},
	}
	resp, err := src.TextChat(context.Background(), req)
	if err != nil {
		t.Fatalf("text chat: %v", err)
	}
	if resp.CompletionText != "The weather in Paris is 18C." {
		t.Errorf("unexpected text: %q", resp.CompletionText)
	}
	if resp.Role != "tool" {
		t.Errorf("expected role=tool, got %q", resp.Role)
	}
	if len(resp.ToolsCallName) != 1 || resp.ToolsCallName[0] != "get_weather" {
		t.Errorf("unexpected tool names: %v", resp.ToolsCallName)
	}
	if len(resp.ToolsCallIDs) != 1 || resp.ToolsCallIDs[0] != "call_1" {
		t.Errorf("unexpected tool ids: %v", resp.ToolsCallIDs)
	}
	if len(resp.ToolsCallArgs) != 1 || resp.ToolsCallArgs[0]["city"] != "Paris" {
		t.Errorf("unexpected tool args: %v", resp.ToolsCallArgs)
	}
	if resp.Usage == nil || resp.Usage.InputOther != 8 || resp.Usage.InputCached != 2 || resp.Usage.Output != 5 {
		t.Errorf("unexpected usage: %+v", resp.Usage)
	}
}

func TestOpenAIResponsesSourceTextChatStream(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		send := func(payload string) {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
		send(`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":""}}`)
		send(`{"type":"response.output_text.delta","delta":"Hello"}`)
		send(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"city\":\"Paris\"}"}`)
		send(`{"type":"response.output_text.delta","delta":" world"}`)
		send(`{"type":"response.completed","response":{"id":"resp_s","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello world"}]},{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}"}],"usage":{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":0}}}}`)
	})

	src := NewOpenAIResponsesSource(map[string]interface{}{
		"api_base": srv.URL,
		"key":      "sk-test",
		"model":    "gpt-5",
	}, map[string]interface{}{})

	ch, err := src.TextChatStream(context.Background(), &provider.ProviderRequest{
		Prompt:    "hello",
		SessionID: "test",
	})
	if err != nil {
		t.Fatalf("text chat stream: %v", err)
	}

	var text strings.Builder
	var final *provider.LLMResponse
	for item := range ch {
		if item.IsChunk {
			text.WriteString(item.CompletionText)
		} else {
			final = item
		}
	}
	if text.String() != "Hello world" {
		t.Errorf("accumulated text = %q, want %q", text.String(), "Hello world")
	}
	if final == nil {
		t.Fatal("expected a final consolidated chunk")
	}
	if final.CompletionText != "Hello world" {
		t.Errorf("final text = %q, want %q", final.CompletionText, "Hello world")
	}
	if len(final.ToolsCallName) != 1 || final.ToolsCallName[0] != "get_weather" {
		t.Errorf("unexpected final tool names: %v", final.ToolsCallName)
	}
	if len(final.ToolsCallArgs) != 1 || final.ToolsCallArgs[0]["city"] != "Paris" {
		t.Errorf("unexpected final tool args: %v", final.ToolsCallArgs)
	}
	if final.Usage == nil || final.Usage.InputOther != 10 || final.Usage.Output != 5 {
		t.Errorf("unexpected final usage: %+v", final.Usage)
	}
}

func TestKimiCodeSourceTextChat(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("User-Agent"); got != kimiCodeUserAgent {
			t.Errorf("unexpected User-Agent: %q", got)
		}
		if got := r.Header.Get("x-api-key"); got != "sk-test" {
			t.Errorf("unexpected api key: %q", got)
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "kimi-for-coding" {
			t.Errorf("unexpected model: %v", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","content":[{"type":"text","text":"Hello from Kimi"}],"usage":{"input_tokens":5,"output_tokens":3,"cache_read_input_tokens":1}}`)
	})

	src := NewKimiCodeSource(map[string]interface{}{
		"api_base": srv.URL,
		"key":      "sk-test",
		"model":    "kimi-for-coding",
	}, map[string]interface{}{})

	resp, err := src.TextChat(context.Background(), &provider.ProviderRequest{
		Prompt:    "hi",
		SessionID: "test",
	})
	if err != nil {
		t.Fatalf("text chat: %v", err)
	}
	if resp.CompletionText != "Hello from Kimi" {
		t.Errorf("unexpected text: %q", resp.CompletionText)
	}
	if resp.Usage == nil || resp.Usage.InputOther != 5 || resp.Usage.InputCached != 1 || resp.Usage.Output != 3 {
		t.Errorf("unexpected usage: %+v", resp.Usage)
	}
}

func TestKimiCodeSourceDefaults(t *testing.T) {
	src := NewKimiCodeSource(map[string]interface{}{"key": "sk-test"}, map[string]interface{}{})
	if src.apiBase != kimiCodeAPIBase {
		t.Errorf("unexpected api_base: %q", src.apiBase)
	}
	if src.GetModel() != kimiCodeDefaultModel {
		t.Errorf("unexpected default model: %q", src.GetModel())
	}
	if got := src.customHeaders["User-Agent"]; got != kimiCodeUserAgent {
		t.Errorf("unexpected default User-Agent: %q", got)
	}
}

func TestSpecialChatCreateProvider(t *testing.T) {
	for _, typ := range []string{"openai_responses", "kimi_code_chat_completion"} {
		p, err := provider.CreateProvider(typ, map[string]interface{}{
			"type": typ, "model": "m",
		}, map[string]interface{}{})
		if err != nil {
			t.Fatalf("create %s: %v", typ, err)
		}
		if p.Meta().Type != typ {
			t.Errorf("type mismatch: got %q want %q", p.Meta().Type, typ)
		}
		if _, ok := p.(provider.ChatProvider); !ok {
			t.Errorf("%s is not a ChatProvider", typ)
		}
	}
}
