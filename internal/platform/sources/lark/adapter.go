// Package lark implements a Lark (Feishu) platform adapter.
// Ported 1:1 from astrbot/core/platform/sources/lark/ (Python) using the
// official larksuite oapi-sdk-go/v3 (long connection ws + REST im APIs).
package lark

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

var logger = log.GetDefault().WithComponent("Lark")

const defaultDomain = "https://open.feishu.cn"

// Adapter implements the Lark (Feishu) bot adapter with both long-connection
// (socket) and webhook event modes, mirroring lark_adapter.py.
type Adapter struct {
	config   map[string]interface{}
	settings map[string]interface{}

	EventBus *core.EventBus

	appID      string
	appSecret  string
	domain     string
	botName    string
	botOpenID  string
	connMode   string // "socket" | "webhook"
	webhookID  string
	encryptKey string
	verifyTok  string

	client   *lark.Client
	wsClient *larkws.Client
	webhook  *LarkWebhookServer

	mu       sync.Mutex
	evIDTime map[string]time.Time

	stopCh   chan struct{}
	stopOnce sync.Once
}

// New creates a Lark adapter.
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		config:   config,
		settings: settings,
		EventBus: eventBus,
		botName:  "astrbot",
		evIDTime: make(map[string]time.Time),
		stopCh:   make(chan struct{}),
	}
	a.appID, _ = config["app_id"].(string)
	a.appSecret, _ = config["app_secret"].(string)
	a.domain, _ = config["domain"].(string)
	if a.domain == "" {
		a.domain = defaultDomain
	}
	a.connMode, _ = config["lark_connection_mode"].(string)
	if a.connMode == "" {
		a.connMode = "socket"
	}
	a.webhookID, _ = config["webhook_uuid"].(string)
	a.encryptKey, _ = config["lark_encrypt_key"].(string)
	a.verifyTok, _ = config["lark_verification_token"].(string)
	return a
}

// SetEventBus injects the event bus (implements platform.EventBusSetter).
func (a *Adapter) SetEventBus(bus platform.EventBus) {
	if eb, ok := bus.(*core.EventBus); ok {
		a.EventBus = eb
	}
}

// ID returns the adapter instance id.
func (a *Adapter) ID() string {
	if id, ok := a.config["id"].(string); ok {
		return id
	}
	return "lark"
}

// Type returns the platform type.
func (a *Adapter) Type() string { return "lark" }

// Start boots the adapter: refreshes bot info, then starts the long
// connection or registers the webhook callback.
func (a *Adapter) Start(ctx context.Context) error {
	// REST client (tencent access token auto-managed by the SDK).
	a.client = lark.NewClient(a.appID, a.appSecret,
		lark.WithLogLevel(larkcore.LogLevelError),
		lark.WithOpenBaseUrl(a.domain),
	)

	if err := a.refreshBotInfo(ctx); err != nil {
		logger.I18nWarn("启动时获取飞书机器人信息失败: %v", err)
	}

	if a.connMode == "webhook" {
		a.webhook = NewLarkWebhookServer(a.appID, a.appSecret, a.encryptKey, a.verifyTok)
		a.webhook.SetCallback(func(eventData map[string]interface{}) {
			if eventID := webhookEventID(eventData); eventID != "" {
				if a.isDuplicateEvent(eventID) {
					logger.Debug("[Lark Webhook] 跳过重复事件: %s", eventID)
					return
				}
			}
			eventType := ""
			if header, ok := eventData["header"].(map[string]interface{}); ok {
				eventType, _ = header["event_type"].(string)
			}
			if eventType != "im.message.receive_v1" {
				logger.Debug("[Lark Webhook] 未处理的事件类型: %s", eventType)
				return
			}
			a.handleWebhookEvent(eventData)
		})
		if a.webhookID != "" {
			logger.I18nInfo("飞书(Lark) Webhook 模式已启用, webhook_uuid=%s", a.webhookID)
		} else {
			logger.I18nWarn("飞书(Lark) Webhook 模式已启用，但未配置 webhook_uuid")
		}
		return nil
	}

	// Long-connection mode (default; mirrors Python lark.ws.Client).
	dispatcher := larkevent.NewEventDispatcher("", "")
	dispatcher.OnP2MessageReceiveV1(func(_ context.Context, event *larkim.P2MessageReceiveV1) error {
		a.convertMsg(event)
		return nil
	})

	a.wsClient = larkws.NewClient(a.appID, a.appSecret,
		larkws.WithDomain(a.domain),
		larkws.WithLogLevel(larkcore.LogLevelError),
		larkws.WithEventHandler(dispatcher),
	)
	a.wsClient.SetOnError(func(err error) {
		logger.I18nWarn("飞书长连接错误: %v", err)
	})

	logger.I18nInfo("飞书(Lark) 长连接模式启动, self_id=%s", a.botOpenID)
	go func() {
		if err := a.wsClient.Start(ctx); err != nil {
			logger.I18nWarn("飞书长连接退出: %v", err)
		}
	}()
	return nil
}

