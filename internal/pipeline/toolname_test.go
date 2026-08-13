
package pipeline

import (
	"testing"
)

func TestPluginToolSafeName(t *testing.T) {
	// Valid ASCII names pass through unchanged.
	if got := pluginToolSafeName("get_weather"); got != "get_weather" {
		t.Fatalf("ascii name mangled: %s", got)
	}
	// Chinese names become a legal, stable name matching ^[a-zA-Z0-9_-]+$.
	names := []string{"Github的维基百科DeepWiKi", "Fetch网页内容抓取"}
	seen := map[string]bool{}
	for _, n := range names {
		safe := pluginToolSafeName(n)
		if !validToolNameRe.MatchString(safe) {
			t.Fatalf("sanitized name %q still invalid", safe)
		}
		if seen[safe] {
			t.Fatalf("sanitized name collision for %q -> %q", n, safe)
		}
		seen[safe] = true
		// Stable across calls.
		if pluginToolSafeName(n) != safe {
			t.Fatalf("sanitized name not stable for %q", n)
		}
	}
}
