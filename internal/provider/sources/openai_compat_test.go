package sources

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

const chatCompletionResp = `{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

func chatHandler(t *testing.T, bodyCh chan map[string]interface{}, headerCh chan http.Header) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if headerCh != nil {
			headerCh <- r.Header
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if bodyCh != nil {
			bodyCh <- body
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatCompletionResp)
	}
}

func TestOpenAICompatProvidersConstructible(t *testing.T) {
	types := []string{
		"groq_chat_completion",
		"xai_chat_completion",
		"zhipu_chat_completion",
		"longcat_chat_completion",
		"aihubmix_chat_completion",
		"xiaomi_chat_completion",
	}
	for _, typ := range types {
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
			t.Errorf("%s does not implement provider.ChatProvider", typ)
		}
	}
}

func TestGroqSourceStripsAssistantReasoning(t *testing.T) {
	bodyCh := make(chan map[string]interface{}, 1)
	srv := newTestServer(t, chatHandler(t, bodyCh, nil))

	src := NewGroqSource(map[string]interface{}{
		"api_base": srv.URL, "key": "sk-test", "model": "grok-3",
	}, map[string]interface{}{})
	req := &provider.ProviderRequest{
		Prompt: "hello",
		Contexts: []map[string]interface{}{
			{"role": "assistant", "content": "prev", "reasoning_content": "thinking", "reasoning": "r"},
			{"role": "user", "content": "q"},
		},
	}
	if _, err := src.TextChat(context.Background(), req); err != nil {
		t.Fatalf("text chat: %v", err)
	}

	body := <-bodyCh
	msgs, ok := body["messages"].([]interface{})
	if !ok {
		t.Fatalf("messages not a list: %T", body["messages"])
	}
	for _, raw := range msgs {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("message not a map: %T", raw)
		}
		if msg["role"] == "assistant" {
			if _, has := msg["reasoning_content"]; has {
				t.Errorf("assistant message still has reasoning_content: %v", msg)
			}
			if _, has := msg["reasoning"]; has {
				t.Errorf("assistant message still has reasoning: %v", msg)
			}
			if msg["content"] != "prev" {
				t.Errorf("assistant content changed: %v", msg["content"])
			}
		}
	}
}

func TestXAISourceNativeSearchInjection(t *testing.T) {
	bodyCh := make(chan map[string]interface{}, 1)
	srv := newTestServer(t, chatHandler(t, bodyCh, nil))

	src := NewXAISource(map[string]interface{}{
		"api_base": srv.URL, "key": "sk-test", "model": "grok-3",
		"xai_native_search": true,
	}, map[string]interface{}{})
	if _, err := src.TextChat(context.Background(), &provider.ProviderRequest{Prompt: "hi"}); err != nil {
		t.Fatalf("text chat: %v", err)
	}

	body := <-bodyCh
	sp, ok := body["search_parameters"].(map[string]interface{})
	if !ok {
		t.Fatalf("search_parameters missing: %v", body)
	}
	if sp["mode"] != "auto" {
		t.Errorf("search mode = %v, want auto", sp["mode"])
	}
}

func TestXAISourceNativeSearchDisabled(t *testing.T) {
	bodyCh := make(chan map[string]interface{}, 1)
	srv := newTestServer(t, chatHandler(t, bodyCh, nil))

	src := NewXAISource(map[string]interface{}{
		"api_base": srv.URL, "key": "sk-test", "model": "grok-3",
		"xai_native_search": false,
	}, map[string]interface{}{})
	if _, err := src.TextChat(context.Background(), &provider.ProviderRequest{Prompt: "hi"}); err != nil {
		t.Fatalf("text chat: %v", err)
	}

	body := <-bodyCh
	if _, has := body["search_parameters"]; has {
		t.Errorf("search_parameters should not be injected when disabled")
	}
}

func TestAIHubMixSourceAddsAppCodeHeader(t *testing.T) {
	headerCh := make(chan http.Header, 1)
	srv := newTestServer(t, chatHandler(t, nil, headerCh))

	src := NewAIHubMixSource(map[string]interface{}{
		"api_base": srv.URL, "key": "sk-test", "model": "gpt-4o",
	}, map[string]interface{}{})
	if _, err := src.TextChat(context.Background(), &provider.ProviderRequest{Prompt: "hi"}); err != nil {
		t.Fatalf("text chat: %v", err)
	}

	hdr := <-headerCh
	if got := hdr.Get("APP-Code"); got != "KRLC5702" {
		t.Errorf("APP-Code header = %q, want KRLC5702", got)
	}
}

func TestXiaomiSourceDefaults(t *testing.T) {
	src := NewXiaomiSource(map[string]interface{}{"key": "sk-test"}, map[string]interface{}{})
	if src.apiBase != "https://api.xiaomimimo.com/v1" {
		t.Errorf("api_base = %q, want https://api.xiaomimimo.com/v1", src.apiBase)
	}
	if src.GetModel() != "mimo-v2.5" {
		t.Errorf("model = %q, want mimo-v2.5", src.GetModel())
	}
}

func TestXiaomiSourceGetModelsFallback(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	src := NewXiaomiSource(map[string]interface{}{
		"api_base": srv.URL, "key": "sk-test",
	}, map[string]interface{}{})

	models, err := src.GetModels(context.Background())
	if err != nil {
		t.Fatalf("get models: %v", err)
	}
	if len(models) != len(xiaomiModels) {
		t.Errorf("model count = %d, want %d", len(models), len(xiaomiModels))
	}
	for i, m := range xiaomiModels {
		if models[i] != m {
			t.Errorf("model[%d] = %q, want %q", i, models[i], m)
		}
	}
}

func TestLongcatSourceAPIBaseNormalization(t *testing.T) {
	src := NewLongcatSource(map[string]interface{}{"key": "sk-test"}, map[string]interface{}{})
	if src.apiBase != "https://api.longcat.chat/openai/v1" {
		t.Errorf("default api_base = %q", src.apiBase)
	}

	src2 := NewLongcatSource(map[string]interface{}{
		"api_base": "https://api.longcat.chat/openai", "key": "sk-test",
	}, map[string]interface{}{})
	if src2.apiBase != "https://api.longcat.chat/openai/v1" {
		t.Errorf("normalized api_base = %q, want .../openai/v1", src2.apiBase)
	}
}

func TestOpenAISourceStreamToolCallMalformedIndex(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		send := func(payload string) {
			_, _ = io.WriteString(w, "data: "+payload+"\n\n")
			flusher.Flush()
		}
		send(`{"choices":[{"delta":{"tool_calls":[{"index":-1,"id":"c1","function":{"name":"bad","arguments":"{}"}}]}}]}`)
		send(`{"choices":[{"delta":{"tool_calls":[{"index":1000,"id":"c2","function":{"name":"huge","arguments":"{}"}}]}}]}`)
		send(`{"choices":[{"delta":{"content":"hi"}}]}`)
		send(`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		send(`[DONE]`)
	})

	src := NewOpenAISource(map[string]interface{}{
		"api_base": srv.URL, "key": "sk-test", "model": "gpt-4o",
	}, map[string]interface{}{})

	ch, err := src.TextChatStream(context.Background(), &provider.ProviderRequest{Prompt: "hi"})
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
	if final.CompletionText != "hi" {
		t.Errorf("final text = %q, want %q", final.CompletionText, "hi")
	}
	if len(final.ToolsCallName) != 0 {
		t.Errorf("malformed tool calls should be skipped, got %v", final.ToolsCallName)
	}
}

func TestOpenAISourceStreamReadErrorPropagates(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		flusher.Flush()
		// Drop the connection mid-stream so the reader hits a hard error.
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
	})

	src := NewOpenAISource(map[string]interface{}{
		"api_base": srv.URL, "key": "sk-test", "model": "gpt-4o",
	}, map[string]interface{}{})

	ch, err := src.TextChatStream(context.Background(), &provider.ProviderRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var gotErr bool
	for item := range ch {
		if item.Role == "err" {
			gotErr = true
		}
	}
	if !gotErr {
		t.Fatal("expected an err chunk when the stream is interrupted")
	}
}
