// Package wecom_ai_bot implements the 企业微信智能机器人 (WeCom AI Bot) platform adapter.
// 1:1 移植自 astrbot/core/platform/sources/wecom_ai_bot/：
//   - Webhook 回调模式：/webhook/wecom-ai-bot（GET 验证 + POST 消息），JSON 加解密（WXBizJsonMsgCrypt）；
//   - 长连接模式：WSS 长连接（aibot_subscribe 订阅 + 心跳 + 消息帧）；
//   - 流式响应：输出队列 + webhook 轮询聚合（make_text_stream / make_mixed_stream）；
//   - 主动消息：消息推送 webhook（msg_push_webhook_url）或长连接 aibot_respond_msg。
package wecom_ai_bot

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

var logger = log.GetDefault().WithComponent("WecomAIBot")

const (
	// longConnectionMode 长连接模式
	longConnectionMode = "long_connection"
	// webhookMode webhook 回调模式
	webhookMode = "webhook"
)

// WecomAIBotAdapter 企业微信智能机器人适配器。
type Adapter struct {
	config   map[string]interface{}
	settings map[string]interface{}

	// EventBus 事件总线（lifecycle 通过 SetEventBus 注入）
	EventBus platform.EventBus

	id                       string
	connectionMode           string
	token                    string
	encodingAESKey           string
	port                     int
	host                     string
	botName                  string
	initialRespondText       string
	friendMessageWelcomeText string
	unifiedWebhookMode       bool
	webhookUUID              string
	msgPushWebhookURL        string
	onlyUseWebhookURLToSend  bool
	longConnectionBotID      string
	longConnectionSecret     string
	longConnectionWSURL      string
	heartbeatInterval        int

	apiClient            *WecomAIBotAPIClient
	server               *WecomAIBotServer
	longConnectionClient *WecomAIBotLongConnectionClient
	webhookClient        *WecomAIBotWebhookClient

	queueMgr *WecomAIQueueMgr

	// streamPlainCache 流式文本缓存（stream_id → 已聚合文本）
	streamPlainCache map[string]string
	// streamImageCache 流式图片缓存（stream_id → 等待 finish 帧一并返回的图片 base64）
	streamImageCache map[string][]string
	streamCacheMu    sync.Mutex
	// sessionStreamMap 会话 ID → 最近的 stream_id（Go 端 Send 接口只有 sessionID）
	sessionStreamMap sync.Map

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// New 构造企业微信智能机器人适配器。
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		config:           config,
		settings:         settings,
		EventBus:         eventBus,
		queueMgr:         NewWecomAIQueueMgr(),
		streamPlainCache: make(map[string]string),
		streamImageCache: make(map[string][]string),
		stopCh:           make(chan struct{}),
	}
	a.id, _ = config["id"].(string)
	if a.id == "" {
		a.id = "wecom_ai_bot"
	}
	a.connectionMode, _ = config["wecom_ai_bot_connection_mode"].(string)
	if a.connectionMode == "" {
		a.connectionMode = webhookMode
	}
	a.token = configStr(config, "token")
	if a.token == "" {
		a.token = configStr(config, "wecomaibot_token")
	}
	a.encodingAESKey = configStr(config, "encoding_aes_key")
	if a.encodingAESKey == "" {
		a.encodingAESKey = configStr(config, "wecomaibot_encoding_aes_key")
	}
	a.port = configInt(config, "port", 6198)
	a.host = configStr(config, "callback_server_host")
	if a.host == "" {
		a.host = "0.0.0.0"
	}
	a.botName = configStr(config, "wecom_ai_bot_name")
	a.initialRespondText = configStr(config, "wecomaibot_init_respond_text")
	a.friendMessageWelcomeText = configStr(config, "wecomaibot_friend_message_welcome_text")
	a.unifiedWebhookMode = configBool(config, "unified_webhook_mode")
	a.webhookUUID = configStr(config, "webhook_uuid")
	a.msgPushWebhookURL = strings.TrimSpace(configStr(config, "msg_push_webhook_url"))
	a.onlyUseWebhookURLToSend = configBool(config, "only_use_webhook_url_to_send")
	a.longConnectionBotID = configStr(config, "wecomaibot_ws_bot_id")
	if a.longConnectionBotID == "" {
		a.longConnectionBotID = configStr(config, "long_connection_bot_id")
	}
	a.longConnectionSecret = configStr(config, "wecomaibot_ws_secret")
	if a.longConnectionSecret == "" {
		a.longConnectionSecret = configStr(config, "long_connection_secret")
	}
	a.longConnectionWSURL = configStr(config, "wecomaibot_ws_url")
	if a.longConnectionWSURL == "" {
		a.longConnectionWSURL = "wss://openws.work.weixin.qq.com"
	}
	a.heartbeatInterval = configInt(config, "wecomaibot_heartbeat_interval", 30)

	if a.connectionMode == longConnectionMode {
		if a.longConnectionBotID == "" || a.longConnectionSecret == "" {
			logger.I18nWarn("企业微信智能机器人长连接模式缺少 BotID 或 Secret，连接可能失败")
		}
		a.longConnectionClient = NewWecomAIBotLongConnectionClient(
			a.longConnectionBotID,
			a.longConnectionSecret,
			a.longConnectionWSURL,
			a.heartbeatInterval,
			a.processLongConnectionPayload,
		)
	} else {
		a.apiClient = NewWecomAIBotAPIClient(a.token, a.encodingAESKey)
		a.server = NewWecomAIBotServer(a.host, a.port, a.apiClient, a.processMessage)
	}

	// 消息推送 webhook 客户端
	if a.msgPushWebhookURL != "" {
		client, err := NewWecomAIBotWebhookClient(a.msgPushWebhookURL)
		if err != nil {
			logger.I18nError("企业微信消息推送 webhook 配置无效: %v", err)
		} else {
			a.webhookClient = client
		}
	}
	return a
}

