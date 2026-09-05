// kb markdown_chunker.go：标题感知分块器（对齐 Python 本体
// knowledge_base/chunking/markdown.py 的 MarkdownChunker 全语义）。
//
// 按 Markdown 标题层级切分文档，每个章节作为独立 chunk；块前注入标题
// 路径（include_heading_context）；超长章节内部递归分割（复用 ChunkText
// 固定窗口为回退，对齐本体的 RecursiveCharacterChunker 回退位）；纯标题
// 节合并进下一个有内容块；过短相邻块合并；```/~~~ 围栏代码块内的 # 不
// 误判；无标题结构回退固定窗口。
package knowledgebase

import (
	"fmt"
	"regexp"
	"strings"
)

// chunkBody 中间产物：chunk 文本 + 是否有实质正文（纯标题节 false）。
type chunkBody struct {
	text    string
	hasBody bool
}

// MarkdownChunkOptions 分块参数（对齐 MarkdownChunker.__init__）。
type MarkdownChunkOptions struct {
	ChunkSize             int    // 每 chunk 最大字符数（默认 1024）
	ChunkOverlap          int    // 递归分割重叠（默认 50）
	IncludeHeadingContext bool   // 块前附加父级标题路径（默认 true）
	MaxHeadingDepth       int    // 最大识别深度 1-6（默认 4）
	MinChunkSize          int    // 相邻过短块合并阈值（0=不合并）
	ContinuationPrefix    string // 续接前缀（默认 "..."）
}

// MarkdownChunk 用标题感知策略分块（对齐 MarkdownChunker.chunk）。
func MarkdownChunk(text string, opts MarkdownChunkOptions) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if opts.ChunkSize <= 0 {
		opts.ChunkSize = 1024
	}
	if opts.ChunkOverlap < 0 {
		opts.ChunkOverlap = 0
	}
	// include_heading_context 默认 true（对齐本体）；显式 false 保留。
	// 布尔零值问题：调用方未设置时无法区分 false/未传——用 *bool 语义
	// 复杂化不值得，约定 opts 字面量必须显式设置 IncludeHeadingContext。
	if opts.ContinuationPrefix == "" && opts.IncludeHeadingContext {
		opts.ContinuationPrefix = "..."
	}
	if opts.MaxHeadingDepth <= 0 {
		opts.MaxHeadingDepth = 4
	}
	if opts.MaxHeadingDepth > 6 {
		opts.MaxHeadingDepth = 6
	}
	if opts.ContinuationPrefix == "" {
		opts.ContinuationPrefix = "..."
	}

	sections := parseMarkdownSections(text, opts.MaxHeadingDepth)
	if len(sections) == 0 {
		// 回退：无标题结构 → 固定窗口（对齐本体回退 RecursiveCharacterChunker）
		return ChunkText(text, opts.ChunkSize, opts.ChunkOverlap)
	}

	var raw []chunkBody
	for _, sec := range sections {
		prefix := buildContextPrefix(sec.headingPath, opts.IncludeHeadingContext)
		full := prefix + sec.text
		if len([]rune(full)) <= opts.ChunkSize {
			raw = append(raw, chunkBody{strings.TrimSpace(full), sec.hasBody})
			continue
		}
		// 章节过长：内部递归分割，扣除前缀长度（对齐 _sections_to_chunks）
		prefixLen := len([]rune(estimatePrefix(sec.headingPath, opts)))
		effective := opts.ChunkSize - prefixLen
		if effective < opts.ChunkSize/4 {
			effective = opts.ChunkSize / 4
		}
		sub := ChunkText(sec.text, effective, opts.ChunkOverlap)
		for i, sc := range sub {
			raw = append(raw, chunkBody{
				text:    applyHeadingContext(sec.headingPath, sc, i > 0, opts),
				hasBody: true,
			})
		}
	}

	// 纯标题节合并到下一个有内容块
	merged := mergeHeadingOnly(raw, opts.ChunkSize)
	// 过短相邻块合并
	merged = mergeShort(merged, opts)
	return merged
}

