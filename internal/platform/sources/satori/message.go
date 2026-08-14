package satori

// 消息转换层：Satori 事件 → AstrBot 消息组件，以及 AstrBot 消息链 → Satori XML。
// 1:1 移植自 satori_adapter.py 的 parse_satori_elements/_parse_xml_node/_extract_quote_element
// 与 satori_event.py 的 _convert_component_to_satori_static 等。

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// ---------------------------------------------------------------------------
// 事件 → AstrBot 消息
// ---------------------------------------------------------------------------

// convertSatoriMessage 将一条 Satori message-created 事件转换为 AstrBot 消息
// （对齐 Python convert_satori_message）。
func convertSatoriMessage(msg, user, channel, guild, login map[string]interface{}, timestamp int64, hasTimestamp bool) *platform.AstrBotMessage {
	abm := platform.NewAstrBotMessage()
	abm.MessageID, _ = msg["id"].(string)
	abm.RawMessage = map[string]interface{}{
		"message": msg,
		"user":    user,
		"channel": channel,
		"guild":   guild,
		"login":   login,
	}

	// 群组消息与私聊消息的区分（guild 存在且有 id 时为群聊）
	if guildID := strField(guild, "id"); guildID != "" {
		abm.Type = platform.GroupMessage
		abm.Group = &platform.Group{GroupID: guildID}
	} else {
		abm.Type = platform.FriendMessage
	}
	abm.SessionID, _ = channel["id"].(string)

	// 发送者
	userID, _ := user["id"].(string)
	nick, _ := user["nick"].(string)
	if nick == "" {
		nick, _ = user["name"].(string)
	}
	abm.Sender = platform.MessageMember{UserID: userID, Nickname: nick}

	// 机器人自身 id（login.user.id）
	if lu, ok := login["user"].(map[string]interface{}); ok {
		abm.SelfID, _ = lu["id"].(string)
	}

	// 消息链
	abm.Message = []message.Component{}

	content, _ := msg["content"].(string)
	quote, _ := msg["quote"].(map[string]interface{})
	contentForParsing := content // 副本

	// 提取 <quote> 标签
	if strings.Contains(content, "<quote") {
		if qi := extractQuoteElement(content); qi != nil {
			quote = qi.quote
			contentForParsing = qi.contentWithoutQuote
		}
	}

	if quote != nil {
		// 引用消息 → Reply 组件
		quoteABM := convertQuoteMessage(quote)
		if quoteABM != nil {
			reply := &message.Reply{
				MessageID:  quoteABM.MessageID,
				SenderID:   quoteABM.Sender.UserID,
				SenderNick: quoteABM.Sender.Nickname,
				Chain:      quoteABM.Message,
				MessageStr: quoteABM.MessageStr,
				CreatedAt:  time.Unix(quoteABM.Timestamp, 0),
			}
			abm.Message = append(abm.Message, reply)
		}
	}

	// 解析消息内容
	contentElements := parseSatoriElements(contentForParsing)
	abm.Message = append(abm.Message, contentElements...)

	// 纯文本（仅拼接 Plain 组件，对齐 Python）
	var msgStr strings.Builder
	for _, comp := range contentElements {
		if p, ok := comp.(*message.Plain); ok {
			msgStr.WriteString(p.Text)
		}
	}
	abm.MessageStr = msgStr.String()

	// 时间戳：优先使用 Satori 事件中的时间戳
	if hasTimestamp {
		abm.Timestamp = timestamp
	}
	return abm
}

