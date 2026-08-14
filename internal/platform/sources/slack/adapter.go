// Package slack 实现 Slack 平台适配器，支持 Socket Mode 与 Webhook 两种连接模式。
// 1:1 移植自 astrbot/core/platform/sources/slack/：
//   - slack_adapter.py（本文件）
//   - client.py（client.go）
//   - slack_event.py（message.go）
//
// 使用官方 SDK github.com/slack-go/slack（v0.27.0）：
//   - Socket Mode：github.com/slack-go/slack/socketmode（WebSocket 长连接）
//   - 发送/上传/React：chat.postMessage / files.upload / reactions.add
package slack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

var logger = log.GetDefault().WithComponent("Slack")

// Adapter 实现 Slack 平台适配器。
type Adapter struct {
	config   map[string]interface{}
	settings map[string]interface{}

	EventBus *core.EventBus

	botToken     string
	appToken     string
	signingSecret string

	connectionMode    string // "socket" | "webhook"
	unifiedWebhookMode bool
	webhookHost       string
	webhookPort       int
	webhookPath       string
	webhookUUID       string

	client    *slack.Client
	socket    *socketmode.Client
	socketCancel context.CancelFunc
	webhook   *SlackWebhookServer
	botSelfID string

	stopCh chan struct{}
	mu     sync.Mutex
}

// New 创建 Slack 适配器。
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		config:   config,
		settings: settings,
		EventBus: eventBus,
		stopCh:   make(chan struct{}),
	}
	a.botToken, _ = config["bot_token"].(string)
	a.appToken, _ = config["app_token"].(string)
	a.signingSecret, _ = config["signing_secret"].(string)
	a.connectionMode, _ = config["slack_connection_mode"].(string)
	if a.connectionMode == "" {
		a.connectionMode = "socket"
	}
	a.unifiedWebhookMode, _ = config["unified_webhook_mode"].(bool)
	a.webhookHost, _ = config["slack_webhook_host"].(string)
	if a.webhookHost == "" {
		a.webhookHost = "0.0.0.0"
	}
	if port, ok := config["slack_webhook_port"].(float64); ok {
		a.webhookPort = int(port)
	}
	if a.webhookPort == 0 {
		a.webhookPort = 3000
	}
	a.webhookPath, _ = config["slack_webhook_path"].(string)
	if a.webhookPath == "" {
		a.webhookPath = "/astrbot-slack-webhook/callback"
	}
	a.webhookUUID, _ = config["webhook_uuid"].(string)

	if strings.TrimSpace(a.botToken) == "" {
		logger.I18nError("Slack bot_token 是必需的")
		return a
	}
	if a.connectionMode == "socket" && strings.TrimSpace(a.appToken) == "" {
		logger.I18nError("Socket Mode 需要 app_token")
		return a
	}
	if a.connectionMode == "webhook" && strings.TrimSpace(a.signingSecret) == "" {
		logger.I18nError("Webhook Mode 需要 signing_secret")
		return a
	}

	a.client = slack.New(a.botToken)
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
	return "slack"
}

// Type 返回平台类型。
func (a *Adapter) Type() string { return "slack" }

// Start 启动适配器：先做 auth_test 获取机器人自身 ID，再按连接模式启动。
// 对应 Python 的 run()。
func (a *Adapter) Start(ctx context.Context) error {
	if a.client == nil {
		return fmt.Errorf("slack: 适配器未初始化（缺少 bot_token）")
	}
	// 获取机器人自身用户 ID（auth_test）
	if resp, err := a.client.AuthTestContext(ctx); err == nil {
		a.botSelfID = resp.UserID
		logger.I18nInfo("Slack auth test OK. Bot ID: %s", a.botSelfID)
	} else {
		logger.I18nWarn("Slack auth test 失败: %v", err)
	}

	switch a.connectionMode {
	case "socket":
		return a.startSocketMode(ctx)
	case "webhook":
		return a.startWebhookMode(ctx)
	default:
		return fmt.Errorf("不支持的连接模式: %s，请使用 'socket' 或 'webhook'", a.connectionMode)
	}
}

// socketLogAdapter 把 AstrBot 日志适配为 slack-go socketmode 的 logger 接口。
type socketLogAdapter struct{}

// Output 实现 socketmode.logger 接口。
func (socketLogAdapter) Output(_ int, s string) error {
	logger.Debug("[socketmode] %s", s)
	return nil
}

