// Package misskey implements a Misskey platform adapter.
// 移植自 astrbot/core/platform/sources/misskey/misskey_adapter.py
package misskey

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand" // nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used -- #nosec G404: 仅用于 WS 重连退避抖动，非安全随机
	"os"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
	"github.com/yitsushi/go-misskey/services/notes/reactions"

	"sync/atomic"
)

var logger = log.GetDefault().WithComponent("Misskey")

// 常量（对应 Python 模块级常量）
const (
	maxFileUploadCount       = 16 // 最大文件上传数量
	defaultUploadConcurrency = 3  // 默认并发上传数
	maxUploadConcurrency     = 10 // 并发上传上限
)

// intVal 从配置中读取整数，兼容 JSON 反序列化产生的 float64 以及 int/json.Number；
// 未设置或类型非法时返回 def。
func intVal(config map[string]interface{}, key string, def int) int {
	v, ok := config[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case float32:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return def
}

// Adapter 是 Misskey 平台适配器。
// 通过 WebSocket streaming 接收 note（提及/回复/引用）、私聊与群聊消息；
// 发送支持 notes/create（含回复/引用/投票/可见性）、聊天消息与文件上传。
type Adapter struct {
	config   map[string]interface{}
	settings map[string]interface{}
	EventBus *core.EventBus

	instanceURL       string
	accessToken       string
	maxMessageLength  int
	defaultVisibility string
	localOnly         bool
	enableChat        bool
	enableFileUpload  bool
	uploadFolder      string

	// 下载/安全相关选项
	allowInsecureDownloads bool
	downloadTimeout        int
	downloadChunkSize      int
	maxDownloadBytes       int64

	api         *MisskeyAPI
	instanceID  string
	running     atomic.Bool
	botSelfID   string
	botUsername string
	userCache   map[string]map[string]interface{}

	// sessionEvents 记录每个会话最近一次发布到事件总线的 core.Event 指针。
	// core.Event 在 pipeline 处理过程中会被阶段/插件原地修改（如写入
	// Metadata["extra_data"]，对应 Python session.extra_data 的 SDK 事件
	// metadata 通道），发送帖子时据此读取 cw/poll/renote_id/channel_id。
	sessionEventsMu sync.Mutex
	sessionEvents   map[string]*core.Event
	sessionEventsQ  []string // FIFO 驱逐队列, 防止会话缓存无限增长

	stopCh   chan struct{}
	stopOnce sync.Once
}

// sessionEventsMax 会话事件缓存上限。
const sessionEventsMax = 128

// New 创建 Misskey 适配器。
// config 为平台实例配置（misskey_instance_url / misskey_token 等，字段名与 Python 一致）。
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	if config == nil {
		config = map[string]interface{}{}
	}
	instanceURL, _ := config["misskey_instance_url"].(string)
	accessToken, _ := config["misskey_token"].(string)
	instanceID, _ := config["id"].(string)
	if instanceID == "" {
		instanceID = "misskey"
	}
	maxMessageLength := intVal(config, "max_message_length", 3000)
	defaultVisibility, _ := config["misskey_default_visibility"].(string)
	if defaultVisibility == "" {
		defaultVisibility = "public"
	}
	localOnly, _ := config["misskey_local_only"].(bool)
	enableChat, _ := config["misskey_enable_chat"].(bool)
	// 注意：Python 的默认值是 True，Go 的布尔零值为 false，
	// 因此使用 "未设置则为 True" 的读取方式保持 1:1 行为。
	if _, ok := config["misskey_enable_chat"]; !ok {
		enableChat = true
	}
	enableFileUpload, _ := config["misskey_enable_file_upload"].(bool)
	if _, ok := config["misskey_enable_file_upload"]; !ok {
		enableFileUpload = true
	}
	uploadFolder, _ := config["misskey_upload_folder"].(string)

	allowInsecure, _ := config["misskey_allow_insecure_downloads"].(bool)
	downloadTimeout := intVal(config, "misskey_download_timeout", 15)
	downloadChunkSize := intVal(config, "misskey_download_chunk_size", 64*1024)
	maxDownloadBytes := int64(intVal(config, "misskey_max_download_bytes", 0))

	return &Adapter{
		config:                 config,
		settings:               settings,
		EventBus:               eventBus,
		instanceURL:            strings.TrimRight(instanceURL, "/"),
		accessToken:            accessToken,
		maxMessageLength:       maxMessageLength,
		defaultVisibility:      defaultVisibility,
		localOnly:              localOnly,
		enableChat:             enableChat,
		enableFileUpload:       enableFileUpload,
		uploadFolder:           uploadFolder,
		allowInsecureDownloads: allowInsecure,
		downloadTimeout:        downloadTimeout,
		downloadChunkSize:      downloadChunkSize,
		maxDownloadBytes:       maxDownloadBytes,
		instanceID:             instanceID,
		userCache:              make(map[string]map[string]interface{}),
		sessionEvents:          make(map[string]*core.Event),
		stopCh:                 make(chan struct{}),
	}
}

