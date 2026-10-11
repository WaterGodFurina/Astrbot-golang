package coze

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDoJSONNon2xxReturnsError 验证 doJSON 非 2xx 时不再把错误 body 当 JSON
// 解析返回（问题 D），对齐 Python raise_for_status 语义。
func TestDoJSONNon2xxReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"code":4100,"msg":"invalid request"}`)
	}))
	defer srv.Close()

	c := NewAPIClient("key", srv.URL, nil)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/v3/conversation/message/clear_context", strings.NewReader(`{}`))
	_, err := c.doJSON(req)
	if err == nil {
		t.Fatalf("doJSON 应在非 2xx 时返回错误")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid request") {
		t.Fatalf("错误应包含状态码与 body 片段，got: %v", err)
	}
}

// TestDoJSON2xxParsesBody 正常 2xx 路径不受影响。
func TestDoJSON2xxParsesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		io.WriteString(w, `{"code":0,"data":{"ok":true}}`)
	}))
	defer srv.Close()

	c := NewAPIClient("key", srv.URL, nil)
	out, err := c.GetMessageList(context.Background(), "conv1", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := out["code"].(float64); code != 0 {
		t.Fatalf("out = %v", out)
	}
}

// TestUploadFileNon2xx 上传失败同样返回状态码+body（含认证失败分支）。
func TestUploadFileNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"code":4101,"msg":"unauthorized"}`)
	}))
	defer srv.Close()

	c := NewAPIClient("bad-key", srv.URL, nil)
	_, err := c.UploadFile(context.Background(), []byte("data"))
	if err == nil || !strings.Contains(err.Error(), "认证失败") {
		t.Fatalf("401 应返回认证失败错误，got: %v", err)
	}
}

// TestCheckHTTPStatusBodyLimit body 片段限 4KB。
func TestCheckHTTPStatusBodyLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, strings.Repeat("x", 100_000))
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	err = checkHTTPStatus(resp)
	if err == nil {
		t.Fatal("want error")
	}
	if len(err.Error()) > 4200 {
		t.Fatalf("错误信息应限 4KB 左右，got %d bytes", len(err.Error()))
	}
}

// TestStreamChatSSE 基本事件解析（保障状态码检查改动未破坏流式路径）。
func TestStreamChatSSE(t *testing.T) {
	body := "event: conversation.message.delta\n" +
		`data: {"content":"你"}` + "\n\n" +
		"event: conversation.chat.completed\n" +
		`data: {"status":"done"}` + "\n\n"
	var events []string
	err := streamChatSSE(strings.NewReader(body), func(eventType string, data map[string]interface{}) error {
		events = append(events, eventType)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0] != "conversation.message.delta" || events[1] != "conversation.chat.completed" {
		t.Fatalf("events = %v", events)
	}
	_ = json.Marshal
}