// startSocketMode 启动 Socket Mode 客户端（对应 Python SlackSocketClient）。
func (a *Adapter) startSocketMode(ctx context.Context) error {
	socketCtx, cancel := context.WithCancel(ctx)
	a.socketCancel = cancel
	a.socket = socketmode.New(a.client,
		socketmode.OptionLog(socketLogAdapter{}),
	)
	logger.I18nInfo("Slack 适配器 (Socket Mode) 启动中...")

	go func() {
		if err := a.socket.RunContext(socketCtx); err != nil {
			logger.I18nWarn("Slack Socket Mode 连接退出: %v", err)
		}
	}()
	go a.socketEventLoop(socketCtx)
	return nil
}

// socketEventLoop 消费 Socket Mode 事件（对应 Python _handle_events）。
func (a *Adapter) socketEventLoop(ctx context.Context) {
	for {
		select {
		case <-a.stopCh:
			return
		case <-ctx.Done():
			return
		case evt, ok := <-a.socket.Events:
			if !ok {
				return
			}
			a.handleSocketModeEvent(ctx, evt)
		}
	}
}

// handleSocketModeEvent 处理单个 Socket Mode 事件。
func (a *Adapter) handleSocketModeEvent(ctx context.Context, evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeEventsAPI:
		req := evt.Request
		if req == nil {
			return
		}
		// 确认收到事件（对应 Python 的 send_socket_mode_response）
		if err := a.socket.SendCtx(ctx, socketmode.Response{EnvelopeID: req.EnvelopeID}); err != nil {
			logger.I18nWarn("发送 Socket Mode 确认失败: %v", err)
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(req.Payload, &payload); err != nil {
			logger.I18nWarn("解析 Socket Mode 事件失败: %v", err)
			return
		}
		a.handleSocketEvent(payload)
	case socketmode.EventTypeConnected:
		logger.I18nInfo("Slack Socket Mode 已连接")
	case socketmode.EventTypeConnectionError, socketmode.EventTypeErrorBadMessage,
		socketmode.EventTypeErrorWriteFailed, socketmode.EventTypeIncomingError:
		logger.I18nWarn("Slack Socket Mode 连接异常: %v", evt.Data)
	}
}

// handleSocketEvent 处理 Socket Mode 的 events_api 事件负载。
// 对应 Python 的 _handle_socket_event。
func (a *Adapter) handleSocketEvent(payload map[string]interface{}) {
	rawEvent, ok := payload["event"].(map[string]interface{})
	if !ok {
		return
	}
	a.processIncomingEvent(rawEvent)
}

// handleWebhookEvent 处理 Webhook 模式的 event_callback 事件。
// 对应 Python 的 _handle_webhook_event。
func (a *Adapter) handleWebhookEvent(eventData map[string]interface{}) {
	rawEvent, ok := eventData["event"].(map[string]interface{})
	if !ok {
		return
	}
	a.processIncomingEvent(rawEvent)
}

// processIncomingEvent 过滤并处理 Slack 事件。
// 忽略机器人自己的消息、消息编辑/删除（对应 Python 中的公共过滤逻辑）。
func (a *Adapter) processIncomingEvent(event map[string]interface{}) {
	// 忽略机器人自己的消息和消息编辑
	subtype, _ := event["subtype"].(string)
	switch subtype {
	case "bot_message", "message_changed", "message_deleted":
		return
	}
	if botID, _ := event["bot_id"].(string); botID != "" {
		return
	}
	eventType, _ := event["type"].(string)
	if eventType == "message" || eventType == "app_mention" {
		abm := a.convertMessage(event)
		if abm != nil {
			a.handleMsg(abm)
		}
	}
}

// startWebhookMode 启动 Webhook 模式（对应 Python run() 的 webhook 分支）。
func (a *Adapter) startWebhookMode(ctx context.Context) error {
	a.webhook = NewSlackWebhookServer(a.signingSecret, a.webhookPath, a.handleWebhookEvent)

	// 统一 webhook 模式：不启动独立服务器，等待 dashboard 回调注入
	if a.unifiedWebhookMode && a.webhookUUID != "" {
		logger.I18nInfo("%s(Slack) 统一 Webhook 已启用, webhook_uuid=%s", a.ID(), a.webhookUUID)
		<-ctx.Done()
		return nil
	}
	logger.I18nInfo("Slack 适配器 (Webhook Mode) 启动中，监听 %s:%d%s...", a.webhookHost, a.webhookPort, a.webhookPath)
	return a.webhook.Start(ctx, a.webhookHost, a.webhookPort)
}

// Stop 关闭适配器。
func (a *Adapter) Stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	select {
	case <-a.stopCh:
	default:
		close(a.stopCh)
	}
	if a.socket != nil {
		if a.socketCancel != nil {
			a.socketCancel()
		}
	}
	if a.webhook != nil {
		a.webhook.Stop()
	}
	logger.I18nInfo("Slack 适配器已被关闭")
	return nil
}