// SetEventBus 注入事件总线（必须实现，断言 *core.EventBus）。
func (a *Adapter) SetEventBus(bus platform.EventBus) {
	if eb, ok := bus.(*core.EventBus); ok {
		a.EventBus = eb
	}
}

// ID 返回实例 ID。
func (a *Adapter) ID() string { return a.instanceID }

// Type 返回平台类型名。
func (a *Adapter) Type() string { return "misskey" }

// Start 启动适配器：初始化 API 客户端、获取当前用户信息，随后启动 WebSocket 连接循环。
func (a *Adapter) Start(ctx context.Context) error {
	if a.instanceURL == "" || a.accessToken == "" {
		return fmt.Errorf("misskey 配置不完整，无法启动")
	}
	api, err := NewMisskeyAPI(a.instanceURL, a.accessToken, a.allowInsecureDownloads, a.downloadTimeout, a.downloadChunkSize, a.maxDownloadBytes)
	if err != nil {
		return fmt.Errorf("misskey 初始化 API 客户端失败: %w", err)
	}
	a.api = api
	a.running.Store(true)

	// 获取当前用户信息（对应 run() 中的 get_current_user）
	userInfo, err := a.api.GetCurrentUser(ctx)
	if err != nil {
		a.running.Store(false)
		return fmt.Errorf("misskey 获取用户信息失败: %w", err)
	}
	a.botSelfID = userInfo.ID
	a.botUsername = userInfo.Username
	logger.I18nInfo("Misskey 已连接用户: %s (ID: %s)", a.botUsername, a.botSelfID)

	go a.startWebSocketConnection(ctx)
	return nil
}

// Stop 停止适配器。
func (a *Adapter) Stop() error {
	a.running.Store(false)
	a.stopOnce.Do(func() { close(a.stopCh) })
	if a.api != nil {
		a.api.Close()
	}
	return nil
}

// React 对指定 note 添加表情回应（notes/reactions/create）。
func (a *Adapter) React(sessionID, messageID, emoji string) error {
	if a.api == nil {
		return fmt.Errorf("misskey API 客户端未初始化")
	}
	return a.api.client.Notes().Reactions().Create(reactions.CreateRequest{
		NoteID:   messageID,
		Reaction: emoji,
	})
}

