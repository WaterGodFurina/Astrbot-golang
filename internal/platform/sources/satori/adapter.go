// Package satori implements a Satori protocol platform adapter.
// 1:1 移植自 astrbot/core/platform/sources/satori/（Python）:
//   - WebSocket 信令（IDENTIFY/心跳/READY/EVENT/META）与重连策略对齐 satori_adapter.py
//   - 事件转换（消息元素解析/引用消息/自消息过滤）对齐 convert_satori_message
//   - 发送（message.create 经 HTTP API）对齐 send_http_request
//   - 使用 github.com/FloatTech/satori-go 提供的事件/登录等类型模型
package satori

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	satorilib "github.com/FloatTech/satori-go"
	"github.com/gorilla/websocket"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

var logger = log.GetDefault().WithComponent("Satori")

// Satori WebSocket 信令 opcode（对齐 Python 的 op 常量）
const (
	opEvent    = 0 // EVENT 事件
	opPing     = 1 // PING 心跳
	opPong     = 2 // PONG 心跳回应
	opIdentify = 3 // IDENTIFY 鉴权
	opReady    = 4 // READY 连接就绪
	opMeta     = 5 // META 元数据
)

const (
	defaultAPIBase     = "http://localhost:5140/satori/v1"
	defaultEndpoint    = "ws://localhost:5140/satori/v1/events"
	maxRetries         = 10               // 最大重连次数（对齐 Python max_retries = 10）
	maxReconnectDelay  = 60               // 重连延迟上限（秒）
	maxWSMessageSize   = 10 * 1024 * 1024 // 10MB，对齐 Python max_size
	defaultHTTPTimeout = 30 * time.Second // 对齐 Python ClientTimeout(total=30)
)

// Adapter 是 Satori 协议平台适配器。
type Adapter struct {
	platform.BaseAdapter
	config   map[string]interface{}
	settings map[string]interface{}

	// 配置项（字段名与 Python default.py 完全一致）
	apiBaseURL        string // satori_api_base_url
	token             string // satori_token
	endpoint          string // satori_endpoint
	autoReconnect     bool   // satori_auto_reconnect
	heartbeatInterval int    // satori_heartbeat_interval
	reconnectDelay    int    // satori_reconnect_delay

	mu            sync.Mutex
	running       bool
	sequence      int64             // 事件序列号（断线重连时用于增量续传）
	logins        []satorilib.Login // 连接成功后的登录信息（logins[0] 用于发送路由）
	readyReceived bool
	ws            *websocket.Conn
	httpClient    *http.Client
	stopCh        chan struct{}
}

// New 根据平台配置创建设置 Satori 适配器。
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		BaseAdapter:       *platform.NewBaseAdapter(configID(config), "satori"),
		config:            config,
		settings:          settings,
		apiBaseURL:        defaultAPIBase,
		endpoint:          defaultEndpoint,
		autoReconnect:     true,
		heartbeatInterval: 10,
		reconnectDelay:    5,
		httpClient:        &http.Client{Timeout: defaultHTTPTimeout},
		stopCh:            make(chan struct{}),
	}
	if v, ok := config["satori_api_base_url"].(string); ok && v != "" {
		a.apiBaseURL = v
	}
	if v, ok := config["satori_token"].(string); ok {
		a.token = v
	}
	if v, ok := config["satori_endpoint"].(string); ok && v != "" {
		a.endpoint = v
	}
	if v, ok := config["satori_auto_reconnect"].(bool); ok {
		a.autoReconnect = v
	}
	if v, ok := configInt(config, "satori_heartbeat_interval"); ok && v > 0 {
		a.heartbeatInterval = v
	}
	if v, ok := configInt(config, "satori_reconnect_delay"); ok && v > 0 {
		a.reconnectDelay = v
	}
	if eventBus != nil {
		a.SetEventBus(eventBus)
	}
	return a
}

// configID 返回平台实例 id（默认 "satori"）。
func configID(config map[string]interface{}) string {
	if id, ok := config["id"].(string); ok && id != "" {
		return id
	}
	return "satori"
}