// convertQuoteMessage 转换引用消息（对齐 Python _convert_quote_message）。
func convertQuoteMessage(quote map[string]interface{}) *platform.AstrBotMessage {
	abm := platform.NewAstrBotMessage()
	abm.MessageID, _ = quote["id"].(string)

	// 解析引用消息的发送者
	author, _ := quote["author"].(map[string]interface{})
	if author != nil {
		userID, _ := author["id"].(string)
		nick, _ := author["nick"].(string)
		if nick == "" {
			nick, _ = author["name"].(string)
		}
		abm.Sender = platform.MessageMember{UserID: userID, Nickname: nick}
	} else {
		// 如果没有作者信息，使用默认值
		userID, _ := quote["user_id"].(string)
		abm.Sender = platform.MessageMember{UserID: userID, Nickname: "内容"}
	}

	// 解析引用消息内容
	quoteContent, _ := quote["content"].(string)
	abm.Message = parseSatoriElements(quoteContent)

	var msgStr strings.Builder
	for _, comp := range abm.Message {
		if p, ok := comp.(*message.Plain); ok {
			msgStr.WriteString(p.Text)
		}
	}
	abm.MessageStr = msgStr.String()

	if ts, ok := quote["timestamp"].(float64); ok && ts != 0 {
		abm.Timestamp = int64(ts)
	}

	// 如果没有任何内容，使用默认文本
	if strings.TrimSpace(abm.MessageStr) == "" {
		abm.MessageStr = "[引用消息]"
	}
	return abm
}

// strField 读取 map 中指定字段的字符串值。
func strField(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	if v, ok := m[key]; ok && v != nil {
		return fmt.Sprintf("%v", v)
	}
	return ""
}

// ---------------------------------------------------------------------------
// Satori 消息元素（XML）解析
// ---------------------------------------------------------------------------

// quoteInfo 保存 <quote> 标签的提取结果（对齐 Python _extract_quote_element 返回值）。
type quoteInfo struct {
	quote               map[string]interface{}
	contentWithoutQuote string
}

// extractQuoteElement 从消息内容中提取 <quote> 标签（对齐 Python _extract_quote_element）。
// 先尝试 XML 解析（处理命名空间前缀），失败时退化为正则提取。
func extractQuoteElement(content string) *quoteInfo {
	processed := wrapWithNamespaceRoot(content)
	root, err := parseXMLTree(processed)
	if err == nil {
		if quoteElem := findXMLNode(root, "quote"); quoteElem != nil {
			innerContent := quoteElem.innerContent()
			contentWithoutQuote := content
			// 序列化 quote 元素后若与原文一致才替换（对齐 Python ET.tostring 替换逻辑；
			// 带命名空间前缀时序列化形式不同，替换不生效，quote 保留在内容中被解析时跳过）
			if ser := serializeXMLNode(quoteElem); ser != "" && strings.Contains(content, ser) {
				contentWithoutQuote = strings.Replace(content, ser, "", 1)
			}
			return &quoteInfo{
				quote: map[string]interface{}{
					"id":      quoteElem.attr("id"),
					"content": innerContent,
				},
				contentWithoutQuote: contentWithoutQuote,
			}
		}
		return nil
	}
	// XML 解析失败，使用正则提取（对齐 Python _extract_quote_with_regex）
	logger.Warn("XML解析失败，使用正则提取: %v", err)
	return extractQuoteWithRegex(content)
}

// extractQuoteWithRegex 使用正则表达式提取 quote 标签信息
// （对齐 Python _extract_quote_with_regex）。
func extractQuoteWithRegex(content string) *quoteInfo {
	quotePattern := regexp.MustCompile(`(?s)<quote\s+([^>]*)>(.*?)</quote>`)
	match := quotePattern.FindStringSubmatch(content)
	if len(match) == 0 {
		return nil
	}
	attrsStr := match[1]
	innerContent := match[2]

	idPattern := regexp.MustCompile(`id\s*=\s*["']([^"']*)["']`)
	quoteID := ""
	if idMatch := idPattern.FindStringSubmatch(attrsStr); len(idMatch) > 1 {
		quoteID = idMatch[1]
	}
	contentWithoutQuote := strings.TrimSpace(strings.Replace(content, match[0], "", 1))

	return &quoteInfo{
		quote: map[string]interface{}{
			"id":      quoteID,
			"content": innerContent,
		},
		contentWithoutQuote: contentWithoutQuote,
	}
}