// startWebSocketConnection 建立 WebSocket 连接循环（对应 _start_websocket_connection）。
// 失败时按指数退避（1s 起，1.5 倍增长，上限 300s）+ 随机抖动重连。
func (a *Adapter) startWebSocketConnection(ctx context.Context) {
	backoffDelay := 1.0
	const maxBackoff = 300.0
	const backoffMultiplier = 1.5
	connectionAttempts := 0

	for a.running.Load() {
		select {
		case <-a.stopCh:
			return
		case <-ctx.Done():
			return
		default:
		}

		connectionAttempts++
		if a.api == nil {
			logger.Error("Misskey API 客户端未初始化")
			break
		}

		streaming := a.api.GetStreamingClient()
		a.registerEventHandlers(streaming)

		if streaming.Connect() {
			logger.I18nInfo("Misskey WebSocket 已连接 (尝试 #%d)", connectionAttempts)
			connectionAttempts = 0
			if _, err := streaming.SubscribeChannel("main", nil); err != nil {
				logger.Warn("Misskey 订阅 main 频道失败: %v", err)
			}
			if a.enableChat {
				if _, err := streaming.SubscribeChannel("messaging", nil); err != nil {
					logger.Warn("Misskey 订阅 messaging 频道失败: %v", err)
				}
				if _, err := streaming.SubscribeChannel("messagingIndex", nil); err != nil {
					logger.Warn("Misskey 订阅 messagingIndex 频道失败: %v", err)
				}
				logger.I18nInfo("Misskey 聊天频道已订阅")
			}
			backoffDelay = 1.0
			streaming.Listen()
		} else {
			logger.Error("Misskey WebSocket 连接失败 (尝试 #%d)", connectionAttempts)
		}

		if a.running.Load() {
			jitter := rand.Float64() // #nosec G404 -- WS 重连退避抖动，非安全随机
			sleepTime := backoffDelay + jitter
			logger.I18nInfo("Misskey %.1f秒后重连 (下次尝试 #%d)", sleepTime, connectionAttempts+1)
			select {
			case <-a.stopCh:
				return
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(sleepTime * float64(time.Second))):
			}
			backoffDelay *= backoffMultiplier
			if backoffDelay > maxBackoff {
				backoffDelay = maxBackoff
			}
		}
	}
}

// registerEventHandlers 注册事件处理器（对应 _register_event_handlers）。
func (a *Adapter) registerEventHandlers(streaming *StreamingClient) {
	streaming.AddMessageHandler("notification", a.handleNotification)
	streaming.AddMessageHandler("main:notification", a.handleNotification)

	if a.enableChat {
		streaming.AddMessageHandler("newChatMessage", a.handleChatMessage)
		streaming.AddMessageHandler("messaging:newChatMessage", a.handleChatMessage)
		streaming.AddMessageHandler("_debug", a.debugHandler)
	}
}

// handleNotification 处理通知事件（对应 _handle_notification）。
// 仅处理 mention / reply / quote 三种类型，且需命中机器人。
func (a *Adapter) handleNotification(data map[string]interface{}) {
	notificationType, _ := data["type"].(string)
	userID, _ := data["userId"].(string)
	logger.Debug("Misskey 收到通知事件: type=%s, user_id=%s", notificationType, userID)

	if notificationType == "mention" || notificationType == "reply" || notificationType == "quote" {
		note, _ := data["note"].(map[string]interface{})
		if note != nil && a.isBotMentioned(note) {
			text, _ := note["text"].(string)
			if len([]rune(text)) > 50 {
				text = truncateRunes(text, 50)
			}
			logger.I18nInfo("Misskey 处理贴文提及: %s...", text)
			abm := a.convertMessage(note)
			a.publishMessage(abm)
		}
	}
}

