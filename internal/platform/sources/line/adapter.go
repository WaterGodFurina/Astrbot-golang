// Package line 实现 LINE Messaging API 平台适配器。
// 1:1 移植自 astrbot/core/platform/sources/line/：
//   - line_adapter.py（本文件）
//   - line_api.py（line_api.go）
//   - line_event.py（message.go）
//
// 使用官方 SDK github.com/line/line-bot-sdk-go/v8（v8.22.0）。
// LINE 固定使用统一 Webhook 模式（/api/v1/webhooks/platforms/{webhook_uuid}），
// 通过 dashboard 的 RegisterWebhook 注入回调。
package line

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/internal/utils"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// 事件去重窗口（30 分钟），对应 Python 的 _clean_expired_events。
const lineEventDedupWindow = 30 * time.Minute

// 回复令牌有效期（LINE replyToken 自事件起 1 分钟内有效，这里留 5 分钟余量）。
const replyTokenTTL = 5 * time.Minute

// Adapter 实现 LINE 平台适配器。
type Adapter struct {
	config   map[string]interface{}
	settings map[string]interface{}

	EventBus *core.EventBus

	lineAPI     *LineAPIClient
	destination string // webhook payload 中的 destination（LINE bot 用户 ID）

	webhookID string

	// replyTokens 记录最近收到的 replyToken（sessionID -> token），
	// 发送时优先使用回复接口，失败后回退到主动推送（对应 Python 的 send）。
	replyTokens map[string]replyTokenEntry
	rtMu        sync.Mutex

	// 事件去重（webhookEventId）
	evIDTime map[string]time.Time
	mu       sync.Mutex

	mediaBaseURL string
	stopCh       chan struct{}
}

// replyTokenEntry 记录 replyToken 及其接收时间。
type replyTokenEntry struct {
	token string
	at    time.Time
}

// New 创建 LINE 适配器（config 需要 channel_access_token 与 channel_secret）。
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		config:      config,
		settings:    settings,
		EventBus:    eventBus,
		replyTokens: map[string]replyTokenEntry{},
		evIDTime:    map[string]time.Time{},
		stopCh:      make(chan struct{}),
	}
	channelAccessToken, _ := config["channel_access_token"].(string)
	channelSecret, _ := config["channel_secret"].(string)
	if strings.TrimSpace(channelAccessToken) == "" || strings.TrimSpace(channelSecret) == "" {
		lineLogger.I18nError("LINE 适配器需要 channel_access_token 和 channel_secret。")
		return a
	}
	a.lineAPI, _ = NewLineAPIClient(channelAccessToken, channelSecret)
	a.webhookID, _ = config["webhook_uuid"].(string)
	return a
}

// SetEventBus 注入事件总线（实现 platform.EventBusSetter）。
func (a *Adapter) SetEventBus(bus platform.EventBus) {
	if eb, ok := bus.(*core.EventBus); ok {
		a.EventBus = eb
	}
}

// ID 返回适配器实例 ID。
func (a *Adapter) ID() string {
	if id, ok := a.config["id"].(string); ok {
		return id
	}
	return "line"
}

// Type 返回平台类型。
func (a *Adapter) Type() string { return "line" }

// Start 启动适配器。LINE 固定使用统一 Webhook 模式，只需记录 webhook 信息。
func (a *Adapter) Start(ctx context.Context) error {
	if a.webhookID != "" {
		lineLogger.I18nInfo("%s(LINE) 统一 Webhook 已启用, webhook_uuid=%s", a.ID(), a.webhookID)
	} else {
		lineLogger.I18nWarn("[LINE] webhook_uuid 为空，统一 Webhook 可能无法接收消息。")
	}
	return nil
}

// Stop 关闭适配器。
func (a *Adapter) Stop() error {
	close(a.stopCh)
	lineLogger.I18nInfo("LINE 适配器已关闭")
	return nil
}

// WebhookUUID 返回统一 Webhook 的 uuid。
func (a *Adapter) WebhookUUID() string { return a.webhookID }

// WebhookCallback 是统一 Webhook 的入口（/api/v1/webhooks/platforms/{uuid}）。
// 对应 Python 的 webhook_callback：校验 X-Line-Signature 签名。
func (a *Adapter) WebhookCallback(w http.ResponseWriter, r *http.Request) {
	if a.lineAPI == nil {
		http.Error(w, "LINE 适配器未初始化", http.StatusInternalServerError)
		return
	}
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return
	}
	signature := r.Header.Get("x-line-signature")
	if !a.lineAPI.VerifySignature(rawBody, signature) {
		lineLogger.I18nWarn("[LINE] invalid webhook signature")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("invalid signature"))
		return
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		lineLogger.I18nWarn("[LINE] invalid webhook body: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad request"))
		return
	}
	if payload == nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad request"))
		return
	}

	a.handleWebhookEvent(payload)
	_, _ = w.Write([]byte("ok"))
}

