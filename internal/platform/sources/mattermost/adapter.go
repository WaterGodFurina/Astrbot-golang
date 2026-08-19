// Package mattermost implements a Mattermost platform adapter.
// 移植自 astrbot/core/platform/sources/mattermost/mattermost_adapter.py
package mattermost

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
	"github.com/gorilla/websocket"
)

var logger = log.GetDefault().WithComponent("Mattermost")

// Adapter 是 Mattermost 平台适配器。
// 通过 WebSocket 长连接接收消息（认证后持续监听 posted 事件），
// 通过 REST API 发送文本/图片/文件消息。
type Adapter struct {
	config         map[string]interface{}
	settings       map[string]interface{}
	EventBus       *core.EventBus
	baseURL        string
	token          string
	reconnectDelay float64

	client      *MattermostClient
	instanceID  string
	botSelfID   string
	botUsername string

	mu       sync.Mutex
	running  bool
	wsConn   *websocket.Conn
	stopCh   chan struct{}
	stopOnce sync.Once

	// 最近一次消息转换产生的附件临时文件路径（随事件发布，对应 Python 的 temporary_file_paths）
	lastTempPaths []string

	// 帖子去重（对应 _seen_post_ids / _seen_post_queue / _dedup_ttl）
	seenPostIDs   map[string]float64
	seenPostQueue [][2]interface{} // [post_id, seen_at]
	dedupTTL      float64
}

// New 创建 Mattermost 适配器。
// config 为平台实例配置（mattermost_url / mattermost_bot_token / mattermost_reconnect_delay 等）。
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	baseURL := ""
	if v, ok := config["mattermost_url"].(string); ok {
		baseURL = strings.TrimRight(v, "/")
	}
	token := ""
	if v, ok := config["mattermost_bot_token"].(string); ok {
		token = strings.TrimSpace(v)
	}
	reconnectDelay := 5.0
	if v, ok := config["mattermost_reconnect_delay"].(float64); ok {
		reconnectDelay = v
	}
	instanceID, _ := config["id"].(string)
	if instanceID == "" {
		instanceID = "mattermost"
	}

	return &Adapter{
		config:         config,
		settings:       settings,
		EventBus:       eventBus,
		baseURL:        baseURL,
		token:          token,
		reconnectDelay: reconnectDelay,
		client:         NewMattermostClient(baseURL, token),
		instanceID:     instanceID,
		stopCh:         make(chan struct{}),
		seenPostIDs:    make(map[string]float64),
		dedupTTL:       300.0,
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
func (a *Adapter) Type() string { return "mattermost" }

// Start 启动适配器：先做认证测试（GET /users/me），随后进入 WebSocket 监听循环。
func (a *Adapter) Start(ctx context.Context) error {
	if a.baseURL == "" {
		return fmt.Errorf("mattermost URL 是必需的")
	}
	if a.token == "" {
		return fmt.Errorf("mattermost bot token 是必需的")
	}

	// 认证测试（对应 run() 中的 get_me）
	me, err := a.client.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("mattermost 认证失败: %w", err)
	}
	a.botSelfID = stringVal(me["id"])
	a.botUsername = stringVal(me["username"])
	if a.botSelfID == "" {
		return fmt.Errorf("mattermost 认证成功但返回了空 user id")
	}
	logger.I18nInfo("Mattermost 认证测试通过. Bot: @%s (%s)", a.botUsername, a.botSelfID)

	a.mu.Lock()
	a.running = true
	a.mu.Unlock()

	go a.wsLoop(ctx)
	return nil
}

// Stop 停止适配器并关闭连接。
func (a *Adapter) Stop() error {
	a.mu.Lock()
	a.running = false
	conn := a.wsConn
	a.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	a.stopOnce.Do(func() { close(a.stopCh) })
	a.client.Close()
	return nil
}

// Send 发送消息链到指定频道（sessionID 即 channel_id）。
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	if chain == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := a.client.SendMessageChain(ctx, sessionID, chain)
	return err
}

