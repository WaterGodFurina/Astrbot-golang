// Package misskey - Misskey WebSocket Streaming 客户端。
// 移植自 astrbot/core/platform/sources/misskey/misskey_api.py 中的 StreamingClient，
// WebSocket 部分使用 gorilla/websocket 手写（go-misskey 未提供 streaming 封装）。
package misskey

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var streamingLogger = log.GetDefault().WithComponent("Misskey-WebSocket")

// StreamHandler 处理单个 streaming 事件的回调（对应 Python 的 Callable[[dict], Awaitable[None]]）。
type StreamHandler func(data map[string]interface{})

// StreamingClient 维护与 Misskey 实例 /streaming 端点的长连接。
type StreamingClient struct {
	instanceURL string
	accessToken string

	mu              sync.Mutex
	conn            *websocket.Conn
	isConnected     bool
	running         bool
	messageHandlers map[string]StreamHandler
	channels        map[string]string // channel_id -> channel_type
	desiredChannels map[string]map[string]interface{}
}

// NewStreamingClient 创建 streaming 客户端。
func NewStreamingClient(instanceURL, accessToken string) *StreamingClient {
	return &StreamingClient{
		instanceURL:     strings.TrimRight(instanceURL, "/"),
		accessToken:     accessToken,
		messageHandlers: make(map[string]StreamHandler),
		channels:        make(map[string]string),
		desiredChannels: make(map[string]map[string]interface{}),
	}
}

// streamURL 构造 streaming WebSocket 地址（对应 Python connect 中的 URL 拼接）。
func streamURL(instanceURL, token string) string {
	wsURL := strings.Replace(instanceURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
	return wsURL + "/streaming?i=" + token
}

// Connect 建立 WebSocket 连接；成功返回 true（对应 connect）。
// 重连时会重新订阅 desired_channels。
func (s *StreamingClient) Connect() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	url := streamURL(s.instanceURL, s.accessToken)
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		streamingLogger.Error("Misskey WebSocket 连接失败: %v", err)
		s.isConnected = false
		return false
	}
	s.conn = conn
	s.isConnected = true
	s.running = true
	streamingLogger.Info("Misskey WebSocket 已连接")

	// 重新订阅之前期望的频道（对应 Python connect 中的 desired_channels 重订阅）
	if len(s.desiredChannels) > 0 {
		for channelType, params := range s.desiredChannels {
			if _, err := s.subscribeChannelLocked(channelType, params); err != nil {
				streamingLogger.Warn("Misskey WebSocket 重新订阅 %s 失败: %v", channelType, err)
			}
		}
	}
	return true
}

// Disconnect 关闭连接（对应 disconnect）。
func (s *StreamingClient) Disconnect() {
	s.mu.Lock()
	s.running = false
	conn := s.conn
	s.conn = nil
	s.isConnected = false
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	streamingLogger.Info("Misskey WebSocket 连接已断开")
}

// SubscribeChannel 订阅频道并返回 channel_id（对应 subscribe_channel）。
func (s *StreamingClient) SubscribeChannel(channelType string, params map[string]interface{}) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isConnected || s.conn == nil {
		return "", fmt.Errorf("WebSocket 未连接")
	}
	s.desiredChannels[channelType] = params
	return s.subscribeChannelLocked(channelType, params)
}

// subscribeChannelLocked 在已持锁的情况下发送 connect 帧。
func (s *StreamingClient) subscribeChannelLocked(channelType string, params map[string]interface{}) (string, error) {
	if !s.isConnected || s.conn == nil {
		return "", fmt.Errorf("WebSocket 未连接")
	}
	channelID := uuid.NewString()
	if params == nil {
		params = map[string]interface{}{}
	}
	msg := map[string]interface{}{
		"type": "connect",
		"body": map[string]interface{}{
			"channel": channelType,
			"id":      channelID,
			"params":  params,
		},
	}
	if err := s.conn.WriteJSON(msg); err != nil {
		return "", err
	}
	s.channels[channelID] = channelType
	return channelID, nil
}

// UnsubscribeChannel 退订频道（对应 unsubscribe_channel）。
func (s *StreamingClient) UnsubscribeChannel(channelID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isConnected || s.conn == nil {
		return
	}
	channelType, ok := s.channels[channelID]
	if !ok {
		return
	}
	_ = s.conn.WriteJSON(map[string]interface{}{
		"type": "disconnect",
		"body": map[string]interface{}{"id": channelID},
	})
	delete(s.channels, channelID)
	// 若该频道类型不再被任何 channel_id 引用，则从期望频道中移除
	stillUsed := false
	for _, ct := range s.channels {
		if ct == channelType {
			stillUsed = true
			break
		}
	}
	if !stillUsed {
		delete(s.desiredChannels, channelType)
	}
}

