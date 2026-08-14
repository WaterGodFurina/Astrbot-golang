package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// toolResultIDRe strips characters that would let a provider-supplied tool call
// id escape the tool_results directory (path traversal via "../" or absolute
// paths).
var toolResultIDRe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// Tool-result inline limits (mirrors Python ToolLoopAgentRunner): oversized
// results are spilled to a file and only a preview is returned to the model,
// with a read-tool hint so it can fetch the full content without re-running.
const (
	maxInlineToolResultChars = 55000 // ~27500 tokens (chars/2 estimate)
	maxPreviewChars          = 14000 // ~7000 tokens
)

// materializeToolResult truncates oversized tool output: the inline result is
// capped at a preview, the full output is written to data/temp/tool_results/,
// and a notice tells the model the path and how to read it with the file-read
// tool. Small results pass through unchanged.
func materializeToolResult(result, toolCallID string) string {
	if len([]rune(result)) <= maxInlineToolResultChars {
		return result
	}
	preview := truncateRunes(result, maxPreviewChars)
	dir := filepath.Join("data", "temp", "tool_results")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return preview + "\n\n[工具结果过长，且无法写入溢出文件]"
	}
	safeID := toolResultIDRe.ReplaceAllString(toolCallID, "_")
	if safeID == "" {
		safeID = fmt.Sprintf("tool_%d", time.Now().UnixNano())
	}
	name := fmt.Sprintf("%s_%s.txt", safeID, fmt.Sprintf("%x", time.Now().UnixNano()))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(result), 0o644); err != nil {
		return preview + "\n\n[工具结果过长，且无法写入溢出文件]"
	}
	notice := fmt.Sprintf(
		"\n\n[工具结果过长已溢出] 完整结果已保存到 %s（共 %d 字符）。如需查看完整内容，"+
			"请使用 astrbot_file_read_tool 读取该文件（可配合 offset/limit 参数分页查看）。",
		path, len([]rune(result)))
	return preview + notice
}

// truncateRunes returns the longest prefix of s with at most max runes.
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