// handleChatMessage 处理聊天消息事件（对应 _handle_chat_message）。
func (a *Adapter) handleChatMessage(data map[string]interface{}) {
	senderID, _ := data["fromUserId"].(string)
	if senderID == "" {
		if fromUser, ok := data["fromUser"].(map[string]interface{}); ok {
			senderID, _ = fromUser["id"].(string)
		}
	}
	roomID, _ := data["toRoomId"].(string)
	logger.Debug("Misskey 收到聊天事件: sender_id=%s, room_id=%s, is_self=%v", senderID, roomID, senderID == a.botSelfID)

	if senderID == a.botSelfID {
		return
	}

	if roomID != "" {
		rawText, _ := data["text"].(string)
		logger.Debug("Misskey 检查群聊消息: %q, 机器人用户名: %q", rawText, a.botUsername)
		abm := a.convertRoomMessage(data)
		msgStr := abm.MessageStr
		if len([]rune(msgStr)) > 50 {
			msgStr = truncateRunes(msgStr, 50)
		}
		logger.I18nInfo("Misskey 处理群聊消息: %s...", msgStr)
		a.publishMessage(abm)
		return
	}
	abm := a.convertChatMessage(data)
	msgStr := abm.MessageStr
	if len([]rune(msgStr)) > 50 {
		msgStr = truncateRunes(msgStr, 50)
	}
	logger.I18nInfo("Misskey 处理私聊消息: %s...", msgStr)
	a.publishMessage(abm)
}

// debugHandler 记录未处理的事件（对应 _debug_handler）。
func (a *Adapter) debugHandler(data map[string]interface{}) {
	eventType, _ := data["type"].(string)
	if eventType == "" {
		eventType = "unknown"
	}
	channel, _ := data["channel"].(string)
	if channel == "" {
		channel = "unknown"
	}
	logger.Debug("Misskey 收到未处理事件: type=%s, channel=%s", eventType, channel)
}

// isBotMentioned 判断 note 是否提及机器人（对应 _is_bot_mentioned）。
func (a *Adapter) isBotMentioned(note map[string]interface{}) bool {
	text, _ := note["text"].(string)
	if text == "" {
		return false
	}
	if a.botUsername != "" && strings.Contains(text, "@"+a.botUsername) {
		return true
	}
	if mentions, ok := note["mentions"].([]interface{}); ok {
		for _, m := range mentions {
			if s, ok := m.(string); ok && s == a.botSelfID {
				return true
			}
		}
	}
	if reply, ok := note["reply"].(map[string]interface{}); ok {
		if replyUser, ok := reply["user"].(map[string]interface{}); ok {
			replyUserID, _ := replyUser["id"].(string)
			if replyUserID == a.botSelfID {
				return a.botUsername != "" && strings.Contains(text, "@"+a.botUsername)
			}
		}
	}
	return false
}

