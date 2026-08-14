package qqofficial_webhook

// Webhook 回调事件解析：将 QQ 官方开放平台的 webhook 消息载荷解析为 AstrBot 消息。
// 1:1 移植自 qqofficial_platform_adapter.py 的 _parse_from_qqofficial /
// _parse_face_message / _append_attachments / _normalize_attachment_url。

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// messageKind 标识 webhook 事件对应的 botpy 消息类型。
type messageKind string

const (
	kindGroup   messageKind = "group"   // GroupMessage（群消息）
	kindC2C     messageKind = "c2c"     // C2CMessage（C2C 单聊）
	kindChannel messageKind = "channel" // Message（频道消息）
	kindDirect  messageKind = "direct"  // DirectMessage（频道私聊）
)

// parseFromQQOfficial 将 webhook 载荷中的消息数据（payload["d"]）解析为 AstrBot 消息
// （对齐 Python _parse_from_qqofficial，kind 对应 Python 中的 isinstance 分支）。
func parseFromQQOfficial(d map[string]interface{}, msgType platform.MessageType, kind messageKind, forceGroupMention bool) *platform.AstrBotMessage {
	abm := platform.NewAstrBotMessage()
	abm.Type = msgType
	abm.Timestamp = time.Now().Unix()
	abm.RawMessage = d
	abm.MessageID, _ = d["id"].(string)

	msg := []message.Component{}

	// 引用消息（message_type == 103）
	messageReference, _ := d["message_reference"].(map[string]interface{})
	quotedMessageID := strOf(messageReference["message_id"])
	rawMessageType, _ := d["message_type"].(float64)
	isQuotedMessage := int(rawMessageType) == 103
	msgElements, _ := d["msg_elements"].([]interface{})
	quotedMessageStr := ""
	quotedElementMessageID := ""
	quotedChain := []message.Component{}
	if isQuotedMessage && len(msgElements) > 0 {
		if e, ok := msgElements[0].(map[string]interface{}); ok {
			quotedContent, _ := e["content"].(string)
			quotedAttachments, _ := e["attachments"].([]interface{})
			quotedElementMessageID = strOf(firstNonEmpty(e["id"], e["message_id"]))
			quotedMessageStr = parseFaceMessage(strings.TrimSpace(quotedContent))
			if quotedMessageStr != "" {
				quotedChain = append(quotedChain, &message.Plain{Text: quotedMessageStr})
			}
			quotedChain = append(quotedChain, appendAttachments(quotedAttachments)...)
		}
	}
	if quotedMessageID != "" || quotedElementMessageID != "" || len(quotedChain) > 0 {
		msg = append(msg, &message.Reply{
			MessageID:  strOf(firstNonEmpty(quotedMessageID, quotedElementMessageID)),
			Chain:      quotedChain,
			MessageStr: quotedMessageStr,
		})
	}

	switch kind {
	case kindGroup:
		// 群消息：发送者为 member_openid，处理 @机器人 提及
		author, _ := d["author"].(map[string]interface{})
		abm.Sender = platform.MessageMember{
			UserID:   strOf(author["member_openid"]),
			Nickname: strOf(author["username"]),
		}
		groupOpenID, _ := d["group_openid"].(string)
		abm.Group = &platform.Group{GroupID: groupOpenID}

		botMentions := []map[string]interface{}{}
		if mentions, ok := d["mentions"].([]interface{}); ok {
			for _, m := range mentions {
				mm, ok := m.(map[string]interface{})
				if !ok {
					continue
				}
				if isYou, _ := mm["is_you"].(bool); isYou && strOf(mm["id"]) != "" {
					botMentions = append(botMentions, mm)
				}
			}
		}
		botMentionIDs := []string{}
		for _, m := range botMentions {
			botMentionIDs = append(botMentionIDs, strOf(m["id"]))
		}
		groupMentioned := len(botMentionIDs) > 0 || forceGroupMention

		plainContentRaw, _ := d["content"].(string)
		for _, mentionID := range botMentionIDs {
			plainContentRaw = strings.ReplaceAll(plainContentRaw, "<@"+mentionID+">", "")
			plainContentRaw = strings.ReplaceAll(plainContentRaw, "<@!"+mentionID+">", "")
		}
		abm.MessageStr = parseFaceMessage(strings.TrimSpace(plainContentRaw))
		if len(botMentionIDs) > 0 {
			abm.SelfID = botMentionIDs[0]
		} else {
			abm.SelfID = "qq_official"
		}
		if groupMentioned {
			mentionName := ""
			if len(botMentions) > 0 {
				mentionName = strOf(botMentions[0]["username"])
			}
			msg = append(msg, &message.At{TargetID: abm.SelfID, Name: mentionName})
		}
		msg = append(msg, &message.Plain{Text: abm.MessageStr})
		msg = append(msg, appendAttachments(mapList(d["attachments"]))...)
		abm.Message = msg

	case kindC2C:
		// C2C 消息：发送者为 user_openid
		author, _ := d["author"].(map[string]interface{})
		abm.Sender = platform.MessageMember{
			UserID:   strOf(author["user_openid"]),
			Nickname: strOf(author["username"]),
		}
		content, _ := d["content"].(string)
		abm.MessageStr = parseFaceMessage(strings.TrimSpace(content))
		abm.SelfID = "unknown_selfid"
		msg = append(msg, &message.At{TargetID: "qq_official"})
		msg = append(msg, &message.Plain{Text: abm.MessageStr})
		msg = append(msg, appendAttachments(mapList(d["attachments"]))...)
		abm.Message = msg

	case kindChannel:
		// 频道消息：发送者为 author.id，self_id 取第一个提及
		author, _ := d["author"].(map[string]interface{})
		abm.Sender = platform.MessageMember{
			UserID:   strOf(author["id"]),
			Nickname: strOf(author["username"]),
		}
		mentions, _ := d["mentions"].([]interface{})
		if len(mentions) > 0 {
			if m, ok := mentions[0].(map[string]interface{}); ok {
				abm.SelfID = strOf(m["id"])
			}
		}
		content, _ := d["content"].(string)
		plainContent := parseFaceMessage(strings.TrimSpace(strings.ReplaceAll(content, "<@!"+abm.SelfID+">", "")))

		msg = append(msg, appendAttachments(mapList(d["attachments"]))...)
		abm.MessageStr = plainContent
		msg = append(msg, &message.At{TargetID: "qq_official"})
		msg = append(msg, &message.Plain{Text: plainContent})
		abm.Message = msg
		channelID, _ := d["channel_id"].(string)
		abm.Group = &platform.Group{GroupID: channelID}

	case kindDirect:
		// 频道私聊消息：发送者为 author.id，无 self_id
		author, _ := d["author"].(map[string]interface{})
		abm.Sender = platform.MessageMember{
			UserID:   strOf(author["id"]),
			Nickname: strOf(author["username"]),
		}
		content, _ := d["content"].(string)
		plainContent := parseFaceMessage(strings.TrimSpace(content))

		msg = append(msg, appendAttachments(mapList(d["attachments"]))...)
		abm.MessageStr = plainContent
		msg = append(msg, &message.At{TargetID: "qq_official"})
		msg = append(msg, &message.Plain{Text: plainContent})
		abm.Message = msg

	default:
		return abm
	}

	if abm.SelfID == "" {
		abm.SelfID = "qq_official"
	}
	return abm
}