// SetEventBus 注入事件总线（实现 platform.EventBusSetter）。
func (a *Adapter) SetEventBus(bus platform.EventBus) {
	a.EventBus = bus
}

// ID 返回适配器实例 ID。
func (a *Adapter) ID() string { return a.id }

// Type 返回平台类型。
func (a *Adapter) Type() string { return "wecom_ai_bot" }

// Start 启动适配器：
//   - 长连接模式：启动长连接客户端 + 队列监听器；
//   - 统一 webhook 模式：只运行队列监听器；
//   - 否则：启动回调服务器 + 队列监听器。
func (a *Adapter) Start(ctx context.Context) error {
	if a.connectionMode == longConnectionMode {
		if a.longConnectionClient == nil {
			return fmt.Errorf("长连接客户端未初始化")
		}
		logger.I18nInfo("启动企业微信智能机器人长连接模式: %s", a.longConnectionWSURL)
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.longConnectionClient.Start(ctx)
		}()
	} else {
		webhookUUID := a.webhookUUID
		if a.unifiedWebhookMode && webhookUUID != "" {
			logger.I18nInfo("企业微信智能机器人 已启用统一 Webhook 模式, webhook_uuid=%s", webhookUUID)
		} else {
			if a.server == nil {
				return fmt.Errorf("webhook 服务器未初始化")
			}
			logger.I18nInfo("启动企业微信智能机器人适配器，监听 %s:%d", a.host, a.port)
			if err := a.server.Start(); err != nil {
				return err
			}
		}
	}
	a.startQueueListener()
	return nil
}

// startQueueListener 启动队列监听器（清理过期响应 + 每队列消息回调）。
func (a *Adapter) startQueueListener() {
	a.queueMgr.SetListener(a.handleQueuedMessage)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				a.queueMgr.CleanupExpiredResponses(300)
				a.cleanupStreamPlainCache()
			case <-a.stopCh:
				return
			}
		}
	}()
}

// Stop 关闭适配器。
func (a *Adapter) Stop() error {
	logger.I18nInfo("企业微信智能机器人适配器正在关闭...")
	a.stopOnce.Do(func() { close(a.stopCh) })
	if a.longConnectionClient != nil {
		a.longConnectionClient.Shutdown()
	}
	if a.server != nil {
		a.server.Shutdown()
	}
	a.wg.Wait()
	return nil
}

// WebhookUUID 返回统一 Webhook 模式的标识（实现 platform.WebhookPlatform）。
func (a *Adapter) WebhookUUID() string { return a.webhookUUID }