// wsLoop 反复尝试建立 WebSocket 连接并监听（对应 run 中的重连循环）。
func (a *Adapter) wsLoop(ctx context.Context) {
	for {
		if !a.isRunning() {
			return
		}
		err := a.wsConnectAndListen(ctx)
		if err != nil {
			if !a.isRunning() {
				return
			}
			logger.I18nWarn("Mattermost websocket 断开: %v. %.1fs 后重连.", err, a.reconnectDelay)
		}
		if !a.isRunning() {
			return
		}
		select {
		case <-a.stopCh:
			return
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(a.reconnectDelay * float64(time.Second))):
		}
	}
}

func (a *Adapter) isRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// wsConnectAndListen 建立 WebSocket 连接并监听（对应 _ws_connect_and_listen）。
func (a *Adapter) wsConnectAndListen(ctx context.Context) error {
	wsURL := buildWSURL(a.baseURL)
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
	}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.wsConn = conn
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.wsConn = nil
		a.mu.Unlock()
		_ = conn.Close()
	}()

	// 收到 pong 时刷新读超时（gorilla 的读超时是绝对时间，控制帧不会自动重置）
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	})

	// 认证：等待服务端 hello 事件，回送 authentication_challenge（对应 mattermostdriver 的 _init_connection）
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("等待 Mattermost hello 事件失败: %w", err)
	}
	var hello map[string]interface{}
	if err := json.Unmarshal(raw, &hello); err != nil {
		logger.Debug("Mattermost websocket 收到非 JSON 文本帧: %q", string(raw))
		hello = map[string]interface{}{}
	}
	seq := 0.0
	if v, ok := hello["seq"].(float64); ok {
		seq = v
	}
	if err := conn.WriteJSON(map[string]interface{}{
		"seq":    seq + 1,
		"action": "authentication_challenge",
		"data":   map[string]interface{}{"token": a.token},
	}); err != nil {
		return err
	}

	// 心跳：每 30s 发送 ping 保活（对应 Python websockets 的 ping_interval=30）
	pingStop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingStop:
				return
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
					return
				}
			}
		}
	}()
	defer close(pingStop)

	// 读取循环：接收 posted 等事件
	for {
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(raw, &payload); err != nil {
			logger.Debug("Mattermost websocket 收到非 JSON 文本帧: %q", string(raw))
			continue
		}
		a.handleWsEvent(payload)
	}
}

// handleWsEvent 处理单个 WebSocket 事件（对应 _handle_ws_event）。
func (a *Adapter) handleWsEvent(payload map[string]interface{}) {
	if event, _ := payload["event"].(string); event != "posted" {
		if event == "hello" {
			logger.Debug("Mattermost websocket 已登录 (hello)")
		}
		return
	}

	data, ok := payload["data"].(map[string]interface{})
	if !ok {
		return
	}
	rawPost, ok := data["post"].(string)
	if !ok {
		return
	}
	post := ParseWebsocketPost(rawPost)
	if post == nil {
		return
	}

	userID := stringVal(post["user_id"])
	if userID == "" || userID == a.botSelfID {
		return
	}
	if t, _ := post["type"].(string); t != "" {
		return
	}

	postID := stringVal(post["id"])
	if postID != "" && a.isDuplicatePost(postID) {
		return
	}

	abm := a.convertMessage(post, data)
	if abm == nil {
		return
	}
	a.publishMessage(abm)
}

// isDuplicatePost 检查帖子 ID 是否重复（对应 _is_duplicate_post）。
func (a *Adapter) isDuplicatePost(postID string) bool {
	now := float64(time.Now().UnixNano()) / 1e9
	a.pruneSeenPosts(now)
	if _, ok := a.seenPostIDs[postID]; ok {
		return true
	}
	a.seenPostIDs[postID] = now
	a.seenPostQueue = append(a.seenPostQueue, [2]interface{}{postID, now})
	return false
}