// Send 发送消息链（对应 send_by_session）。
// sessionID 支持三种格式: "chat%<user_id>" / "room%<room_id>" / "note%<user_id>"。
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	if a.api == nil {
		logger.Error("Misskey API 客户端未初始化")
		return nil
	}
	if chain == nil {
		return nil
	}

	text, hasAt := serializeMessageChain(chain.Chain)

	// 从 session_id 中提取用户 ID 用于缓存查询（对应 Python 的 user_id_for_cache 逻辑）
	if !hasAt && sessionID != "" {
		userIDForCache := ""
		if strings.Contains(sessionID, "%") {
			parts := strings.SplitN(sessionID, "%", 2)
			if len(parts) >= 2 {
				userIDForCache = parts[1]
			}
		}
		var userInfo map[string]interface{}
		if userIDForCache != "" {
			userInfo, _ = getUserCacheEntry(a.userCache, userIDForCache)
		}
		text = AddAtMentionIfNeeded(text, userInfo, hasAt)
	}

	// 检查是否有文件组件
	hasFileComponents := false
	for _, comp := range chain.Chain {
		if isComponentFileLike(comp) {
			hasFileComponents = true
			break
		}
	}

	if strings.TrimSpace(text) == "" && !hasFileComponents {
		logger.Warn("Misskey 消息内容为空且无文件组件，跳过发送")
		return nil
	}
	if len([]rune(text)) > a.maxMessageLength {
		text = truncateRunes(text, a.maxMessageLength) + "..."
	}

	var fileIDs []string
	var fallbackURLs []string

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 关闭文件上传时仅发送纯文本聊天/房间消息（对应 _send_text_only_message）：
	// note 会话（发帖）在文件上传关闭时与 Python 行为一致，直接跳过。
	if !a.enableFileUpload {
		return a.sendTextOnlyMessage(ctx, sessionID, text)
	}

	// 并发上传（对应 Python 的 semaphore 逻辑）
	uploadConcurrency := intVal(a.config, "misskey_upload_concurrency", defaultUploadConcurrency)
	if uploadConcurrency > maxUploadConcurrency {
		uploadConcurrency = maxUploadConcurrency
	}
	if uploadConcurrency < 1 {
		uploadConcurrency = 1
	}

	// 收集所有可能包含文件/URL 信息的组件
	var fileComponents []message.Component
	for _, comp := range chain.Chain {
		if isComponentFileLike(comp) {
			fileComponents = append(fileComponents, comp)
		}
	}
	if len(fileComponents) > maxFileUploadCount {
		logger.Warn("Misskey 文件数量超过限制 (%d > %d)，只上传前%d个文件", len(fileComponents), maxFileUploadCount, maxFileUploadCount)
		fileComponents = fileComponents[:maxFileUploadCount]
	}

	// 并发上传（限流）
	sem := make(chan struct{}, uploadConcurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, comp := range fileComponents {
		wg.Add(1)
		go func(comp message.Component) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			urlCandidate, localPath := ResolveComponentURLOrPath(comp)
			if urlCandidate == "" && localPath == "" {
				return
			}
			preferredName := ""
			if f, ok := comp.(*message.File); ok && f.Name != "" {
				preferredName = f.Name
			}
			if img, ok := comp.(*message.Image); ok && preferredName == "" {
				preferredName = img.File
			}

			fileID := ""
			// URL 上传：下载后本地上传（对应 upload_and_find_file）
			if urlCandidate != "" {
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(a.downloadTimeout+10)*time.Second)
				id, err := a.api.uploadAndFindFile(ctx, urlCandidate, preferredName, a.uploadFolder)
				cancel()
				if err == nil {
					fileID = id
				} else {
					logger.Debug("Misskey URL 上传失败 %s: %v", urlCandidate, err)
				}
			}
			// 本地文件上传（对应 upload_local_with_retries）
			if fileID == "" && localPath != "" {
				fileID = UploadLocalWithRetries(a.api, localPath, preferredName, a.uploadFolder)
			}

			mu.Lock()
			defer mu.Unlock()
			if fileID != "" {
				fileIDs = append(fileIDs, fileID)
			} else if urlCandidate != "" {
				// 上传失败时把 URL 追加进 fallbackURLs，避免文件组件静默丢失
				fallbackURLs = append(fallbackURLs, urlCandidate)
			}

			// 清理临时文件（对应 Python 的 finally 清理逻辑）
			if localPath != "" && isFileExists(localPath) && strings.HasPrefix(localPath, os.TempDir()) {
				_ = os.Remove(localPath)
				logger.Debug("Misskey 已清理临时文件: %s", localPath)
			}
		}(comp)
	}
	wg.Wait()

	// 按 session 类型分发发送
	if IsValidRoomSessionID(sessionID) {
		roomID := ExtractRoomIDFromSessionID(sessionID)
		if len(fallbackURLs) > 0 {
			text = text + "\n" + strings.Join(fallbackURLs, "\n")
		}
		payload := map[string]interface{}{"toRoomId": roomID, "text": text}
		if len(fileIDs) > 0 {
			payload["fileIds"] = fileIDs
		}
		if _, err := a.api.SendRoomMessage(ctx, payload); err != nil {
			logger.Error("Misskey 发送房间消息失败: %v", err)
		}
		return nil
	}

	if sessionID != "" && IsValidChatSessionID(sessionID) {
		userID := ExtractUserIDFromSessionID(sessionID)
		if len(fallbackURLs) > 0 {
			text = text + "\n" + strings.Join(fallbackURLs, "\n")
		}
		payload := map[string]interface{}{"toUserId": userID, "text": text}
		if len(fileIDs) > 0 {
			// 聊天消息只支持单个文件，使用 fileId 而不是 fileIds
			payload["fileId"] = fileIDs[0]
			if len(fileIDs) > 1 {
				logger.Warn("Misskey 聊天消息只支持单个文件，忽略其余 %d 个文件", len(fileIDs)-1)
			}
		}
		if _, err := a.api.SendMessage(ctx, payload); err != nil {
			logger.Error("Misskey 发送聊天消息失败: %v", err)
		}
		return nil
	}

	if sessionID == "" {
		logger.Warn("Misskey 无效的 session_id，跳过发送: %q", sessionID)
		return nil
	}

	// 回退到发帖逻辑（note% 前缀）
	userIDForCache := sessionID
	if strings.Contains(sessionID, "%") {
		userIDForCache = strings.SplitN(sessionID, "%", 2)[1]
	}
	userInfoForReply, _ := getUserCacheEntry(a.userCache, userIDForCache)

	visibility, visibleUserIDs := resolveMessageVisibility(userIDForCache, a.userCache, a.botSelfID, nil, a.defaultVisibility)
	logger.Debug("Misskey 解析可见性: visibility=%s, visible_user_ids=%v, session_id=%s, user_id_for_cache=%s", visibility, visibleUserIDs, sessionID, userIDForCache)

	fields := a.extractAdditionalFields(sessionID, chain)
	if len(fallbackURLs) > 0 {
		text = text + "\n" + strings.Join(fallbackURLs, "\n")
	}

	// 从缓存中获取原消息 ID 作为 reply_id
	replyID := ""
	if v, ok := userInfoForReply["reply_to_note_id"].(string); ok {
		replyID = v
	}

	if _, err := a.api.CreateNote(text, visibility, replyID, visibleUserIDs, fileIDs, a.localOnly, fields.cw, fields.poll, fields.renoteID, fields.channelID); err != nil {
		logger.Error("Misskey 发送帖子失败: %v", err)
	}
	return nil
}