// configInt 读取整数配置（JSON 数字可能以 float64 或 int 形式出现）。
func configInt(config map[string]interface{}, key string) (int, bool) {
	switch v := config[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	case string:
		n := 0
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

// SetEventBus 注入事件总线（实现 platform.EventBusSetter）。
func (a *Adapter) SetEventBus(bus platform.EventBus) {
	a.BaseAdapter.SetEventBus(bus)
}

// ID 返回平台实例 id。
func (a *Adapter) ID() string { return a.BaseAdapter.ID() }

// Type 返回平台类型名。
func (a *Adapter) Type() string { return "satori" }

// Start 启动适配器：建立 WebSocket 连接并在后台重连（对齐 Python run()）。
func (a *Adapter) Start(ctx context.Context) error {
	a.mu.Lock()
	a.running = true
	a.mu.Unlock()
	go a.runLoop(ctx)
	return nil
}

// Stop 关闭适配器（对齐 Python terminate()）。
func (a *Adapter) Stop() error {
	a.mu.Lock()
	a.running = false
	ws := a.ws
	a.mu.Unlock()
	if ws != nil {
		_ = ws.Close()
	}
	select {
	case <-a.stopCh:
	default:
		close(a.stopCh)
	}
	logger.I18nInfo("Satori 适配器已关闭")
	return nil
}

// isRunning 报告适配器是否仍在运行。
func (a *Adapter) isRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// runLoop 连接循环：失败时按指数退避重连，最多 10 次（对齐 Python run()）。
func (a *Adapter) runLoop(ctx context.Context) {
	retryCount := 0
	for a.isRunning() {
		err := a.connectWebsocket(ctx)
		if err != nil {
			// 连接被关闭与普通异常分别记日志（对齐 Python 的 ConnectionClosed / Exception 分支）
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) || strings.Contains(strings.ToLower(err.Error()), "close") {
				logger.I18nWarn("Satori WebSocket 连接关闭: %v", err)
			} else {
				logger.I18nError("Satori WebSocket 连接失败: %v", err)
			}
			// 连接成功（收到 READY）后再断开属于正常运行中断连，不累计失败次数，
			// retryCount 语义为"连续失败次数"，避免运行期间累计断连 10 次后永久停机。
			a.mu.Lock()
			ready := a.readyReceived
			a.mu.Unlock()
			if ready {
				retryCount = 0
			} else {
				retryCount++
			}
		} else {
			retryCount = 0
		}

		if !a.isRunning() {
			break
		}
		if retryCount >= maxRetries {
			logger.I18nError("达到最大重试次数 (%d)，停止重试", maxRetries)
			break
		}
		if !a.autoReconnect {
			break
		}
		// 指数退避：delay * 2^(retry_count-1)，上限 60 秒（对齐 Python）。
		// retryCount 可能为 0（成功连接后正常断连），此时使用基础延迟。
		delay := a.reconnectDelay
		if retryCount > 1 {
			delay = a.reconnectDelay << (retryCount - 1)
		}
		if delay > maxReconnectDelay {
			delay = maxReconnectDelay
		}
		select {
		case <-ctx.Done():
			return
		case <-a.stopCh:
			return
		case <-time.After(time.Duration(delay) * time.Second):
		}
	}
}

// connectWebsocket 建立一次 WebSocket 连接并处理消息（对齐 Python connect_websocket）。
func (a *Adapter) connectWebsocket(ctx context.Context) error {
	logger.I18nInfo("Satori 适配器正在连接到 WebSocket: %s", a.endpoint)
	logger.I18nInfo("Satori 适配器 HTTP API 地址: %s", a.apiBaseURL)

	if !strings.HasPrefix(a.endpoint, "ws://") && !strings.HasPrefix(a.endpoint, "wss://") { // nosemgrep: javascript.lang.security.detect-insecure-websocket.detect-insecure-websocket -- 仅校验端点协议前缀字面量，非连接构造
		logger.I18nError("无效的WebSocket URL: %s", a.endpoint)
		return fmt.Errorf("WebSocket URL必须以ws://或wss://开头: %s", a.endpoint) // nosemgrep: javascript.lang.security.detect-insecure-websocket.detect-insecure-websocket
	}

	ws, _, err := websocket.DefaultDialer.Dial(a.endpoint, nil)
	if err != nil {
		logger.I18nError("Satori WebSocket 连接异常: %v", err)
		return err
	}
	ws.SetReadLimit(maxWSMessageSize)
	a.mu.Lock()
	a.ws = ws
	// 每次连接尝试重置 READY 标记，供 runLoop 判断本次连接是否成功建立。
	a.readyReceived = false
	a.mu.Unlock()

	// 对齐 Python 的 await asyncio.sleep(0.1)
	time.Sleep(100 * time.Millisecond)

	if err := a.sendIdentify(ws); err != nil {
		_ = ws.Close()
		return err
	}

	// 启动心跳循环（对齐 Python heartbeat_task）
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go a.heartbeatLoop(hbCtx, ws)

	for {
		select {
		case <-a.stopCh:
			_ = ws.Close()
			return nil
		case <-ctx.Done():
			_ = ws.Close()
			return nil
		default:
		}
		_, data, err := ws.ReadMessage()
		if err != nil {
			logger.I18nWarn("Satori WebSocket 连接关闭: %v", err)
			_ = ws.Close()
			return err
		}
		a.handleMessage(string(data))
	}
}

// sendIdentify 发送 IDENTIFY 信令（对齐 Python send_identify）。
func (a *Adapter) sendIdentify(ws *websocket.Conn) error {
	body := map[string]interface{}{
		"token": a.token, // 字符串
	}
	// 只有在有序列号时才添加 sn 字段
	a.mu.Lock()
	sn := a.sequence
	a.mu.Unlock()
	if sn > 0 {
		body["sn"] = sn
	}
	payload := map[string]interface{}{
		"op":   opIdentify,
		"body": body,
	}
	data, _ := json.Marshal(payload)
	if err := ws.WriteMessage(websocket.TextMessage, data); err != nil {
		logger.I18nError("发送 IDENTIFY 信令失败: %v", err)
		return err
	}
	return nil
}

// heartbeatLoop 周期性发送心跳（对齐 Python heartbeat_loop）。
func (a *Adapter) heartbeatLoop(ctx context.Context, ws *websocket.Conn) {
	ticker := time.NewTicker(time.Duration(a.heartbeatInterval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.stopCh:
			return
		case <-ticker.C:
			if ws == nil {
				return
			}
			ping := map[string]interface{}{
				"op":   opPing,
				"body": map[string]interface{}{},
			}
			data, _ := json.Marshal(ping)
			if err := ws.WriteMessage(websocket.TextMessage, data); err != nil {
				logger.I18nError("Satori WebSocket 发送心跳失败: %v", err)
				return
			}
		}
	}
}

// handleMessage 处理一条 WebSocket 信令（对齐 Python handle_message）。
func (a *Adapter) handleMessage(message string) {
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(message), &data); err != nil {
		logger.I18nError("解析 WebSocket 消息失败: %v, 消息内容: %s", err, message)
		return
	}
	opVal, hasOp := data["op"].(float64)
	if !hasOp {
		return
	}
	body, _ := data["body"].(map[string]interface{})
	if body == nil {
		body = map[string]interface{}{}
	}
	switch int(opVal) {
	case opReady: // READY
		a.handleReady(body)
	case opPong: // PONG，无需处理
	case opEvent: // EVENT
		a.handleEvent(body)
		a.updateSequence(body)
	case opMeta: // META
		a.updateSequence(body)
	}
}

