// 企业微信智能机器人长连接客户端。
// 1:1 移植自 wecomai_long_connection.py：
//   - WSS 认证握手：发送 aibot_subscribe 并等待订阅响应；
//   - 心跳：按间隔发送 ping 命令；
//   - 消息帧处理：aibot_msg_callback / aibot_event_callback 分发到消息处理器；
//   - 命令发送：带 req_id 等待响应，冲突（errcode=6000）指数退避重试；
//   - 断线自动重连（指数退避 1s → 30s）。
package wecom_ai_bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WecomAIBotLongConnectionClient 企业微信智能机器人 WebSocket 长连接客户端。
type WecomAIBotLongConnectionClient struct {
	botID   string
	secret  string
	wsURL   string
	heartbeatInterval time.Duration
	messageHandler    func(map[string]interface{})

	conn  *websocket.Conn
	httpDialer *websocket.Dialer

	mu              sync.Mutex
	responseWaiters map[string]chan map[string]interface{}
	sendLock        sync.Mutex
	commandLock     sync.Mutex

	shutdownCh chan struct{}
	stopOnce   sync.Once
}

// NewWecomAIBotLongConnectionClient 构造长连接客户端。
func NewWecomAIBotLongConnectionClient(botID, secret, wsURL string, heartbeatInterval int,
	messageHandler func(map[string]interface{})) *WecomAIBotLongConnectionClient {
	interval := heartbeatInterval
	if interval < 5 {
		interval = 5
	}
	return &WecomAIBotLongConnectionClient{
		botID:             botID,
		secret:            secret,
		wsURL:             wsURL,
		heartbeatInterval: time.Duration(interval) * time.Second,
		messageHandler:    messageHandler,
		responseWaiters:   make(map[string]chan map[string]interface{}),
		shutdownCh:        make(chan struct{}),
		httpDialer: &websocket.Dialer{
			HandshakeTimeout: 15 * time.Second,
		},
	}
}

// genReqID 生成请求 ID（对应 gen_req_id，uuid4 hex）。
func genReqID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// Start 启动长连接并自动重连（对应 start）。
func (c *WecomAIBotLongConnectionClient) Start(ctx context.Context) {
	reconnectDelay := time.Second
	for {
		if c.isShutdown() {
			return
		}
		err := c.runOnce(ctx)
		if err != nil {
			logger.I18nError("[WecomAI][LongConn] 长连接异常: %v", err)
		}
		if c.isShutdown() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-c.shutdownCh:
			return
		case <-time.After(reconnectDelay):
		}
		reconnectDelay *= 2
		if reconnectDelay > 30*time.Second {
			reconnectDelay = 30 * time.Second
		}
	}
}

// isShutdown 是否已关闭。
func (c *WecomAIBotLongConnectionClient) isShutdown() bool {
	select {
	case <-c.shutdownCh:
		return true
	default:
		return false
	}
}

// runOnce 建立一次长连接并处理消息（对应 _run_once）。
func (c *WecomAIBotLongConnectionClient) runOnce(ctx context.Context) error {
	logger.I18nInfo("[WecomAI][LongConn] 正在连接: %s", c.wsURL)
	conn, _, err := c.httpDialer.DialContext(ctx, c.wsURL, nil)
	if err != nil {
		return fmt.Errorf("WebSocket 连接失败: %w", err)
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
		_ = conn.Close()
	}()

	// 订阅
	if err := c.subscribe(); err != nil {
		return err
	}
	logger.I18nInfo("[WecomAI][LongConn] 订阅成功，已建立长连接")

	// 心跳
	heartbeatStop := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(c.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if c.isShutdown() {
					return
				}
				if ok := c.SendCommand("ping", genReqID(), nil); !ok {
					logger.I18nWarn("[WecomAI][LongConn] 发送心跳失败")
					return
				}
			case <-heartbeatStop:
				return
			case <-c.shutdownCh:
				return
			}
		}
	}()

	for {
		if c.isShutdown() {
			break
		}
		_ = conn.SetReadDeadline(time.Now().Add(3 * c.heartbeatInterval))
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		c.handleTextMessage(string(data))
	}
	close(heartbeatStop)
	<-heartbeatDone
	return nil
}

// subscribe 发送 aibot_subscribe 并等待响应（对应 _subscribe）。
func (c *WecomAIBotLongConnectionClient) subscribe() error {
	reqID := genReqID()
	payload := map[string]interface{}{
		"cmd":     "aibot_subscribe",
		"headers": map[string]interface{}{"req_id": reqID},
		"body": map[string]interface{}{
			"bot_id": c.botID,
			"secret": c.secret,
		},
	}
	if err := c.sendJSON(payload); err != nil {
		return fmt.Errorf("发送订阅请求失败: %w", err)
	}

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("WebSocket 未建立")
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("订阅失败: %w", err)
	}
	var reply map[string]interface{}
	if err := json.Unmarshal(data, &reply); err != nil {
		return fmt.Errorf("订阅失败: 响应解析错误")
	}
	if errCode, ok := reply["errcode"].(float64); ok && int(errCode) != 0 {
		return fmt.Errorf("订阅失败 errcode=%v errmsg=%v", reply["errcode"], reply["errmsg"])
	}
	return nil
}