// sendTextOnlyMessage 发送纯文本消息（无文件上传，对应 _send_text_only_message）。
// 仅处理 chat/room 会话；note 会话不发送（与 Python 行为一致）。
func (a *Adapter) sendTextOnlyMessage(ctx context.Context, sessionID, text string) error {
	if sessionID == "" {
		return nil
	}
	if IsValidUserSessionID(sessionID) {
		userID := ExtractUserIDFromSessionID(sessionID)
		payload := map[string]interface{}{"toUserId": userID, "text": text}
		if _, err := a.api.SendMessage(ctx, payload); err != nil {
			logger.Error("Misskey 发送聊天消息失败: %v", err)
		}
		return nil
	}
	if IsValidRoomSessionID(sessionID) {
		roomID := ExtractRoomIDFromSessionID(sessionID)
		payload := map[string]interface{}{"toRoomId": roomID, "text": text}
		if _, err := a.api.SendRoomMessage(ctx, payload); err != nil {
			logger.Error("Misskey 发送房间消息失败: %v", err)
		}
	}
	return nil
}

// rememberSessionEvent 记录会话最近一次发布的事件指针（带容量上限, FIFO 驱逐）。
func (a *Adapter) rememberSessionEvent(sessionID string, event *core.Event) {
	if sessionID == "" || event == nil {
		return
	}
	a.sessionEventsMu.Lock()
	defer a.sessionEventsMu.Unlock()
	if _, exists := a.sessionEvents[sessionID]; !exists && len(a.sessionEventsQ) >= sessionEventsMax {
		// 驱逐最旧会话, 保证缓存不无限增长
		oldest := a.sessionEventsQ[0]
		a.sessionEventsQ = a.sessionEventsQ[1:]
		delete(a.sessionEvents, oldest)
	}
	if _, exists := a.sessionEvents[sessionID]; !exists {
		a.sessionEventsQ = append(a.sessionEventsQ, sessionID)
	}
	a.sessionEvents[sessionID] = event
}