// pruneSeenPosts 清理超过 TTL 的帖子 ID（对应 _prune_seen_posts）。
func (a *Adapter) pruneSeenPosts(now float64) {
	for len(a.seenPostQueue) > 0 {
		head := a.seenPostQueue[0]
		queuedPostID, _ := head[0].(string)
		seenAt, _ := head[1].(float64)
		if now-seenAt <= a.dedupTTL {
			break
		}
		a.seenPostQueue = a.seenPostQueue[1:]
		if current, ok := a.seenPostIDs[queuedPostID]; ok && current == seenAt {
			delete(a.seenPostIDs, queuedPostID)
		}
	}
}

// convertMessage 将 Mattermost post 数据转换为 AstrBotMessage（对应 convert_message）。
func (a *Adapter) convertMessage(post, data map[string]interface{}) *platform.AstrBotMessage {
	channelID := stringVal(post["channel_id"])
	if channelID == "" {
		return nil
	}
	channelType := stringVal(data["channel_type"])
	if channelType == "" {
		channelType = "O"
	}
	senderID := stringVal(post["user_id"])
	senderName := strings.TrimLeft(stringVal(data["sender_name"]), "@")
	if senderName == "" {
		senderName = senderID
	}
	messageText := stringVal(post["message"])
	var fileIDs []string
	if ids, ok := post["file_ids"].([]interface{}); ok {
		for _, id := range ids {
			if s := strings.TrimSpace(stringVal(id)); s != "" {
				fileIDs = append(fileIDs, s)
			}
		}
	}

	abm := platform.NewAstrBotMessage()
	abm.SelfID = a.botSelfID
	abm.Sender = platform.MessageMember{UserID: senderID, Nickname: senderName}
	abm.SessionID = channelID
	abm.MessageID = stringVal(post["id"])
	if abm.MessageID == "" {
		abm.MessageID = channelID
	}
	abm.RawMessage = post
	abm.Timestamp = parseTimestamp(post["create_at"])
	abm.Message = a.parseTextComponents(messageText)

	if channelType == "D" {
		abm.Type = platform.FriendMessage
	} else {
		abm.Type = platform.GroupMessage
		abm.Group = &platform.Group{GroupID: channelID}
	}

	var tempPaths []string
	if len(fileIDs) > 0 {
		attachmentComponents, paths := a.client.ParsePostAttachments(context.Background(), fileIDs)
		abm.Message = append(abm.Message, attachmentComponents...)
		tempPaths = paths
	}

	abm.MessageStr = buildMessageStr(abm.Message, messageText, a.botSelfID)
	a.mu.Lock()
	a.lastTempPaths = tempPaths
	a.mu.Unlock()
	return abm
}

// parseTextComponents 将消息文本解析为组件列表（对应 _parse_text_components）。
// 命中机器人 @提及 的位置会转换为 At 组件。
func (a *Adapter) parseTextComponents(messageText string) []message.Component {
	if messageText == "" {
		return []message.Component{}
	}
	var components []message.Component
	if a.botUsername == "" {
		return []message.Component{&message.Plain{Text: messageText}}
	}

	lastEnd := 0
	for _, span := range findMentionSpans(messageText, a.botUsername) {
		start, end := span[0], span[1]
		if start > lastEnd {
			components = append(components, &message.Plain{Text: messageText[lastEnd:start]})
		}
		components = append(components, &message.At{TargetID: a.botSelfID, Name: a.botUsername})
		lastEnd = end
	}
	if lastEnd < len(messageText) {
		components = append(components, &message.Plain{Text: messageText[lastEnd:]})
	}
	if len(components) == 0 {
		components = append(components, &message.Plain{Text: messageText})
	}
	return components
}