// WebhookCallback 统一 Webhook 回调入口（实现 platform.WebhookPlatform）。
func (a *Adapter) WebhookCallback(w http.ResponseWriter, r *http.Request) {
	if a.connectionMode == longConnectionMode || a.server == nil {
		http.Error(w, "long_connection mode does not accept webhook callbacks", http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodGet {
		a.server.handleVerify(w, r)
		return
	}
	a.server.handleCallback(w, r)
}

// cleanupStreamPlainCache 清理已无输出队列的流式缓存（配合队列清理，避免泄漏）。
func (a *Adapter) cleanupStreamPlainCache() {
	a.streamCacheMu.Lock()
	defer a.streamCacheMu.Unlock()
	for streamID := range a.streamPlainCache {
		if !a.queueMgr.HasBackQueue(streamID) {
			delete(a.streamPlainCache, streamID)
		}
	}
	for streamID := range a.streamImageCache {
		if !a.queueMgr.HasBackQueue(streamID) {
			delete(a.streamImageCache, streamID)
		}
	}
}

// handleQueuedMessage 处理队列中的消息（对应 _handle_queued_message）。
func (a *Adapter) handleQueuedMessage(item *QueueItem) {
	if item == nil || item.MessageData == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			logger.I18nError("处理队列消息时发生异常: %v", r)
		}
	}()
	abm := a.convertMessage(item)
	if abm == nil {
		return
	}
	a.handleMsg(abm)
}

