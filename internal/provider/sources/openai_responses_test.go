package sources

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// TestOpenAIResponsesFallbackToolCallOrder verifies that when a stream ends
// without a terminal event, the consolidated tool calls keep the order in
// which the function_call items first appeared (L-46.2b).
func TestOpenAIResponsesFallbackToolCallOrder(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		send := func(payload string) {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
		send(`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"alpha","arguments":""}}`)
		send(`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"beta","arguments":""}}`)
		send(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"v\":1}"}`)
		send(`{"type":"response.function_call_arguments.delta","item_id":"fc_2","delta":"{\"v\":2}"}`)
	})

	src := NewOpenAIResponsesSource(map[string]interface{}{
		"api_base": srv.URL,
		"key":      "sk-test",
		"model":    "gpt-5",
	}, map[string]interface{}{})

	ch, err := src.TextChatStream(context.Background(), &provider.ProviderRequest{
		Prompt: "hello",
		Tools:  []map[string]interface{}{{"type": "function", "function": map[string]interface{}{"name": "alpha"}}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var final *provider.LLMResponse
	for item := range ch {
		if !item.IsChunk {
			final = item
		}
	}
	if final == nil {
		t.Fatal("expected a final consolidated chunk")
	}
	if len(final.ToolsCallName) != 2 {
		t.Fatalf("expected 2 tool calls, got %v", final.ToolsCallName)
	}
	if final.ToolsCallName[0] != "alpha" || final.ToolsCallName[1] != "beta" {
		t.Errorf("tool call order must follow first-seen order, got %v", final.ToolsCallName)
	}
	if len(final.ToolsCallIDs) != 2 || final.ToolsCallIDs[0] != "call_1" || final.ToolsCallIDs[1] != "call_2" {
		t.Errorf("unexpected tool call ids: %v", final.ToolsCallIDs)
	}
	if final.ToolsCallArgs[0]["v"] != float64(1) || final.ToolsCallArgs[1]["v"] != float64(2) {
		t.Errorf("unexpected tool args: %v", final.ToolsCallArgs)
	}
}
