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
	larkcontactv3 "github.com/larksuite/oapi-sdk-go/v3/service/contact/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// userNameCacheTTL 用户名缓存 TTL（成功 1800s，失败 60s，容量 1000）。
const (
	userNameCacheTTLSeconds        = 1800
	userNameFailureCacheTTLSeconds = 60
	userNameCacheMaxSize           = 1000
	userNameLookupTimeoutSeconds   = 5
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

	// replyIDs 记录每个会话最近一次收到消息的 message_id：
	// 对齐本体 lark_event.py:551-558 的 send 始终 reply 原消息语义
	// （reply_message_id=self.message_obj.message_id）。
	replyIDs map[string]string

	// streamCards 记录进行中的 CardKit 流式卡片（sessionID → card + sequence）。
	streamCards map[string]*streamCard

	// userNameCache 私聊用户名缓存（open_id → name + expireAt）。
	// 对齐 Python lark_adapter.py _user_name_cache：成功缓存 1800s，失败缓存 60s，容量 1000。
	userNameCache   map[string]userNameCacheEntry
	userNameCacheMu sync.Mutex

	// privateChatMu/privateChat 记录私聊 open_id → chat_id 路由（键为
	// "private_chat:{open_id}"，对齐 py v4.28.2 lark_adapter.py 的 sp 存储）。
	// 私聊发送被拒（典型错误码 230101）时用 chat_id 回退重发一次。
	privateChatMu sync.RWMutex
	privateChat   map[string]string
}

type userNameCacheEntry struct {
	name     string
	expireAt time.Time
}

// New creates a Lark adapter.
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		config:        config,
		settings:      settings,
		EventBus:      eventBus,
		botName:       "astrbot",
		evIDTime:      make(map[string]time.Time),
		replyIDs:      make(map[string]string),
		streamCards:   make(map[string]*streamCard),
		userNameCache: make(map[string]userNameCacheEntry),
		privateChat:   make(map[string]string),
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
	// 从 dataDir 加载已持久化的私聊路由（不可用时静默降级为仅内存）。
	a.loadPrivateChatRoutes()
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
	// 对齐 Python lark_adapter.py:574：仅群聊设置 group_id，且不设 group_name
	// （Go 此前把 chat_id 当群名，属偏差）。私聊不构造 Group。
	if abm.Type == platform.GroupMessage && chatID != "" {
		abm.Group = &platform.Group{GroupID: chatID}
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
	// 对齐 Python #9872：构建 message_str 时剥离首位 bot 自提及
	abm.MessageStr = buildMessageStrWithBotID(parsed, a.botOpenID, a.botName)

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
	senderName := senderOpenID
	if len(senderOpenID) > 8 {
		senderName = senderOpenID[:8]
	}
	// 对齐 Python #9872：私聊消息通过 contact API 查询真实姓名
	if abm.Type == platform.FriendMessage {
		senderType := ""
		if event.Event.Sender.SenderType != nil {
			senderType = *event.Event.Sender.SenderType
		}
		if senderType == "user" {
			senderName = a.lookupUserName(senderOpenID)
		}
	}
	abm.Sender = platform.MessageMember{
		UserID:   senderOpenID,
		Nickname: senderName,
	}

	sessionID := senderOpenID
	if abm.Type == platform.GroupMessage {
		sessionID = abm.GroupID()
	} else if chatType == "p2p" && chatID != "" {
		// 对齐 py v4.28.2（commit 32a75139）：私聊入站消息记录并持久化
		// open_id → chat_id 路由，供 open_id 被拒时回退重发。
		a.savePrivateChatRoute(senderOpenID, chatID)
	}
	abm.SessionID = sessionID

	// 记录回复目标（send 始终 reply 原消息，对齐本体 send 语义）。
	a.rememberReplyID(sessionID, msgID)

	a.handleMsg(abm)
}

// rememberReplyID 记录会话最近一次收到消息的 message_id。unique_session 开启时
// pipeline 会把群聊 ConvID 改写为 "senderID%chatID"，因此同时记录两种键。
func (a *Adapter) rememberReplyID(sessionID, msgID string) {
	if sessionID == "" || msgID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.replyIDs[sessionID] = msgID
	if idx := strings.Index(sessionID, "%"); idx >= 0 {
		a.replyIDs[sessionID[idx+1:]] = msgID
	}
}