// processMessage 处理接收到的消息（对应 _process_message）。
// 返回加密后的响应消息，无需响应时返回空串。
func (a *Adapter) processMessage(messageData map[string]interface{}, callbackParams map[string]string) (string, error) {
	if a.apiClient == nil {
		logger.I18nError("Webhook 消息处理失败: API 客户端未初始化")
		return "", nil
	}
	msgtype, _ := messageData["msgtype"].(string)
	if msgtype == "" {
		logger.I18nWarn("消息类型未知，忽略: %v", messageData)
		return "", nil
	}
	sessionID := a.extractSessionID(messageData)
	switch msgtype {
	case "text", "image", "mixed":
		// 用户发送文本/图片/混合消息
		streamID := fmt.Sprintf("%s_%s", sessionID, GenerateRandomString(10))
		a.enqueueMessage(messageData, callbackParams, streamID, sessionID)
		a.queueMgr.SetPendingResponse(streamID, callbackParams)
		a.sessionStreamMap.Store(sessionID, streamID)

		if a.onlyUseWebhookURLToSend && a.webhookClient != nil {
			return "", nil
		}
		if a.initialRespondText != "" {
			resp := (WecomAIBotStreamMessageBuilder{}).MakeTextStream(streamID, a.initialRespondText, false)
			return a.apiClient.EncryptMessage(resp, callbackParams["nonce"], callbackParams["timestamp"]), nil
		}
		return "", nil
	case "stream":
		// 微信服务器请求获取流的更新
		streamID := mapNested(messageData, "stream", "id")
		if streamID == "" {
			return "", nil
		}
		if !a.queueMgr.HasBackQueue(streamID) {
			a.streamCacheMu.Lock()
			delete(a.streamPlainCache, streamID)
			a.streamCacheMu.Unlock()
			if !a.queueMgr.IsStreamFinished(streamID, 60) {
				logger.I18nWarn("Cannot find back queue for stream_id: %s", streamID)
			} else {
				logger.Debug("Stream already finished, returning end message: %s", streamID)
			}
			// 返回结束标志，告诉微信服务器流已结束
			endMessage := (WecomAIBotStreamMessageBuilder{}).MakeTextStream(streamID, "", true)
			return a.apiClient.EncryptMessage(endMessage, callbackParams["nonce"], callbackParams["timestamp"]), nil
		}
		queue := a.queueMgr.GetOrCreateBackQueue(streamID)
		if len(queue) == 0 {
			logger.Debug("No new messages in back queue for stream_id: %s", streamID)
			return "", nil
		}

		// 聚合输出队列中的所有增量消息
		a.streamCacheMu.Lock()
		cachedPlainContent := a.streamPlainCache[streamID]
		a.streamCacheMu.Unlock()
		latestPlainContent := cachedPlainContent
		var imageBase64 []string
		finish := false
	drain:
		for len(queue) > 0 {
			msg := <-queue
			switch msg.Type {
			case "plain":
				plainData := msg.Data
				if msg.Streaming {
					// 流式 plain 载荷已是累加内容
					cachedPlainContent = plainData
				} else {
					// 分段非流式发送追加
					cachedPlainContent += plainData
				}
				latestPlainContent = cachedPlainContent
			case "image":
				imageBase64 = append(imageBase64, msg.ImageData)
			case "break":
				continue
			case "end", "complete":
				// 流结束
				finish = true
				a.queueMgr.RemoveQueues(streamID, true)
				a.streamCacheMu.Lock()
				delete(a.streamPlainCache, streamID)
				a.streamCacheMu.Unlock()
				break drain
			}
		}

		logger.Debug("Aggregated content: %s, image: %d, finish: %v", latestPlainContent, len(imageBase64), finish)
		// 图片缓存：对齐本体（wecomai_adapter.py:310-321）图片仅随 finish 帧返回；
		// 非 finish 轮询先并入缓存，避免中途过早下发，finish 时一次性携带。
		a.streamCacheMu.Lock()
		cachedImages := a.streamImageCache[streamID]
		a.streamCacheMu.Unlock()
		if !finish {
			a.streamCacheMu.Lock()
			a.streamPlainCache[streamID] = cachedPlainContent
			if len(imageBase64) > 0 {
				a.streamImageCache[streamID] = append(cachedImages, imageBase64...)
			}
			a.streamCacheMu.Unlock()
			imageBase64 = nil
			cachedImages = nil
		}
		if finish && latestPlainContent == "" && len(imageBase64) == 0 && len(cachedImages) == 0 {
			endMessage := (WecomAIBotStreamMessageBuilder{}).MakeTextStream(streamID, "", true)
			return a.apiClient.EncryptMessage(endMessage, callbackParams["nonce"], callbackParams["timestamp"]), nil
		}
		if latestPlainContent != "" || len(imageBase64) > 0 || len(cachedImages) > 0 {
			var msgItems []interface{}
			if finish {
				// finish 帧：合并历史缓存与本次轮询新到的图片并清空缓存。
				allImages := make([]string, 0, len(cachedImages)+len(imageBase64))
				allImages = append(allImages, cachedImages...)
				allImages = append(allImages, imageBase64...)
				for _, imgB64 := range allImages {
					imgData, err := base64Decode(imgB64)
					if err != nil {
						continue
					}
					msgItems = append(msgItems, map[string]interface{}{
						"msgtype": MSGTypeImage,
						"image": map[string]interface{}{
							"base64": imgB64,
							"md5":    CalculateImageMD5(imgData),
						},
					})
				}
				a.streamCacheMu.Lock()
				delete(a.streamImageCache, streamID)
				a.streamCacheMu.Unlock()
			}
			plainMessage := (WecomAIBotStreamMessageBuilder{}).MakeMixedStream(streamID, latestPlainContent, msgItems, finish)
			encryptedMessage := a.apiClient.EncryptMessage(plainMessage, callbackParams["nonce"], callbackParams["timestamp"])
			if encryptedMessage != "" {
				logger.Debug("Stream message sent successfully, stream_id: %s", streamID)
			} else {
				logger.I18nError("消息加密失败")
			}
			return encryptedMessage, nil
		}
		return "", nil
	case "event":
		event, _ := messageData["event"].(map[string]interface{})
		if event == nil {
			return "", nil
		}
		if ev, _ := event["event"].(string); ev == "enter_chat" && a.friendMessageWelcomeText != "" {
			// 用户进入会话，发送欢迎消息
			resp := (WecomAIBotStreamMessageBuilder{}).MakeText(a.friendMessageWelcomeText)
			return a.apiClient.EncryptMessage(resp, callbackParams["nonce"], callbackParams["timestamp"]), nil
		}
	}
	return "", nil
}