// WebhookUUID 返回统一 Webhook 的 uuid（仅 webhook 模式启用）。
func (a *Adapter) WebhookUUID() string {
	if a.connectionMode != "webhook" || a.webhookUUID == "" {
		return ""
	}
	return a.webhookUUID
}

// WebhookCallback 是统一 Webhook 的入口（/api/v1/webhooks/platforms/{uuid}）。
// 对应 Python 的 webhook_callback。
func (a *Adapter) WebhookCallback(w http.ResponseWriter, r *http.Request) {
	if a.connectionMode != "webhook" || a.webhook == nil {
		writeJSONError(w, http.StatusBadRequest, map[string]interface{}{
			"error": "Slack adapter is not in webhook mode",
		})
		return
	}
	a.webhook.HandleCallback(w, r)
}

// convertMessage 将 Slack 事件转换为 AstrBotMessage。
// 对应 Python 的 convert_message。
func (a *Adapter) convertMessage(event map[string]interface{}) *platform.AstrBotMessage {
	logger.Debug("[slack] RawMessage %v", event)

	abm := platform.NewAstrBotMessage()
	abm.SelfID = a.botSelfID
	abm.Message = []message.Component{}

	// 获取用户信息
	userID, _ := event["user"].(string)
	userName := a.fetchUserName(context.Background(), userID)
	abm.Sender = platform.MessageMember{UserID: userID, Nickname: userName}

	// 判断消息类型（群组/私聊）
	channelID, _ := event["channel"].(string)
	abm.Type = platform.GroupMessage
	if !a.isIMChannel(context.Background(), channelID) {
		abm.Group = &platform.Group{GroupID: channelID, GroupName: channelID}
		abm.SessionID = channelID
	} else {
		abm.Type = platform.FriendMessage
		abm.SessionID = userID
	}

	// 消息 ID 与时间戳
	abm.MessageID, _ = event["client_msg_id"].(string)
	if abm.MessageID == "" {
		abm.MessageID = randomUUIDHex()
	}
	if ts, ok := event["ts"].(string); ok {
		if f, err := strconv.ParseFloat(ts, 64); err == nil {
			abm.Timestamp = int64(f)
		}
	}

	// 处理消息内容
	messageText, _ := event["text"].(string)
	abm.MessageStr = messageText

	// 优先使用 blocks 字段解析消息
	if blocks, ok := event["blocks"].([]interface{}); ok {
		abm.Message = parseBlocks(blocks)
		// 更新 message_str（仅拼接 Plain 文本）
		var sb strings.Builder
		for _, comp := range abm.Message {
			if p, ok := comp.(*message.Plain); ok {
				sb.WriteString(p.Text)
			}
		}
		abm.MessageStr = sb.String()
	} else if messageText != "" {
		// 处理传统文本消息（<@USER> 提及）
		if strings.Contains(messageText, "<@") {
			mentions := mentionRegex.FindAllStringSubmatch(messageText, -1)
			seen := map[string]bool{}
			for _, m := range mentions {
				mid := m[1]
				if seen[mid] {
					continue
				}
				seen[mid] = true
				name := a.fetchUserName(context.Background(), mid)
				abm.Message = append(abm.Message, &message.At{TargetID: mid, Name: name})
			}
			// 清理消息文本中的 @ 标记
			if cleanText := strings.TrimSpace(mentionRegex.ReplaceAllString(messageText, "")); cleanText != "" {
				abm.Message = append(abm.Message, &message.Plain{Text: cleanText})
			}
		} else {
			abm.Message = append(abm.Message, &message.Plain{Text: messageText})
		}
	}

	// 处理文件附件
	if files, ok := event["files"].([]interface{}); ok {
		for _, raw := range files {
			fileInfo, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			fileName, _ := fileInfo["name"].(string)
			if fileName == "" {
				fileName = "unknown"
			}
			fileURL, _ := fileInfo["url_private"].(string)
			mimetype, _ := fileInfo["mimetype"].(string)
			if strings.HasPrefix(mimetype, "image/") {
				if b64, err := a.getFileBase64(context.Background(), fileURL); err == nil {
					abm.Message = append(abm.Message, &message.Image{Base64: b64})
				}
			} else {
				// TODO: 下载鉴权（与 Python 一致，仅透传私有 URL）
				abm.Message = append(abm.Message, &message.File{Name: fileName, URL: fileURL})
			}
		}
	}

	abm.RawMessage = event
	return abm
}

