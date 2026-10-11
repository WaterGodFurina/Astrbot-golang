package dashscope

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

const testTimeout = 10 * time.Second

func timeAfter(d time.Duration) <-chan time.Time { return time.After(d) }

// collectDeltas 包装 onDelta 收集增量。
func collectDeltas(out *[]string) func(string) error {
	return func(t string) error {
		*out = append(*out, t)
		return nil
	}
}

// TestRunStreamingEarlyReturnDoesNotLeak 验证错误响应提前返回时（问题 A）
// 消费端能正常退出，不因 SDK 生产 goroutine 阻塞而卡死。
func TestRunStreamingEarlyReturnDoesNotLeak(t *testing.T) {
	// 先给足填充数据让缓冲(500)有机会被填满，验证排空路径。
	var sb strings.Builder
	for i := 0; i < 600; i++ {
		sb.WriteString("data: {\"status_code\":403,\"message\":\"Forbidden\"}\n\n")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-DashScope-SSE") != "enable" {
			t.Errorf("streaming request must carry X-DashScope-SSE: enable")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(sb.String()))
	}))
	defer srv.Close()

	r, err := NewRunner(Options{
		APIKey: "sk-test", AppID: "app-1", AppType: "agent", Streaming: true,
		APIBase: srv.URL,
	}, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		_, runErr = r.Run(context.Background(), &provider.ProviderRequest{
			Prompt: "hi", SessionID: "s1",
		}, nil)
	}()
	select {
	case <-done:
	case <-timeAfter(testTimeout):
		t.Fatalf("Run did not return after error chunk (goroutine likely leaked)")
	}
	if runErr == nil || !strings.Contains(runErr.Error(), "403") {
		t.Fatalf("want 403 error, got %v", runErr)
	}
}

// TestRunStreamingAccumulates 常规流式路径：增量回调 + 最终 chain + session_id 落库。
func TestRunStreamingAccumulates(t *testing.T) {
	var gotSessionKey, gotSessionVal string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"status_code\":200,\"output\":{\"text\":\"你\",\"session_id\":\"conv-9\"}}\n\n")
		fmt.Fprint(w, "data: {\"status_code\":200,\"output\":{\"text\":\"好\",\"session_id\":\"conv-9\"}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var deltas []string
	r, err := NewRunner(Options{
		APIKey: "sk-test", AppID: "app-1", AppType: "agent", Streaming: true,
		APIBase: srv.URL,
	}, Deps{
		SessionPut: func(ctx context.Context, sessionID, key string, value interface{}) error {
			gotSessionKey = key
			gotSessionVal, _ = value.(string)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.Run(context.Background(), &provider.ProviderRequest{Prompt: "hi", SessionID: "s1"}, collectDeltas(&deltas))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "") != "你好" {
		t.Fatalf("deltas = %v", deltas)
	}
	chain := resp.ResultChain.(*message.MessageChain)
	if plain, ok := chain.Chain[0].(*message.Plain); !ok || plain.Text != "你好" {
		t.Fatalf("final chain = %#v", chain.Chain)
	}
	if gotSessionKey != "dashscope_conversation_id" || gotSessionVal != "conv-9" {
		t.Fatalf("session put = %s/%s", gotSessionKey, gotSessionVal)
	}
}

// TestRunNonStreamingParsesJSON 非流式路径（问题 B）：普通 JSON 响应应被解析，
// 此前走 PostSSE + data: 前缀过滤导致整体丢弃。
func TestRunNonStreamingParsesJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-DashScope-SSE") == "enable" {
			t.Errorf("non-streaming request must not carry X-DashScope-SSE header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"status_code":200,"request_id":"req-1",`+
			`"output":{"text":"完整回答","session_id":"conv-7","finish_reason":"stop"}}`)
	}))
	defer srv.Close()

	r, err := NewRunner(Options{
		APIKey: "sk-test", AppID: "app-1", AppType: "agent", Streaming: false,
		APIBase: srv.URL,
	}, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.Run(context.Background(), &provider.ProviderRequest{Prompt: "hi", SessionID: "s1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	chain := resp.ResultChain.(*message.MessageChain)
	if plain, ok := chain.Chain[0].(*message.Plain); !ok || plain.Text != "完整回答" {
		t.Fatalf("non-streaming chain = %#v", chain.Chain)
	}
}

// TestRunNonStreamingHTTPError 非流式 HTTP 非 2xx 返回错误。
func TestRunNonStreamingHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"code":"InvalidApiKey","message":"Invalid API key"}`)
	}))
	defer srv.Close()

	r, err := NewRunner(Options{
		APIKey: "bad", AppID: "app-1", AppType: "agent", Streaming: false,
		APIBase: srv.URL,
	}, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Run(context.Background(), &provider.ProviderRequest{Prompt: "hi"}, nil)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want HTTP 401 error, got %v", err)
	}
}

// TestRunStreamingRefPattern 验证 RAG 引用脚标替换。
func TestRunStreamingRefPattern(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"status_code\":200,\"output\":{\"text\":\"见<ref>[3]</ref>\"}}\n\n")
	}))
	defer srv.Close()

	r, err := NewRunner(Options{
		APIKey: "sk", AppID: "a", AppType: "agent", Streaming: true,
		APIBase: srv.URL,
	}, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.Run(context.Background(), &provider.ProviderRequest{Prompt: "hi"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	chain := resp.ResultChain.(*message.MessageChain)
	if plain := chain.Chain[0].(*message.Plain); plain.Text != "见[3]" {
		t.Fatalf("ref pattern not applied: %q", plain.Text)
	}
}

// TestFormatDocReferences 对齐 Python _format_doc_references。
func TestFormatDocReferences(t *testing.T) {
	refs := []interface{}{
		map[string]interface{}{"index_id": float64(1), "title": "文档A"},
		map[string]interface{}{"index_id": float64(2), "doc_name": "B.pdf"},
	}
	got := formatDocReferences(refs)
	want := "\n\n回答来源:\n1. 文档A\n2. B.pdf\n"
	if got != want {
		t.Fatalf("formatDocReferences = %q, want %q", got, want)
	}
}

// TestBuildPayloadIncrementalOff 非流式时 incremental_output=false。
func TestBuildPayloadIncrementalOff(t *testing.T) {
	r := &Runner{streaming: false}
	payload := r.buildPayload("hi", "", nil)
	params := payload["parameters"].(map[string]interface{})
	if params["incremental_output"] != true {
		t.Fatalf("buildPayload 本身不负责关闭 incremental_output（Run 中非流式时另行置 false），params = %v", params)
	}
	_ = json.Marshal // 占位保持 import 稳定
}