type mdSection struct {
	headingPath []string
	text        string
	hasBody     bool
}

var (
	mdFenceRe    = regexp.MustCompile(`(?m)^(` + "`{3,}" + `|~{3,})`)
	mdHeadingFmt = "^(#{1,%d})\\s*(.+)$"
)

// parseMarkdownSections 解析 Markdown 章节列表（对齐 _parse_sections：
// 围栏代码块跳过、标题栈维护路径、首标题前的 preamble 单独成节）。
func parseMarkdownSections(text string, maxDepth int) []mdSection {
	fences := findFencedRanges(text)
	headingRe := regexp.MustCompile(`(?m)^(#{1,` + fmt.Sprint(maxDepth) + `})\s*(.+)$`)

	type heading struct {
		level int
		title string
		start int
		end   int
	}
	var headings []heading
	// FindAllStringSubmatchIndex 布局：[0,1]=整体、[2,3]=组1(#)、[4,5]=组2(标题)
	for _, m := range headingRe.FindAllStringSubmatchIndex(text, -1) {
		if inRanges(m[0], fences) {
			continue
		}
		level := len(text[m[2]:m[3]])
		title := strings.TrimSpace(text[m[4]:m[5]])
		headings = append(headings, heading{level, title, m[0], m[1]})
	}
	if len(headings) == 0 {
		return nil
	}

	var sections []mdSection
	// 首标题前内容
	if pre := strings.TrimSpace(text[:headings[0].start]); pre != "" {
		sections = append(sections, mdSection{nil, pre, true})
	}

	var stack []heading
	for i, h := range headings {
		for len(stack) > 0 && stack[len(stack)-1].level >= h.level {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, h)

		contentStart := h.end
		contentEnd := len(text)
		if i+1 < len(headings) {
			contentEnd = headings[i+1].start
		}
		body := strings.TrimSpace(text[contentStart:contentEnd])
		sectionText := text[h.start:h.end]
		if body != "" {
			sectionText += "\n" + body
		}
		path := make([]string, 0, len(stack))
		for _, s := range stack[:len(stack)-1] {
			path = append(path, s.title)
		}
		sections = append(sections, mdSection{path, sectionText, body != ""})
	}
	return sections
}

// findFencedRanges 找围栏代码块范围（``` 或 ~~~ 成对）。
func findFencedRanges(text string) [][2]int {
	var ranges [][2]int
	matches := mdFenceRe.FindAllStringSubmatchIndex(text, -1)
	i := 0
	for i < len(matches) {
		open := matches[i]
		fence := text[open[2]:open[3]]
		closed := false
		for j := i + 1; j < len(matches); j++ {
			cand := text[matches[j][2]:matches[j][3]]
			if cand[0] == fence[0] && len(cand) >= len(fence) {
				ranges = append(ranges, [2]int{open[0], matches[j][1]})
				i = j + 1
				closed = true
				break
			}
		}
		if !closed {
			ranges = append(ranges, [2]int{open[0], len(text)})
			break
		}
	}
	return ranges
}

func inRanges(pos int, ranges [][2]int) bool {
	for _, r := range ranges {
		if r[0] <= pos && pos < r[1] {
			return true
		}
	}
	return false
}

// buildContextPrefix 标题路径前缀（"A > B\n\n"）。
func buildContextPrefix(path []string, enabled bool) string {
	if !enabled || len(path) == 0 {
		return ""
	}
	return strings.Join(path, " > ") + "\n\n"
}

// estimatePrefix 前缀长度估算（续接格式 "... A > B\n\n"）。
func estimatePrefix(path []string, opts MarkdownChunkOptions) string {
	if !opts.IncludeHeadingContext || len(path) == 0 {
		return ""
	}
	return opts.ContinuationPrefix + " " + strings.Join(path, " > ") + "\n\n"
}