// Stop shuts down the adapter.
func (a *Adapter) Stop() error {
	a.stopOnce.Do(func() { close(a.stopCh) })
	if a.wsClient != nil {
		a.wsClient.Close()
	}
	logger.I18nInfo("飞书(Lark) 适配器已关闭")
	return nil
}

// refreshBotInfo fetches the app name and bot open_id (bot_info.py).
func (a *Adapter) refreshBotInfo(ctx context.Context) error {
	raw, err := a.client.Get(ctx, "/open-apis/bot/v3/info", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return err
	}
	var data struct {
		Code int `json:"code"`
		Bot  struct {
			AppName string `json:"app_name"`
			OpenID  string `json:"open_id"`
		} `json:"bot"`
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal([]byte(raw.RawBody), &data); err != nil {
		return err
	}
	if data.Code != 0 {
		return fmt.Errorf("获取飞书机器人信息失败: %s", data.Msg)
	}
	if data.Bot.AppName != "" {
		a.botName = data.Bot.AppName
	}
	if data.Bot.OpenID != "" {
		a.botOpenID = data.Bot.OpenID
	}
	return nil
}

// webhookEventID 提取事件的 event_id: schema v2 位于 header 中, 兼容旧格式的顶层字段。
func webhookEventID(eventData map[string]interface{}) string {
	if header, ok := eventData["header"].(map[string]interface{}); ok {
		if id, _ := header["event_id"].(string); id != "" {
			return id
		}
	}
	id, _ := eventData["event_id"].(string)
	return id
}

// isDuplicateEvent mirrors _is_duplicate_event (30-minute window).
func (a *Adapter) isDuplicateEvent(eventID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for id, ts := range a.evIDTime {
		if now.Sub(ts) > 30*time.Minute {
			delete(a.evIDTime, id)
		}
	}
	if _, ok := a.evIDTime[eventID]; ok {
		return true
	}
	a.evIDTime[eventID] = now
	return false
}

// convertMsg converts an im.message.receive_v1 event into a core.Event and
// publishes it (mirrors convert_msg + handle_msg).
func (a *Adapter) convertMsg(event *larkim.P2MessageReceiveV1) {
	if event.Event == nil || event.Event.Message == nil {
		logger.Debug("[Lark] 收到空事件")
		return
	}
	msg := event.Event.Message
	msgStr := ""
	if msg.Content != nil {
		msgStr = *msg.Content
	}
	chatType := ""
	if msg.ChatType != nil {
		chatType = *msg.ChatType
	}
	logger.I18nInfo("飞书收到消息 (chat=%s): %s", chatType, msgStr)

	abm := platform.NewAstrBotMessage()
	if msg.CreateTime != nil {
		if ts, err := strconv.ParseInt(*msg.CreateTime, 10, 64); err == nil {
			abm.Timestamp = ts / 1000
		}
	}
	abm.Message = []message.Component{}
	chatID := ""
	if msg.ChatId != nil {
		chatID = *msg.ChatId
	}
	abm.Type = platform.FriendMessage
	if msg.ChatType != nil && *msg.ChatType == "group" {
		abm.Type = platform.GroupMessage
	}
	abm.Group = &platform.Group{GroupID: ""}
	if abm.Type == platform.GroupMessage && chatID != "" {
		abm.Group = &platform.Group{GroupID: chatID, GroupName: chatID}
	}
	abm.SelfID = a.botOpenID
	if a.botOpenID == "" {
		abm.SelfID = a.botName
	}
	abm.MessageStr = ""

	// At map + reply from parent id.
	atList := map[string]*message.At{}
	if msg.ParentId != nil && *msg.ParentId != "" {
		if replySeg := a.buildReplyFromParentID(context.Background(), *msg.ParentId); replySeg != nil {
			abm.Message = append(abm.Message, replySeg)
		}
	}

	atMap := map[string]*message.At{}
	if msg.Mentions != nil {
		for _, m := range msg.Mentions {
			if m == nil {
				continue
			}
			openID := ""
			if m.Id != nil && m.Id.OpenId != nil {
				openID = *m.Id.OpenId
			}
			name := ""
			if m.Name != nil {
				name = *m.Name
			}
			key := ""
			if m.Key != nil {
				key = *m.Key
			}
			at := &message.At{TargetID: openID, Name: name}
			atList[key] = at
			atMap[key] = at
			if (a.botOpenID != "" && openID == a.botOpenID) || name == a.botName {
				abm.SelfID = openID
				if abm.SelfID == "" {
					abm.SelfID = a.botOpenID
				}
				if abm.SelfID == "" {
					abm.SelfID = a.botName
				}
			}
		}
	}

	if msg.Content == nil {
		logger.I18nWarn("飞书消息内容为空")
		return
	}
	var contentJSON map[string]interface{}
	if err := json.Unmarshal([]byte(*msg.Content), &contentJSON); err != nil {
		logger.I18nWarn("解析飞书消息内容失败: %v", err)
		return
	}

	msgType := ""
	if msg.MessageType != nil {
		msgType = *msg.MessageType
	}
	msgID := ""
	if msg.MessageId != nil {
		msgID = *msg.MessageId
	}
	parsed := a.parseMessageComponents(context.Background(), msgID, msgType, contentJSON, atMap)
	abm.Message = append(abm.Message, parsed...)
	abm.MessageStr = buildMessageStr(parsed)

	if msgID == "" {
		logger.I18nWarn("飞书消息缺少 message_id")
		return
	}
	if event.Event.Sender == nil || event.Event.Sender.SenderId == nil ||
		event.Event.Sender.SenderId.OpenId == nil {
		logger.I18nWarn("飞书消息发送者信息不完整")
		return
	}
	abm.MessageID = msgID
	abm.RawMessage = event
	senderOpenID := *event.Event.Sender.SenderId.OpenId
	abm.Sender = platform.MessageMember{
		UserID:   senderOpenID,
		Nickname: senderOpenID,
	}
	if len(senderOpenID) > 8 {
		abm.Sender.Nickname = senderOpenID[:8]
	}

	sessionID := senderOpenID
	if abm.Type == platform.GroupMessage {
		sessionID = abm.GroupID()
	}
	abm.SessionID = sessionID

	a.handleMsg(abm)
}

// handleMsg publishes the message event into the pipeline.
func (a *Adapter) handleMsg(abm *platform.AstrBotMessage) {
	if a.EventBus == nil {
		return
	}
	chatID := abm.GroupID()
	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   "lark",
			PlatformID: a.ID(),
			SelfID:     abm.SelfID,
			SenderID:   abm.Sender.UserID,
			SenderName: abm.Sender.Nickname,
			ConvID:     abm.SessionID,
			IsGroup:    abm.Type == platform.GroupMessage,
			IsAtBot:    abm.Type != platform.GroupMessage,
		},
		Message:    &message.MessageChain{Chain: abm.Message},
		MessageStr: abm.MessageStr,
		Timestamp:  time.Unix(abm.Timestamp, 0),
		MessageObj: &core.MessageObj{
			MessageID: abm.MessageID,
			SelfID:    abm.SelfID,
		},
		Metadata: map[string]interface{}{},
	}
	if chatID != "" {
		event.Source.ConvID = chatID
	}
	if err := a.EventBus.Publish(event); err != nil {
		logger.Error("Failed to publish event: %v", err)
	}
}