// extractNamespacePrefixes 提取 XML 内容中的命名空间前缀（对齐 Python _extract_namespace_prefixes）。
func extractNamespacePrefixes(content string) []string {
	prefixes := []string{}
	seen := map[string]bool{}
	i := 0
	for i < len(content) {
		// 查找开始标签
		if content[i] == '<' && i+1 < len(content) && content[i+1] != '/' {
			tagEnd := strings.Index(content[i:], ">")
			if tagEnd == -1 {
				i++
				continue
			}
			tagContent := content[i+1 : i+tagEnd]
			// 检查是否有命名空间前缀
			if strings.Contains(tagContent, ":") && !strings.Contains(tagContent, "xmlns:") {
				parts := strings.Fields(tagContent)
				if len(parts) > 0 {
					tagName := parts[0]
					if idx := strings.Index(tagName, ":"); idx != -1 {
						prefix := tagName[:idx]
						// 确保是有效的命名空间前缀
						if isAlnumPrefix(prefix) && !seen[prefix] {
							seen[prefix] = true
							prefixes = append(prefixes, prefix)
						}
					}
				}
			}
			i += tagEnd + 1
			continue
		}
		// 查找结束标签
		if content[i] == '<' && i+1 < len(content) && content[i+1] == '/' {
			tagEnd := strings.Index(content[i:], ">")
			if tagEnd == -1 {
				i++
				continue
			}
			tagContent := content[i+2 : i+tagEnd]
			if strings.Contains(tagContent, ":") {
				prefix := strings.SplitN(tagContent, ":", 2)[0]
				if isAlnumPrefix(prefix) && !seen[prefix] {
					seen[prefix] = true
					prefixes = append(prefixes, prefix)
				}
			}
			i += tagEnd + 1
			continue
		}
		i++
	}
	return prefixes
}

// isAlnumPrefix 判断字符串是否由字母数字（或含下划线）组成（对齐 Python 的 isalnum 检查）。
func isAlnumPrefix(s string) bool {
	return isASCIIAlnum(s) || isASCIIAlnum(strings.ReplaceAll(s, "_", ""))
}

// isASCIIAlnum 判断字符串是否全为 ASCII 字母或数字。
func isASCIIAlnum(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// wrapWithNamespaceRoot 包装消息内容：为命名空间前缀声明临时 URI，并包裹 <root>。
func wrapWithNamespaceRoot(content string) string {
	if strings.Contains(content, ":") && !strings.HasPrefix(content, "<root") {
		prefixes := extractNamespacePrefixes(content)
		nsDecls := make([]string, 0, len(prefixes))
		for _, p := range prefixes {
			nsDecls = append(nsDecls, fmt.Sprintf(`xmlns:%s="http://temp.uri/%s"`, p, p))
		}
		return "<root " + strings.Join(nsDecls, " ") + ">" + content + "</root>"
	}
	if !strings.HasPrefix(content, "<root") {
		return "<root>" + content + "</root>"
	}
	return content
}

// parseSatoriElements 解析 Satori 消息元素数组（对齐 Python parse_satori_elements）。
func parseSatoriElements(content string) []message.Component {
	elements := []message.Component{}
	if content == "" {
		return elements
	}

	processed := wrapWithNamespaceRoot(content)
	d := xml.NewDecoder(strings.NewReader(processed))
	// 跳过根元素 <root> 的开始标签，直接遍历其子节点
	for {
		tok, err := d.Token()
		if err != nil {
			if strings.TrimSpace(content) != "" {
				elements = append(elements, &message.Plain{Text: content})
			}
			return elements
		}
		if _, ok := tok.(xml.StartElement); ok {
			break
		}
	}
	if err := walkSatoriChildren(d, &elements); err != nil {
		logger.Warn("解析 Satori 元素时发生解析错误: %v, 错误内容: %s", err, content)
		// 如果解析失败，将整个内容当作纯文本
		if strings.TrimSpace(content) != "" {
			elements = append(elements, &message.Plain{Text: content})
		}
	}

	// 如果没有解析到任何元素，将整个内容当作纯文本
	if len(elements) == 0 && strings.TrimSpace(content) != "" {
		elements = append(elements, &message.Plain{Text: content})
	}
	return elements
}

// walkSatoriChildren 递归解析父节点下的所有子节点，转换为消息组件
// （对齐 Python _parse_xml_node 的节点遍历：node.text / child / child.tail）。
func walkSatoriChildren(d *xml.Decoder, elements *[]message.Component) error {
	var pending []byte
	flushText := func() {
		if len(pending) > 0 {
			if len(strings.TrimSpace(string(pending))) > 0 {
				*elements = append(*elements, &message.Plain{Text: string(pending)})
			}
			pending = nil
		}
	}
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.CharData:
			pending = append(pending, t...)
		case xml.StartElement:
			// 子元素前的文本（父节点的 text 或上一个子元素的 tail）
			flushText()
			if err := handleSatoriElement(d, t, elements); err != nil {
				return err
			}
		case xml.EndElement:
			// 末尾文本（最后一个子元素的 tail）
			flushText()
			return nil
		}
	}
}