// buildMentionPattern 构造机器人 @提及 的匹配器（对应 _build_mention_pattern）。
// Python 使用 lookbehind/lookahead 正则，Go 的 RE2 不支持环视，
// 因此改为手动边界扫描：匹配 @username（大小写不敏感），
// 且前后不能是用户名组成字符（[A-Za-z0-9_.-]）。
// asciiLower 仅对 ASCII 大写字母做小写转换，其余字符（含多字节 UTF-8）原样保留，
// 保证转换前后字节长度一致。strings.ToLower 对个别 Unicode 字符（如 İ）小写后
// 字节长度会变化，导致在 lowerText 上取的提及下标与原文切片错位。
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func findMentionSpans(text, botUsername string) [][]int {
	if botUsername == "" || text == "" {
		return nil
	}
	lowerText := asciiLower(text)
	lowerUser := asciiLower(botUsername)
	needle := "@" + lowerUser
	var spans [][]int
	idx := 0
	for {
		at := strings.Index(lowerText[idx:], needle)
		if at < 0 {
			break
		}
		start := idx + at
		end := start + len(needle)
		// 前一个字符不能是用户名组成字符（对应 (?<![A-Za-z0-9_.-])）
		if start > 0 && isUsernameBoundaryChar(text[start-1]) {
			idx = start + 1
			continue
		}
		// 后一个字符不能是用户名组成字符（对应 (?![A-Za-z0-9_.-])）
		if end < len(text) && isUsernameBoundaryChar(text[end]) {
			idx = start + 1
			continue
		}
		spans = append(spans, []int{start, end})
		idx = end
	}
	return spans
}

// isUsernameBoundaryChar 判断字符是否属于用户名组成字符集 [A-Za-z0-9_.-]。
func isUsernameBoundaryChar(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '_' || c == '.' || c == '-'
}

// buildMessageStr 从组件生成消息文本（对应 _build_message_str）。
// 开头的自我提及（且其前无文本）会被跳过。
func buildMessageStr(components []message.Component, fallback, selfID string) string {
	var textParts []string
	leadingSelfMentionSkipped := false

	for _, comp := range components {
		switch c := comp.(type) {
		case *message.Plain:
			textParts = append(textParts, c.Text)
		case *message.At:
			isSelfMention := c.TargetID == selfID
			if !leadingSelfMentionSkipped && isSelfMention {
				leadingSelfMentionSkipped = true
				if len(textParts) == 0 || strings.TrimSpace(strings.Join(textParts, "")) == "" {
					continue
				}
			}
			mentionName := strings.TrimSpace(c.Name)
			if mentionName == "" {
				mentionName = strings.TrimSpace(c.TargetID)
			}
			if mentionName != "" {
				textParts = append(textParts, "@"+mentionName)
			}
		}
	}
	messageStr := strings.TrimSpace(strings.Join(textParts, ""))
	if messageStr == "" {
		return strings.TrimSpace(fallback)
	}
	return messageStr
}

// parseTimestamp 解析 create_at 时间戳（毫秒转秒，对应 _parse_timestamp）。
func parseTimestamp(rawValue interface{}) int64 {
	switch v := rawValue.(type) {
	case float64:
		return msToSeconds(int64(v))
	case int64:
		return msToSeconds(v)
	case int:
		return msToSeconds(int64(v))
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return msToSeconds(n)
		}
	}
	return time.Now().Unix()
}

// msToSeconds 将毫秒时间戳转换为秒；已是秒级（<=1e12）的保持不变。
func msToSeconds(ts int64) int64 {
	if ts > 1_000_000_000_000 {
		return ts / 1000
	}
	return ts
}

// stringVal 安全的 interface{} → string 转换。
func stringVal(v interface{}) string {
	switch s := v.(type) {
	case string:
		return s
	case json.Number:
		return s.String()
	case float64:
		return strconv.FormatFloat(s, 'f', -1, 64)
	default:
		return ""
	}
}

// publishMessage 将 AstrBotMessage 发布到事件总线（构造 core.Event）。
func (a *Adapter) publishMessage(abm *platform.AstrBotMessage) {
	if a.EventBus == nil {
		logger.Error("Mattermost 事件总线未配置，无法发布事件")
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
	// 附件临时文件路径（对应 Python 的 temporary_file_paths 属性）
	a.mu.Lock()
	if len(a.lastTempPaths) > 0 {
		event.Metadata["temporary_file_paths"] = a.lastTempPaths
	}
	a.mu.Unlock()
	if err := a.EventBus.Publish(event); err != nil {
		logger.Error("Mattermost 发布事件失败: %v", err)
	}
}