// handleWebhookEvent 处理整个 webhook payload（对应 Python 的 handle_webhook_event）。
func (a *Adapter) handleWebhookEvent(payload map[string]interface{}) {
	if destination, ok := payload["destination"].(string); ok {
		if d := strings.TrimSpace(destination); d != "" {
			a.destination = d
		}
	}

	rawEvents, ok := payload["events"].([]interface{})
	if !ok {
		return
	}
	for _, raw := range rawEvents {
		event, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		eventID := ""
		if id, ok := event["webhookEventId"].(string); ok {
			eventID = id
		}
		if eventID != "" && a.isDuplicateEvent(eventID) {
			lineLogger.Debug("[LINE] 重复事件已跳过: %s", eventID)
			continue
		}
		abm := a.convertMessage(event)
		if abm == nil {
			continue
		}
		a.handleMsg(abm)
	}
}

// convertMessage 将 LINE webhook 事件转换为 AstrBotMessage。
// 对应 Python 的 convert_message。
func (a *Adapter) convertMessage(event map[string]interface{}) *platform.AstrBotMessage {
	eventType, _ := event["type"].(string)
	if eventType != "message" {
		return nil
	}
	if mode, ok := event["mode"].(string); ok && mode == "standby" {
		return nil
	}

	source, ok := event["source"].(map[string]interface{})
	if !ok {
		return nil
	}
	msg, ok := event["message"].(map[string]interface{})
	if !ok {
		return nil
	}

	sourceType, _ := source["type"].(string)
	userID := strings.TrimSpace(fmt.Sprintf("%v", source["userId"]))
	groupID := strings.TrimSpace(fmt.Sprintf("%v", source["groupId"]))
	roomID := strings.TrimSpace(fmt.Sprintf("%v", source["roomId"]))

	abm := platform.NewAstrBotMessage()
	abm.SelfID = a.destination
	if abm.SelfID == "" {
		abm.SelfID = a.ID()
	}
	abm.Message = []message.Component{}
	abm.RawMessage = event

	// 消息 ID：优先 message.id，其次 webhookEventId、deliveryId
	msgID := fmt.Sprintf("%v", msg["id"])
	if msgID == "<nil>" || msgID == "" {
		msgID = event["webhookEventId"].(string)
	}
	if msgID == "" || msgID == "<nil>" {
		if dc, ok := event["deliveryContext"].(map[string]interface{}); ok {
			msgID = fmt.Sprintf("%v", dc["deliveryId"])
		}
	}
	if msgID == "" || msgID == "<nil>" {
		msgID = randomHex(16)
	}
	abm.MessageID = msgID

	// 时间戳：毫秒 → 秒
	if ts, ok := event["timestamp"].(float64); ok {
		ms := int64(ts)
		if ms > 1_000_000_000_000 {
			abm.Timestamp = ms / 1000
		} else {
			abm.Timestamp = ms
		}
	}

	switch sourceType {
	case "group", "room":
		abm.Type = platform.GroupMessage
		containerID := groupID
		if containerID == "" {
			containerID = roomID
		}
		abm.Group = &platform.Group{GroupID: containerID, GroupName: containerID}
		abm.SessionID = containerID
		abm.Sender = platform.MessageMember{UserID: userID, Nickname: userID}
		if abm.Sender.UserID == "" {
			abm.Sender.UserID = containerID
		}
		truncateNick(&abm.Sender)
	case "user":
		abm.Type = platform.FriendMessage
		abm.SessionID = userID
		abm.Sender = platform.MessageMember{UserID: userID, Nickname: userID}
		truncateNick(&abm.Sender)
	default:
		abm.Type = platform.OtherMessage
		sessionID := userID
		if sessionID == "" {
			sessionID = groupID
		}
		if sessionID == "" {
			sessionID = roomID
		}
		if sessionID == "" {
			sessionID = "unknown"
		}
		abm.SessionID = sessionID
		abm.Sender = platform.MessageMember{UserID: sessionID, Nickname: sessionID}
		truncateNick(&abm.Sender)
	}

	components := a.parseLineMessageComponents(msg)
	if len(components) == 0 {
		return nil
	}
	abm.Message = components
	abm.MessageStr = buildMessageStr(components)

	// 记录 replyToken 供发送时优先回复（对应 Python event.send 的 reply 逻辑）
	if replyToken, ok := event["replyToken"].(string); ok && replyToken != "" {
		a.rtMu.Lock()
		a.replyTokens[abm.SessionID] = replyTokenEntry{token: replyToken, at: time.Now()}
		a.rtMu.Unlock()
	}
	return abm
}

