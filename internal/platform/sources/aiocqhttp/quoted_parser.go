package aiocqhttp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// quotedParserSettings mirrors
// astrbot/core/utils/quoted_message/settings.py (OneBot forward-message
// fetching parts). max_component_chain_depth / max_quoted_fallback_images are
// applied pipeline-side by quoted_message_parser.go.
type quotedParserSettings struct {
	maxForwardFetch     int
	maxForwardNodeDepth int
	warnOnActionFailure bool
}

func defaultQuotedParserSettings() quotedParserSettings {
	return quotedParserSettings{
		maxForwardFetch:     32,
		maxForwardNodeDepth: 6,
	}
}

// resolveQuotedParserSettings reads provider_settings.quoted_message_parser
// from the adapter settings (provider_settings attached by lifecycle).
func resolveQuotedParserSettings(settings map[string]interface{}) quotedParserSettings {
	s := defaultQuotedParserSettings()
	ps, ok := settings["provider_settings"].(map[string]interface{})
	if !ok {
		return s
	}
	raw, ok := ps["quoted_message_parser"].(map[string]interface{})
	if !ok {
		return s
	}
	if v, ok := raw["max_forward_fetch"].(int); ok && v >= 0 {
		s.maxForwardFetch = v
	}
	if v, ok := raw["max_forward_fetch"].(float64); ok && v >= 0 {
		s.maxForwardFetch = int(v)
	}
	if v, ok := raw["max_forward_node_depth"].(int); ok && v > 0 {
		s.maxForwardNodeDepth = v
	}
	if v, ok := raw["max_forward_node_depth"].(float64); ok && v > 0 {
		s.maxForwardNodeDepth = int(v)
	}
	if v, ok := raw["warn_on_action_failure"].(bool); ok {
		s.warnOnActionFailure = v
	}
	return s
}

// parseOneBotSegments converts OneBot v11 message segments into components,
// collecting forward-message ids for later fetching. Inline forward node
// content is parsed recursively (mirrors _parse_onebot_segments).
// The depth limit guards nested forward nodes (max_forward_node_depth).
// groupID 为消息所属群号（群消息事件携带，私聊/无上下文传空串），供 @ 段
// 的群昵称解析使用（get_group_member_info，对齐 Python adapter 的同步调用）。
func (a *Adapter) parseOneBotSegments(segments []interface{}, depth int, groupID string) ([]message.Component, []string) {
	chain := []message.Component{}
	forwardIDs := []string{}

	for _, seg := range segments {
		segMap, ok := seg.(map[string]interface{})
		if !ok {
			continue
		}
		segType, _ := segMap["type"].(string)
		data, _ := segMap["data"].(map[string]interface{})

		switch segType {
		case "text", "plain":
			text, _ := data["text"].(string)
			chain = append(chain, &message.Plain{Text: text})
		case "markdown":
			// markdown 消息段按纯文本接收（对齐 Python adapter.py:400-404：
			// 优先 data.markdown，回退 data.content，转成 Plain 并保留原文）。
			text, _ := data["markdown"].(string)
			if text == "" {
				text, _ = data["content"].(string)
			}
			chain = append(chain, &message.Plain{Text: text})
		case "at":
			qq := toString(data["qq"])
			name, _ := data["name"].(string)
			if qq == "all" {
				chain = append(chain, &message.AtAll{})
			} else {
				// 事件段通常不带昵称，经 get_group_member_info 拉取
				// card/nickname（对齐 Python adapter.py:344-397；同步调用，
				// 失败或拿不到时降级用段内原始 name 值）。
				if name == "" {
					name = a.fetchGroupMemberNickname(groupID, qq)
				}
				chain = append(chain, &message.At{TargetID: qq, Name: name})
			}
		case "image":
			url, _ := data["url"].(string)
			file, _ := data["file"].(string)
			chain = append(chain, &message.Image{URL: url, File: file})
		case "record":
			url, _ := data["url"].(string)
			file, _ := data["file"].(string)
			chain = append(chain, &message.Record{URL: url, File: file})
		case "reply":
			rid := toString(data["id"])
			if rid == "" {
				rid = toString(data["message_id"])
			}
			if rid != "" {
				chain = append(chain, &message.Reply{MessageID: rid})
			}
		case "face":
			chain = append(chain, &message.Face{ID: toString(data["id"])})
		case "file":
			url, _ := data["url"].(string)
			name, _ := data["name"].(string)
			// NapCat 事件里 file 是文件名、file_id 才是真正的文件 ID
			// （对齐 Python aiocqhttp_platform_adapter 用 data["file_id"]）。
			fileID := toString(data["file_id"])
			if fileID == "" {
				fileID = toString(data["file"])
			}
			if name == "" {
				name = fileID
			}
			chain = append(chain, &message.File{URL: url, Name: name, FileID: fileID})
		case "video":
			url, _ := data["url"].(string)
			file, _ := data["file"].(string)
			chain = append(chain, &message.Video{URL: url, FileID: file})
		case "json":
			jsonStr, _ := data["data"].(string)
			var jsonData map[string]interface{}
			if jsonStr != "" {
				_ = json.Unmarshal([]byte(jsonStr), &jsonData)
			}
			if jsonData == nil {
				jsonData = make(map[string]interface{})
			}
			chain = append(chain, &message.Json{Data: jsonData})
		case "forward", "forward_msg", "nodes":
			fid := toString(data["id"])
			if fid == "" {
				fid = toString(data["message_id"])
			}
			if fid != "" {
				forwardIDs = append(forwardIDs, fid)
				chain = append(chain, &message.Nodes{ForwardIDs: []string{fid}})
				continue
			}
			// Inline node content.
			if content, ok := data["content"].([]interface{}); ok {
				nodes := a.parseForwardNodes(content, depth+1, groupID)
				if len(nodes) > 0 {
					chain = append(chain, &message.Nodes{Nodes: nodes})
				}
			}
		case "node":
			// A single node (may appear in forwarded messages).
			fid := toString(data["id"])
			if fid != "" {
				forwardIDs = append(forwardIDs, fid)
				chain = append(chain, &message.Nodes{ForwardIDs: []string{fid}})
				continue
			}
			if nodes := a.parseForwardNodes([]interface{}{segMap}, depth+1, groupID); len(nodes) > 0 {
				chain = append(chain, &message.Nodes{Nodes: nodes})
			}
		}
	}
	return chain, forwardIDs
}