// lookupReplyID 查询会话的回复目标 message_id。
func (a *Adapter) lookupReplyID(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if id, ok := a.replyIDs[sessionID]; ok {
		return id
	}
	if idx := strings.Index(sessionID, "%"); idx >= 0 {
		return a.replyIDs[sessionID[idx+1:]]
	}
	return ""
}

// handleMsg publishes the message event into the pipeline.
func (a *Adapter) handleMsg(abm *platform.AstrBotMessage) {
	if a.EventBus == nil {
		return
	}
	chatID := abm.GroupID()
	// IsAtBot 反映消息链中是否真的 @ 了本机器人（对齐 Python：由链中 At 判定，
	// 而非"非群即真"）。
	isAtBot := false
	for _, comp := range abm.Message {
		if at, ok := comp.(*message.At); ok && at.TargetID != "" && at.TargetID == abm.SelfID {
			isAtBot = true
			break
		}
	}
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
			IsAtBot:    isAtBot,
		},
		Message:    &message.MessageChain{Chain: abm.Message},
		MessageStr: abm.MessageStr,
		Timestamp:  time.Unix(abm.Timestamp, 0),
		MessageObj: &core.MessageObj{
			MessageID:   abm.MessageID,
			SelfID:      abm.SelfID,
			SessionID:   abm.SessionID,
			MessageType: string(abm.Type),
			Platform:    "lark",
			MessageStr:  abm.MessageStr,
			RawMessage:  abm.RawMessage,
			Timestamp:   time.Unix(abm.Timestamp, 0),
			Group:       abm.Group,
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

// Send sends a message chain to a Lark session (active push / send_by_session):
// 群会话用 chat_id，私聊用 open_id，**不引用**上一条消息。
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	return a.sendChain(sessionID, chain, "")
}

// SendByEvent sends a chain as a response to the triggering event (Python
// LarkMessageEvent.send): 引用触发消息（reply_with_quote 注入的 Reply 组件优先，
// 否则回填事件 MessageObj.MessageID），对齐 lark_event.py:751-757。
func (a *Adapter) SendByEvent(sessionID string, chain *message.MessageChain, event *core.Event) error {
	replyMessageID := ""
	if event != nil && event.MessageObj != nil {
		replyMessageID = event.MessageObj.MessageID
	}
	if chain != nil {
		for _, comp := range chain.Chain {
			if reply, ok := comp.(*message.Reply); ok && reply.MessageID != "" {
				replyMessageID = reply.MessageID
				break
			}
		}
	}
	return a.sendChain(sessionID, chain, replyMessageID)
}

