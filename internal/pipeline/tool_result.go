package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// toolResultIDRe strips characters that would let a provider-supplied tool call id escape the tool_results directory (path traversal via "../" or absolute paths).
var toolResultIDRe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// Tool-result inline limits ported from opencode tool/truncate.ts (MAX_LINES/MAX_BYTES + spill-to-file with preview + read hint). The byte budget adapts to the model context (ProcessStage.toolOutputBudget: 1/5 of provider_settings.max_context_length); maxOutputLines mirrors opencode's default MAX_LINES.
const (
	defaultCtxTokens = 32000  // fallback when provider_settings.max_context_length is unset/-1 (auto)
	minOutputBudget  = 8000   // runes
	maxOutputBudget  = 200000 // runes (guards pathological max_context_length values)
	maxOutputLines   = 2000   // opencode truncate.MAX_LINES
	previewRunes     = 14000
)

// toolOutputBudget returns the inline-output budget in runes: 1/5 of the model context window in tokens, converted with the same ~2-runes/token heuristic as estimateContextTokens.
func (s *ProcessStage) toolOutputBudget() int {
	tokens := defaultCtxTokens
	if s.providerConf != nil && s.providerConf.MaxContextLength > 0 {
		tokens = s.providerConf.MaxContextLength
	}
	budget := tokens / 5 * 2
	if budget < minOutputBudget {
		budget = minOutputBudget
	}
	if budget > maxOutputBudget {
		budget = maxOutputBudget
	}
	return budget
}

// spillFunc persists a full oversized result under the sanitized name and returns a path the model can read with the file tools (host path for local runtime, an in-sandbox path for sandbox runtime so sandbox file_read can actually reach it).
type spillFunc func(name, content string) (path string, ok bool)

// materializeToolResult mirrors opencode Truncate.output: within budget pass through unchanged; otherwise keep a head preview, spill the full text via the runtime-appropriate sink, and append the "...N lines truncated... full output saved to X ... use Grep/Read(offset/limit)" hint. file_read itself windows per opencode read.ts, so each follow-up read returns one segment the model digests before the next.
func materializeToolResult(result, toolCallID string, budget int, spill spillFunc) string {
	runes := []rune(result)
	totalLines := strings.Count(result, "\n") + 1
	if len(runes) <= budget && totalLines <= maxOutputLines {
		return result
	}
	preview := string(runes[:minInt(len(runes), minInt(previewRunes, budget))])
	if pLines := strings.Count(preview, "\n") + 1; pLines > maxOutputLines {
		keep := 0
		for i, r := range preview {
			if i > 0 && r == '\n' {
				keep++
				if keep >= maxOutputLines {
					preview = preview[:i]
					break
				}
			}
		}
	}
	safeID := toolResultIDRe.ReplaceAllString(toolCallID, "_")
	if safeID == "" {
		safeID = "tool_result"
	}
	safeID = truncateRunes(safeID, 80) + fmt.Sprintf("_%x", time.Now().UnixNano())
	path, ok := spill(safeID, result)
	if !ok {
		return preview + fmt.Sprintf("\n\n...(output truncated: first %d of %d chars; the full output could not be saved)...", len([]rune(preview)), len(runes))
	}
	removedLines := 0
	if pv := strings.Count(preview, "\n") + 1; pv < totalLines {
		removedLines = totalLines - pv
	}
	hint := fmt.Sprintf("The tool call succeeded but the output was truncated. Full output saved to: %s (total %d chars / %d lines). Use astrbot_grep_tool to search that file or astrbot_file_read_tool with offset/limit to view specific sections — it returns one window at a time; digest/summarize each segment before reading the next, and do NOT read the whole file at once.", path, len(runes), totalLines)
	if removedLines > 0 {
		return fmt.Sprintf("%s\n\n...%d lines truncated...\n\n%s", preview, removedLines, hint)
	}
	return fmt.Sprintf("%s\n\n%s", preview, hint)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// spillHostToolResult writes the overflow file under data/temp/tool_results (local runtime: the model's file tools read the host filesystem).
func spillHostToolResult(name, content string) (string, bool) {
	dir := filepath.Join("data", "temp", "tool_results")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", false
	}
	path := filepath.Join(dir, name+".txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", false
	}
	return path, true
}

// materializeForRuntime applies the inline budget with the runtime-appropriate spill sink: sandbox results land inside the sandbox workspace (visible to sandbox file tools), everything else on the host.
func (s *ProcessStage) materializeForRuntime(result, toolCallID, runtime, sessionID string) string {
	budget := s.toolOutputBudget()
	spill := func(spillName, content string) (string, bool) { return spillHostToolResult(spillName, content) }
	if runtime == "sandbox" && s.sandboxMgr != nil {
		spill = func(spillName, content string) (string, bool) {
			path := sandboxWorkdir + "/.astrbot-tool-results/" + spillName + ".txt"
			if err := s.sandboxMgr.WriteFile(context.Background(), sessionID, path, content); err != nil {
				return "", false
			}
			return path, true
		}
	}
	return materializeToolResult(result, toolCallID, budget, spill)
}

// truncateRunes returns the longest prefix of s with at most max runes.
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
