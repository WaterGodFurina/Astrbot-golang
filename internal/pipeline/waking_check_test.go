package pipeline

import (
	"context"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/star"
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

// TestWakingCheckIgnoreBotSelf: ignore_bot_self_message=true drops messages
// whose sender id equals the bot's own id.
func TestWakingCheckIgnoreBotSelf(t *testing.T) {
	s := wakingStage(t, false, false)
	s.ignoreBotSelfMessage = true
	ev := wakeEvent("/hello", false, "bot-openid", []message.Component{&message.Plain{Text: "/hello"}})
	ev.Source.SenderID = "bot-openid"
	ev.Source.SelfID = "bot-openid"
	if _, err := s.Process(context.Background(), ev); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !ev.IsStopped() {
		t.Error("bot self message must be stopped when ignore_bot_self_message=true")
	}
}

// TestWakingCheckUniqueSession: unique_session rewrites the group conversation
// id per member for supported platforms.
func TestWakingCheckUniqueSession(t *testing.T) {
	s := wakingStage(t, false, false)
	s.uniqueSession = true
	ev := wakeEvent("hello", true, "bot-openid", []message.Component{&message.Plain{Text: "hello"}})
	ev.Source.Platform = "qq_official"
	ev.Source.SenderID = "u123"
	ev.Source.ConvID = "g456"
	if _, err := s.Process(context.Background(), ev); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if ev.Source.ConvID != "u123" {
		t.Errorf("qq_official unique session: want u123, got %q", ev.Source.ConvID)
	}
	ev2 := wakeEvent("hello", true, "bot-openid", []message.Component{&message.Plain{Text: "hello"}})
	ev2.Source.Platform = "lark"
	ev2.Source.SenderID = "u456"
	ev2.Source.ConvID = "g789"
	if _, err := s.Process(context.Background(), ev2); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if ev2.Source.ConvID != "u456%g789" {
		t.Errorf("lark unique session: want u456%%g789, got %q", ev2.Source.ConvID)
	}
}

// TestWakingCheckEmptyMention: a lone @mention of the bot triggers the LLM
// greeting when empty_mention_waiting is enabled.
func TestWakingCheckEmptyMention(t *testing.T) {
	s := wakingStage(t, false, false)
	s.emptyMentionWaiting = true
	s.emptyMentionWaitingNeedReply = true
	ev := wakeEvent("", true, "bot-openid", []message.Component{&message.At{TargetID: "bot-openid"}})
	ev.Source.SelfID = "bot-openid"
	if _, err := s.Process(context.Background(), ev); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !ev.IsAtOrWakeCommand {
		t.Error("lone @mention should wake the bot")
	}
	if ev.PlainText != emptyMentionPrompt {
		t.Error("empty mention should inject the greeting prompt")
	}
	// need_reply=false stops the event instead.
	s2 := wakingStage(t, false, false)
	s2.emptyMentionWaiting = true
	s2.emptyMentionWaitingNeedReply = false
	ev2 := wakeEvent("", true, "bot-openid", []message.Component{&message.At{TargetID: "bot-openid"}})
	ev2.Source.SelfID = "bot-openid"
	if _, err := s2.Process(context.Background(), ev2); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !ev2.IsStopped() {
		t.Error("empty mention with need_reply=false must stop the event")
	}
}

// TestFindMatchingHandlersPermissionDenied: a handler guarded by an admin
// permission filter is not matched for non-admin events, and the permission
// denied flag is raised (ported from waking_check permission handling).
func TestFindMatchingHandlersPermissionDenied(t *testing.T) {
	s := &ProcessStage{pluginMgr: star.NewManagerSimple()}
	s.pluginMgr.Handlers().Append(&star.StarHandlerMetadata{
		HandlerFullName:   "p_cmd",
		HandlerName:       "cmd",
		HandlerModulePath: "test_plugin",
		EventType:         star.EventTypeFilter,
		EventFilters:      []star.HandlerFilter{star.NewCommandFilter("admin_cmd", nil, nil), star.NewPermissionFilter(star.PermissionAdmin)},
		Enabled:           true,
	})
	ev := &core.Event{}
	ev.MessageStr = "admin_cmd"
	ev.IsAtOrWakeCommand = true
	ev.Role = "member"
	handlers, denied := s.findMatchingHandlers(ev)
	if len(handlers) != 0 {
		t.Error("non-admin must not match an admin-only handler")
	}
	if !denied {
		t.Error("permission denied flag must be raised")
	}
	ev.Role = "admin"
	handlers, denied = s.findMatchingHandlers(ev)
	if len(handlers) != 1 || denied {
		t.Error("admin must match the admin-only handler")
	}
}

// TestFindMatchingHandlersDisableBuiltin: disable_builtin_commands skips
// built-in command handlers.
func TestFindMatchingHandlersDisableBuiltin(t *testing.T) {
	s := &ProcessStage{pluginMgr: star.NewManagerSimple(), disableBuiltinCommands: true}
	s.pluginMgr.Handlers().Append(&star.StarHandlerMetadata{
		HandlerFullName:   "builtin_help",
		HandlerName:       "help",
		HandlerModulePath: "astrbot.builtin_stars.builtin_commands.main",
		EventType:         star.EventTypeFilter,
		EventFilters:      []star.HandlerFilter{star.NewCommandFilter("help", nil, nil)},
		Enabled:           true,
	})
	s.pluginMgr.Handlers().Append(&star.StarHandlerMetadata{
		HandlerFullName:   "p_echo",
		HandlerName:       "echo",
		HandlerModulePath: "test_plugin",
		EventType:         star.EventTypeFilter,
		EventFilters:      []star.HandlerFilter{star.NewCommandFilter("echo", nil, nil)},
		Enabled:           true,
	})
	ev := &core.Event{}
	ev.MessageStr = "help"
	ev.IsAtOrWakeCommand = true
	handlers, _ := s.findMatchingHandlers(ev)
	if len(handlers) != 0 {
		t.Error("builtin handler must be skipped when disable_builtin_commands=true")
	}
	ev.MessageStr = "echo"
	handlers, _ = s.findMatchingHandlers(ev)
	if len(handlers) != 1 {
		t.Error("plugin handler must still match")
	}
}

// TestWakingCheckAdminRole: senders listed in config "admins_id" get
// event.Role="admin" (so admin-gated commands like /provider respond); everyone
// else is "member". Mirrors astrbot/core/pipeline/waking_check/stage.py.
func TestWakingCheckAdminRole(t *testing.T) {
	s := NewWakingCheckStage()
	if err := s.Initialize(&PipelineContext{
		AstrbotConfig: map[string]interface{}{
			"wake_prefix": []interface{}{"/"},
			"admins_id":   []interface{}{"astrbot", "u-admin"},
		},
	}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	admin := wakeEvent("/provider", true, "bot", []message.Component{&message.Plain{Text: "/provider"}})
	admin.Source.SenderID = "u-admin"
	if _, err := s.Process(context.Background(), admin); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if admin.Role != "admin" {
		t.Errorf("sender in admins_id: want role admin, got %q", admin.Role)
	}
	if !admin.Source.IsAdmin {
		t.Error("Source.IsAdmin should be true for an admin sender")
	}

	normal := wakeEvent("/provider", true, "bot", []message.Component{&message.Plain{Text: "/provider"}})
	normal.Source.SenderID = "u-normal"
	if _, err := s.Process(context.Background(), normal); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if normal.Role != "member" {
		t.Errorf("non-admin sender: want role member, got %q", normal.Role)
	}
	if normal.Source.IsAdmin {
		t.Error("Source.IsAdmin should be false for a normal sender")
	}
}