// Send sends a message chain to a Lark session. Group sessions use chat_id,
// private sessions use open_id (mirrors send_by_session).
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	if a.client == nil {
		return fmt.Errorf("lark: client not ready")
	}
	receiveIDType := "open_id"
	receiveID := sessionID
	if a.isGroupConv(sessionID) {
		receiveIDType = "chat_id"
		if strings.Contains(receiveID, "%") {
			receiveID = receiveID[strings.Index(receiveID, "%")+1:]
		}
	}
	return sendMessageChain(context.Background(), a.client, chain, "", receiveID, receiveIDType)
}

// React adds an emoji reaction to a message (CreateMessageReaction API).
func (a *Adapter) React(sessionID, messageID, emoji string) error {
	if a.client == nil {
		return fmt.Errorf("lark: client not ready")
	}
	req := larkim.NewCreateMessageReactionReqBuilder().
		MessageId(messageID).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().
			ReactionType(larkim.NewEmojiBuilder().EmojiType(emoji).Build()).
			Build()).
		Build()
	resp, err := a.client.Im.MessageReaction.Create(context.Background(), req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("lark: reaction failed(%d): %s", resp.Code, resp.Msg)
	}
	return nil
}

// isGroupConv reports whether a session id is a group chat (chat ids are
// 16-digit numeric strings, open ids start with "ou_").
func (a *Adapter) isGroupConv(sessionID string) bool {
	return !strings.HasPrefix(sessionID, "ou_") && !strings.HasPrefix(sessionID, "oc_")
}