// processLongConnectionPayload 处理长连接回调消息（对应 _process_long_connection_payload）。
func (a *Adapter) processLongConnectionPayload(payload map[string]interface{}) {
	cmd, _ := payload["cmd"].(string)
	headers, _ := payload["headers"].(map[string]interface{})
	body, ok := payload["body"].(map[string]interface{})
	if !ok || body == nil {
		return
	}
	reqID, _ := headers["req_id"].(string)

	switch cmd {
	case "aibot_msg_callback":
		sessionID := a.extractSessionID(body)
		streamID := fmt.Sprintf("%s_%s", sessionID, GenerateRandomString(10))
		callbackParams := map[string]string{"req_id": reqID}
		a.enqueueMessage(body, callbackParams, streamID, sessionID)
		a.queueMgr.SetPendingResponse(streamID, map[string]string{
			"req_id":          reqID,
			"connection_mode": longConnectionMode,
		})
		a.sessionStreamMap.Store(sessionID, streamID)

		if a.initialRespondText != "" && reqID != "" {
			a.sendLongConnectionRespondMsg(reqID, map[string]interface{}{
				"msgtype": "stream",
				"stream": map[string]interface{}{
					"id":      streamID,
					"finish":  false,
					"content": a.initialRespondText,
				},
			})
		}
	case "aibot_event_callback":
		event, _ := body["event"].(map[string]interface{})
		if event == nil {
			return
		}
		eventType, _ := event["eventtype"].(string)
		if eventType == "enter_chat" && a.friendMessageWelcomeText != "" && reqID != "" {
			a.sendLongConnectionRespondWelcome(reqID)
		} else if eventType == "disconnected_event" {
			logger.I18nWarn("[WecomAI][LongConn] 收到 disconnected_event，旧连接将被关闭")
		}
	}
}

// sendLongConnectionRespondWelcome 长连接发送欢迎语（对应 _send_long_connection_respond_welcome）。
func (a *Adapter) sendLongConnectionRespondWelcome(reqID string) bool {
	if a.longConnectionClient == nil {
		return false
	}
	return a.longConnectionClient.SendCommand("aibot_respond_welcome_msg", reqID, map[string]interface{}{
		"msgtype": "text",
		"text":    map[string]interface{}{"content": a.friendMessageWelcomeText},
	})
}

// sendLongConnectionRespondMsg 长连接发送响应消息（对应 _send_long_connection_respond_msg）。
func (a *Adapter) sendLongConnectionRespondMsg(reqID string, body map[string]interface{}) bool {
	if a.longConnectionClient == nil {
		return false
	}
	return a.longConnectionClient.SendCommand("aibot_respond_msg", reqID, body)
}

// extractSessionID 从消息数据中提取会话 ID（对应 _extract_session_id）：
// 群聊使用 chatid，单聊使用 from.userid。
func (a *Adapter) extractSessionID(messageData map[string]interface{}) string {
	chattype, _ := messageData["chattype"].(string)
	if chattype == "group" {
		chatID, _ := messageData["chatid"].(string)
		if chatID == "" {
			chatID = "default_group"
		}
		return FormatSessionID("wecomai", chatID)
	}
	from, _ := messageData["from"].(map[string]interface{})
	userID, _ := from["userid"].(string)
	if userID == "" {
		userID = "default_user"
	}
	return FormatSessionID("wecomai", userID)
}

// enqueueMessage 将消息放入队列进行异步处理（对应 _enqueue_message）。
func (a *Adapter) enqueueMessage(messageData map[string]interface{}, callbackParams map[string]string, streamID, sessionID string) {
	inputQueue := a.queueMgr.GetOrCreateQueue(streamID)
	a.queueMgr.GetOrCreateBackQueue(streamID)
	inputQueue <- &QueueItem{
		MessageData:    messageData,
		CallbackParams: callbackParams,
		SessionID:      sessionID,
	}
	logger.Debug("[WecomAI] 消息已入队: %s", streamID)
}

