package pipeline

import (
	"path/filepath"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/conversation"
	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/db"
	"github.com/WaterGodFurina/Astrbot-golang/internal/star"
)

func TestFilterHandlersBySession(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "filter.db"))
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	defer database.Close()
	cm := conversation.NewManager(database)
	umo := "qq:GroupMessage:group:1"
	if err := cm.SetSessionRule(umo, conversation.RulePluginConfig, map[string]interface{}{
		"disabled_plugins": []string{"plugin_a"},
		"enabled_plugins":  []string{},
	}); err != nil {
		t.Fatalf("set rule: %v", err)
	}

	s := NewProcessStage()
	s.convMgr = cm
	event := &core.Event{
		Source: core.EventSource{Platform: "qq", ConvID: "group:1", IsGroup: true},
	}
	handlers := []*star.StarHandlerMetadata{
		{HandlerFullName: "h1", PluginName: "plugin_a"},
		{HandlerFullName: "h2", PluginName: "plugin_b"},
		{HandlerFullName: "h3", PluginName: ""}, // built-in, never filtered
	}
	filtered := s.filterHandlersBySession(event, handlers)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 handlers, got %d", len(filtered))
	}
	for _, h := range filtered {
		if h.PluginName == "plugin_a" {
			t.Fatal("plugin_a should be disabled by rule")
		}
	}

	// enabled-only mode: only listed plugins pass.
	if err := cm.SetSessionRule(umo, conversation.RulePluginConfig, map[string]interface{}{
		"disabled_plugins": []string{},
		"enabled_plugins":  []string{"plugin_a"},
	}); err != nil {
		t.Fatalf("set rule: %v", err)
	}
	filtered2 := s.filterHandlersBySession(event, handlers)
	if len(filtered2) != 2 { // plugin_a + built-in
		t.Fatalf("expected 2 handlers in enabled mode, got %d", len(filtered2))
	}
}