// handleSatoriElement 处理一个 Satori 消息元素（对齐 Python _parse_xml_node 的分支逻辑）。
func handleSatoriElement(d *xml.Decoder, start xml.StartElement, elements *[]message.Component) error {
	// 获取标签名，去除命名空间前缀
	tagName := strings.ToLower(start.Name.Local)

	// 属性（去除命名空间前缀）
	attrs := map[string]string{}
	for _, a := range start.Attr {
		attrs[a.Name.Local] = a.Value
	}

	switch tagName {
	case "at":
		userID := attrs["id"]
		if userID == "" {
			userID = attrs["name"]
		}
		*elements = append(*elements, &message.At{TargetID: userID, Name: userID})
		return skipElement(d)
	case "img", "image":
		src := attrs["src"]
		if src == "" {
			return skipElement(d)
		}
		*elements = append(*elements, message.ImageFromURL(src))
		return skipElement(d)
	case "file":
		src := attrs["src"]
		name := attrs["name"]
		if name == "" {
			name = "文件"
		}
		if src != "" {
			*elements = append(*elements, &message.File{Name: name, URL: src})
		}
		return skipElement(d)
	case "audio", "record":
		src := attrs["src"]
		if src == "" {
			return skipElement(d)
		}
		// Python 会下载并转换为 wav；Go 直接保留 URL
		*elements = append(*elements, &message.Record{URL: src})
		return skipElement(d)
	case "quote":
		// quote 标签已经被特殊处理
		return skipElement(d)
	case "face":
		faceID := attrs["id"]
		faceName := attrs["name"]
		faceType := attrs["type"]
		switch {
		case faceName != "":
			*elements = append(*elements, &message.Plain{Text: "[表情:" + faceName + "]"})
		case faceID != "" && faceType != "":
			*elements = append(*elements, &message.Plain{Text: "[表情ID:" + faceID + ",类型:" + faceType + "]"})
		case faceID != "":
			*elements = append(*elements, &message.Plain{Text: "[表情ID:" + faceID + "]"})
		default:
			*elements = append(*elements, &message.Plain{Text: "[表情]"})
		}
		return skipElement(d)
	case "ark":
		// 作为纯文本添加到消息链中
		data := attrs["data"]
		if data != "" {
			decoded := html.UnescapeString(data)
			*elements = append(*elements, &message.Plain{Text: "[ARK卡片数据: " + decoded + "]"})
		} else {
			*elements = append(*elements, &message.Plain{Text: "[ARK卡片]"})
		}
		return skipElement(d)
	case "json":
		// JSON 标签视为 ARK 卡片消息（对齐 Python 的行为）
		data := attrs["data"]
		if data != "" {
			decoded := html.UnescapeString(data)
			*elements = append(*elements, &message.Plain{Text: "[ARK卡片数据: " + decoded + "]"})
		} else {
			*elements = append(*elements, &message.Plain{Text: "[JSON卡片]"})
		}
		return skipElement(d)
	default:
		// 未知标签，递归处理其内容（walkSatoriChildren 会消费该元素的 EndElement）
		return walkSatoriChildren(d, elements)
	}
}