// truncateNick 将昵称截断为前 8 个字符（对应 Python 的 sender_id[:8]）。
func truncateNick(m *platform.MessageMember) {
	nick := m.UserID
	if len(nick) > 8 {
		nick = nick[:8]
	}
	m.Nickname = nick
}

// parseLineMessageComponents 解析 LINE 消息内容为消息组件。
// 对应 Python 的 _parse_line_message_components。
func (a *Adapter) parseLineMessageComponents(msg map[string]interface{}) []message.Component {
	msgType, _ := msg["type"].(string)
	messageID := strings.TrimSpace(fmt.Sprintf("%v", msg["id"]))

	switch msgType {
	case "text":
		text, _ := msg["text"].(string)
		if mention, ok := msg["mention"].(map[string]interface{}); ok {
			return parseTextWithMentions(text, mention)
		}
		if text != "" {
			return []message.Component{&message.Plain{Text: text}}
		}
		return nil
	case "image":
		if comp := a.buildImageComponent(messageID, msg); comp != nil {
			return []message.Component{comp}
		}
		return []message.Component{&message.Plain{Text: "[image]"}}
	case "video":
		if comp := a.buildVideoComponent(messageID, msg); comp != nil {
			return []message.Component{comp}
		}
		return []message.Component{&message.Plain{Text: "[video]"}}
	case "audio":
		if comp := a.buildAudioComponent(messageID, msg); comp != nil {
			return []message.Component{comp}
		}
		return []message.Component{&message.Plain{Text: "[audio]"}}
	case "file":
		if comp := a.buildFileComponent(messageID, msg); comp != nil {
			return []message.Component{comp}
		}
		return []message.Component{&message.Plain{Text: "[file]"}}
	case "sticker":
		return []message.Component{&message.Plain{Text: "[sticker]"}}
	default:
		return []message.Component{&message.Plain{Text: "[" + msgType + "]"}}
	}
}

// parseTextWithMentions 解析带 @ 提及的文本消息。
// 对应 Python 的 _parse_text_with_mentions：按 index 排序，切分文本与提及。
// 注意：LINE 的 index 按 Unicode 字符计数，Go 侧需用 []rune 切片（与 Python
// 按代码点切分一致，避免中文等多字节字符导致错位）。
func parseTextWithMentions(text string, mentionObj map[string]interface{}) []message.Component {
	rawMentions, ok := mentionObj["mentionees"].([]interface{})
	if !ok || len(rawMentions) == 0 {
		if text != "" {
			return []message.Component{&message.Plain{Text: text}}
		}
		return nil
	}

	type mentionItem struct {
		start  int
		length int
		item   map[string]interface{}
	}
	var normalized []mentionItem
	for _, raw := range rawMentions {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		start, ok1 := item["index"].(float64)
		length, ok2 := item["length"].(float64)
		if !ok1 || !ok2 {
			continue
		}
		normalized = append(normalized, mentionItem{start: int(start), length: int(length), item: item})
	}
	// 按起始位置排序
	for i := 1; i < len(normalized); i++ {
		for j := i; j > 0 && normalized[j].start < normalized[j-1].start; j-- {
			normalized[j], normalized[j-1] = normalized[j-1], normalized[j]
		}
	}

	runes := []rune(text)
	textLen := len(runes)
	var ret []message.Component
	cursor := 0
	for _, m := range normalized {
		if m.start > cursor {
			if part := string(runes[cursor:m.start]); part != "" {
				ret = append(ret, &message.Plain{Text: part})
			}
		}
		label := ""
		if m.start+m.length <= textLen {
			label = string(runes[m.start : m.start+m.length])
		}
		if label == "" {
			label = "@user"
		}
		mentionType, _ := m.item["type"].(string)
		if mentionType == "user" {
			targetID := strings.TrimSpace(fmt.Sprintf("%v", m.item["userId"]))
			ret = append(ret, &message.At{TargetID: targetID, Name: strings.TrimPrefix(label, "@")})
		} else {
			ret = append(ret, &message.Plain{Text: label})
		}
		if m.start+m.length > cursor {
			cursor = m.start + m.length
		}
	}
	if cursor < textLen {
		if tail := string(runes[cursor:]); tail != "" {
			ret = append(ret, &message.Plain{Text: tail})
		}
	}
	return ret
}