// convertMessage 转换队列中的消息数据为 AstrBotMessage（对应 convert_message）。
func (a *Adapter) convertMessage(payload *QueueItem) *platform.AstrBotMessage {
	messageData := payload.MessageData
	sessionID := payload.SessionID

	msgtype, _ := messageData["msgtype"].(string)
	content := ""
	var imageBase64 []string

	// 需要下载解密的图片 URL 列表
	var imgURLs [][2]string
	var msgItems []interface{}

	if msgtype == MSGTypeText {
		content = (WecomAIBotMessageParser{}).ParseTextMessage(messageData)
	} else if msgtype == MSGTypeImage {
		if imagePayload, ok := messageData["image"].(map[string]interface{}); ok {
			if imageURL, _ := imagePayload["url"].(string); imageURL != "" {
				aesKey, _ := imagePayload["aeskey"].(string)
				imgURLs = append(imgURLs, [2]string{imageURL, aesKey})
			}
		}
	} else if msgtype == MSGTypeMixed {
		// 提取混合消息中的文本内容
		msgItems = (WecomAIBotMessageParser{}).ParseMixedMessage(messageData)
		var textParts []string
		for _, item := range msgItems {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if m["msgtype"] == MSGTypeText {
				if textContent := mapNested(m, "text", "content"); textContent != "" {
					textParts = append(textParts, textContent)
				}
			} else if m["msgtype"] == MSGTypeImage {
				if imagePayload, ok := m["image"].(map[string]interface{}); ok {
					if imageURL, _ := imagePayload["url"].(string); imageURL != "" {
						aesKey, _ := imagePayload["aeskey"].(string)
						imgURLs = append(imgURLs, [2]string{imageURL, aesKey})
					}
				}
			}
		}
		content = strings.Join(textParts, " ")
	} else {
		content = fmt.Sprintf("[%s消息]", msgtype)
	}

	// 并行处理图片下载和解密
	if len(imgURLs) > 0 {
		type result struct {
			ok   bool
			data string
		}
		results := make([]result, len(imgURLs))
		var wg sync.WaitGroup
		for i, pair := range imgURLs {
			wg.Add(1)
			go func(i int, imageURL, aesKey string) {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						logger.I18nError("处理加密图片时发生异常: %v", r)
					}
				}()
				if aesKey == "" {
					aesKey = a.encodingAESKey
				}
				ok, data := processEncryptedImage(imageURL, aesKey)
				results[i] = result{ok: ok, data: data}
			}(i, pair[0], pair[1])
		}
		wg.Wait()
		for _, r := range results {
			if r.ok {
				imageBase64 = append(imageBase64, r.data)
			} else {
				logger.I18nError("处理加密图片失败: %s", r.data)
			}
		}
	}

	// 构建 AstrBotMessage
	abm := platform.NewAstrBotMessage()
	abm.SelfID = a.botName
	abm.MessageStr = content
	if abm.MessageStr == "" {
		abm.MessageStr = "[未知消息]"
	}
	abm.MessageID = randomHexID()
	abm.Timestamp = time.Now().Unix()
	abm.RawMessage = payload

	// 发送者信息
	from, _ := messageData["from"].(map[string]interface{})
	userID, _ := from["userid"].(string)
	if userID == "" {
		userID = "unknown"
	}
	abm.Sender = platform.MessageMember{UserID: userID, Nickname: userID}

	// 消息类型
	if chattype, _ := messageData["chattype"].(string); chattype == "group" {
		abm.Type = platform.GroupMessage
		// 对齐 Python wecomai_adapter.py #9808：GROUP_MESSAGE 且有 chatid 时补全 GroupID
		if chatID, ok := messageData["chatid"].(string); ok && chatID != "" {
			abm.Group = &platform.Group{GroupID: chatID}
		}
	} else {
		abm.Type = platform.FriendMessage
	}
	abm.SessionID = sessionID

	// 消息内容
	abm.Message = []message.Component{}

	// 处理 At
	if a.botName != "" && strings.Contains(abm.MessageStr, "@"+a.botName) {
		abm.MessageStr = strings.TrimSpace(strings.ReplaceAll(abm.MessageStr, "@"+a.botName, ""))
		abm.Message = append(abm.Message, &message.At{TargetID: a.botName, Name: a.botName})
	}
	abm.Message = append(abm.Message, &message.Plain{Text: abm.MessageStr})
	for _, imgB64 := range imageBase64 {
		abm.Message = append(abm.Message, &message.Image{Base64: imgB64})
	}

	logger.Debug("WecomAIAdapter: %v", abm.Message)
	return abm
}

// handleMsg 处理消息，发布事件（对应 handle_msg）。
func (a *Adapter) handleMsg(abm *platform.AstrBotMessage) {
	defer func() {
		if r := recover(); r != nil {
			logger.I18nError("处理消息时发生异常: %v", r)
		}
	}()
	if a.EventBus == nil {
		logger.I18nError("企业微信智能机器人适配器尚未注入事件总线，消息被丢弃")
		return
	}
	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   "wecom_ai_bot",
			PlatformID: a.ID(),
			SelfID:     abm.SelfID,
			SenderID:   abm.Sender.UserID,
			SenderName: abm.Sender.Nickname,
			ConvID:     abm.SessionID,
			IsGroup:    abm.Type == platform.GroupMessage,
			IsAtBot:    true, // 企业微信智能机器人消息默认视为唤醒（对应 is_wake = True）
		},
		Message:    &message.MessageChain{Chain: abm.Message},
		MessageStr: abm.MessageStr,
		Timestamp:  time.Unix(abm.Timestamp, 0),
		MessageObj: &core.MessageObj{
			MessageID:   abm.MessageID,
			SelfID:      abm.SelfID,
			SessionID:   abm.SessionID,
			MessageType: string(abm.Type),
			Platform:    "wecom_ai_bot",
			MessageStr:  abm.MessageStr,
			RawMessage:  abm.RawMessage,
		},
		Metadata: map[string]interface{}{},
	}
	if err := a.EventBus.Publish(event); err != nil {
		logger.I18nError("发布企业微信智能机器人事件失败: %v", err)
	}
}