// skipElement 跳过当前元素的完整子树（包括其 EndElement）。
func skipElement(d *xml.Decoder) error {
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		if _, ok := tok.(xml.EndElement); ok {
			return nil
		}
	}
}

// ---------------------------------------------------------------------------
// 轻量 XML 树（用于 <quote> 提取）
// ---------------------------------------------------------------------------

// xmlNode 表示一个 XML 元素节点。
type xmlNode struct {
	start    xml.StartElement
	text     string
	children []*xmlNode
	tail     string
}

// attr 返回节点属性值（按属性原名查找）。
func (n *xmlNode) attr(name string) string {
	for _, a := range n.start.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// innerContent 返回元素内部内容（文本 + 子元素序列化 + 子元素 tail，
// 对齐 Python quote_element.text + tostring(child) + child.tail）。
func (n *xmlNode) innerContent() string {
	var sb strings.Builder
	sb.WriteString(n.text)
	for _, c := range n.children {
		sb.WriteString(serializeXMLNode(c))
		sb.WriteString(c.tail)
	}
	return sb.String()
}

// parseXMLTree 将 XML 文档解析为元素树。
func parseXMLTree(doc string) (*xmlNode, error) {
	d := xml.NewDecoder(strings.NewReader(doc))
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, err
		}
		if se, ok := tok.(xml.StartElement); ok {
			root := &xmlNode{start: se}
			if err := buildXMLChildren(d, root); err != nil {
				return nil, err
			}
			return root, nil
		}
	}
}

// buildXMLChildren 构建子节点树，并收集 text/tail。
func buildXMLChildren(d *xml.Decoder, parent *xmlNode) error {
	var pending []byte
	setPending := func() {
		if len(pending) > 0 {
			if len(parent.children) == 0 {
				parent.text = string(pending)
			} else {
				parent.children[len(parent.children)-1].tail = string(pending)
			}
			pending = nil
		}
	}
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.CharData:
			pending = append(pending, t...)
		case xml.StartElement:
			setPending()
			child := &xmlNode{start: t}
			if err := buildXMLChildren(d, child); err != nil {
				return err
			}
			parent.children = append(parent.children, child)
		case xml.EndElement:
			setPending()
			return nil
		}
	}
}

// findXMLNode 在树中按标签名（忽略大小写与命名空间）查找元素（文档序 DFS）。
func findXMLNode(n *xmlNode, name string) *xmlNode {
	if strings.ToLower(n.start.Name.Local) == name {
		return n
	}
	for _, c := range n.children {
		if found := findXMLNode(c, name); found != nil {
			return found
		}
	}
	return nil
}

// serializeXMLNode 将节点序列化为 XML 字符串。
// 手写序列化以对齐 Python ElementTree.tostring 的行为：
// 空元素输出自闭合 <tag/>，文本转义 & < >，属性值额外转义双引号。
func serializeXMLNode(n *xmlNode) string {
	var sb strings.Builder
	writeXMLNode(&sb, n)
	return sb.String()
}

// writeXMLNode 递归写出节点。
func writeXMLNode(sb *strings.Builder, n *xmlNode) {
	name := n.start.Name.Local
	sb.WriteString("<")
	sb.WriteString(name)
	for _, a := range n.start.Attr {
		sb.WriteString(" ")
		sb.WriteString(a.Name.Local)
		sb.WriteString(`="`)
		sb.WriteString(escapeAttrValue(a.Value))
		sb.WriteString(`"`)
	}
	if n.text == "" && len(n.children) == 0 {
		sb.WriteString("/>")
		return
	}
	sb.WriteString(">")
	if n.text != "" {
		sb.WriteString(escapeTextContent(n.text))
	}
	for _, c := range n.children {
		writeXMLNode(sb, c)
		if c.tail != "" {
			sb.WriteString(escapeTextContent(c.tail))
		}
	}
	sb.WriteString("</")
	sb.WriteString(name)
	sb.WriteString(">")
}