// fetchGroupMemberNickname 通过 get_group_member_info 拉取群成员的群昵称
// （card），card 为空时回退 nickname；查询失败或无群上下文时返回空串，
// 调用方降级用事件段内原始 name（对齐 Python adapter.py:355-374 的同步
// get_group_member_info 调用，无缓存参数对齐 no_cache=False 默认行为）。
func (a *Adapter) fetchGroupMemberNickname(groupID, userID string) string {
	if groupID == "" || userID == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ret, err := a.CallActionCtx(ctx, "get_group_member_info", map[string]interface{}{
		"group_id": groupID,
		"user_id":  userID,
	})
	if err != nil {
		logger.Debug("aiocqhttp: 获取 @ 用户信息失败 qq=%s: %v", userID, err)
		return ""
	}
	if card, _ := ret["card"].(string); card != "" {
		return card
	}
	if nick, _ := ret["nickname"].(string); nick != "" {
		return nick
	}
	return ""
}

// parseForwardNodes converts a get_forward_msg node list into Node components
// (mirrors _extract_text_forward_ids_and_images_from_forward_nodes, but
// preserves the component structure). Nested forwards are capped at
// max_forward_node_depth. groupID 透传给 parseOneBotSegments（@ 昵称解析）。
func (a *Adapter) parseForwardNodes(nodes []interface{}, depth int, groupID string) []*message.Node {
	if depth > a.quotedParser.maxForwardNodeDepth {
		return nil
	}
	out := []*message.Node{}
	for _, node := range nodes {
		nodeMap, ok := node.(map[string]interface{})
		if !ok {
			continue
		}
		data, _ := nodeMap["data"].(map[string]interface{})
		if data == nil {
			data = nodeMap
		}
		uin := toString(data["uin"])
		if uin == "" {
			uin = toString(data["user_id"])
		}
		name, _ := data["name"].(string)

		rawContent := data["content"]
		if rawContent == nil {
			rawContent = nodeMap["message"]
		}
		var content []message.Component
		switch rc := rawContent.(type) {
		case []interface{}:
			// Parse each segment; nested node segments recurse with depth+1.
			for _, seg := range rc {
				segMap, ok := seg.(map[string]interface{})
				if !ok {
					continue
				}
				segType, _ := segMap["type"].(string)
				if segType == "node" || segType == "nodes" {
					if subNodes := a.parseForwardNodes([]interface{}{segMap}, depth+1, groupID); len(subNodes) > 0 {
						content = append(content, &message.Nodes{Nodes: subNodes})
					}
					continue
				}
				parsed, _ := a.parseOneBotSegments([]interface{}{segMap}, depth, groupID)
				content = append(content, parsed...)
			}
		case string:
			content = append(content, &message.Plain{Text: rc})
		}
		if len(content) == 0 {
			continue
		}
		out = append(out, &message.Node{UIN: uin, Name: name, Content: content})
	}
	return out
}

