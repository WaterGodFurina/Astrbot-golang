package telegram

import "strings"

// telegramifyMarkdown 将 Markdown 文本转换为 Telegram MarkdownV2（对齐 Python
// telegramify_markdown.markdownify 的常用子集：粗体/斜体/删除线/行内代码/
// 代码块/链接）。转换失败或 Telegram 拒绝时，调用方会回退纯文本发送。
//
// MarkdownV2 转义规则：普通文本转义 `_*[]()~`>#+-=|{}.!\`；代码内只转义
// “ ` “ 与 `\`；链接 URL 内只转义 `)` 与 `\`。
func telegramifyMarkdown(s string) string {
	return convertMarkdownV2(s)
}

func convertMarkdownV2(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		// 代码块 ```...```
		if strings.HasPrefix(s[i:], "```") {
			if end := strings.Index(s[i+3:], "```"); end >= 0 {
				content := s[i+3 : i+3+end]
				b.WriteString("```")
				b.WriteString(escapeCode(content))
				b.WriteString("```")
				i = i + 3 + end + 3
				continue
			}
		}
		// 行内代码 `...`
		if s[i] == '`' {
			if end := strings.IndexByte(s[i+1:], '`'); end >= 0 {
				content := s[i+1 : i+1+end]
				b.WriteString("`")
				b.WriteString(escapeCode(content))
				b.WriteString("`")
				i = i + 1 + end + 1
				continue
			}
		}
		// 链接 [text](url)
		if s[i] == '[' {
			if text, url, n, ok := parseMarkdownLink(s[i:]); ok {
				b.WriteString("[")
				b.WriteString(escapeLinkText(text))
				b.WriteString("](")
				b.WriteString(escapeLinkURL(url))
				b.WriteString(")")
				i += n
				continue
			}
		}
		// 粗体 **...** / __...__
		if strings.HasPrefix(s[i:], "**") {
			if inner, n, ok := parsePaired(s[i:], "**"); ok {
				b.WriteString("*")
				b.WriteString(convertMarkdownV2(inner))
				b.WriteString("*")
				i += n
				continue
			}
		}
		if strings.HasPrefix(s[i:], "__") {
			if inner, n, ok := parsePaired(s[i:], "__"); ok {
				b.WriteString("*")
				b.WriteString(convertMarkdownV2(inner))
				b.WriteString("*")
				i += n
				continue
			}
		}
		// 删除线 ~~...~~
		if strings.HasPrefix(s[i:], "~~") {
			if inner, n, ok := parsePaired(s[i:], "~~"); ok {
				b.WriteString("~")
				b.WriteString(convertMarkdownV2(inner))
				b.WriteString("~")
				i += n
				continue
			}
		}
		// 斜体 *...* / _..._
		if s[i] == '*' {
			if inner, n, ok := parsePaired(s[i:], "*"); ok {
				b.WriteString("_")
				b.WriteString(convertMarkdownV2(inner))
				b.WriteString("_")
				i += n
				continue
			}
		}
		if s[i] == '_' {
			if inner, n, ok := parsePaired(s[i:], "_"); ok {
				b.WriteString("_")
				b.WriteString(convertMarkdownV2(inner))
				b.WriteString("_")
				i += n
				continue
			}
		}
		b.WriteString(escapeMDChar(s[i]))
		i++
	}
	return b.String()
}

// parsePaired 解析以 delim 包裹的内容，返回内部文本与消耗的字节数。
// 空内部或找不到闭合返回 ok=false。
func parsePaired(s, delim string) (string, int, bool) {
	rest := s[len(delim):]
	idx := strings.Index(rest, delim)
	if idx <= 0 {
		return "", 0, false
	}
	return rest[:idx], len(delim) + idx + len(delim), true
}

// parseMarkdownLink 解析 [text](url)，返回 text/url/消耗字节数。
func parseMarkdownLink(s string) (string, string, int, bool) {
	closeBracket := strings.IndexByte(s, ']')
	if closeBracket < 0 || closeBracket+1 >= len(s) || s[closeBracket+1] != '(' {
		return "", "", 0, false
	}
	closeParen := strings.IndexByte(s[closeBracket+2:], ')')
	if closeParen < 0 {
		return "", "", 0, false
	}
	text := s[1:closeBracket]
	url := s[closeBracket+2 : closeBracket+2+closeParen]
	return text, url, closeBracket + 2 + closeParen + 1, true
}

// mdSpecial 是需要转义的 MarkdownV2 特殊字符。
func isMDSpecial(c byte) bool {
	switch c {
	case '_', '*', '[', ']', '(', ')', '~', '`', '>', '#', '+', '-', '=', '|', '{', '}', '.', '!', '\\':
		return true
	}
	return false
}

func escapeMDChar(c byte) string {
	if isMDSpecial(c) {
		return "\\" + string(c)
	}
	return string(c)
}

// escapeCode 转义代码内的 “ ` “ 与 `\`。
func escapeCode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '`' || s[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// escapeLinkText 转义链接文本中的 MarkdownV2 特殊字符。
func escapeLinkText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		b.WriteString(escapeMDChar(s[i]))
	}
	return b.String()
}

// escapeLinkURL 转义链接 URL 中的 `)` 与 `\`。
func escapeLinkURL(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, ")", "\\)")
	return s
}