// AddMessageHandler 注册事件处理器（对应 add_message_handler）。
func (s *StreamingClient) AddMessageHandler(eventType string, handler StreamHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messageHandlers[eventType] = handler
}

// Listen 阻塞监听消息直到连接关闭（对应 listen）。
func (s *StreamingClient) Listen() {
	s.mu.Lock()
	conn := s.conn
	running := s.running && s.isConnected
	s.mu.Unlock()
	if conn == nil || !running {
		streamingLogger.Error("Misskey WebSocket 未连接")
		return
	}

	// 心跳：每 30s 发送 ping（对应 websockets 库的 ping_interval=30, ping_timeout=10）。
	// 收到 pong 会重置读超时。
	pingStop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingStop:
				return
			case <-ticker.C:
				s.mu.Lock()
				conn := s.conn
				s.mu.Unlock()
				if conn == nil {
					return
				}
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
			}
		}
	}()
	defer close(pingStop)

	for {
		// 读超时由 pong 重置；若 90s 无任何数据则视为失联
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var data map[string]interface{}
		if err := json.Unmarshal(raw, &data); err != nil {
			streamingLogger.Warn("Misskey WebSocket 无法解析消息: %v", err)
			continue
		}
		s.handleMessage(data)
	}

	s.mu.Lock()
	s.isConnected = false
	s.mu.Unlock()
	streamingLogger.Warn("Misskey WebSocket 连接已关闭")
	s.Disconnect()
}

// HandleMessage 处理一条 streaming 消息（对应 _handle_message）。
// 支持 "channel" 消息（按 channel_type:event_type 或 event_type 分发）与直接消息。
func (s *StreamingClient) HandleMessage(data map[string]interface{}) {
	s.handleMessage(data)
}

// handleMessage 是 HandleMessage 的内部实现。
func (s *StreamingClient) handleMessage(data map[string]interface{}) {
	messageType, _ := data["type"].(string)
	body, _ := data["body"].(map[string]interface{})

	streamingLogger.Info("Misskey WebSocket 收到消息类型: %s %s", messageType, buildChannelSummary(messageType, body))

	s.mu.Lock()
	handlers := make(map[string]StreamHandler, len(s.messageHandlers))
	for k, v := range s.messageHandlers {
		handlers[k] = v
	}
	channels := make(map[string]string, len(s.channels))
	for k, v := range s.channels {
		channels[k] = v
	}
	s.mu.Unlock()

	if messageType == "channel" {
		channelID, _ := body["id"].(string)
		eventType, _ := body["type"].(string)
		eventBody, _ := body["body"].(map[string]interface{})
		if eventBody == nil {
			eventBody = map[string]interface{}{}
		}

		if channelType, ok := channels[channelID]; ok {
			handlerKey := channelType + ":" + eventType
			if handler, ok := handlers[handlerKey]; ok {
				handler(eventBody)
			} else if handler, ok := handlers[eventType]; ok {
				handler(eventBody)
			} else {
				if debug, ok := handlers["_debug"]; ok {
					debug(map[string]interface{}{
						"type":    eventType,
						"body":    eventBody,
						"channel": channelType,
					})
				}
			}
		}
		return
	}

	if handler, ok := handlers[messageType]; ok {
		handler(body)
		return
	}
	if debug, ok := handlers["_debug"]; ok {
		debug(data)
	}
}

// buildChannelSummary 构造用于日志的消息摘要（对应 _build_channel_summary）。
func buildChannelSummary(messageType string, body map[string]interface{}) string {
	if body == nil {
		return ""
	}
	inner := body
	if b, ok := body["body"].(map[string]interface{}); ok {
		inner = b
	}
	note, _ := inner["note"].(map[string]interface{})
	if note == nil {
		return ""
	}
	text, _ := note["text"].(string)
	noteID, _ := note["id"].(string)
	files, _ := note["files"].([]interface{})
	hasFiles := len(files) > 0
	isHidden, _ := note["isHidden"].(bool)
	username := ""
	if user, ok := note["user"].(map[string]interface{}); ok {
		username, _ = user["username"].(string)
	}
	shortText := "[no-text]"
	if text != "" {
		if len(text) > 80 {
			shortText = text[:80]
		} else {
			shortText = text
		}
	}
	return fmt.Sprintf("note_id=%s | user=%s | text=%s | files=%v | hidden=%v",
		noteID, username, shortText, hasFiles, isHidden)
}