// escapeTextContent 转义元素文本（对齐 Python ElementTree 的 _escape_cdata）。
func escapeTextContent(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// escapeAttrValue 转义属性值（对齐 Python ElementTree 的 _escape_attrib）。
func escapeAttrValue(s string) string {
	s = escapeTextContent(s)
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}

// ---------------------------------------------------------------------------
// AstrBot 消息链 → Satori XML（发送）
// ---------------------------------------------------------------------------

// buildSatoriContent 将消息链转换为 Satori 消息内容（对齐 Python send 中的拼接逻辑）。
func buildSatoriContent(chain *message.MessageChain) string {
	var sb strings.Builder
	for _, comp := range chain.Chain {
		// 普通组件转换
		sb.WriteString(convertComponentToSatori(comp))
		// 特殊处理 Node 与 Nodes 组件
		if node, ok := comp.(*message.Node); ok {
			sb.WriteString(convertNodeToSatori(node))
		} else if nodes, ok := comp.(*message.Nodes); ok {
			sb.WriteString(convertNodesToSatori(nodes))
		}
	}
	return sb.String()
}

// convertComponentToSatori 将单个消息组件转换为 Satori 格式
// （对齐 Python _convert_component_to_satori_static）。
func convertComponentToSatori(comp message.Component) string {
	switch c := comp.(type) {
	case *message.Plain:
		text := c.Text
		text = strings.ReplaceAll(text, "&", "&amp;")
		text = strings.ReplaceAll(text, "<", "&lt;")
		text = strings.ReplaceAll(text, ">", "&gt;")
		return text
	case *message.At:
		if c.TargetID != "" {
			return fmt.Sprintf(`<at id="%s"/>`, c.TargetID)
		}
		if c.Name != "" {
			return fmt.Sprintf(`<at name="%s"/>`, c.Name)
		}
	case *message.Image:
		dataURL, err := imageToDataURL(c)
		if err == nil && dataURL != "" {
			return fmt.Sprintf(`<img src="%s"/>`, dataURL)
		}
		logger.Error("图片转换为base64失败: %v", err)
	case *message.File:
		name := c.Name
		if name == "" {
			name = "文件"
		}
		ref := c.URL
		if ref == "" {
			ref = c.Path
		}
		return fmt.Sprintf(`<file src="%s" name="%s"/>`, ref, name)
	case *message.Record:
		b64, err := recordToBase64(c)
		if err == nil && b64 != "" {
			return fmt.Sprintf(`<audio src="data:audio/wav;base64,%s"/>`, b64)
		}
		logger.Error("语音转换为base64失败: %v", err)
	case *message.Reply:
		return fmt.Sprintf(`<reply id="%s"/>`, c.MessageID)
	case *message.Video:
		ref := c.Path
		if ref == "" {
			ref = c.URL
		}
		if ref == "" {
			ref = c.FileID
		}
		return fmt.Sprintf(`<video src="%s"/>`, ref)
	case *message.Forward:
		return fmt.Sprintf(`<message id="%s" forward/>`, c.ID)
	}
	// 对于其他未处理的组件类型，返回空字符串
	return ""
}

// convertNodeToSatori 将单个转发节点转换为 Satori 格式
// （对齐 Python _convert_node_to_satori_static）。
func convertNodeToSatori(node *message.Node) string {
	var content strings.Builder
	for _, comp := range node.Content {
		content.WriteString(convertComponentToSatori(comp))
	}
	s := content.String()
	// 如果内容为空，添加默认内容
	if strings.TrimSpace(s) == "" {
		s = "[转发消息]"
	}
	// 构建 Satori 格式的转发节点
	authorAttrs := []string{}
	if node.UIN != "" {
		authorAttrs = append(authorAttrs, fmt.Sprintf(`id="%s"`, node.UIN))
	}
	if node.Name != "" {
		authorAttrs = append(authorAttrs, fmt.Sprintf(`name="%s"`, node.Name))
	}
	return fmt.Sprintf(`<message><author %s/>%s</message>`, strings.Join(authorAttrs, " "), s)
}

// convertNodesToSatori 将多个转发节点转换为 Satori 格式的合并转发
// （对齐 Python _convert_nodes_to_satori_static）。
func convertNodesToSatori(nodes *message.Nodes) string {
	nodeParts := []string{}
	for _, node := range nodes.Nodes {
		if node == nil {
			continue
		}
		if nodeContent := convertNodeToSatori(node); nodeContent != "" {
			nodeParts = append(nodeParts, nodeContent)
		}
	}
	if len(nodeParts) == 0 {
		return ""
	}
	return "<message forward>" + strings.Join(nodeParts, "") + "</message>"
}

// imageToDataURL 将图片组件解析为带 MIME 的 data URL
// （对齐 Python resolve_media_ref_to_base64_data + to_data_url）。
func imageToDataURL(img *message.Image) (string, error) {
	var data []byte
	mimeType := ""
	switch {
	case img.Base64 != "":
		b64, err := decodeBase64Payload(img.Base64)
		if err != nil {
			return "", err
		}
		data = b64
	case img.Path != "":
		raw, err := os.ReadFile(img.Path)
		if err != nil {
			return "", err
		}
		data = raw
		mimeType = mimeTypeByExt(filepath.Ext(img.Path))
	case img.File != "":
		raw, err := os.ReadFile(img.File)
		if err != nil {
			return "", err
		}
		data = raw
		mimeType = mimeTypeByExt(filepath.Ext(img.File))
	case img.URL != "":
		resp, err := http.Get(img.URL)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", err
		}
		data = raw
		mimeType = mimeTypeByExt(filepath.Ext(resp.Request.URL.Path))
	default:
		return "", fmt.Errorf("图片组件没有可用的资源引用")
	}
	if mimeType == "" {
		mimeType = sniffImageMIME(data)
	}
	if mimeType == "" {
		mimeType = "image/jpeg" // 对齐 Python 默认 image/jpeg
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// recordToBase64 将语音组件解析为 base64 字符串。
func recordToBase64(rec *message.Record) (string, error) {
	var data []byte
	switch {
	case rec.Base64 != "":
		b64, err := decodeBase64Payload(rec.Base64)
		if err != nil {
			return "", err
		}
		data = b64
	case rec.Path != "":
		raw, err := os.ReadFile(rec.Path)
		if err != nil {
			return "", err
		}
		data = raw
	case rec.File != "":
		raw, err := os.ReadFile(rec.File)
		if err != nil {
			return "", err
		}
		data = raw
	case rec.URL != "":
		resp, err := http.Get(rec.URL)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", err
		}
		data = raw
	default:
		return "", fmt.Errorf("语音组件没有可用的资源引用")
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// decodeBase64Payload 解码 base64 载荷（支持 data: URI 前缀与缺失填充）。
func decodeBase64Payload(s string) ([]byte, error) {
	if idx := strings.Index(s, "base64,"); idx >= 0 {
		s = s[idx+7:]
	}
	s = strings.TrimSpace(s)
	if data, err := base64.StdEncoding.DecodeString(s); err == nil {
		return data, nil
	}
	if data, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return data, nil
	}
	return nil, fmt.Errorf("无效的 base64 数据")
}

// mimeTypeByExt 根据文件扩展名推断 MIME 类型。
func mimeTypeByExt(ext string) string {
	if ext == "" {
		return ""
	}
	return mime.TypeByExtension(strings.ToLower(ext))
}

// sniffImageMIME 通过魔数推断图片 MIME 类型。
func sniffImageMIME(data []byte) string {
	if len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}) {
		return "image/png"
	}
	if len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return "image/jpeg"
	}
	if len(data) >= 6 && bytes.Equal(data[:6], []byte{'G', 'I', 'F', '8', '7', 'a'}) {
		return "image/gif"
	}
	if len(data) >= 12 && bytes.Equal(data[:4], []byte{'R', 'I', 'F', 'F'}) &&
		bytes.Equal(data[8:12], []byte{'W', 'E', 'B', 'P'}) {
		return "image/webp"
	}
	return ""
}