// parseFaceMessage 将 QQ 官方面部表情标签转换为可读文本
// （对齐 Python _parse_face_message，与 qqofficial 适配器一致）。
func parseFaceMessage(content string) string {
	re := regexp.MustCompile(`<faceType=\d+[^>]*>`)
	return re.ReplaceAllStringFunc(content, func(tag string) string {
		extMatch := regexp.MustCompile(`ext="([^"]*)"`).FindStringSubmatch(tag)
		if len(extMatch) > 1 {
			if decoded, err := base64.StdEncoding.DecodeString(extMatch[1]); err == nil {
				var ext map[string]interface{}
				if json.Unmarshal(decoded, &ext) == nil {
					if text, ok := ext["text"].(string); ok && text != "" {
						return "[表情:" + text + "]"
					}
				}
			}
		}
		return "[表情]"
	})
}

// normalizeAttachmentURL 规范化附件 URL（无协议前缀时补 https://）。
func normalizeAttachmentURL(url string) string {
	if url == "" {
		return ""
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return url
	}
	return "https://" + url
}

// appendAttachments 将附件数组转换为消息组件
// （对齐 Python _append_attachments）。
func appendAttachments(attachments []interface{}) []message.Component {
	var out []message.Component
	for _, att := range attachments {
		a, ok := att.(map[string]interface{})
		if !ok {
			continue
		}
		contentType := strings.ToLower(strOf(firstNonEmpty(a["content_type"], a["contentType"])))
		url := normalizeAttachmentURL(strOf(a["url"]))
		filename := strOf(firstNonEmpty(a["filename"], a["name"]))
		if filename == "" {
			filename = "attachment"
		}
		if url == "" {
			continue
		}

		ext := strings.ToLower(filepath.Ext(filename))
		imageExts := map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".bmp": true}
		audioExts := map[string]bool{".mp3": true, ".wav": true, ".ogg": true, ".m4a": true, ".amr": true, ".silk": true}
		videoExts := map[string]bool{".mp4": true, ".mov": true, ".avi": true, ".mkv": true, ".webm": true}

		if strings.HasPrefix(contentType, "image") {
			out = append(out, message.ImageFromURL(url))
		} else if strings.HasPrefix(contentType, "voice") || audioExts[ext] {
			// Python 会下载并转换为 wav；Go 直接保留 URL
			out = append(out, &message.Record{URL: url})
		} else if strings.HasPrefix(contentType, "video") || videoExts[ext] {
			out = append(out, &message.Video{URL: url})
		} else if strings.HasPrefix(contentType, "image") || imageExts[ext] {
			out = append(out, message.ImageFromURL(url))
		} else {
			out = append(out, &message.File{Name: filename, URL: url})
		}
	}
	return out
}

// mapList 将 []interface{} 转换为 map 列表。
func mapList(v interface{}) []interface{} {
	if l, ok := v.([]interface{}); ok {
		return l
	}
	return nil
}

// strOf 将任意值转换为字符串（对齐 Python 的 str() 语义）。
func strOf(v interface{}) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	case float64:
		return strconv.FormatFloat(s, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(s), 'f', -1, 64)
	case int:
		return strconv.Itoa(s)
	case int64:
		return strconv.FormatInt(s, 10)
	case bool:
		return strconv.FormatBool(s)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// firstNonEmpty 返回第一个非空值（对齐 Python 的 `a or b` 语义：空串/0/false 视为假值）。
func firstNonEmpty(values ...interface{}) interface{} {
	for _, v := range values {
		if v == nil {
			continue
		}
		switch s := v.(type) {
		case string:
			if s != "" {
				return s
			}
		case float64:
			if s != 0 {
				return s
			}
		case float32:
			if s != 0 {
				return s
			}
		case int:
			if s != 0 {
				return s
			}
		case int64:
			if s != 0 {
				return s
			}
		case bool:
			if s {
				return s
			}
		default:
			return v
		}
	}
	return nil
}