// handleTextMessage 处理收到的文本消息（对应 _handle_text_message）。
func (c *WecomAIBotLongConnectionClient) handleTextMessage(text string) {
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		logger.I18nWarn("[WecomAI][LongConn] 收到非 JSON 消息: %s", text)
		return
	}

	// 若 req_id 有等待者则先完成响应
	headers, _ := payload["headers"].(map[string]interface{})
	if reqID, ok := headers["req_id"].(string); ok && reqID != "" {
		c.mu.Lock()
		waiter := c.responseWaiters[reqID]
		c.mu.Unlock()
		if waiter != nil {
			select {
			case waiter <- payload:
			default:
			}
			return
		}
	}

	cmd, _ := payload["cmd"].(string)
	if cmd == "aibot_msg_callback" || cmd == "aibot_event_callback" {
		if c.messageHandler != nil {
			c.messageHandler(payload)
		}
		return
	}

	if errCode, ok := payload["errcode"].(float64); ok && int(errCode) != 0 {
		logger.I18nWarn("[WecomAI][LongConn] 服务端返回错误: errcode=%v errmsg=%v", payload["errcode"], payload["errmsg"])
	}
}

// SendCommand 发送长连接命令（对应 send_command）。
func (c *WecomAIBotLongConnectionClient) SendCommand(cmd, reqID string, body map[string]interface{}) bool {
	headers := map[string]interface{}{"req_id": reqID}
	payload := map[string]interface{}{"cmd": cmd, "headers": headers}
	if body != nil {
		payload["body"] = body
	}

	c.commandLock.Lock()
	defer c.commandLock.Unlock()

	const maxRetries = 3
	for attempt := 0; attempt <= maxRetries; attempt++ {
		response := c.sendAndWaitResponse(reqID, payload)
		if response == nil {
			if attempt < maxRetries {
				time.Sleep(backoff(attempt))
				continue
			}
			return false
		}
		errCode, _ := response["errcode"].(float64)
		if int(errCode) == 0 {
			if _, ok := response["errcode"]; !ok {
				return true
			}
			return true
		}
		if int(errCode) == 6000 && attempt < maxRetries {
			// 命令冲突，退避重试
			d := backoff(attempt)
			logger.I18nWarn("[WecomAI][LongConn] 命令冲突(errcode=6000)，将重试。cmd=%s req_id=%s attempt=%d", cmd, reqID, attempt+1)
			time.Sleep(d)
			continue
		}
		logger.I18nWarn("[WecomAI][LongConn] 命令失败: cmd=%s req_id=%s errcode=%v errmsg=%v", cmd, reqID, response["errcode"], response["errmsg"])
		return false
	}
	return false
}

// sendAndWaitResponse 发送命令并等待响应（对应 _send_and_wait_response）。
func (c *WecomAIBotLongConnectionClient) sendAndWaitResponse(reqID string, payload map[string]interface{}) map[string]interface{} {
	waiter := make(chan map[string]interface{}, 1)
	c.mu.Lock()
	c.responseWaiters[reqID] = waiter
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.responseWaiters, reqID)
		c.mu.Unlock()
	}()

	if err := c.sendJSON(payload); err != nil {
		logger.I18nWarn("[WecomAI][LongConn] 发送命令失败: %v", err)
		return nil
	}
	select {
	case resp := <-waiter:
		return resp
	case <-time.After(10 * time.Second):
		logger.I18nWarn("[WecomAI][LongConn] 等待命令响应超时: cmd=%v req_id=%s", payload["cmd"], reqID)
		return nil
	case <-c.shutdownCh:
		return nil
	}
}

// sendJSON 发送 JSON 消息（对应 _send_json）。
func (c *WecomAIBotLongConnectionClient) sendJSON(payload map[string]interface{}) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("长连接尚未建立")
	}
	c.sendLock.Lock()
	defer c.sendLock.Unlock()
	return conn.WriteJSON(payload)
}

// Shutdown 关闭长连接（对应 shutdown）。
func (c *WecomAIBotLongConnectionClient) Shutdown() {
	c.stopOnce.Do(func() {
		close(c.shutdownCh)
	})
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// backoff 计算第 attempt 次重试的退避时间（0.2 * 2^attempt，上限 2s）。
func backoff(attempt int) time.Duration {
	d := 200 * time.Millisecond
	for i := 0; i < attempt; i++ {
		d *= 2
		if d >= 2*time.Second {
			return 2 * time.Second
		}
	}
	return d
}
