// 企业微信（WeCom）回调消息解析与事件转换。
// 1:1 移植自 wecom_event.py 的解析逻辑与 wechatpy.enterprise 的 parse_message。
package wecom

import (
	"encoding/xml"
	"net/url"
	"path"
	"strings"
	"time"
)

// WecomMessage 企业微信回调消息（对应 wechatpy 的 BaseMessage 及其子类）。
type WecomMessage struct {
	// Type 消息类型：text / image / voice / event / unknown ...
	Type string
	// Source 发送者（FromUserName）
	Source string
	// ID 消息 ID（MsgId）
	ID string
	// Time 消息时间戳（CreateTime，秒）
	Time int64
	// Agent 应用 ID（AgentID）
	Agent string
	// ToUserName 接收者
	ToUserName string

	// 文本消息内容
	Content string
	// 图片消息：图片 URL 与 MediaID
	PicURL string
	MediaID string
	// 语音消息格式
	Format string

	// 事件
	Event string
	// 客服事件：Token 与 OpenKfId
	Token   string
	OpenKfID string

	// Raw 原始 XML 解析后的数据（raw_message）
	Raw map[string]interface{}
}

// wecomXML 与回调 XML 一一对应的解析结构（CDATA 自动处理）。
type wecomXML struct {
	XMLName      xml.Name `xml:"xml"`
	ToUserName   string   `xml:"ToUserName"`
	FromUserName string   `xml:"FromUserName"`
	CreateTime   int64    `xml:"CreateTime"`
	MsgType      string   `xml:"MsgType"`
	Content      string   `xml:"Content"`
	PicURL       string   `xml:"PicUrl"`
	MediaID      string   `xml:"MediaId"`
	Format       string   `xml:"Format"`
	MsgID        string   `xml:"MsgId"`
	AgentID      string   `xml:"AgentID"`
	Event        string   `xml:"Event"`
	Token        string   `xml:"Token"`
	OpenKFID     string   `xml:"OpenKfId"`
}

// ParseWecomMessage 解析企业微信回调 XML（对应 Python parse_message）。
func ParseWecomMessage(xmlData string) (*WecomMessage, error) {
	var raw wecomXML
	if err := xml.Unmarshal([]byte(xmlData), &raw); err != nil {
		return nil, err
	}
	msg := &WecomMessage{
		Type:       raw.MsgType,
		Source:     raw.FromUserName,
		ID:         raw.MsgID,
		Time:       raw.CreateTime,
		Agent:      raw.AgentID,
		ToUserName: raw.ToUserName,
		Content:    raw.Content,
		PicURL:     raw.PicURL,
		MediaID:    raw.MediaID,
		Format:     raw.Format,
		Event:      raw.Event,
		Token:      raw.Token,
		OpenKfID:   raw.OpenKFID,
		Raw: map[string]interface{}{
			"ToUserName":   raw.ToUserName,
			"FromUserName": raw.FromUserName,
			"CreateTime":   raw.CreateTime,
			"MsgType":      raw.MsgType,
			"Content":      raw.Content,
			"PicUrl":       raw.PicURL,
			"MediaId":      raw.MediaID,
			"Format":       raw.Format,
			"MsgId":        raw.MsgID,
			"AgentID":      raw.AgentID,
			"Event":        raw.Event,
			"Token":        raw.Token,
			"OpenKfId":     raw.OpenKFID,
		},
	}
	if msg.Type == "" {
		msg.Type = "unknown"
	}
	return msg, nil
}

// ExtractWecomMediaFilename 从 Content-Disposition 中提取企业微信素材文件名
// （1:1 移植 wecom_adapter.py 的 _extract_wecom_media_filename）。
func ExtractWecomMediaFilename(disposition string) string {
	if disposition == "" {
		return ""
	}
	for _, part := range strings.Split(disposition, ";") {
		token := strings.TrimSpace(part)
		tokenLower := strings.ToLower(token)
		if strings.HasPrefix(tokenLower, "filename*=") {
			value := strings.Trim(strings.SplitN(token, "=", 2)[1], `"`)
			value = strings.TrimSpace(value)
			if strings.HasPrefix(strings.ToLower(value), "utf-8''") {
				value = value[7:]
			}
			if decoded, err := url.QueryUnescape(value); err == nil {
				value = decoded
			}
			name := path.Base(strings.ReplaceAll(value, "\\", "/"))
			if name != "" && name != "." && name != "/" {
				return name
			}
			return ""
		}
		if strings.HasPrefix(tokenLower, "filename=") {
			value := strings.Trim(strings.SplitN(token, "=", 2)[1], `"`)
			value = strings.TrimSpace(value)
			name := path.Base(strings.ReplaceAll(value, "\\", "/"))
			if name != "" && name != "." && name != "/" {
				return name
			}
			return ""
		}
	}
	return ""
}

// SplitPlain 将长文本分割成多个不超过 2048 字符的小文本，优先在标点符号处分割
// （1:1 移植 wecom_event.py 的 split_plain，按字符计数）。
func SplitPlain(plain string) []string {
	const maxLen = 2048
	runes := []rune(plain)
	if len(runes) <= maxLen {
		return []string{plain}
	}
	var result []string
	start := 0
	for start < len(runes) {
		// 剩下的字符串长度<2048时结束
		if start+maxLen >= len(runes) {
			result = append(result, string(runes[start:]))
			break
		}

		// 向前搜索分割标点符号
		end := start + maxLen
		cutPosition := end
		for i := end; i > start; i-- {
			if strings.ContainsRune("。！？.!?\n;；", runes[i-1]) {
				cutPosition = i
				break
			}
		}

		// 没找到合适的位置分割, 直接切分
		if cutPosition == end && end < len(runes) {
			cutPosition = end
		}

		result = append(result, string(runes[start:cutPosition]))
		start = cutPosition
	}
	return result
}

// IsKFMsgOrEvent 判断是否为微信客服消息回调事件（kf_msg_or_event）。
// Python 侧 wechatpy 将 kf_msg_or_event 解析为 UnknownMessage（type="unknown"），
// 这里以 MsgType=event + Event=kf_msg_or_event 等价判断。
func (m *WecomMessage) IsKFMsgOrEvent() bool {
	return m.Type == "event" && m.Event == "kf_msg_or_event"
}

// nowUnix 供内部使用（便于测试替换）。
func nowUnix() int64 { return time.Now().Unix() }
