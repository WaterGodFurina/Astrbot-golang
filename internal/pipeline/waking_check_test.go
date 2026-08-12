package pipeline

import (
	"context"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// wakeEvent builds a message event for waking-check tests.
func wakeEvent(msgStr string, isGroup bool, selfID string, chain []message.Component) *core.Event {
	return &core.Event{
		Type:       core.EventMessage,
		Message:    &message.MessageChain{Chain: chain},
		MessageStr: msgStr,
		PlainText:  msgStr,
		Source:     core.EventSource{IsGroup: isGroup, SelfID: selfID},
	}
}

func wakingStage(t *testing.T, friendNeedsPrefix bool, ignoreAtAll bool) *WakingCheckStage {
	t.Helper()
	s := NewWakingCheckStage()
	if err := s.Initialize(&PipelineContext{
		AstrbotConfig: map[string]interface{}{
			"wake_prefix": []interface{}{"/"},
			"platform_settings": map[string]interface{}{
				"friend_message_needs_wake_prefix": friendNeedsPrefix,
				"ignore_at_all":                    ignoreAtAll,
			},
		},
	}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return s
}

// TestWakingCheckFriendAutoWake: friend messages auto-respond when
// friend_message_needs_wake_prefix is false (no prefix / no @ needed).
func TestWakingCheckFriendAutoWake(t *testing.T) {
	s := wakingStage(t, false, false)
	ev := wakeEvent("hello there", false, "bot", []message.Component{&message.Plain{Text: "hello there"}})
	if _, err := s.Process(context.Background(), ev); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !ev.IsAtOrWakeCommand {
		t.Error("friend message should auto-wake when friend_message_needs_wake_prefix=false")
	}
}

// TestWakingCheckFriendNeedsPrefix: when friend_message_needs_wake_prefix is
// true, a plain friend message must NOT wake.
func TestWakingCheckFriendNeedsPrefix(t *testing.T) {
	s := wakingStage(t, true, false)
	ev := wakeEvent("hello there", false, "bot", []message.Component{&message.Plain{Text: "hello there"}})
	if _, err := s.Process(context.Background(), ev); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if ev.IsAtOrWakeCommand {
		t.Error("friend message must not wake when friend_message_needs_wake_prefix=true")
	}
}

// TestWakingCheckGroupMention: @mentioning the bot in a group wakes it. The At
// TargetID must equal Source.SelfID (the bot's real id).
func TestWakingCheckGroupMention(t *testing.T) {
	s := wakingStage(t, false, false)
	ev := wakeEvent("hello", true, "bot-openid", []message.Component{
		&message.At{TargetID: "bot-openid", Name: "bot"},
		&message.Plain{Text: "hello"},
	})
	if _, err := s.Process(context.Background(), ev); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !ev.IsAtOrWakeCommand {
		t.Error("group @mention of the bot should wake it")
	}
	if v, _ := ev.GetExtra("llm_wake").(bool); !v {
		t.Error("group @mention should enable chat (llm_wake=true)")
	}
}

// TestWakingCheckGroupAtAll: @all wakes the bot unless ignore_at_all.
func TestWakingCheckGroupAtAll(t *testing.T) {
	s := wakingStage(t, false, false)
	ev := wakeEvent("hello", true, "bot-openid", []message.Component{
		&message.AtAll{},
		&message.Plain{Text: "hello"},
	})
	if _, err := s.Process(context.Background(), ev); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !ev.IsAtOrWakeCommand {
		t.Error("group @all should wake the bot by default")
	}

	s2 := wakingStage(t, false, true)
	ev2 := wakeEvent("hello", true, "bot-openid", []message.Component{&message.AtAll{}})
	if _, err := s2.Process(context.Background(), ev2); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if ev2.IsAtOrWakeCommand {
		t.Error("group @all must not wake when ignore_at_all=true")
	}
}

// TestWakingCheckGroupPlainNotWoken: a plain group message (no prefix, no @,
// no @all) must NOT be woken — this is the design behavior the user asked about.
func TestWakingCheckGroupPlainNotWoken(t *testing.T) {
	s := wakingStage(t, false, false)
	ev := wakeEvent("hello group", true, "bot-openid", []message.Component{&message.Plain{Text: "hello group"}})
	if _, err := s.Process(context.Background(), ev); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if ev.IsAtOrWakeCommand {
		t.Error("plain group message must not wake the bot")
	}
}

// TestWakingCheckGroupPrefix: "/" prefix wakes a group message.
func TestWakingCheckGroupPrefix(t *testing.T) {
	s := wakingStage(t, false, false)
	ev := wakeEvent("/help", true, "bot-openid", []message.Component{&message.Plain{Text: "/help"}})
	if _, err := s.Process(context.Background(), ev); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !ev.IsAtOrWakeCommand {
		t.Error("group message with / prefix should wake the bot")
	}
}
