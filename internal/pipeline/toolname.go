package pipeline

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
)

// validToolNameRe matches the OpenAI tool-name pattern (^[a-zA-Z0-9_-]+$).
var validToolNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// isValidToolName reports whether a tool name conforms to the OpenAI schema
// pattern; invalid names cause the provider to reject the whole request.
func isValidToolName(name string) bool {
	return validToolNameRe.MatchString(name)
}

// pluginToolSafeName returns a provider-safe, stable tool name for a plugin
// tool. Legal ASCII names are kept verbatim; illegal ones (e.g. Chinese tool
// names) are rewritten to a readable ASCII prefix plus a short hash so they
// remain unique across plugins and stable for execution lookup.
func pluginToolSafeName(name string) string {
	if isValidToolName(name) {
		return name
	}
	var ascii strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			ascii.WriteRune(r)
		}
	}
	prefix := ascii.String()
	if len(prefix) > 20 {
		prefix = prefix[:20]
	}
	if prefix == "" {
		prefix = "tool"
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return prefix + "_" + strings.ToLower(fmt.Sprintf("%x", h.Sum32()))
}
