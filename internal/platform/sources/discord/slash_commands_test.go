package discord

import (
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/star"
	"github.com/bwmarrin/discordgo"
)

// TestExtractCommandInfoValid: a top-level command with a Discord-valid name
// is extracted with its description.
func TestExtractCommandInfoValid(t *testing.T) {
	f := star.NewCommandFilter("help", nil, nil)
	h := &star.StarHandlerMetadata{Desc: "显示帮助"}
	info := extractCommandInfo(f, h)
	if info == nil {
		t.Fatal("valid command must be extracted")
	}
	if info[0] != "help" || info[1] != "显示帮助" {
		t.Errorf("got %v", info)
	}
}

// TestExtractCommandInfoSubcommandSkipped: sub-commands are skipped.
func TestExtractCommandInfoSubcommandSkipped(t *testing.T) {
	parent := star.NewCommandGroupFilter("admin", nil, nil)
	child := star.NewCommandFilter("ban", nil, parent)
	h := &star.StarHandlerMetadata{Desc: "x"}
	if info := extractCommandInfo(child, h); info != nil {
		t.Errorf("sub-command must be skipped, got %v", info)
	}
}

// TestExtractCommandInfoInvalidName: uppercase / invalid names are skipped.
func TestExtractCommandInfoInvalidName(t *testing.T) {
	h := &star.StarHandlerMetadata{}
	for _, name := range []string{"Help", "my command", "averylongcommandnameexceedingthirtytwocharacters", "bad/name"} {
		f := star.NewCommandFilter(name, nil, nil)
		if info := extractCommandInfo(f, h); info != nil {
			t.Errorf("invalid name %q must be skipped, got %v", name, info)
		}
	}
}

// TestExtractCommandInfoDescriptionTruncated: descriptions over 100 chars
// are truncated.
func TestExtractCommandInfoDescriptionTruncated(t *testing.T) {
	f := star.NewCommandFilter("longcmd", nil, nil)
	long := ""
	for i := 0; i < 120; i++ {
		long += "x"
	}
	h := &star.StarHandlerMetadata{Desc: long}
	info := extractCommandInfo(f, h)
	if info == nil {
		t.Fatal("expected extraction")
	}
	if len(info[1]) != 100 {
		t.Errorf("description must be truncated to 100, got %d", len(info[1]))
	}
}

// TestExtractCommandInfoNonCommandFilter: non-command filters are ignored.
func TestExtractCommandInfoNonCommandFilter(t *testing.T) {
	f := &star.AlwaysMatchFilter{}
	h := &star.StarHandlerMetadata{}
	if info := extractCommandInfo(f, h); info != nil {
		t.Errorf("non-command filter must be skipped, got %v", info)
	}
}

// TestCollectFromRegistry: registerCommands only picks up enabled handlers
// with valid command filters (mirrors Python _collect_and_register_commands).
func TestCollectFromRegistry(t *testing.T) {
	a := New(map[string]interface{}{"id": "discord"}, nil, nil)
	mgr := star.NewManagerSimple()

	// Enabled top-level command -> registered.
	h1 := &star.StarHandlerMetadata{
		HandlerFullName:   "p1_echo",
		HandlerModulePath: "plugin1",
		EventType:         star.EventTypeFilter,
		EventFilters:      []star.HandlerFilter{star.NewCommandFilter("echo", nil, nil)},
		Desc:              "回显",
		Enabled:           true,
	}
	mgr.Handlers().Append(h1)

	// Handler from a plugin that is not activated is not in the registry at
	// all (plugins register/unregister with the registry); nothing to skip.

	// Sub-command -> skipped.
	h3 := &star.StarHandlerMetadata{
		HandlerFullName:   "p3_ban",
		HandlerModulePath: "plugin3",
		EventType:         star.EventTypeFilter,
		EventFilters:      []star.HandlerFilter{star.NewCommandFilter("ban", nil, star.NewCommandGroupFilter("admin", nil, nil))},
		Desc:              "ban",
		Enabled:           true,
	}
	mgr.Handlers().Append(h3)

	// Uppercase name -> skipped.
	h4 := &star.StarHandlerMetadata{
		HandlerFullName:   "p4_bad",
		HandlerModulePath: "plugin4",
		EventType:         star.EventTypeFilter,
		EventFilters:      []star.HandlerFilter{star.NewCommandFilter("BadName", nil, nil)},
		Desc:              "bad",
		Enabled:           true,
	}
	mgr.Handlers().Append(h4)

	a.SetStarManager(mgr)
	if a.starMgr == nil {
		t.Fatal("star manager not injected")
	}

	// Collect the same way registerCommands does (no live Discord session).
	commands := []*discordgo.ApplicationCommand{}
	for _, handler := range mgr.Handlers().GetFilterHandlers() {
		for _, filter := range handler.EventFilters {
			if info := extractCommandInfo(filter, handler); info != nil {
				commands = append(commands, &discordgo.ApplicationCommand{Name: info[0], Description: info[1]})
			}
		}
	}
	if len(commands) != 1 || commands[0].Name != "echo" {
		t.Errorf("expected only [echo], got %d commands", len(commands))
	}
}
