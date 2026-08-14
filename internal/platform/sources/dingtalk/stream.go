package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// 钉钉重连配置 (对应 Python dingtalk_adapter.py 的 DINGTALK_RECONNECT_* 常量)。
const (
	dingtalkReconnectInitialDelay  = 10  // 初始重连延迟(秒)
	dingtalkReconnectMaxDelay      = 300 // 最大重连延迟(秒)
	dingtalkReconnectStableSeconds = 300 // 稳定运行时长(秒), 超过后重置重连次数
)

// dingtalkReconnectDelay 计算重连延迟 (对应 Python _dingtalk_reconnect_delay)。
func dingtalkReconnectDelay(retryCount int) time.Duration {
	safeRetryCount := retryCount
	if safeRetryCount < 1 {
		safeRetryCount = 1
	}
	// 饱和退避: 钳制移位指数, 防止连续失败过多后 1<<(safeRetryCount-1)
	// 溢出为 0/负数, 使退避延迟归零退化为热循环。
	exp := safeRetryCount - 1
	if exp > 30 {
		exp = 30
	}
	delay := int64(dingtalkReconnectInitialDelay) << exp
	if delay <= 0 {
		return time.Duration(dingtalkReconnectMaxDelay) * time.Second
	}
	if delay > dingtalkReconnectMaxDelay {
		delay = dingtalkReconnectMaxDelay
	}
	return time.Duration(delay) * time.Second
}

// dingFrame 对应 dingtalk_stream 的流帧结构。
type dingFrame struct {
	SpecVersion string          `json:"specVersion"`
	Type        string          `json:"type"` // System / Event / Callback / 其他
	MessageID   string          `json:"messageId"`
	Headers     dingFrameHeader `json:"headers"`
	Data        json.RawMessage `json:"data"`
}

// dingFrameHeader 流帧头。
type dingFrameHeader struct {
	Topic     string `json:"topic"`
	EventID   string `json:"eventId"`
	EventType string `json:"eventType"`
	MessageID string `json:"messageId"`
}

// dingAckFrame 对应 dingtalk_stream 的 AckMessage.to_dict()。
type dingAckFrame struct {
	Code    int               `json:"code"`
	Headers map[string]string `json:"headers"`
	Message string            `json:"message"`
	Data    string            `json:"data"` // JSON 字符串
}

// openConnectionResult 对应 gateway/connections/open 接口的返回。
type openConnectionResult struct {
	Endpoint string `json:"endpoint"`
	Ticket   string `json:"ticket"`
}

// openConnection 调用 /v1.0/gateway/connections/open 获取长连接地址
// (对应 dingtalk_stream.DingTalkStreamClient.open_connection)。
func (a *Adapter) openConnection(ctx context.Context) (*openConnectionResult, error) {
	payload := map[string]interface{}{
		"clientId":     a.clientID,
		"clientSecret": a.clientSecret,
		"subscriptions": []map[string]string{
			{"type": "CALLBACK", "topic": chatTopic},
		},
		"ua":      "dingtalk-stream-sdk-go/1.0",
		"localIp": "",
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dingtalkOpenAPI+"/v1.0/gateway/connections/open", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("打开钉钉长连接失败: HTTP %d, %s", resp.StatusCode, string(respBody))
	}
	var result openConnectionResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("解析钉钉长连接响应失败: %v", err)
	}
	if result.Endpoint == "" || result.Ticket == "" {
		return nil, fmt.Errorf("钉钉长连接响应缺少 endpoint/ticket: %s", string(respBody))
	}
	return &result, nil
}

// startStream 建立钉钉 Stream 长连接并阻塞处理事件, 直到连接断开。
func (a *Adapter) startStream(ctx context.Context) error {
	// 1. 获取长连接 endpoint
	connection, err := a.openConnection(ctx)
	if err != nil {
		return err
	}
	logger.I18nInfo("钉钉 endpoint 已获取: %s", connection.Endpoint)

	// 2. 建立 WebSocket 连接 (endpoint 后携带 ticket 参数)
	uri := connection.Endpoint
	sep := "?"
	if strings.Contains(uri, "?") {
		sep = "&"
	}
	uri += sep + "ticket=" + url.QueryEscape(connection.Ticket)

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, uri, nil)
	if err != nil {
		return fmt.Errorf("连接钉钉长连接失败: %v", err)
	}
	defer conn.Close()
	a.wsMu.Lock()
	a.wsConn = conn
	a.wsMu.Unlock()
	logger.I18nInfo("钉钉长连接已建立")

	// 3. 启动 WS 层心跳 (对应 SDK 的 keepalive)
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go a.wsKeepalive(hbCtx, conn)

	// 4. 读事件循环
	for {
		// 120 秒读超时: 服务端周期性发送 Ping, 触发自动 Pong, 超时视为连接失效
		if err := conn.SetReadDeadline(time.Now().Add(120 * time.Second)); err != nil {
			return err
		}
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if netErr, ok := err.(interface{ Timeout() bool }); ok && netErr.Timeout() {
				logger.I18nWarn("钉钉长连接读超时, 判定连接失效")
				return err
			}
			return fmt.Errorf("钉钉长连接读取失败: %v", err)
		}
		var frame dingFrame
		if err := json.Unmarshal(msg, &frame); err != nil {
			logger.I18nError("解析钉钉长连接帧失败: %v, 原始内容: %s", err, string(msg))
			continue
		}
		if done := a.handleFrame(ctx, conn, &frame); done {
			return nil
		}
	}
}

