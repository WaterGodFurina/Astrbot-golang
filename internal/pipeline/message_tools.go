package pipeline

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// sendMessageToolSchema is the OpenAI schema for send_message_to_user.
func sendMessageToolSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "send_message_to_user",
			"description": "Send a message to the user (current session, or a target session). Each message is an object: {type: 'plain'|'image'|'record'|'video'|'file'|'mention_user', text, path, url, mention_user_id}.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"messages": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"type":            map[string]interface{}{"type": "string", "enum": []interface{}{"plain", "image", "record", "video", "file", "mention_user"}},
								"text":            map[string]interface{}{"type": "string", "description": "Text content (plain/record)."},
								"path":            map[string]interface{}{"type": "string", "description": "Local file path for media."},
								"url":             map[string]interface{}{"type": "string", "description": "Public URL for media."},
								"mention_user_id": map[string]interface{}{"type": "string", "description": "User id to @mention."},
							},
						},
						"description": "Required. The list of message parts to send.",
					},
					"session": map[string]interface{}{
						"type":        "string",
						"description": "Optional. Target session as 'platform:message_type:session_id'. Defaults to the current session.",
					},
				},
				"required": []interface{}{"messages"},
			},
		},
	}
}

// groupHistoryToolSchema is the OpenAI schema for get_group_message_history.
func groupHistoryToolSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "get_group_message_history",
			"description": "Get the recent message history of the current group chat.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "Optional. Number of messages to return. Default 20, max 50.",
					},
				},
			},
		},
	}
}

// providerLTMBool reads a bool under provider_ltm_settings.
func providerLTMBool(cfg map[string]interface{}, key string) bool {
	ps, _ := cfg["provider_ltm_settings"].(map[string]interface{})
	if ps == nil {
		return false
	}
	b, _ := ps[key].(bool)
	return b
}

// providerLTMInt reads an int under provider_ltm_settings with a default.
func providerLTMInt(cfg map[string]interface{}, key string, def int) int {
	ps, _ := cfg["provider_ltm_settings"].(map[string]interface{})
	if ps == nil {
		return def
	}
	switch v := ps[key].(type) {
	case int:
		if v > 0 {
			return v
		}
	case float64:
		if v > 0 {
			return int(v)
		}
	}
	return def
}

// kbAgenticMode reads provider kb_agentic_mode from the top-level config.
func kbAgenticMode(cfg map[string]interface{}) bool {
	v, _ := cfg["kb_agentic_mode"].(bool)
	return v
}

// executeSendMessage implements send_message_to_user: sends an arbitrary
// message chain to the current session (or an explicitly targeted one). The
// session is "platform:message_type:session_id"; the third segment is the
// conversation/session id used by Send.
func (s *ProcessStage) executeSendMessage(event *core.Event, args map[string]interface{}) string {
	if s.platformMgr == nil {
		return "Error: 平台管理器不可用"
	}
	raw, _ := args["messages"].([]interface{})
	if len(raw) == 0 {
		return "Error: send_message_to_user requires a non-empty `messages` array."
	}
	platform := event.Source.Platform
	sessionID := event.Source.ConvID
	if sess, ok := args["session"].(string); ok {
		parts := strings.SplitN(sess, ":", 3)
		if parts[0] != "" {
			platform = parts[0]
		}
		if len(parts) >= 3 {
			sessionID = parts[2]
		}
	}
	chain := &message.MessageChain{}
	for _, m := range raw {
		mm, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		if comp := buildSendComponent(mm); comp != nil {
			chain.Chain = append(chain.Chain, comp)
		}
	}
	if len(chain.Chain) == 0 {
		return "Error: no valid message components in `messages`."
	}
	if err := s.platformMgr.Send(platform, sessionID, chain); err != nil {
		return "Error sending message: " + err.Error()
	}
	return "消息已发送。"
}

// buildSendComponent maps a send_message_to_user message dict to a component.
func buildSendComponent(m map[string]interface{}) message.Component {
	t, _ := m["type"].(string)
	text, _ := m["text"].(string)
	path, _ := m["path"].(string)
	url, _ := m["url"].(string)
	switch t {
	case "plain", "":
		return &message.Plain{Text: text}
	case "image":
		if path != "" {
			return message.ImageFromFile(path)
		}
		if url != "" {
			return message.ImageFromURL(url)
		}
		if b64, ok := m["base64"].(string); ok && b64 != "" {
			return message.ImageFromBase64(b64)
		}
	case "record":
		return &message.Record{Path: path, File: path, URL: url, Text: text}
	case "video":
		return &message.Video{Path: path, URL: url}
	case "file":
		name, _ := m["name"].(string)
		if name == "" {
			name = filepath.Base(path)
		}
		return &message.File{Path: path, URL: url, Name: name}
	case "mention_user":
		uid, _ := m["mention_user_id"].(string)
		return &message.At{TargetID: uid}
	}
	return nil
}

// executeGroupHistory implements get_group_message_history: reads the last N
// platform messages for the current session from the DB history table.
func (s *ProcessStage) executeGroupHistory(event *core.Event, args map[string]interface{}) string {
	if s.database == nil {
		return "Error: 数据库不可用"
	}
	if !providerLTMBool(s.config, "group_message_history_enable") {
		return "Error: group message history is disabled (provider_ltm_settings.group_message_history_enable)."
	}
	if !event.Source.IsGroup {
		return "Error: get_group_message_history is only available in group chats."
	}
	limit := 20
	if v, ok := args["limit"].(float64); ok {
		limit = int(v)
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 50 {
		limit = 50
	}
	rows, err := s.database.GetPlatformMessageHistory(event.Source.Platform, event.UnifiedMsgOrigin(), limit)
	if err != nil {
		return "Error reading group history: " + err.Error()
	}
	var sb strings.Builder
	sb.WriteString("id,time,role,sender,text\n")
	for _, r := range rows {
		role := "user"
		if r.SenderID == event.Source.SelfID {
			role = "assistant"
		}
		sb.WriteString(fmt.Sprintf("%d,%s,%s,%s,%s\n", r.ID, r.CreatedAt, role, r.SenderID, strings.ReplaceAll(r.Content, "\n", " ")))
	}
	return sb.String()
}