// WebhookUUID returns the unified-webhook uuid for webhook mode.
func (a *Adapter) WebhookUUID() string {
	return a.webhookID
}

// WebhookCallback is the unified webhook entry (/api/v1/webhooks/platforms/{uuid}).
func (a *Adapter) WebhookCallback(w http.ResponseWriter, r *http.Request) {
	if a.webhook == nil {
		http.Error(w, "lark webhook not initialized", http.StatusInternalServerError)
		return
	}
	a.webhook.HandleCallback(w, r)
}

// handleWebhookEvent decodes a P2MessageReceiveV1 event from raw JSON
// (mirrors LarkWebhookServer.handle_webhook_event -> processor.do).
func (a *Adapter) handleWebhookEvent(eventData map[string]interface{}) {
	raw, err := json.Marshal(eventData)
	if err != nil {
		return
	}
	var ev larkim.P2MessageReceiveV1
	if err := json.Unmarshal(raw, &ev); err != nil {
		logger.I18nWarn("解析飞书 Webhook 事件失败: %v", err)
		return
	}
	a.convertMsg(&ev)
}

// buildMessageStr mirrors _build_message_str_from_components.
func buildMessageStr(components []message.Component) string {
	parts := []string{}
	for _, comp := range components {
		switch c := comp.(type) {
		case *message.Plain:
			text := strings.TrimSpace(c.Text)
			if text != "" {
				parts = append(parts, text)
			}
		case *message.At:
			name := strings.TrimSpace(c.Name)
			if name == "" {
				name = strings.TrimSpace(c.TargetID)
			}
			if name != "" {
				parts = append(parts, "@"+name)
			}
		case *message.Image:
			parts = append(parts, "[image]")
		case *message.File:
			name := c.Name
			if name == "" {
				name = "[file]"
			}
			parts = append(parts, name)
		case *message.Record:
			parts = append(parts, "[audio]")
		case *message.Video:
			parts = append(parts, "[video]")
		}
	}
	return strings.Join(parts, " ")
}

// parsePostContent flattens a post message's content into a tag list.
func parsePostContent(content map[string]interface{}) []map[string]interface{} {
	result := []map[string]interface{}{}
	raw, ok := content["content"].([]interface{})
	if !ok {
		return result
	}
	for _, item := range raw {
		switch arr := item.(type) {
		case []interface{}:
			for _, comp := range arr {
				if m, ok := comp.(map[string]interface{}); ok {
					result = append(result, m)
				}
			}
		case map[string]interface{}:
			result = append(result, arr)
		}
	}
	return result
}