// sendChain sends a message chain, optionally quoting replyMessageID. Group
// sessions use chat_id, private sessions use open_id (mirrors send_by_session).
// 私聊发送优先 open_id，若被飞书拒绝（典型 230101）且已记录 open_id → chat_id
// 路由，则复用同一 UUID 以 chat_id 重试一次（对齐 py v4.28.2 commit 32a75139）。
func (a *Adapter) sendChain(sessionID string, chain *message.MessageChain, replyMessageID string) error {
	if a.client == nil {
		return fmt.Errorf("lark: client not ready")
	}
	receiveIDType := "open_id"
	receiveID := sessionID
	fallbackChatID := ""
	if a.isGroupConv(sessionID) {
		receiveIDType = "chat_id"
		if strings.Contains(receiveID, "%") {
			receiveID = receiveID[strings.Index(receiveID, "%")+1:]
		}
	} else {
		// 私聊优先 open_id；仅当发送被拒时才会用到已知 chat_id。
		fallbackChatID = a.lookupPrivateChatID(receiveID)
	}
	return sendMessageChain(context.Background(), a.client, chain, replyMessageID, receiveID, receiveIDType, fallbackChatID)
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

// isGroupConv 判断会话 id 是否为群聊：群 chat_id 以 "oc_" 开头，私聊 open_id
// 以 "ou_" 开头。开启 unique_session 后群会话 id 为 "sender%group"
// （buildUniqueSessionID: lark → senderID + "%" + groupID），此时也不能只看前缀，
// 需看 "%" 之后的 group 段（对齐 Python send_by_session 用 session.message_type 判群）。
func (a *Adapter) isGroupConv(sessionID string) bool {
	if strings.HasPrefix(sessionID, "oc_") {
		return true
	}
	if idx := strings.Index(sessionID, "%"); idx >= 0 {
		groupID := sessionID[idx+1:]
		return strings.HasPrefix(groupID, "oc_")
	}
	return false
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
// 对齐 Python #9872：首位为 bot 自身提及时跳过（不修改原组件列表，仅影响文本投影）。
func buildMessageStr(components []message.Component) string {
	return buildMessageStrWithBotID(components, "", "")
}

// buildMessageStrWithBotID 构建文本投影，botSelfID/botName 非空时跳过首位 bot 自提及。
func buildMessageStrWithBotID(components []message.Component, botSelfID, botName string) string {
	parts := []string{}
	normalizedSelfID := strings.TrimSpace(botSelfID)
	normalizedBotName := strings.TrimSpace(botName)
	for i, comp := range components {
		if i == 0 {
			if at, ok := comp.(*message.At); ok {
				mentionID := strings.TrimSpace(at.TargetID)
				mentionName := strings.TrimSpace(at.Name)
				isSelf := normalizedSelfID != "" && mentionID != "" && mentionID == normalizedSelfID
				if !isSelf && mentionID == "" && normalizedBotName != "" {
					isSelf = mentionName == normalizedBotName
				}
				if isSelf {
					continue
				}
			}
		}
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

// lookupUserName 对齐 Python lark_adapter.py #9872：通过 contact v3 user get 查询私聊用户真实姓名。
// 带 TTL 缓存：成功缓存 1800s，失败缓存 60s，容量 1000（超过淘汰最早条目）。
func (a *Adapter) lookupUserName(senderOpenID string) string {
	a.userNameCacheMu.Lock()
	entry, ok := a.userNameCache[senderOpenID]
	a.userNameCacheMu.Unlock()
	if ok && time.Now().Before(entry.expireAt) {
		return entry.name
	}
	if ok {
		a.userNameCacheMu.Lock()
		delete(a.userNameCache, senderOpenID)
		a.userNameCacheMu.Unlock()
	}

	cachedName := ""
	nameCacheTTL := userNameFailureCacheTTLSeconds
	ctx, cancel := context.WithTimeout(context.Background(), userNameLookupTimeoutSeconds*time.Second)
	defer cancel()

	req := larkcontactv3.NewGetUserReqBuilder().
		UserId(senderOpenID).
		UserIdType("open_id").
		Build()
	resp, err := a.client.Contact.V3.User.Get(ctx, req)
	if err != nil {
		logger.Debug("[Lark] Sender name lookup failed for %s: %v", senderOpenID, err)
	} else if resp.Success() && resp.Data != nil && resp.Data.User != nil && resp.Data.User.Name != nil {
		cachedName = strings.TrimSpace(*resp.Data.User.Name)
		if cachedName != "" {
			nameCacheTTL = userNameCacheTTLSeconds
		}
	} else {
		logger.Debug("[Lark] Sender name lookup failed for %s: code=%d msg=%s", senderOpenID, resp.Code, resp.Msg)
	}

	if cachedName == "" {
		cachedName = senderOpenID
		if len(cachedName) > 8 {
			cachedName = cachedName[:8]
		}
	}
	a.userNameCacheMu.Lock()
	a.userNameCache[senderOpenID] = userNameCacheEntry{
		name:     cachedName,
		expireAt: time.Now().Add(time.Duration(nameCacheTTL) * time.Second),
	}
	if len(a.userNameCache) > userNameCacheMaxSize {
		// 淘汰过期时间最早（即最旧）的条目，而不是 map 随机序里的任意一个，
		// 保证热用户不会被随机挤掉。
		oldestKey := ""
		var oldestExpire time.Time
		for k, e := range a.userNameCache {
			if oldestKey == "" || e.expireAt.Before(oldestExpire) {
				oldestKey, oldestExpire = k, e.expireAt
			}
		}
		delete(a.userNameCache, oldestKey)
	}
	a.userNameCacheMu.Unlock()
	return cachedName
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