// wsKeepalive 定时发送 WS 层 Ping 控制帧 (对应 SDK 的 websockets ping_interval)。
func (a *Adapter) wsKeepalive(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				logger.Debug("钉钉长连接发送心跳失败: %v", err)
				return
			}
		}
	}
}

// sendAck 通过 WebSocket 回复确认帧 (对应 SDK 的 ack.to_dict())。
// 注意: SDK 中 ack.headers.message_id 取自已接收帧 headers 里的 messageId。
func (a *Adapter) sendAck(conn *websocket.Conn, frame *dingFrame, code int, message string) {
	messageID := frame.Headers.MessageID
	if messageID == "" {
		messageID = frame.MessageID
	}
	ack := dingAckFrame{
		Code: code,
		Headers: map[string]string{
			"messageId":   messageID,
			"contentType": "application/json",
		},
		Message: message,
		Data:    "{}",
	}
	if frame.Type == "Callback" {
		// Callback 类确认: data 为 {"response": message} 的 JSON 字符串
		ack.Data = fmt.Sprintf(`{"response": %s}`, marshalString(message))
	}
	payload, err := json.Marshal(ack)
	if err != nil {
		return
	}
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		logger.Debug("钉钉长连接发送确认失败: %v", err)
	}
}

// marshalString 将字符串编码为 JSON 字符串字面量。
func marshalString(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}

// handleFrame 路由处理不同类型的流帧 (对应 SDK 的 route_message)。
// 返回 true 表示应断开连接 (disconnect 指令)。
func (a *Adapter) handleFrame(ctx context.Context, conn *websocket.Conn, frame *dingFrame) bool {
	switch frame.Type {
	case "System":
		if frame.Headers.Topic == "disconnect" {
			logger.I18nInfo("收到钉钉 disconnect 指令, 断开当前连接")
			return true
		}
		logger.Debug("钉钉未知 System 消息: %s", string(frame.Data))
		a.sendAck(conn, frame, 200, "OK")
	case "Event":
		// 事件消息: 仅确认, 不做处理 (对应 Python 的 MyEventHandler)
		logger.Debug("钉钉事件: topic=%s eventId=%s", frame.Headers.Topic, frame.Headers.EventID)
		a.sendAck(conn, frame, 200, "OK")
	case "Callback":
		a.handleCallback(ctx, conn, frame)
	default:
		logger.Debug("钉钉未知消息类型: %q, 原始内容: %s", frame.Type, string(frame.Data))
	}
	return false
}

// handleCallback 处理回调消息 (机器人消息)。
func (a *Adapter) handleCallback(ctx context.Context, conn *websocket.Conn, frame *dingFrame) {
	if frame.Headers.Topic != chatTopic {
		logger.Debug("钉钉未知回调主题: %s", frame.Headers.Topic)
		a.sendAck(conn, frame, 200, "OK")
		return
	}
	// Callback 帧的 data 为 JSON 字符串, 需要先解析出字符串再解析为对象
	// (对应 SDK 的 json.loads(data))
	var rawData string
	if err := json.Unmarshal(frame.Data, &rawData); err != nil {
		logger.I18nError("解析钉钉回调数据失败: %v", err)
		a.sendAck(conn, frame, 400, "BAD_REQUEST")
		return
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(rawData), &data); err != nil {
		logger.I18nError("解析钉钉回调数据失败: %v", err)
		a.sendAck(conn, frame, 400, "BAD_REQUEST")
		return
	}
	// 先 ack 再异步处理: 图片/语音/文件下载可能耗时数十秒, 若同步执行完再
	// ack 会超过钉钉 stream 的 ack 时限, 触发消息重投/断连。
	a.sendAck(conn, frame, 200, "OK")
	select {
	case a.msgCh <- data:
	default:
		logger.I18nWarn("钉钉回调处理队列已满, 丢弃一条消息")
	}
}

// msgLoop 串行处理回调消息, 保证处理顺序与下载/转码等耗时操作不阻塞 WS 读循环。
func (a *Adapter) msgLoop() {
	for {
		select {
		case <-a.ctx.Done():
			return
		case data := <-a.msgCh:
			msg := parseChatbotMessage(data)
			logger.Debug("钉钉收到消息: %+v", msg)
			abm := a.convertMsg(msg)
			if abm != nil {
				a.handleMsg(abm)
			}
		}
	}
}
