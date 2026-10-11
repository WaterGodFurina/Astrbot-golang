package deerflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// TestIsStreamTimeout 问题 C 的判定辅助函数：覆盖 ctx 包装错误与文本兜底。
func TestIsStreamTimeout(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"裸超时", context.DeadlineExceeded, true},
		{"wrapped 超时", fmt.Errorf("read body: %w", context.DeadlineExceeded), true},
		{"wrapped 取消", fmt.Errorf("Get: %w", context.Canceled), true},
		{"http client timeout 文本", errors.New("Post: net/http: timeout awaiting response headers (Client.Timeout exceeded while awaiting headers)"), true},
		{"普通错误", errors.New("connection refused"), false},
		{"errStop", errStop, false},
	}
	for _, tc := range cases {
		if got := isStreamTimeout(tc.err); got != tc.want {
			t.Fatalf("%s: isStreamTimeout = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRunTimeoutReturnsPartialResult 问题 C 集成：StreamRun 内部 reqCtx 超时
// （父 ctx 未超时），应返回部分结果 + timedOut 注记 + role=err，而不是报错。
// 此前检查父 ctx.Err() 恒 nil 导致该路径不可达。
func TestRunTimeoutReturnsPartialResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// 先吐一条 values 事件（部分结果），然后挂住模拟长时间推理。
		fmt.Fprint(w, "event: values\n"+
			`data: {"messages":[{"role":"user","content":"hi"},{"id":"m1","role":"ai","content":"部分回答"}]}`+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	var gotSessionThread string
	r, err := NewRunner(Options{
		APIBase:     srv.URL,
		TimeoutSec:  0.3, // 短超时触发 StreamRun 内部 reqCtx 超时
		Streaming:   false,
		AssistantID: "lead_agent",
	}, Deps{
		Logger: discardLogger(),
		SessionGet: func(ctx context.Context, sessionID, key string) (interface{}, error) {
			if key == deerflowThreadIDKey {
				return "thread-1", nil
			}
			return nil, nil
		},
		SessionPut: func(ctx context.Context, sessionID, key string, value interface{}) error {
			gotSessionThread, _ = value.(string)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := r.Run(context.Background(), &provider.ProviderRequest{Prompt: "hi", SessionID: "s1"}, nil)
	if err != nil {
		t.Fatalf("超时应返回部分结果而非错误，got: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Run 未及时返回")
	}
	if resp.Role != "err" {
		t.Fatalf("role = %q, want err", resp.Role)
	}
	chain := resp.ResultChain.(*message.MessageChain)
	var text string
	for _, c := range chain.Chain {
		if p, ok := c.(*message.Plain); ok {
			text += p.Text
		}
	}
	if !strings.Contains(text, "部分回答") {
		t.Fatalf("应包含部分结果文本，got %q", text)
	}
	if !strings.Contains(text, "timed out") {
		t.Fatalf("应包含 timed out 注记，got %q", text)
	}
	if gotSessionThread != "" {
		t.Log("thread id persisted:", gotSessionThread)
	}
}

// TestNewAPIClientProxy 配置 proxy 时 Transport.Proxy 生效（问题 F）；
// 未配置时不覆盖调用方的 Transport。
func TestNewAPIClientProxy(t *testing.T) {
	c := NewAPIClient("", "", "", "http://127.0.0.1:7890", nil)
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatalf("配置 proxy 后 Transport.Proxy 不应为 nil")
	}
	req, _ := http.NewRequest("GET", "http://example.com/", nil)
	u, err := tr.Proxy(req)
	if err != nil || u == nil || u.Host != "127.0.0.1:7890" {
		t.Fatalf("Proxy() = %v, %v", u, err)
	}

	// 空 proxy：不注入。
	c2 := NewAPIClient("", "", "", "", nil)
	if c2.http.Transport != nil {
		t.Fatalf("空 proxy 不应设置 Transport")
	}

	// 非 http(s) scheme 的非法值：忽略。
	c3 := NewAPIClient("", "", "", "not-a-proxy", nil)
	if c3.http.Transport != nil {
		t.Fatalf("非法 proxy 不应设置 Transport")
	}
}

// TestStreamRunSSE 流式解析冒烟（保证生命周期/超时改动未破坏常规消费）。
func TestStreamRunSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: values\n"+
			`data: {"messages":[{"id":"m1","role":"ai","content":"回答"}]}`+"\n\n")
		fmt.Fprint(w, "event: end\n"+`data: {}`+"\n\n")
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "", "", "", nil)
	var events []string
	err := c.StreamRun(context.Background(), "t1", map[string]interface{}{}, 5*time.Second, func(event string, data interface{}) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0] != "values" || events[1] != "end" {
		t.Fatalf("events = %v", events)
	}
}
