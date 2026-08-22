package pipeline

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
)

func toolPermEvent(role string) *core.Event {
	return &core.Event{
		Role:   role,
		Source: core.EventSource{Platform: "qq", ConvID: "group:1", SenderID: "u1"},
	}
}

// TestToolPermissionAdminOnlyDeniesMember: a tool configured admin-only in
// tool_permissions is refused for a member event before any dispatcher /
// executor runs (review 1.2-6). The fallthrough "尚未实现" reply proves the
// handler path was never reached.
func TestToolPermissionAdminOnlyDeniesMember(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{
		"tool_permissions": map[string]interface{}{
			"my_plugin_tool": map[string]interface{}{"permission": "admin"},
		},
	})
	event := toolPermEvent("member")
	result := s.executeTool(context.Background(), event, "local", "my_plugin_tool", map[string]interface{}{})
	if !strings.Contains(result, "需要管理员权限") || !strings.Contains(result, "my_plugin_tool") {
		t.Fatalf("member must be denied with a permission notice, got %q", result)
	}
	if strings.Contains(result, "尚未实现") {
		t.Fatalf("denied tool must not reach the dispatcher, got %q", result)
	}
	// The tool loop's timeout wrapper goes through the same guard.
	if got := s.executeToolWithTimeout(event, "local", "my_plugin_tool", nil); !strings.Contains(got, "需要管理员权限") {
		t.Fatalf("executeToolWithTimeout must deny too, got %q", got)
	}
	// A member role is the default for events that never passed WakingCheck.
	if got := s.executeTool(context.Background(), toolPermEvent(""), "local", "my_plugin_tool", nil); !strings.Contains(got, "需要管理员权限") {
		t.Fatalf("unset role must be treated as member, got %q", got)
	}
}

// TestToolPermissionAdminOnlyAllowsAdmin: the same admin-only tool runs for an
// admin event (the dispatcher fallthrough is reached).
func TestToolPermissionAdminOnlyAllowsAdmin(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{
		"tool_permissions": map[string]interface{}{
			"my_plugin_tool": map[string]interface{}{"permission": "admin"},
		},
	})
	result := s.executeTool(context.Background(), toolPermEvent("admin"), "local", "my_plugin_tool", map[string]interface{}{})
	if strings.Contains(result, "需要管理员权限") {
		t.Fatalf("admin must not be denied, got %q", result)
	}
	if !strings.Contains(result, "尚未实现") {
		t.Fatalf("admin call must reach the dispatcher fallthrough, got %q", result)
	}
}

// TestToolPermissionUnconfiguredToolUnaffected: tools without a
// tool_permissions entry (and member-level entries) never hit the guard.
func TestToolPermissionUnconfiguredToolUnaffected(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{
		"tool_permissions": map[string]interface{}{
			"member_level_tool": map[string]interface{}{"permission": "member"},
		},
	})
	event := toolPermEvent("member")
	for _, name := range []string{"other_plugin_tool", "member_level_tool"} {
		result := s.executeTool(context.Background(), event, "local", name, nil)
		if strings.Contains(result, "需要管理员权限") {
			t.Fatalf("%s must not be denied, got %q", name, result)
		}
		if !strings.Contains(result, "尚未实现") {
			t.Fatalf("%s must reach the dispatcher fallthrough, got %q", name, result)
		}
	}
}

// TestToolPermissionBuiltinToolsPass: builtin tools bypass the guard even when
// hand-configured as admin-only (mirrors Python: the dashboard exposes
// built-ins as readonly and the guard only governs non-builtin tools).
func TestToolPermissionBuiltinToolsPass(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{
		"tool_permissions": map[string]interface{}{
			"get_current_time": map[string]interface{}{"permission": "admin"},
		},
	})
	result := s.executeTool(context.Background(), toolPermEvent("member"), "local", "get_current_time", map[string]interface{}{})
	if strings.Contains(result, "需要管理员权限") {
		t.Fatalf("builtin tool must pass the guard, got %q", result)
	}
	if len(strings.TrimSpace(result)) == 0 || !strings.ContainsAny(result[:4], "0123456789") {
		t.Fatalf("get_current_time must have executed, got %q", result)
	}
}

// TestToolPermissionParsesBothShapes: the dashboard shape
// {"permission": "admin"} and a bare "admin" string are both honoured, and
// malformed levels are ignored.
func TestToolPermissionParsesBothShapes(t *testing.T) {
	perms := parseToolPermissions(map[string]interface{}{
		"tool_permissions": map[string]interface{}{
			"dict_tool":   map[string]interface{}{"permission": "admin"},
			"bare_tool":   "admin",
			"member_tool": map[string]interface{}{"permission": "member"},
			"junk_tool":   map[string]interface{}{"permission": 42},
		},
	})
	if perms["dict_tool"] != "admin" || perms["bare_tool"] != "admin" {
		t.Fatalf("admin entries must parse, got %v", perms)
	}
	if perms["member_tool"] != "member" {
		t.Fatalf("member entry must parse, got %v", perms)
	}
	if _, ok := perms["junk_tool"]; ok {
		t.Fatalf("malformed level must be ignored, got %v", perms)
	}
	if parseToolPermissions(map[string]interface{}{}) != nil {
		t.Fatal("missing tool_permissions must parse to nil")
	}
}

// TestToolPermissionCachedAcrossCalls: the parsed snapshot is cached on the
// stage (one parse for the whole request / stage lifetime) and stays
// race-free under concurrent tool calls.
func TestToolPermissionCachedAcrossCalls(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{
		"tool_permissions": map[string]interface{}{
			"cached_tool": map[string]interface{}{"permission": "admin"},
		},
	})
	event := toolPermEvent("member")
	for i := 0; i < 3; i++ {
		if got := s.executeTool(context.Background(), event, "local", "cached_tool", nil); !strings.Contains(got, "需要管理员权限") {
			t.Fatalf("call %d must stay denied, got %q", i, got)
		}
	}
	if s.adminOnlyTools()["cached_tool"] != "admin" {
		t.Fatal("parsed permission map must be cached on the stage")
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.executeTool(context.Background(), event, "local", "cached_tool", nil)
		}()
	}
	wg.Wait()
}
