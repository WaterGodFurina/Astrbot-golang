package dify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// TestParseResultNilIsEmptyChain 问题 E：流结束无 workflow_finished 时
// finished == nil，此前 default 分支 fmt.Sprint(nil) 产生 "<nil>"。
// 对齐 Python：结果为空时 warning 且不 append。
func TestParseResultNilIsEmptyChain(t *testing.T) {
	r := NewRunner(Options{APIKey: "k", APIType: "workflow"}, Deps{Logger: discardLogger()})
	chain, err := r.parseResult(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.Chain) != 0 {
		t.Fatalf("nil result 应返回空 chain，got %#v", chain.Chain)
	}
	for _, c := range chain.Chain {
		if p, ok := c.(*message.Plain); ok && strings.Contains(p.Text, "<nil>") {
			t.Fatalf("不允许出现 <nil> 文本: %q", p.Text)
		}
	}
}

// TestParseResultEmptyString Chat 结果为空时返回空 chain（不含空 Plain 之外的意外内容）。
func TestParseResultEmptyString(t *testing.T) {
	r := NewRunner(Options{APIKey: "k"}, Deps{})
	chain, err := r.parseResult(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.Chain) != 1 {
		t.Fatalf("空串 Chat 结果对齐 py 返回仅含空 Plain 的 chain，got %#v", chain.Chain)
	}
	if p := chain.Chain[0].(*message.Plain); p.Text != "" {
		t.Fatalf("text = %q", p.Text)
	}
}

// TestParseResultFileList 文件列表输出：py 对全部为 dify file 的列表不 append 文本。
func TestParseResultFileList(t *testing.T) {
	r := NewRunner(Options{APIKey: "k", APIType: "workflow"}, Deps{})
	result := map[string]interface{}{
		"data": map[string]interface{}{
			"outputs": map[string]interface{}{
				"astrbot_wf_output": []interface{}{
					map[string]interface{}{
						"dify_model_identity": "__dify__file__",
						"type":                "image",
						"url":                 "http://example.com/a.png",
						"filename":            "a.png",
					},
				},
			},
			"files": []interface{}{},
		},
	}
	chain, err := r.parseResult(context.Background(), result)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chain.Chain {
		if p, ok := c.(*message.Plain); ok {
			t.Fatalf("纯文件列表不应生成文本组件，got %q", p.Text)
		}
	}
}

// TestParseResultMixedList 混合列表（含非文件项）兜底为整体文本。
func TestParseResultMixedList(t *testing.T) {
	r := NewRunner(Options{APIKey: "k", APIType: "workflow"}, Deps{})
	result := map[string]interface{}{
		"data": map[string]interface{}{
			"outputs": map[string]interface{}{
				"astrbot_wf_output": []interface{}{
					map[string]interface{}{"dify_model_identity": "__dify__file__"},
					"普通文本",
				},
			},
		},
	}
	chain, err := r.parseResult(context.Background(), result)
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.Chain) != 1 {
		t.Fatalf("混合列表应 append 一次整体文本，got %#v", chain.Chain)
	}
}

// TestParseResultTextOutput 普通 string 输出。
func TestParseResultTextOutput(t *testing.T) {
	r := NewRunner(Options{APIKey: "k", APIType: "workflow"}, Deps{})
	result := map[string]interface{}{
		"data": map[string]interface{}{
			"outputs": map[string]interface{}{"astrbot_wf_output": "工作流产出"},
		},
	}
	chain, err := r.parseResult(context.Background(), result)
	if err != nil {
		t.Fatal(err)
	}
	if p := chain.Chain[0].(*message.Plain); p.Text != "工作流产出" {
		t.Fatalf("text = %q", p.Text)
	}
}

// TestDoJSONNon2xxReturnsError 问题 D：非 2xx 不再把错误 body 当 JSON 返回。
func TestDoJSONNon2xxReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"status":403,"message":"forbidden"}`)
	}))
	defer srv.Close()

	c := NewAPIClient("key", srv.URL, nil)
	_, err := c.GetChatConversations(context.Background(), "u1", 20)
	if err == nil {
		t.Fatalf("非 2xx 应返回错误")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("错误应含状态码与 body，got: %v", err)
	}
}

// TestStreamPostNon2xxBodyLimit SSE 请求错误 body 限 4KB。
func TestStreamPostNon2xxBodyLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, strings.Repeat("e", 100_000))
	}))
	defer srv.Close()

	c := NewAPIClient("key", srv.URL, nil)
	err := c.ChatMessages(context.Background(), map[string]interface{}{}, 0, func(m map[string]interface{}) error { return nil })
	if err == nil {
		t.Fatal("want error")
	}
	if len(err.Error()) > 4200 {
		t.Fatalf("错误信息应限 4KB 左右，got %d bytes", len(err.Error()))
	}
}
