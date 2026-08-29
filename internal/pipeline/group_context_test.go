package pipeline

import (
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

func groupCtxConfig() map[string]interface{} {
	return map[string]interface{}{
		"provider_ltm_settings": map[string]interface{}{
			"group_icl_enable":      true,
			"group_message_max_cnt": 100,
			"active_reply": map[string]interface{}{
				"enable": true, "method": "possibility_reply",
				"possibility_reply": 1.0, // always reply for tests
				"whitelist":         []interface{}{"telegram:GroupMessage:g1"},
			},
		},
	}
}

func groupEvent(text string) *core.Event {
	return &core.Event{
		Source: core.EventSource{
			Platform: "telegram", SenderID: "u1", SenderName: "user1",
			ConvID: "g1", IsGroup: true,
		},
		Message:    &message.MessageChain{Chain: []message.Component{&message.Plain{Text: text}}},
		MessageStr: text,
		Metadata:   map[string]interface{}{},
	}
}

// TestGroupContextHandleAndInject: records are injected before the current
// message and consumed afterwards.
func TestGroupContextHandleAndInject(t *testing.T) {
	g := NewGroupChatContext(groupCtxConfig())
	ev1 := groupEvent("第一条群消息")
	g.HandleMessage(ev1)
	ev2 := groupEvent("第二条群消息")
	g.HandleMessage(ev2)

	// Inject history for ev2's LLM request: only ev1 should be injected.
	req := &provider.ProviderRequest{}
	ev3 := groupEvent("当前消息")
	g.HandleMessage(ev3)
	g.OnReqLLM(ev3, req)
	if len(req.ExtraUserContentParts) != 1 {
		t.Fatalf("expected 1 injected part, got %d", len(req.ExtraUserContentParts))
	}
	text, _ := req.ExtraUserContentParts[0]["text"].(string)
	if text == "" {
		t.Fatal("injected part must be text")
	}
	if len(text) < len(groupHistoryHeader) {
		t.Errorf("injected text too short: %q", text)
	}
}

// TestGroupContextNeedActiveReply: whitelist allows the umo; probability 1.0
// triggers.
func TestGroupContextNeedActiveReply(t *testing.T) {
	g := NewGroupChatContext(groupCtxConfig())
	ev := groupEvent("普通群消息")
	ev.IsAtOrWakeCommand = false
	if !g.NeedActiveReply(ev) {
		t.Error("whitelisted group with probability 1.0 must trigger active reply")
	}
	// Woken messages never trigger.
	ev.IsAtOrWakeCommand = true
	if g.NeedActiveReply(ev) {
		t.Error("woken messages must not trigger active reply")
	}
}

// TestGroupContextWhitelistDenies: non-whitelisted umo is not triggered.
func TestGroupContextWhitelistDenies(t *testing.T) {
	g := NewGroupChatContext(groupCtxConfig())
	ev := groupEvent("普通群消息")
	ev.Source.ConvID = "other-group"
	if g.NeedActiveReply(ev) {
		t.Error("non-whitelisted group must not trigger active reply")
	}
}

// TestGroupContextRemoveSession: records are cleared.
func TestGroupContextRemoveSession(t *testing.T) {
	g := NewGroupChatContext(groupCtxConfig())
	g.HandleMessage(groupEvent("m1"))
	g.HandleMessage(groupEvent("m2"))
	if n := g.RemoveSession("telegram:GroupMessage:g1"); n != 2 {
		t.Errorf("expected 2 records removed, got %d", n)
	}
	req := &provider.ProviderRequest{}
	ev := groupEvent("after")
	g.HandleMessage(ev)
	g.OnReqLLM(ev, req)
	if len(req.ExtraUserContentParts) != 0 {
		t.Errorf("records must be empty after RemoveSession, got %d", len(req.ExtraUserContentParts))
	}
}

// TestCollectFileAttachments: File components in the current message and inside
// a quoted reply (Reply.Chain) are surfaced as [File Attachment ...] parts.
// Files without a resolvable download URL are marked accordingly (no network
// is touched in this test).
func TestCollectFileAttachments(t *testing.T) {
	ev := &core.Event{
		Message: &message.MessageChain{Chain: []message.Component{
			&message.Reply{MessageID: "r1", Chain: []message.Component{
				&message.File{Name: "bug.md", FileID: "/abc"}, // no URL
			}},
			&message.File{Name: "top.txt", URL: "https:///ftn_handler//?fname="}, // invalid host URL
		}},
	}
	parts := collectFileAttachments(ev)
	if len(parts) != 2 {
		t.Fatalf("expected 2 attachment parts, got %d: %v", len(parts), parts)
	}
	if parts[0] != "[File Attachment in quoted message: name bug.md (no download URL resolved)]" {
		t.Errorf("quoted attachment mismatch: %q", parts[0])
	}
	if parts[1] != "[File Attachment: name top.txt (no download URL resolved)]" {
		t.Errorf("top-level attachment mismatch: %q", parts[1])
	}
}
