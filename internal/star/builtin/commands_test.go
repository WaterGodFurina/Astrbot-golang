package builtin

import (
	"testing"

	"github.com/AstrBotDevs/AstrBot/internal/config"
	"github.com/AstrBotDevs/AstrBot/internal/conversation"
	"github.com/AstrBotDevs/AstrBot/internal/core"
	"github.com/AstrBotDevs/AstrBot/internal/star"
	"github.com/AstrBotDevs/AstrBot/pkg/message"
)

func newTestDeps(t *testing.T) (Deps, *star.Manager) {
	t.Helper()
	cm := config.NewConfigManager()
	cfg, err := config.New(t.TempDir()+"/cmd_config.json", map[string]interface{}{}, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	cm.Register("default", cfg)
	sm := star.NewManagerSimple()
	deps := Deps{
		StarMgr:         sm,
		ConfigMgr:       cm,
		ConversationMgr: conversation.NewManager(),
	}
	RegisterBuiltin(deps)
	return deps, sm
}

func newEvent(msg string) *core.Event {
	return &core.Event{
		MessageStr:        msg,
		PlainText:         msg,
		IsAtOrWakeCommand: true,
		Role:              "admin",
		Source: core.EventSource{
			Platform: "qq_official",
			ConvID:   "conv1",
			SenderID: "user1",
			IsGroup:  false,
		},
	}
}

func TestHelpCommand(t *testing.T) {
	_, sm := newTestDeps(t)
	e := newEvent("help")
	matched := false
	for _, h := range sm.Handlers().GetFilterHandlers() {
		if h.HandlerName != "help" {
			continue
		}
		fctx := &star.FilterContext{MessageStr: e.MessageStr, IsAtOrWake: true, EventRole: e.Role}
		ok := true
		for _, f := range h.EventFilters {
			if !f.Match(fctx) {
				ok = false
				break
			}
		}
		if ok {
			matched = true
			_ = h.Handler(e)
		}
	}
	if !matched {
		t.Fatal("help command did not match")
	}
	if e.Result == nil || len(e.Result.Chain) == 0 {
		t.Fatal("help command produced no result")
	}
	if plain, ok := e.Result.Chain[0].(*message.Plain); !ok || plain.Text == "" {
		t.Fatal("help result is not plain text")
	}
}

func TestSidCommand(t *testing.T) {
	_, sm := newTestDeps(t)
	e := newEvent("sid")
	for _, h := range sm.Handlers().GetFilterHandlers() {
		if h.HandlerName != "sid" {
			continue
		}
		fctx := &star.FilterContext{MessageStr: e.MessageStr, IsAtOrWake: true, EventRole: e.Role}
		if h.EventFilters[0].Match(fctx) {
			_ = h.Handler(e)
		}
	}
	if e.Result == nil {
		t.Fatal("sid command produced no result")
	}
}

func TestNewConversationCommand(t *testing.T) {
	deps, sm := newTestDeps(t)
	e := newEvent("new")
	for _, h := range sm.Handlers().GetFilterHandlers() {
		if h.HandlerName != "new" {
			continue
		}
		fctx := &star.FilterContext{MessageStr: e.MessageStr, IsAtOrWake: true, EventRole: e.Role}
		if h.EventFilters[0].Match(fctx) {
			_ = h.Handler(e)
		}
	}
	if e.Result == nil {
		t.Fatal("new command produced no result")
	}
	cid := deps.ConversationMgr.GetCurrConversationID("qq_official:conv1")
	if cid == "" {
		t.Fatal("new conversation was not created")
	}
}

func TestProviderCommandAdminOnly(t *testing.T) {
	_, sm := newTestDeps(t)
	// Non-admin should be rejected by the permission filter
	e := newEvent("provider")
	e.Role = "member"
	rejected := false
	for _, h := range sm.Handlers().GetFilterHandlers() {
		if h.HandlerName != "provider" {
			continue
		}
		fctx := &star.FilterContext{MessageStr: e.MessageStr, IsAtOrWake: true, EventRole: e.Role}
		for _, f := range h.EventFilters {
			if !f.Match(fctx) {
				rejected = true
				break
			}
		}
	}
	if !rejected {
		t.Fatal("provider command should require admin")
	}
}

func TestSetUnsetVariables(t *testing.T) {
	_, sm := newTestDeps(t)
	e := newEvent("set mykey hello world")
	for _, h := range sm.Handlers().GetFilterHandlers() {
		if h.HandlerName != "set" {
			continue
		}
		fctx := &star.FilterContext{MessageStr: e.MessageStr, IsAtOrWake: true, EventRole: e.Role}
		if h.EventFilters[0].Match(fctx) {
			_ = h.Handler(e)
		}
	}
	state.mu.Lock()
	vars := state.sessionVars["qq_official:conv1"]
	state.mu.Unlock()
	if vars == nil || vars["mykey"] != "hello world" {
		t.Fatalf("session variable not stored correctly: %v", vars)
	}
}
