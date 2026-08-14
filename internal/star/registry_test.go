package star

import "testing"

// TestGetHandlersByEventTypeMatchesPluginName verifies that per-plugin filtering
// matches the owning plugin via PluginName, not the module path — subprocess
// plugin handlers all share the constant module path "data.plugins", so a
// module-path match could never distinguish them.
func TestGetHandlersByEventTypeMatchesPluginName(t *testing.T) {
	reg := NewStarHandlerRegistry()
	base := func(fullName, pluginName string) *StarHandlerMetadata {
		return &StarHandlerMetadata{
			HandlerFullName:   fullName,
			HandlerModulePath: "data.plugins",
			PluginName:        pluginName,
			EventType:         EventTypeFilter,
			Enabled:           true,
		}
	}
	reg.Append(base("plugin_alpha_test", "alpha"))
	reg.Append(base("plugin_beta_test", "beta"))
	reg.Append(base("plugin_alpha_other", "alpha"))

	got := reg.GetHandlersByEventType(EventTypeFilter, []string{"alpha"})
	if len(got) != 2 {
		t.Fatalf("expected 2 handlers for plugin alpha, got %d", len(got))
	}
	for _, h := range got {
		if h.PluginName != "alpha" {
			t.Errorf("handler %s should belong to alpha, got plugin %q", h.HandlerFullName, h.PluginName)
		}
	}

	if got := reg.GetHandlersByEventType(EventTypeFilter, []string{"nope"}); len(got) != 0 {
		t.Errorf("no handlers should match unknown plugin, got %d", len(got))
	}
	// Empty plugin list = no filtering.
	if got := reg.GetHandlersByEventType(EventTypeFilter, nil); len(got) != 3 {
		t.Errorf("nil plugin list should return all handlers, got %d", len(got))
	}
}

// TestEventMessageTypeFilter verifies the message-type filter actually compares
// against the event message type instead of always passing.
func TestEventMessageTypeFilter(t *testing.T) {
	group := NewEventMessageTypeFilter("GroupMessage")
	friend := NewEventMessageTypeFilter("FriendMessage")

	if group.Match(&FilterContext{EventMessageType: "GroupMessage"}) != true {
		t.Error("GroupMessage filter should match a GroupMessage event")
	}
	if group.Match(&FilterContext{EventMessageType: "FriendMessage"}) != false {
		t.Error("GroupMessage filter must not match a FriendMessage event")
	}
	if friend.Match(&FilterContext{EventMessageType: "FriendMessage"}) != true {
		t.Error("FriendMessage filter should match a FriendMessage event")
	}
	if friend.Match(&FilterContext{EventMessageType: "GroupMessage"}) != false {
		t.Error("FriendMessage filter must not match a GroupMessage event")
	}
	if group.Match(nil) != false {
		t.Error("nil context should never match")
	}
	if group.Match(&FilterContext{}) != false {
		t.Error("empty message type should never match")
	}
}

// TestPluginNameForPrefersHandlerPluginName verifies command ownership is
// derived from the handler's PluginName (set to inst.ID by the subprocess
// bridge) rather than stripping the "plugin_" prefix, which would yield the
// incorrect "<id>_<cmdName>".
func TestPluginNameForPrefersHandlerPluginName(t *testing.T) {
	h := &StarHandlerMetadata{
		HandlerFullName:   "plugin_myplugin_echo",
		HandlerModulePath: "data.plugins",
		PluginName:        "myplugin",
	}
	if got := pluginNameFor(h); got != "myplugin" {
		t.Errorf("pluginNameFor should prefer PluginName, got %q", got)
	}

	// Legacy .so handlers without PluginName still fall back to the prefix.
	legacy := &StarHandlerMetadata{
		HandlerFullName:   "plugin_legacy",
		HandlerModulePath: "data.plugins",
	}
	if got := pluginNameFor(legacy); got != "legacy" {
		t.Errorf("pluginNameFor legacy fallback got %q, want %q", got, "legacy")
	}

	// Builtin handlers resolve to the reserved name.
	builtin := &StarHandlerMetadata{HandlerModulePath: "astrbot.builtin_stars.builtin_commands.main"}
	if got := pluginNameFor(builtin); got != "astrbot" {
		t.Errorf("pluginNameFor builtin got %q, want %q", got, "astrbot")
	}
}