// sessionEvent 读取会话最近一次发布的事件指针。
func (a *Adapter) sessionEvent(sessionID string) *core.Event {
	a.sessionEventsMu.Lock()
	defer a.sessionEventsMu.Unlock()
	return a.sessionEvents[sessionID]
}

// additionalFields 从会话/消息链中提取的额外字段（对应 _extract_additional_fields）。
type additionalFields struct {
	cw        string
	poll      map[string]interface{}
	renoteID  string
	channelID string
}

// extractAdditionalFields 从会话事件与消息链中提取额外字段（对应 _extract_additional_fields）。
//   - cw：对应 Python 遍历消息链取 comp.cw。Go 组件为具体结构体、无动态属性，
//     唯一可携带任意键的是 Json 组件，故约定通过 Json.Data["cw"] 传递。
//   - poll / renote_id / channel_id：对应 Python session.extra_data；Go 侧等价物为
//     core.Event 的 Metadata["extra_data"]（SDK 事件 metadata 通道），键不存在时
//     兼容 Metadata 顶层的等价键。
func (a *Adapter) extractAdditionalFields(sessionID string, chain *message.MessageChain) additionalFields {
	fields := additionalFields{}

	if chain != nil {
		for _, comp := range chain.Chain {
			if j, ok := comp.(*message.Json); ok {
				if cw, ok := j.Data["cw"].(string); ok && cw != "" {
					fields.cw = cw
					break
				}
			}
		}
	}

	event := a.sessionEvent(sessionID)
	if event == nil || event.Metadata == nil {
		return fields
	}
	extra, ok := event.Metadata["extra_data"].(map[string]interface{})
	if !ok {
		// extra_data 键不存在时兼容顶层等价键写法
		extra = event.Metadata
	}
	if poll, ok := extra["poll"].(map[string]interface{}); ok {
		fields.poll = poll
	}
	if v, ok := extra["renote_id"].(string); ok {
		fields.renoteID = v
	}
	if v, ok := extra["channel_id"].(string); ok {
		fields.channelID = v
	}
	return fields
}

// convertMessage 将 Misskey 贴文数据转换为 AstrBotMessage（对应 convert_message）。
// 处理文本、@提及、文件、投票；renote/引用通过 reply 字段映射为 Reply 组件由上层处理。
func (a *Adapter) convertMessage(rawData map[string]interface{}) *platform.AstrBotMessage {
	senderInfo := ExtractSenderInfo(rawData, false)
	abm := CreateBaseMessage(rawData, senderInfo, a.botSelfID, false, "")
	CacheUserInfo(a.userCache, senderInfo, rawData, a.botSelfID, false)

	var messageParts []string
	rawText, _ := rawData["text"].(string)

	if rawText != "" {
		textParts, _ := ProcessAtMention(abm, rawText, a.botUsername, a.botSelfID)
		messageParts = append(messageParts, textParts...)
	}

	files, _ := rawData["files"].([]interface{})
	fileParts := processFiles(abm, files, true)
	messageParts = append(messageParts, fileParts...)

	// 投票数据（对应 _process_poll_data）
	var poll map[string]interface{}
	if p, ok := rawData["poll"].(map[string]interface{}); ok {
		poll = p
	} else if note, ok := rawData["note"].(map[string]interface{}); ok {
		if p, ok := note["poll"].(map[string]interface{}); ok {
			poll = p
		}
	}
	if len(poll) > 0 {
		if raw, ok := abm.RawMessage.(map[string]interface{}); ok {
			raw["poll"] = poll
		}
		pollText := FormatPoll(poll)
		if pollText != "" {
			abm.Message = append(abm.Message, &message.Plain{Text: pollText})
			messageParts = append(messageParts, pollText)
		}
	}

	abm.MessageStr = joinNonEmpty(messageParts)
	return abm
}