// handleReady 处理 READY 信令（对齐 Python READY 分支）。
func (a *Adapter) handleReady(body map[string]interface{}) {
	a.mu.Lock()
	a.readyReceived = true
	a.mu.Unlock()

	if raw, ok := body["logins"].([]interface{}); ok {
		rawJSON, _ := json.Marshal(raw)
		var logins []satorilib.Login
		if json.Unmarshal(rawJSON, &logins) == nil {
			a.mu.Lock()
			a.logins = logins
			a.mu.Unlock()
			// 输出连接成功的 bot 信息
			for i, login := range logins {
				userID, userName := "", ""
				if login.User != nil {
					userID = login.User.ID
					userName = login.User.Name
				}
				logger.I18nInfo("Satori 连接成功 - Bot %d: platform=%s, user_id=%s, user_name=%s",
					i+1, login.Platform, userID, userName)
			}
		}
	}
	a.updateSequence(body)
}

// updateSequence 更新事件序列号（对齐 Python 各分支的 sn 处理）。
func (a *Adapter) updateSequence(body map[string]interface{}) {
	if sn, ok := body["sn"].(float64); ok && sn != 0 {
		a.mu.Lock()
		a.sequence = int64(sn)
		a.mu.Unlock()
	}
}

// handleEvent 处理 EVENT 信令（对齐 Python handle_event）。
func (a *Adapter) handleEvent(eventData map[string]interface{}) {
	a.updateSequence(eventData)

	eventType, _ := eventData["type"].(string)
	if eventType != "message-created" {
		return
	}
	message, _ := eventData["message"].(map[string]interface{})
	user, _ := eventData["user"].(map[string]interface{})
	channel, _ := eventData["channel"].(map[string]interface{})
	guild, _ := eventData["guild"].(map[string]interface{})
	login, _ := eventData["login"].(map[string]interface{})

	timestamp, hasTimestamp := int64(0), false
	if ts, ok := eventData["timestamp"].(float64); ok {
		timestamp, hasTimestamp = int64(ts), true
	}

	// 跳过机器人自己发出的消息（user.id == login.user.id）
	userID, _ := user["id"].(string)
	var loginUID string
	if lu, ok := login["user"].(map[string]interface{}); ok {
		loginUID, _ = lu["id"].(string)
	}
	if loginUID == userID {
		return
	}

	abm := convertSatoriMessage(message, user, channel, guild, login, timestamp, hasTimestamp)
	if abm != nil {
		a.handleMsg(abm)
	}
}