// buildImageComponent 构建图片组件。
// 对应 Python 的 _build_image_component：优先外部 URL，否则下载内容。
func (a *Adapter) buildImageComponent(messageID string, msg map[string]interface{}) *message.Image {
	if externalURL := getExternalContentURL(msg); externalURL != "" {
		return &message.Image{URL: externalURL}
	}
	content, err := a.lineAPI.GetMessageContent(context.Background(), messageID)
	if err != nil || content == nil {
		return nil
	}
	return &message.Image{Base64: utils.BytesToBase64(content.Content)}
}

// buildVideoComponent 构建视频组件。
// 对应 Python 的 _build_video_component：优先外部 URL，否则下载内容存临时文件。
func (a *Adapter) buildVideoComponent(messageID string, msg map[string]interface{}) *message.Video {
	if externalURL := getExternalContentURL(msg); externalURL != "" {
		return &message.Video{URL: externalURL}
	}
	content, err := a.lineAPI.GetMessageContent(context.Background(), messageID)
	if err != nil || content == nil {
		return nil
	}
	suffix := guessSuffix(content.ContentType, ".mp4")
	filePath := storeTempContent("video", messageID, content.Content, suffix, "")
	return &message.Video{Path: filePath}
}

// buildAudioComponent 构建语音组件。
// 对应 Python 的 _build_audio_component：下载内容存临时文件（m4a）。
func (a *Adapter) buildAudioComponent(messageID string, msg map[string]interface{}) *message.Record {
	if externalURL := getExternalContentURL(msg); externalURL != "" {
		return &message.Record{URL: externalURL, Path: externalURL}
	}
	content, err := a.lineAPI.GetMessageContent(context.Background(), messageID)
	if err != nil || content == nil {
		return nil
	}
	suffix := guessSuffix(content.ContentType, ".m4a")
	filePath := storeTempContent("audio", messageID, content.Content, suffix, "")
	return &message.Record{File: filePath, Path: filePath}
}

// buildFileComponent 构建文件组件。
// 对应 Python 的 _build_file_component：下载内容，文件名为
// Content-Disposition 提取名或消息 fileName。
func (a *Adapter) buildFileComponent(messageID string, msg map[string]interface{}) *message.File {
	content, err := a.lineAPI.GetMessageContent(context.Background(), messageID)
	if err != nil || content == nil {
		return nil
	}
	defaultName := strings.TrimSpace(fmt.Sprintf("%v", msg["fileName"]))
	if defaultName == "" {
		defaultName = messageID + ".bin"
	}
	suffix := filepath.Ext(defaultName)
	if suffix == "" {
		suffix = guessSuffix(content.ContentType, ".bin")
	}
	finalName := content.Filename
	if finalName == "" {
		finalName = defaultName
	}
	filePath := storeTempContent("file", messageID, content.Content, suffix, finalName)
	return &message.File{Name: finalName, Path: filePath, URL: filePath}
}

// getExternalContentURL 获取消息的 contentProvider 外部 URL。
// 对应 Python 的 _get_external_content_url。
func getExternalContentURL(msg map[string]interface{}) string {
	provider, ok := msg["contentProvider"].(map[string]interface{})
	if !ok {
		return ""
	}
	if ptype, _ := provider["type"].(string); ptype != "external" {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("%v", provider["originalContentUrl"]))
}

// storeTempContent 把内容写入临时目录并返回文件路径。
// 对应 Python 的 _store_temp_content：文件名前缀含内容类型与消息 ID。
func storeTempContent(contentType, messageID string, content []byte, suffix, originalName string) string {
	tempDir := os.TempDir()
	namePrefix := "line_" + contentType
	if originalName != "" {
		safeStem := sanitizeStem(filepath.Base(originalName))
		if safeStem != "" {
			if len(safeStem) > 64 {
				safeStem = safeStem[:64]
			}
			namePrefix = safeStem
		}
	}
	fileName := fmt.Sprintf("%s_%s_%s%s", namePrefix, messageID, randomHex(6), suffix)
	filePath := filepath.Join(tempDir, fileName)
	if err := os.MkdirAll(tempDir, 0o755); err == nil {
		_ = os.WriteFile(filePath, content, 0o644)
	}
	if resolved, err := filepath.Abs(filePath); err == nil {
		return resolved
	}
	return filePath
}

