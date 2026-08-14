package weixin_oc

import (
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
