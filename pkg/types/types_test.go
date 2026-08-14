package types

import "testing"

// TestIssue9533_SanitizeToolName verifies the fix for issue #9533:
// MCP tool names containing "." cause LLM API rejection.
// The fix sanitizes tool names to match ^[a-zA-Z0-9_-]+$.
func TestIssue9533_SanitizeToolName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"simple", "get_weather", "get_weather"},
		{"with dash", "get-weather", "get-weather"},
		{"with dots", "mcp.server.tool", "mcp_server_tool"},
		{"with slash", "tools/web_search", "tools_web_search"},
		{"with spaces", "get weather", "get_weather"},
		{"with colons", "namespace:tool", "namespace_tool"},
		{"with special chars", "tool@v2!#", "tool_v2__"},
		{"empty", "", "tool"},
		{"numbers", "tool123", "tool123"},
		{"mixed case", "GetWeather", "GetWeather"},
		{"chinese", "搜索工具", "____"},
		{"underscore preserves", "__init__", "__init__"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SanitizeToolName(tt.input)
			if result != tt.expected {
				t.Errorf(
					"SanitizeToolName(%q) = %q, want %q",
					tt.input, result, tt.expected,
				)
			}
			// Verify result matches the allowed pattern
			for i := 0; i < len(result); i++ {
				c := result[i]
				if !((c >= 'a' && c <= 'z') ||
					(c >= 'A' && c <= 'Z') ||
					(c >= '0' && c <= '9') ||
					c == '_' || c == '-') {
					t.Errorf(
						"SanitizeToolName(%q) produced invalid char '%c' at pos %d in result %q",
						tt.input, c, i, result,
					)
				}
			}
		})
	}
}