// handleMsg 将转换后的消息发布到事件总线。
func (a *Adapter) handleMsg(abm *platform.AstrBotMessage) {
	if err := a.PublishEvent(abm.MessageStr, abm); err != nil {
		logger.Error("Failed to publish event: %v", err)
	}
}

// Send 向指定会话发送消息链（对齐 Python send_with_adapter）。
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	if chain == nil || len(chain.Chain) == 0 {
		return nil
	}
	content := buildSatoriContent(chain)
	data := map[string]interface{}{
		"channel_id": sessionID,
		"content":    content,
	}
	platformName, userID := a.currentLogin()
	result := a.sendHTTPRequest(http.MethodPost, "/message.create", data, platformName, userID)
	if len(result) == 0 {
		return fmt.Errorf("satori: 消息发送失败 (channel_id=%s)", sessionID)
	}
	return nil
}

// React 对消息添加表情回应（reaction.create）。
func (a *Adapter) React(sessionID, messageID, emoji string) error {
	if messageID == "" || sessionID == "" {
		return fmt.Errorf("satori: reaction 缺少 message_id 或 channel_id")
	}
	// 使用 satori-go SDK 的 reaction.create 接口
	cli := satorilib.NewClient(satoriAPIRoot(a.apiBaseURL), a.token)
	return cli.CreateReaction(sessionID, messageID, emoji)
}

// satoriAPIRoot 计算 satori-go SDK 需要的 API 根地址。
// SDK 会自行拼接 /v1 前缀（api + /v1/reaction.create），
// 而 Python 的 satori_api_base_url 默认已含 /v1，因此需要去掉尾部 /v1。
func satoriAPIRoot(apiBaseURL string) string {
	return strings.TrimSuffix(strings.TrimRight(apiBaseURL, "/"), "/v1")
}

// currentLogin 返回当前登录信息（logins[0]），用于发送时指定平台路由。
func (a *Adapter) currentLogin() (platformName, userID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.logins) == 0 {
		return "", ""
	}
	login := a.logins[0]
	platformName = login.Platform
	if login.User != nil {
		userID = login.User.ID
	}
	return platformName, userID
}

// sendHTTPRequest 发起 Satori HTTP API 请求（对齐 Python send_http_request）。
// 返回 200 时解析 JSON 响应；其他情况返回空 map。
func (a *Adapter) sendHTTPRequest(method, path string, data map[string]interface{}, platformName, userID string) map[string]interface{} {
	headers := map[string]string{
		"Content-Type": "application/json",
	}
	if a.token != "" {
		headers["Authorization"] = "Bearer " + a.token
	}
	if platformName == "" || userID == "" {
		platformName, userID = a.currentLogin()
	}
	if platformName != "" && userID != "" {
		headers["satori-platform"] = platformName
		headers["satori-user-id"] = userID
	}

	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	url := strings.TrimRight(a.apiBaseURL, "/") + path

	body, _ := json.Marshal(data)
	req, err := http.NewRequest(method, url, strings.NewReader(string(body)))
	if err != nil {
		logger.I18nError("Satori HTTP 请求构造失败: %v", err)
		return map[string]interface{}{}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		logger.I18nError("Satori HTTP 请求异常: %v", err)
		return map[string]interface{}{}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var result map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&result); err == nil {
			return result
		}
		return map[string]interface{}{}
	}
	return map[string]interface{}{}
}