// Send 发送消息到会话（对应 send_by_session + 事件 send）：
//   - 长连接模式：通过 aibot_respond_msg 发送（优先 webhook 推送不支持的组件）；
//   - 仅 webhook 推送模式：全部消息经消息推送 webhook 发送并标记流结束；
//   - 默认：文本进输出队列（供 webhook 轮询聚合），不支持的组件经 webhook 推送。
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	if chain == nil {
		return nil
	}
	streamID := sessionID
	if sid, ok := a.sessionStreamMap.Load(sessionID); ok {
		if s, ok := sid.(string); ok && s != "" {
			streamID = s
		}
	}
	pending := a.queueMgr.GetPendingResponse(streamID)
	connectionMode := ""
	reqID := ""
	if pending != nil {
		connectionMode = pending.CallbackParams["connection_mode"]
		reqID = pending.CallbackParams["req_id"]
	}

	// 对齐 Python send_by_session（wecomai_adapter.py:564-583）：主动消息
	// （无本次用户消息对应的待响应上下文）必须配置消息推送 Webhook，未配置时
	// 报错返回而不是静默降级；正常回复路径（存在待响应上下文）不受影响。
	if pending == nil && a.webhookClient == nil {
		return fmt.Errorf(
			"主动消息发送失败: 未配置企业微信消息推送 Webhook URL，请前往配置添加。"+
				"详见文档: https://docs.astrbot.app/platform/wecom_ai_bot.html#%%E9%%85%%8D%%E7%%BD%%AE-astrbot。session_id=%s",
			sessionID,
		)
	}

	if connectionMode == longConnectionMode && reqID != "" && a.longConnectionClient != nil {
		if a.onlyUseWebhookURLToSend && a.webhookClient != nil {
			if err := a.webhookClient.SendMessageChain(context.Background(), chain, false); err != nil {
				return err
			}
			return nil
		}
		if a.webhookClient != nil {
			if err := a.webhookClient.SendMessageChain(context.Background(), chain, true); err != nil {
				return err
			}
		}
		// LLM 流式回复的增量分片由 StreamFragmenter 承担（见下方实现）；
		// 此分支用于非流式回复/媒体补发：整链单次 finish=true 收尾。
		content := extractPlainTextFromChain(chain, true)
		a.sendLongConnectionRespondMsg(reqID, map[string]interface{}{
			"msgtype": "stream",
			"stream": map[string]interface{}{
				"id":      streamID,
				"finish":  true,
				"content": content,
			},
		})
		return nil
	}

	if a.onlyUseWebhookURLToSend && a.webhookClient != nil {
		if err := a.webhookClient.SendMessageChain(context.Background(), chain, false); err != nil {
			return err
		}
		markStreamComplete(streamID, a.queueMgr)
		return nil
	}

	if a.webhookClient != nil {
		if err := a.webhookClient.SendMessageChain(context.Background(), chain, true); err != nil {
			return err
		}
	}

	sendToBackQueue(chain, streamID, a.queueMgr, false, a.webhookClient != nil)
	return nil
}

// GetAPIClient 获取 API 客户端（对应 get_client）。
func (a *Adapter) GetAPIClient() *WecomAIBotAPIClient { return a.apiClient }

// ---------- 长连接流式增量分片（实现 platform.StreamFragmenter） ----------
//
// 对齐本体 wecomai_event.py:258-301（长连接 send_streaming）：以 0.5s 节流
// 逐段推送 finish=false 的累计文本，流收尾发送 finish=true。节流由宿主流式
// 管线驱动（internal/pipeline/stages.go streamSender.push，500ms 间隔），
// 本适配器只负责帧的传输；帧内容为累计全文（对应本体 increment_plain 累加语义）。
// 仅长连接模式可用；其他情况返回错误，宿主自动回退到按句切分发送。

