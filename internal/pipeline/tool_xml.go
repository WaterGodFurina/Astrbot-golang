package pipeline

import (
	"encoding/xml"
	"regexp"
	"strings"
)

// Anthropic-style XML tool-call blocks: some providers/models emit
// <function_calls><invoke name=...><parameter name=...>...</function_calls>
// and <assistant-user>...</assistant-user> separators. AstrBot's OpenAI
// tool-call path only understands JSON tool_calls, so XML tool calls are
// parsed into real tool calls (and suppressed from the streamed reply).
var (
	xmlToolCallBlockRe = regexp.MustCompile(`(?s)<function_calls>.*?</function_calls>`)
	advisorBlockRe     = regexp.MustCompile(`(?s)<astrbot_advisor>.*?</astrbot_advisor>|<advisor>.*?</advisor>`)
	controlTagRe       = regexp.MustCompile(`(?s)</?function_calls\b[^>]*>|</?invoke\b[^>]*>|</?parameter\b[^>]*>|</?assistant-user\b[^>]*>|</?thinking\b[^>]*>|</?analysis\b[^>]*>|</?user\b[^>]*>|</?astrbot_advisor\b[^>]*>|</?advisor\b[^>]*>|\[Advisor[^\]]*\]|\[Asvisor[^\]]*\]`)
)

// stripToolCallXML removes model control markup (Anthropic-style tool-call
// blocks, advisor/reasoning tags) so it is not sent to the user as text.
func stripToolCallXML(s string) string {
	s = xmlToolCallBlockRe.ReplaceAllString(s, "")
	s = advisorBlockRe.ReplaceAllString(s, "")
	s = controlTagRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// xmlToolCall is one parsed <invoke> tool call.
type xmlToolCall struct {
	name string
	args map[string]interface{}
}

// containsToolXML reports whether a streamed chunk carries Anthropic-style
// tool-call markup (these chunks must not be streamed to the user).
func containsToolXML(s string) bool {
	return strings.Contains(s, "<function_calls>") ||
		strings.Contains(s, "<invoke") ||
		strings.Contains(s, "</invoke>") ||
		strings.Contains(s, "<parameter")
}

// containsControlText reports whether a streamed chunk carries any model
// control markup (tool-call XML or advisor/reasoning tags) that must not be
// streamed to the user.
func containsControlText(s string) bool {
	return containsToolXML(s) ||
		strings.Contains(s, "astrbot_advisor") ||
		strings.Contains(s, "<advisor") ||
		strings.Contains(s, "[Advisor") ||
		strings.Contains(s, "[Asvisor")
}

// controlTextMarkers are the literal markers that must suppress streamed text
// (aligned with containsControlText). Longest first is not required, but the
// set must be complete so prefix matching below holds back any split marker.
var controlTextMarkers = []string{
	"<function_calls>",
	"</function_calls>",
	"<invoke",
	"</invoke>",
	"<parameter",
	"<astrbot_advisor>",
	"astrbot_advisor",
	"<advisor",
	"[Advisor",
	"[Asvisor",
}

// maxControlMarkerLen is the length of the longest control marker; a streamed
// marker can never be split across a longer window than this.
const maxControlMarkerLen = 16 // "</function_calls>"

// isControlMarkerPrefix reports whether p is a prefix of some control marker.
func isControlMarkerPrefix(p string) bool {
	for _, m := range controlTextMarkers {
		if strings.HasPrefix(m, p) {
			return true
		}
	}
	return false
}

// controlTextPendingLen returns the length of the longest suffix of s that is a
// prefix of a control marker. When a marker is split across stream chunks, that
// suffix must be held back (not pushed to the user) until the next chunk
// confirms it is either a real marker or plain text.
func controlTextPendingLen(s string) int {
	if len(s) > maxControlMarkerLen {
		s = s[len(s)-maxControlMarkerLen:]
	}
	for l := len(s); l > 0; l-- {
		if isControlMarkerPrefix(s[len(s)-l:]) {
			return l
		}
	}
	return 0
}

// parseXMLToolCalls extracts tool calls from an Anthropic-style
// <function_calls> block in the model output. Returns ok=false when the output
// has no such block or parsing fails.
func parseXMLToolCalls(s string) ([]xmlToolCall, bool) {
	if !strings.Contains(s, "<function_calls>") {
		return nil, false
	}
	var fc struct {
		Invokes []struct {
			Name   string `xml:"name,attr"`
			Params []struct {
				PName string `xml:"name,attr"`
				PVal  string `xml:",chardata"`
			} `xml:"parameter"`
		} `xml:"invoke"`
	}
	if err := xml.Unmarshal([]byte(s), &fc); err != nil {
		return nil, false
	}
	var calls []xmlToolCall
	for _, inv := range fc.Invokes {
		name := strings.TrimSpace(inv.Name)
		if name == "" {
			continue
		}
		args := map[string]interface{}{}
		for _, p := range inv.Params {
			args[strings.TrimSpace(p.PName)] = p.PVal
		}
		calls = append(calls, xmlToolCall{name: name, args: args})
	}
	return calls, len(calls) > 0
}