// mentionRegex 匹配 <@USER_ID> 提及格式。
var mentionRegex = regexp.MustCompile(`<@([^>]+)>`)

// fetchUserName 获取用户昵称（users.info），失败时回退为 user_id。
// 对应 Python convert_message 中的 users_info 调用。
func (a *Adapter) fetchUserName(ctx context.Context, userID string) string {
	if userID == "" {
		return userID
	}
	if a.client == nil {
		return userID
	}
	users, err := a.client.GetUsersInfoContext(ctx, userID)
	if err != nil || users == nil || len(*users) == 0 {
		return userID
	}
	user := (*users)[0]
	if user.RealName != "" {
		return user.RealName
	}
	if user.Name != "" {
		return user.Name
	}
	return userID
}

// isIMChannel 判断频道是否为私聊（conversations.info 的 is_im）。
// 对应 Python convert_message 中的 conversations_info 调用。
func (a *Adapter) isIMChannel(ctx context.Context, channelID string) bool {
	if channelID == "" || a.client == nil {
		return false
	}
	info, err := a.client.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{
		ChannelID: channelID,
	})
	if err != nil || info == nil {
		return false
	}
	return info.IsIM
}

// getFileBase64 下载 Slack 文件并返回 Base64 编码的内容。
// 对应 Python 的 get_file_base64（Authorization: Bearer 鉴权）。
func (a *Adapter) getFileBase64(ctx context.Context, url string) (string, error) {
	if url == "" {
		return "", fmt.Errorf("文件 URL 为空")
	}
	var buf bytes.Buffer
	if err := a.client.GetFileContext(ctx, url, &buf); err != nil {
		logger.I18nError("下载 Slack 文件失败: %v", err)
		return "", fmt.Errorf("下载文件失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// parseBlocks 解析 Slack blocks 格式的消息内容。
// 对应 Python 的 _parse_blocks。
func parseBlocks(blocks []interface{}) []message.Component {
	var components []message.Component

	for _, raw := range blocks {
		block, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		blockType, _ := block["type"].(string)

		switch blockType {
		case "rich_text":
			elements, _ := block["elements"].([]interface{})
			for _, rawElement := range elements {
				element, ok := rawElement.(map[string]interface{})
				if !ok {
					continue
				}
				elementType, _ := element["type"].(string)
				switch elementType {
				case "rich_text_section":
					components = append(components, parseRichTextSection(element)...)
				case "rich_text_list":
					listItems, _ := element["elements"].([]interface{})
					var listText strings.Builder
					for _, rawItem := range listItems {
						item, ok := rawItem.(map[string]interface{})
						if !ok {
							continue
						}
						if itemType, _ := item["type"].(string); itemType != "rich_text_section" {
							continue
						}
						itemElements, _ := item["elements"].([]interface{})
						var itemText strings.Builder
						for _, rawItemElement := range itemElements {
							itemElement, ok := rawItemElement.(map[string]interface{})
							if !ok {
								continue
							}
							if itemElementType, _ := itemElement["type"].(string); itemElementType == "text" {
								itemText.WriteString(fmt.Sprintf("%v", itemElement["text"]))
							}
						}
						if itemText.Len() > 0 {
							listText.WriteString("• " + itemText.String() + "\n")
						}
					}
					if text := strings.TrimSpace(listText.String()); text != "" {
						components = append(components, &message.Plain{Text: text})
					}
				}
			}
		case "section":
			if textObj, ok := block["text"].(map[string]interface{}); ok {
				if textType, _ := textObj["type"].(string); textType == "mrkdwn" {
					textContent, _ := textObj["text"].(string)
					components = append(components, &message.Plain{Text: textContent})
				}
			}
		}
	}
	return components
}

// parseRichTextSection 处理富文本段落。
// 对应 Python 的 _parse_blocks 中 rich_text_section 分支。
func parseRichTextSection(section map[string]interface{}) []message.Component {
	var components []message.Component
	sectionElements, _ := section["elements"].([]interface{})
	var textParts []string
	flushText := func() {
		textContent := strings.Join(textParts, "")
		if strings.TrimSpace(textContent) != "" {
			components = append(components, &message.Plain{Text: textContent})
		}
		textParts = nil
	}
	for _, rawSectionElement := range sectionElements {
		sectionElement, ok := rawSectionElement.(map[string]interface{})
		if !ok {
			continue
		}
		elementType, _ := sectionElement["type"].(string)
		switch elementType {
		case "text":
			// 普通文本
			textParts = append(textParts, fmt.Sprintf("%v", sectionElement["text"]))
		case "user":
			// @用户提及
			userID, _ := sectionElement["user_id"].(string)
			if userID != "" {
				flushText()
				components = append(components, &message.At{TargetID: userID, Name: ""})
			}
		case "channel":
			// #频道提及
			channelID, _ := sectionElement["channel_id"].(string)
			textParts = append(textParts, "#"+channelID)
		case "link":
			// 链接
			url, _ := sectionElement["url"].(string)
			linkText, _ := sectionElement["text"].(string)
			if linkText == "" {
				linkText = url
			}
			textParts = append(textParts, "["+linkText+"]("+url+")")
		case "emoji":
			// 表情符号
			emojiName, _ := sectionElement["name"].(string)
			textParts = append(textParts, ":"+emojiName+":")
		}
	}
	flushText()
	return components
}

// handleMsg 将消息发布到事件总线（对应 Python 的 handle_msg/commit_event）。
func (a *Adapter) handleMsg(abm *platform.AstrBotMessage) {
	if a.EventBus == nil {
		return
	}
	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   "slack",
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
			Platform:  "slack",
		},
		Metadata: map[string]interface{}{},
	}
	if err := a.EventBus.Publish(event); err != nil {
		logger.I18nError("发布事件失败: %v", err)
	}
}

// Send 发送消息链到 Slack 会话。
// 对应 Python 的 send_by_session：群消息发到频道，私聊发到用户。
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	if a.client == nil {
		return fmt.Errorf("slack: 适配器未初始化（缺少 bot_token）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	blocks, text := parseSlackBlocks(ctx, chain, a.client)
	// 群会话 ID 含 "_" 前缀时取最后一段（对应 Python 的 split("_")[-1]）
	channelID := sessionID
	if strings.Contains(channelID, "_") {
		channelID = channelID[strings.LastIndex(channelID, "_")+1:]
	}

	opts := []slack.MsgOption{slack.MsgOptionText(text, true)}
	if len(blocks) > 0 {
		opts = append(opts, slack.MsgOptionBlocks(blocks...))
	}
	if _, _, err := a.client.PostMessageContext(ctx, channelID, opts...); err != nil {
		// 块发送失败时，尝试只发送文本（对应 Python 的 fallback）
		logger.I18nWarn("Slack 发送消息失败: %v，尝试仅发送文本", err)
		fallbackText := buildFallbackText(chain)
		if _, _, fallbackErr := a.client.PostMessageContext(ctx, channelID, slack.MsgOptionText(fallbackText, true)); fallbackErr != nil {
			logger.I18nError("Slack 发送文本消息失败: %v", fallbackErr)
			return fallbackErr
		}
	}
	return nil
}

// buildFallbackText 构造仅文本的降级消息（对应 Python 的 fallback 拼接）。
func buildFallbackText(chain *message.MessageChain) string {
	if chain == nil {
		return ""
	}
	var parts []string
	for _, comp := range chain.Chain {
		switch c := comp.(type) {
		case *message.Plain:
			parts = append(parts, c.Text)
		case *message.File:
			parts = append(parts, " [文件: "+c.Name+"] ")
		case *message.Image:
			parts = append(parts, " [图片] ")
		}
	}
	return strings.Join(parts, "")
}

// React 给消息添加表情回应（reactions.add）。
// messageID 应为 Slack 消息的时间戳 ts。
func (a *Adapter) React(sessionID, messageID, emoji string) error {
	if a.client == nil {
		return fmt.Errorf("slack: 适配器未初始化")
	}
	channelID := sessionID
	if strings.Contains(channelID, "_") {
		channelID = channelID[strings.LastIndex(channelID, "_")+1:]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := a.client.AddReactionContext(ctx, emoji, slack.ItemRef{
		Channel:   channelID,
		Timestamp: messageID,
	})
	if err != nil {
		return fmt.Errorf("slack: 添加回应失败: %w", err)
	}
	return nil
}

// randomUUIDHex 生成随机十六进制 ID（对应 Python 的 uuid.uuid4().hex）。
func randomUUIDHex() string {
	b := make([]byte, 16)
	_, _ = randRead(b)
	return fmt.Sprintf("%x", b)
}

// writeJSONError 输出 JSON 错误响应。
func writeJSONError(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}

// verifySlackSignature 校验 Slack 请求签名（v0 HMAC-SHA256）。
// 对应 Python client.py 中的签名校验逻辑。
func verifySlackSignature(signingSecret string, body []byte, timestamp, signature string) bool {
	if signingSecret == "" || timestamp == "" || signature == "" {
		return false
	}
	sigBaseString := fmt.Sprintf("v0:%s:%s", timestamp, string(body))
	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write([]byte(sigBaseString))
	expected := "v0=" + fmt.Sprintf("%x", mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}
