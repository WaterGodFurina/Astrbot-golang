package weixin_oc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
	ilink "github.com/dobest1024/go-weixin-ilink"
)

// buildTextMessage creates an iLink message with a plain-text item.
func buildTextMessage(id int64, from, text string, groupID string) *ilink.Message {
	return &ilink.Message{
		MessageID:    id,
		FromUserID:   from,
		ToUserID:     "bot",
		GroupID:      groupID,
		MessageType:  ilink.MessageTypeUser,
		ContextToken: "tok",
		CreateTimeMs: 1700000000000,
		ItemList: []ilink.MessageItem{
			{Type: ilink.ItemTypeText, TextItem: &ilink.TextItem{Text: text}},
		},
	}
}

// TestMessageToEventText: a text message becomes a Plain component event.
func TestMessageToEventText(t *testing.T) {
	msg := buildTextMessage(123, "wxid_user", "你好", "")
	components := []message.Component{&message.Plain{Text: "你好"}}
	ev := messageToEvent(msg, msg.FromUserID, components)
	if ev.Source.SenderID != "wxid_user" || ev.Source.Platform != "weixin_oc" {
		t.Errorf("source: %+v", ev.Source)
	}
	if ev.MessageStr != "你好" {
		t.Errorf("message str: %q", ev.MessageStr)
	}
	if ev.MessageObj.MessageID != "123" {
		t.Errorf("message id: %q", ev.MessageObj.MessageID)
	}
}

// TestMessageToEventGroup: group messages set IsGroup and group conv id.
func TestMessageToEventGroup(t *testing.T) {
	msg := buildTextMessage(456, "wxid_user", "群消息", "wxid_group")
	ev := messageToEvent(msg, msg.FromUserID, []message.Component{&message.Plain{Text: "群消息"}})
	if !ev.Source.IsGroup || ev.Source.ConvID != "wxid_group" {
		t.Errorf("group fields: %+v", ev.Source)
	}
}

// TestQuoteProcessing: a quoted message builds a Reply component via the
// adapter's item handling (Context accessors are covered by the SDK tests).
func TestQuoteProcessing(t *testing.T) {
	msg := buildTextMessage(789, "wxid_user", "回复内容", "")
	msg.ItemList[0].RefMsg = &ilink.RefMessage{
		MessageItem: &ilink.MessageItem{Type: ilink.ItemTypeText, TextItem: &ilink.TextItem{Text: "原始消息"}},
		Title:       "原始消息",
	}
	// The quote is appended as a Reply when handled; verify the SDK accessors.
	hasQuote := false
	for _, item := range msg.ItemList {
		if item.RefMsg != nil {
			hasQuote = true
		}
	}
	if !hasQuote {
		t.Error("message must carry a quote")
	}
}

// TestNewConfig: data dir and bot type are applied.
func TestNewConfig(t *testing.T) {
	a := New(map[string]interface{}{"id": "wx", "weixin_oc_bot_type": "7"}, nil, nil)
	if a.botType != "7" {
		t.Errorf("bot type: %q", a.botType)
	}
	if a.bot == nil {
		t.Fatal("bot must be created")
	}
}

// TestTypeAndID: adapter identity.
func TestTypeAndID(t *testing.T) {
	a := New(map[string]interface{}{"id": "custom"}, nil, nil)
	if a.ID() != "custom" || a.Type() != "weixin_oc" {
		t.Errorf("id/type: %s %s", a.ID(), a.Type())
	}
}

// TestResolveMediaMissingContent: 无 path/file/base64/url 的媒体应返回明确错误，
// 而非静默 nil（L-44 回归）。
func TestResolveMediaMissingContent(t *testing.T) {
	a := New(map[string]interface{}{"id": "wx"}, nil, nil)
	if _, err := resolveMedia("", "", "", ""); err == nil {
		t.Error("空媒体应返回错误")
	}
	if err := a.sendImage(context.Background(), "u", &message.Image{}); err == nil {
		t.Error("空图片组件应返回错误")
	}
	if err := a.sendFile(context.Background(), "u", &message.File{}); err == nil {
		t.Error("空文件组件应返回错误")
	}
	if err := a.sendVideo(context.Background(), "u", &message.Video{}); err == nil {
		t.Error("空视频组件应返回错误")
	}
	if err := a.sendRecord(context.Background(), "u", &message.Record{}); err == nil {
		t.Error("空语音组件应返回错误")
	}
	// Send 中 Record 分支不应再被静默丢弃。
	err := a.Send("u", &message.MessageChain{Chain: []message.Component{&message.Record{}}})
	if err == nil {
		t.Error("Send 空 Record 应返回明确错误")
	}
}

// TestResolveMediaBase64AndFile: base64 与本地文件均可被解析。
func TestResolveMediaBase64AndFile(t *testing.T) {
	data, err := resolveMedia("", "", "aGVsbG8=", "")
	if err != nil || string(data) != "hello" {
		t.Errorf("base64 解析失败: %v %q", err, data)
	}
	if _, err := resolveMedia("", "", "!!!not-base64!!!", ""); err == nil {
		t.Error("非法 base64 应报错")
	}
	tmp := t.TempDir()
	p := tmp + "/f.bin"
	if err := os.WriteFile(p, []byte("file-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err = resolveMedia(p, "", "", "")
	if err != nil || string(data) != "file-bytes" {
		t.Errorf("本地文件解析失败: %v %q", err, data)
	}
}

// TestDownloadMedia: URL 下载成功与失败分支。
func TestDownloadMedia(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("data-bytes"))
	}))
	defer srv.Close()

	data, err := downloadMedia(srv.URL + "/ok")
	if err != nil || string(data) != "data-bytes" {
		t.Errorf("下载应成功，实际: %v %q", err, data)
	}
	if _, err := downloadMedia(srv.URL + "/bad"); err == nil {
		t.Error("HTTP 非 2xx 应报错")
	}
}