// fetchForwardMessage retrieves a combined-forward message via the
// get_forward_msg action and returns its node components. Mirrors
// OneBotClient.get_forward_msg + payload parsing.
func (a *Adapter) fetchForwardMessage(forwardID string) []*message.Node {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ret, err := a.CallActionCtx(ctx, "get_forward_msg", map[string]interface{}{"id": forwardID})
	if err != nil {
		if a.quotedParser.warnOnActionFailure {
			logger.Warn("quoted_message_parser: get_forward_msg 失败 id=%s: %v", forwardID, err)
		}
		return nil
	}
	data, _ := ret["data"].(map[string]interface{})
	if data == nil {
		data = ret
	}
	messages, _ := data["messages"].([]interface{})
	if len(messages) == 0 {
		return nil
	}
	// 转发消息节点内容不解析 @ 群昵称（缺群上下文，与原始行为一致）。
	return a.parseForwardNodes(messages, 0, "")
}

// fetchQuotedContent fetches the message referenced by a reply id
// (get_msg) and collects nested forward ids from its content (mirrors
// QuotedMessageExtractor._fetch_quoted_content). Returns the parsed chain
// and the collected forward ids. groupID/userID provide the file-URL
// resolution context (get_group_file_url / get_private_file_url) for any File
// component in the quoted message; empty values leave file URLs unset.
func (a *Adapter) fetchQuotedContent(messageID, groupID, userID string) ([]message.Component, []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ret, err := a.CallActionCtx(ctx, "get_msg", map[string]interface{}{"message_id": messageID})
	if err != nil {
		if a.quotedParser.warnOnActionFailure {
			logger.Warn("quoted_message_parser: get_msg 失败 id=%s: %v", messageID, err)
		}
		return nil, nil
	}
	data, _ := ret["data"].(map[string]interface{})
	if data == nil {
		data = ret
	}
	segments, _ := data["message"].([]interface{})
	if len(segments) == 0 {
		return nil, nil
	}
	chain, forwardIDs := a.parseOneBotSegments(segments, 0, groupID)
	if groupID != "" || userID != "" {
		a.enrichFileURLsIn(chain, groupID, userID)
	}
	return chain, forwardIDs
}

// resolveNestedForwards performs a BFS over forward-message ids, fetching
// each one with get_forward_msg until max_forward_fetch hops (mirrors
// _collect_text_and_images_from_forward_ids). Returns the node components.
func (a *Adapter) resolveNestedForwards(forwardIDs []string) []*message.Node {
	out := []*message.Node{}
	pending := []string{}
	seen := map[string]bool{}
	for _, fid := range forwardIDs {
		fid = trimSpace(fid)
		if fid != "" {
			pending = append(pending, fid)
		}
	}
	fetchCount := 0
	maxFetch := a.quotedParser.maxForwardFetch
	for len(pending) > 0 && fetchCount < maxFetch {
		current := pending[0]
		pending = pending[1:]
		if seen[current] {
			continue
		}
		seen[current] = true
		fetchCount++
		nodes := a.fetchForwardMessage(current)
		if len(nodes) == 0 {
			continue
		}
		out = append(out, nodes...)
		// Nested forward ids inside fetched nodes.
		for _, n := range nodes {
			collectNodeForwardIDs(n, &pending, seen)
		}
	}
	if len(pending) > 0 && maxFetch > 0 {
		logger.Warn("quoted_message_parser: 在 %d 跳后停止获取嵌套转发消息", maxFetch)
	}
	return out
}

// collectNodeForwardIDs walks node content for nested forward components and
// queues their ids (fetched node content carries nested Nodes).
func collectNodeForwardIDs(node *message.Node, pending *[]string, seen map[string]bool) {
	var walk func(chain []message.Component)
	walk = func(chain []message.Component) {
		for _, comp := range chain {
			switch c := comp.(type) {
			case *message.Nodes:
				for _, n := range c.Nodes {
					if n != nil {
						walk(n.Content)
					}
				}
				for _, fid := range c.ForwardIDs {
					fid = trimSpace(fid)
					if fid != "" && !seen[fid] {
						*pending = append(*pending, fid)
					}
				}
			case *message.Reply:
				walk(c.Chain)
			}
		}
	}
	walk(node.Content)
}

func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n') {
		end--
	}
	return s[start:end]
}

func toString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%v", int64(t))
	case int:
		return fmt.Sprintf("%d", t)
	case int64:
		return fmt.Sprintf("%d", t)
	}
	return ""
}