// applyHeadingContext 为子块附加标题路径（续接块加 continuation 前缀）。
func applyHeadingContext(path []string, content string, isContinuation bool, opts MarkdownChunkOptions) string {
	if !opts.IncludeHeadingContext || len(path) == 0 {
		return strings.TrimSpace(content)
	}
	title := strings.Join(path, " > ")
	if isContinuation {
		return strings.TrimSpace(opts.ContinuationPrefix + " " + title + "\n\n" + content)
	}
	return strings.TrimSpace(title + "\n\n" + content)
}

// mergeHeadingOnly 纯标题节合并进下一个有内容块（对齐
// _merge_heading_only_chunks）。
func mergeHeadingOnly(raw []chunkBody, chunkSize int) []string {
	var merged []string
	pending := ""
	for _, c := range raw {
		if c.text == "" {
			continue
		}
		if !c.hasBody {
			if pending != "" && len([]rune(pending))+len([]rune(c.text))+2 > chunkSize {
				merged = append(merged, strings.TrimSpace(pending))
				pending = ""
			}
			pending += c.text + "\n\n"
		} else {
			if pending != "" {
				combined := pending + c.text
				if len([]rune(combined)) <= chunkSize {
					merged = append(merged, strings.TrimSpace(combined))
				} else {
					merged = append(merged, strings.TrimSpace(pending))
					merged = append(merged, strings.TrimSpace(c.text))
				}
				pending = ""
			} else {
				merged = append(merged, strings.TrimSpace(c.text))
			}
		}
	}
	if pending != "" {
		p := strings.TrimSpace(pending)
		if len(merged) > 0 && len([]rune(merged[len(merged)-1]+"\n\n"+p)) <= chunkSize {
			merged[len(merged)-1] = merged[len(merged)-1] + "\n\n" + p
		} else {
			merged = append(merged, p)
		}
	}
	var out []string
	for _, c := range merged {
		if strings.TrimSpace(c) != "" {
			out = append(out, c)
		}
	}
	return out
}

// mergeShort 合并过短的相邻块（对齐 _merge_short_chunks）。
func mergeShort(chunks []string, opts MarkdownChunkOptions) []string {
	if opts.MinChunkSize <= 0 || len(chunks) <= 1 {
		return chunks
	}
	var final []string
	buf := ""
	for _, c := range chunks {
		if buf != "" {
			combined := buf + "\n\n" + c
			if len([]rune(combined)) <= opts.ChunkSize {
				buf = combined
			} else {
				final = append(final, buf)
				if len([]rune(c)) >= opts.MinChunkSize {
					final = append(final, c)
					buf = ""
				} else {
					buf = c
				}
			}
		} else if len([]rune(c)) < opts.MinChunkSize {
			buf = c
		} else {
			final = append(final, c)
		}
	}
	if buf != "" {
		if len(final) > 0 && len([]rune(final[len(final)-1]+"\n\n"+buf)) <= opts.ChunkSize {
			final[len(final)-1] = final[len(final)-1] + "\n\n" + buf
		} else {
			final = append(final, buf)
		}
	}
	return final
}

// markdownChunkExtensions 对齐本体 kb_helper.py:330-340：这些格式解析后
// 是 Markdown（带标题层级），用标题感知分块；其余格式回退固定窗口。
var markdownChunkExtensions = map[string]bool{
	".adoc": true, ".docx": true, ".epub": true, ".markdown": true,
	".md": true, ".mdx": true, ".mkd": true, ".rst": true,
	".xls": true, ".xlsx": true, ".pptx": true,
}

// ChunkDocument 文档统一分块入口：markdown 族格式走标题感知分块
// （MarkdownChunk），其余走固定窗口（ChunkText）——对齐本体 kb_helper
// 的 effective_chunker 分发。
func ChunkDocument(text, docName string, chunkSize, chunkOverlap int) []string {
	ext := strings.ToLower(strings.TrimLeft(docExt(docName), "."))
	if markdownChunkExtensions["."+ext] {
		return MarkdownChunk(text, MarkdownChunkOptions{
			ChunkSize:             chunkSize,
			ChunkOverlap:          chunkOverlap,
			IncludeHeadingContext: true,
			MinChunkSize:          0,
		})
	}
	return ChunkText(text, chunkSize, chunkOverlap)
}