// convertChatMessage 将 Misskey 私聊消息数据转换为 AstrBotMessage（对应 convert_chat_message）。
func (a *Adapter) convertChatMessage(rawData map[string]interface{}) *platform.AstrBotMessage {
	senderInfo := ExtractSenderInfo(rawData, true)
	abm := CreateBaseMessage(rawData, senderInfo, a.botSelfID, true, "")
	CacheUserInfo(a.userCache, senderInfo, rawData, a.botSelfID, true)

	rawText, _ := rawData["text"].(string)
	if rawText != "" {
		abm.Message = append(abm.Message, &message.Plain{Text: rawText})
	}
	files, _ := rawData["files"].([]interface{})
	processFiles(abm, files, false)

	abm.MessageStr = rawText
	return abm
}

// convertRoomMessage 将 Misskey 群聊消息数据转换为 AstrBotMessage（对应 convert_room_message）。
func (a *Adapter) convertRoomMessage(rawData map[string]interface{}) *platform.AstrBotMessage {
	senderInfo := ExtractSenderInfo(rawData, true)
	roomID, _ := rawData["toRoomId"].(string)
	abm := CreateBaseMessage(rawData, senderInfo, a.botSelfID, false, roomID)
	CacheUserInfo(a.userCache, senderInfo, rawData, a.botSelfID, false)
	CacheRoomInfo(a.userCache, rawData, a.botSelfID)

	rawText, _ := rawData["text"].(string)
	var messageParts []string

	if rawText != "" {
		if a.botUsername != "" && strings.Contains(rawText, "@"+a.botUsername) {
			textParts, _ := ProcessAtMention(abm, rawText, a.botUsername, a.botSelfID)
			messageParts = append(messageParts, textParts...)
		} else {
			abm.Message = append(abm.Message, &message.Plain{Text: rawText})
			messageParts = append(messageParts, rawText)
		}
	}

	files, _ := rawData["files"].([]interface{})
	fileParts := processFiles(abm, files, true)
	messageParts = append(messageParts, fileParts...)

	abm.MessageStr = joinNonEmpty(messageParts)
	return abm
}

// joinNonEmpty 用空格拼接非空字符串（对应 Python 的 message_str 构造）。
func joinNonEmpty(parts []string) string {
	var nonEmpty []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, " ")
}

// publishMessage 将 AstrBotMessage 发布到事件总线。
func (a *Adapter) publishMessage(abm *platform.AstrBotMessage) {
	if a.EventBus == nil {
		logger.Error("Misskey 事件总线未配置，无法发布事件")
		return
	}
	chain := &message.MessageChain{Chain: abm.Message}
	ts := time.Unix(abm.Timestamp, 0)
	event := &core.Event{
		Type:       core.EventMessage,
		Message:    chain,
		MessageStr: abm.MessageStr,
		MessageObj: &core.MessageObj{
			MessageID:   abm.MessageID,
			SelfID:      abm.SelfID,
			SessionID:   abm.SessionID,
			MessageType: string(abm.Type),
			Platform:    a.Type(),
			MessageStr:  abm.MessageStr,
			RawMessage:  abm.RawMessage,
			Timestamp:   ts,
		},
		Source: core.EventSource{
			Platform:   a.Type(),
			PlatformID: a.ID(),
			SelfID:     a.botSelfID,
			SenderID:   abm.Sender.UserID,
			SenderName: abm.Sender.Nickname,
			ConvID:     abm.SessionID,
			IsGroup:    abm.Type == platform.GroupMessage,
		},
		Timestamp: ts,
		Metadata:  make(map[string]interface{}),
	}
	if err := a.EventBus.Publish(event); err != nil {
		logger.Error("Misskey 发布事件失败: %v", err)
	}
	// pipeline 会原地修改事件 (插件可写入 Metadata["extra_data"]),
	// 保留事件指针供发送帖子时读取附加字段
	a.rememberSessionEvent(abm.SessionID, event)
}