// sanitizeStem 将文件名净化：只保留字母数字与 - _ .，去掉首尾的点下划线。
// 对应 Python 的 safe_stem 处理。
func sanitizeStem(name string) string {
	var b strings.Builder
	for _, ch := range name {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' ||
			ch == '-' || ch == '_' || ch == '.' {
			b.WriteRune(ch)
		} else {
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), "._")
}

// buildMessageStr 将组件列表转换为纯文本摘要。
// 对应 Python 的 _build_message_str。
func buildMessageStr(components []message.Component) string {
	var parts []string
	for _, comp := range components {
		switch c := comp.(type) {
		case *message.Plain:
			parts = append(parts, c.Text)
		case *message.At:
			name := c.Name
			if name == "" {
				name = c.TargetID
			}
			parts = append(parts, "@"+name)
		case *message.Image:
			parts = append(parts, "[image]")
		case *message.Video:
			parts = append(parts, "[video]")
		case *message.Record:
			parts = append(parts, "[audio]")
		case *message.File:
			name := c.Name
			if name == "" {
				name = "[file]"
			}
			parts = append(parts, name)
		default:
			parts = append(parts, "["+string(comp.Type())+"]")
		}
	}
	var cleaned []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			cleaned = append(cleaned, strings.TrimSpace(p))
		}
	}
	return strings.Join(cleaned, " ")
}

// isDuplicateEvent 判断事件是否重复（30 分钟窗口）。
// 对应 Python 的 _is_duplicate_event / _clean_expired_events。
func (a *Adapter) isDuplicateEvent(eventID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for id, ts := range a.evIDTime {
		if now.Sub(ts) > lineEventDedupWindow {
			delete(a.evIDTime, id)
		}
	}
	if _, ok := a.evIDTime[eventID]; ok {
		return true
	}
	a.evIDTime[eventID] = now
	return false
}

// handleMsg 将消息发布到事件总线（对应 Python 的 handle_msg/commit_event）。
func (a *Adapter) handleMsg(abm *platform.AstrBotMessage) {
	if a.EventBus == nil {
		return
	}
	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   "line",
			SelfID:     abm.SelfID,
			SenderID:   abm.Sender.UserID,
			SenderName: abm.Sender.Nickname,
			ConvID:     abm.SessionID,
			IsGroup:    abm.Type == platform.GroupMessage,
		},
		Message:    &message.MessageChain{Chain: abm.Message},
		MessageStr: abm.MessageStr,
		Timestamp:  time.Unix(abm.Timestamp, 0),
		MessageObj: &core.MessageObj{
			MessageID: abm.MessageID,
			SelfID:    abm.SelfID,
			SessionID: abm.SessionID,
			Platform:  "line",
		},
		Metadata: map[string]interface{}{},
	}
	if err := a.EventBus.Publish(event); err != nil {
		lineLogger.I18nError("发布事件失败: %v", err)
	}
}

// Send 向会话发送消息链。
// 对应 Python 的 send_by_session：优先用 replyToken 回复，失败则主动推送。
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	if a.lineAPI == nil {
		return fmt.Errorf("line: 适配器未初始化（缺少 channel_access_token/channel_secret）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	messages, err := a.buildLineMessages(ctx, chain)
	if err != nil {
		return err
	}
	if len(messages) == 0 {
		return nil
	}

	sent := false
	if replyToken := a.takeReplyToken(sessionID); replyToken != "" {
		sent = a.lineAPI.ReplyMessage(ctx, replyToken, messages)
	}
	if !sent {
		targetID := sessionID
		if targetID == "" {
			return fmt.Errorf("line: 会话 ID 为空，无法发送消息")
		}
		a.lineAPI.PushMessage(ctx, targetID, messages)
	}
	return nil
}

// takeReplyToken 取出（并清理）会话对应的未过期 replyToken。
func (a *Adapter) takeReplyToken(sessionID string) string {
	a.rtMu.Lock()
	defer a.rtMu.Unlock()
	entry, ok := a.replyTokens[sessionID]
	if !ok {
		return ""
	}
	if time.Since(entry.at) > replyTokenTTL {
		delete(a.replyTokens, sessionID)
		return ""
	}
	delete(a.replyTokens, sessionID)
	return entry.token
}
