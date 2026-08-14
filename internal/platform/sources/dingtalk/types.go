// Package dingtalk implements a DingTalk (钉钉) robot platform adapter.
// 从 astrbot/core/platform/sources/dingtalk/ (Python) 1:1 移植。
// 协议: 钉钉 Stream 长连接 (POST /v1.0/gateway/connections/open 获取 endpoint,
// WebSocket 收事件帧 + WS 层心跳) + REST (发送机器人消息/media 上传/文件下载)。
// 参考文件: dingtalk_adapter.py (适配器) / dingtalk_event.py (事件发送)。
package dingtalk

import (
	"strconv"
)

// 钉钉 Stream 机器人消息回调主题 (对应 dingtalk_stream.ChatbotMessage.TOPIC)。
const chatTopic = "/v1.0/im/bot/messages/get"

// 钉钉 OpenAPI 地址。
const (
	dingtalkOpenAPI = "https://api.dingtalk.com"
	dingtalkOAPI    = "https://oapi.dingtalk.com"
)

// AtUser 对应 dingtalk_stream.AtUser。
type AtUser struct {
	DingtalkID string
	StaffID    string
}

// ChatbotMessage 对应 dingtalk_stream.ChatbotMessage (from_dict 解析结果)。
type ChatbotMessage struct {
	ConversationID   string
	ConversationType string // "2" = 群聊
	CreateAt         int64
	MsgID            string
	SenderID         string
	SenderNick       string
	SenderStaffID    string
	ChatbotUserID    string
	IsInAtList       bool
	RobotCode        string
	MessageType      string // text / picture / richText / audio / voice / file
	AtUsers          []AtUser

	// msgtype 相关的消息内容
	TextContent  string                   // msgtype=text: text.content
	DownloadCode string                   // msgtype=picture: content.downloadCode
	RichText     []map[string]interface{} // msgtype=richText: content.richText
	Content      map[string]interface{}   // 顶层 content 字段 (audio/voice/file 等)
}

// parseChatbotMessage 解析机器人消息回调数据 (对应 ChatbotMessage.from_dict)。
func parseChatbotMessage(d map[string]interface{}) *ChatbotMessage {
	msg := &ChatbotMessage{}
	if v, ok := d["conversationId"].(string); ok {
		msg.ConversationID = v
	}
	if v, ok := d["conversationType"].(string); ok {
		msg.ConversationType = v
	}
	if v, ok := d["createAt"].(float64); ok {
		msg.CreateAt = int64(v)
	}
	if v, ok := d["msgId"].(string); ok {
		msg.MsgID = v
	}
	if v, ok := d["senderId"].(string); ok {
		msg.SenderID = v
	}
	if v, ok := d["senderNick"].(string); ok {
		msg.SenderNick = v
	}
	if v, ok := d["senderStaffId"].(string); ok {
		msg.SenderStaffID = v
	}
	if v, ok := d["chatbotUserId"].(string); ok {
		msg.ChatbotUserID = v
	}
	if v, ok := d["isInAtList"].(bool); ok {
		msg.IsInAtList = v
	}
	if v, ok := d["robotCode"].(string); ok {
		msg.RobotCode = v
	}
	if v, ok := d["msgtype"].(string); ok {
		msg.MessageType = v
	}
	// atUsers 列表
	if rawList, ok := d["atUsers"].([]interface{}); ok {
		for _, item := range rawList {
			userMap, _ := item.(map[string]interface{})
			user := AtUser{}
			if v, ok := userMap["dingtalkId"].(string); ok {
				user.DingtalkID = v
			}
			if v, ok := userMap["staffId"].(string); ok {
				user.StaffID = v
			}
			msg.AtUsers = append(msg.AtUsers, user)
		}
	}
	// 顶层 content (所有非 text 类型的消息内容载体)
	if contentMap, ok := d["content"].(map[string]interface{}); ok {
		msg.Content = contentMap
	}
	switch msg.MessageType {
	case "text":
		if textMap, ok := d["text"].(map[string]interface{}); ok {
			if v, ok := textMap["content"].(string); ok {
				msg.TextContent = v
			}
		}
	case "picture":
		if v, ok := msg.Content["downloadCode"].(string); ok {
			msg.DownloadCode = v
		}
	case "richText":
		if rawList, ok := msg.Content["richText"].([]interface{}); ok {
			for _, item := range rawList {
				if itemMap, ok := item.(map[string]interface{}); ok {
					msg.RichText = append(msg.RichText, itemMap)
				}
			}
		}
	}
	return msg
}

// getString 从 map 中读取字符串字段 (兼容数字类型)。
func getString(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}