// streamRespondBody 构造长连接流式响应帧（对应 stream 分片结构）。
func streamRespondBody(streamID string, finish bool, content string) map[string]interface{} {
	return map[string]interface{}{
		"msgtype": "stream",
		"stream": map[string]interface{}{
			"id":      streamID,
			"finish":  finish,
			"content": content,
		},
	}
}

// resolveLongConnStream 解析会话当前的长连接流上下文（stream_id + req_id）。
func (a *Adapter) resolveLongConnStream(sessionID string) (streamID, reqID string, err error) {
	if a.connectionMode != longConnectionMode {
		return "", "", fmt.Errorf("仅长连接模式支持原生流式增量")
	}
	v, ok := a.sessionStreamMap.Load(sessionID)
	if !ok {
		return "", "", fmt.Errorf("会话没有待响应的流上下文: %s", sessionID)
	}
	streamID, _ = v.(string)
	if streamID == "" {
		return "", "", fmt.Errorf("会话流上下文为空: %s", sessionID)
	}
	if pending := a.queueMgr.GetPendingResponse(streamID); pending != nil {
		reqID = pending.CallbackParams["req_id"]
	}
	if reqID == "" {
		return "", "", fmt.Errorf("流缺少 req_id（响应可能已过期）: %s", streamID)
	}
	return streamID, reqID, nil
}

// StreamStart 打开长连接流式消息并发送首段（finish=false），
// 返回 stream_id 作为宿主侧消息句柄。
func (a *Adapter) StreamStart(sessionID, text string) (string, error) {
	streamID, reqID, err := a.resolveLongConnStream(sessionID)
	if err != nil {
		return "", err
	}
	if a.longConnectionClient == nil {
		return "", fmt.Errorf("长连接客户端未初始化")
	}
	if !a.sendLongConnectionRespondMsg(reqID, streamRespondBody(streamID, false, text)) {
		return "", fmt.Errorf("长连接流式起始帧发送失败: stream_id=%s", streamID)
	}
	logger.Debug("[WecomAI] 流式起始帧已发送: %s", streamID)
	return streamID, nil
}

// StreamUpdate 推送流式增量（累计全文，finish=false）。
func (a *Adapter) StreamUpdate(_ string, msgID, text string) error {
	return a.sendStreamFrame(msgID, false, text)
}

// StreamEnd 收尾流式消息（finish=true）。
func (a *Adapter) StreamEnd(_ string, msgID, text string) error {
	return a.sendStreamFrame(msgID, true, text)
}

// sendStreamFrame 通过长连接发送流式帧；req_id 取自流的待响应缓存。
func (a *Adapter) sendStreamFrame(streamID string, finish bool, text string) error {
	if a.longConnectionClient == nil {
		return fmt.Errorf("长连接客户端未初始化")
	}
	reqID := ""
	if pending := a.queueMgr.GetPendingResponse(streamID); pending != nil {
		reqID = pending.CallbackParams["req_id"]
	}
	if reqID == "" {
		return fmt.Errorf("流缺少 req_id（响应可能已过期）: %s", streamID)
	}
	if !a.sendLongConnectionRespondMsg(reqID, streamRespondBody(streamID, finish, text)) {
		return fmt.Errorf("长连接流式帧发送失败: stream_id=%s finish=%v", streamID, finish)
	}
	return nil
}

// GetServer 获取 HTTP 服务器实例（对应 get_server）。
func (a *Adapter) GetServer() *WecomAIBotServer { return a.server }

// ---------- 工具函数 ----------

// configStr 读取字符串配置。
func configStr(config map[string]interface{}, key string) string {
	if v, ok := config[key].(string); ok {
		return v
	}
	return ""
}

// configInt 读取整型配置（支持 JSON 的 float64 与字符串）。
func configInt(config map[string]interface{}, key string, def int) int {
	switch v := config[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

// configBool 读取布尔配置。
func configBool(config map[string]interface{}, key string) bool {
	if v, ok := config[key].(bool); ok {
		return v
	}
	return false
}

// mapNested 读取嵌套 map 中的字符串字段。
func mapNested(m map[string]interface{}, outer, inner string) string {
	if sub, ok := m[outer].(map[string]interface{}); ok {
		if v, ok := sub[inner].(string); ok {
			return v
		}
	}
	return ""
}

// base64Decode base64 解码。
func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// randomHexID 生成 32 位随机 ID（对应 uuid4().hex 的简化实现）。
func randomHexID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
