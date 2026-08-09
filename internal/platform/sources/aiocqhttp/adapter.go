// Package aiocqhttp implements the OneBot v11 platform adapter.
// Ported from astrbot/core/platform/sources/aiocqhttp/
//
// Supported modes:
//   - Reverse WebSocket (OneBot 实现主动连入本服务的 /ws 端点；事件与 API 调用
//     都通过同一条连接传输) — 推荐，Send 通过连接下发 send_msg。
//   - HTTP POST (OneBot 实现向 / 推送事件)。无可用 WebSocket 连接时 Send 会报错。
package aiocqhttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AstrBotDevs/AstrBot/internal/core"
	"github.com/AstrBotDevs/AstrBot/internal/log"
	"github.com/AstrBotDevs/AstrBot/internal/platform"
	"github.com/AstrBotDevs/AstrBot/pkg/message"
)

var logger = log.GetDefault().WithComponent("aiocqhttp")

// Adapter implements the OneBot v11 reverse WebSocket protocol.
type Adapter struct {
	platform.BaseAdapter
	Host     string
	Port     int
	Token    string
	server   *http.Server
	EventBus *core.EventBus
	SelfID   string
	upgrader websocket.Upgrader

	mu         sync.Mutex
	conns      map[*websocket.Conn]struct{} // active reverse-WS connections
	groupConvs map[string]bool              // convID -> is group (from received events)
}

// New creates an aiocqhttp adapter from config.
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		EventBus:   eventBus,
		conns:      make(map[*websocket.Conn]struct{}),
		groupConvs: make(map[string]bool),
		upgrader: websocket.Upgrader{
			// OneBot implementations connect from arbitrary origins.
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
	a.Host, _ = config["ws_reverse_host"].(string)
	if a.Host == "" {
		a.Host = "0.0.0.0"
	}
	if port, ok := config["ws_reverse_port"].(float64); ok {
		a.Port = int(port)
	}
	if a.Port == 0 {
		a.Port = 6199
	}
	a.Token, _ = config["ws_reverse_token"].(string)
	if id, ok := config["id"].(string); ok {
		a.SelfID = id
	}
	return a
}

// SetEventBus injects the event bus. This overrides BaseAdapter.SetEventBus so
// both the embedded bus (used by PublishEvent) and this adapter's own field
// (used by handleEvent) are wired, since lifecycle creates adapters via the
// factory with a nil bus and injects it afterwards.
func (a *Adapter) SetEventBus(bus platform.EventBus) {
	a.BaseAdapter.SetEventBus(bus)
	if be, ok := bus.(*core.EventBus); ok {
		a.EventBus = be
	}
}

// Start starts the HTTP server for reverse WebSocket connections.
func (a *Adapter) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleHTTP)
	mux.HandleFunc("/ws", a.handleWebSocket)

	a.server = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", a.Host, a.Port),
		Handler: mux,
	}

	go func() {
		logger.Info("aiocqhttp(OneBot v11) adapter listening on %s:%d", a.Host, a.Port)
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("aiocqhttp server error: %v", err)
		}
	}()

	return nil
}

// Stop stops the adapter.
func (a *Adapter) Stop() error {
	if a.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return a.server.Shutdown(ctx)
	}
	return nil
}

// Send sends a message chain to a session.
//
// The target session (from core.Event.UnifiedMsgOrigin) is the convID. We
// track which convIDs are groups from received events so the correct OneBot
// action (send_group_msg vs send_private_msg) is chosen. Delivery happens over
// the reverse WebSocket connection; an error is returned when none is active.
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	segments := a.convertToCQFormat(chain)
	params := map[string]interface{}{"message": segments}
	isGroup := a.groupConvs[sessionID]
	if isGroup {
		params["group_id"] = sessionID
	} else {
		params["user_id"] = sessionID
	}
	return a.sendAction("send_msg", params)
}

// sendAction sends a OneBot v11 API call over an active reverse-WS connection.
func (a *Adapter) sendAction(action string, params map[string]interface{}) error {
	payload, err := json.Marshal(map[string]interface{}{
		"action": action,
		"params": params,
		"echo":   fmt.Sprintf("astrbot-%d", time.Now().UnixNano()),
	})
	if err != nil {
		return err
	}

	a.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(a.conns))
	for c := range a.conns {
		conns = append(conns, c)
	}
	a.mu.Unlock()

	if len(conns) == 0 {
		return fmt.Errorf("aiocqhttp: no active WebSocket connection to send %s", action)
	}

	// Try each connection; drop ones that fail so future sends pick a healthy peer.
	var lastErr error
	for _, c := range conns {
		c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := c.WriteMessage(websocket.TextMessage, payload); err != nil {
			a.removeConn(c)
			lastErr = err
			continue
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no reachable connection")
	}
	return fmt.Errorf("aiocqhttp: failed to send %s: %w", action, lastErr)
}

// handleHTTP handles HTTP POST requests from OneBot v11 implementations.
func (a *Adapter) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Verify access token
	if a.Token != "" {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+a.Token && auth != a.Token {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var event map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		logger.Error("Failed to decode event: %v", err)
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	go a.handleEvent(event)
	w.WriteHeader(http.StatusOK)
}

// handleWebSocket serves the reverse WebSocket endpoint. OneBot
// implementations connect here and both push events and receive API calls.
func (a *Adapter) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if a.Token != "" {
		auth := r.Header.Get("Authorization")
		queryToken := r.URL.Query().Get("access_token")
		if !strings.HasPrefix(auth, "Bearer "+a.Token) && auth != a.Token && queryToken != a.Token {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	conn, err := a.upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Error("WebSocket upgrade failed: %v", err)
		return
	}
	a.addConn(conn)
	logger.Info("Reverse WebSocket client connected (%s)", conn.RemoteAddr())

	defer func() {
		a.removeConn(conn)
		conn.Close()
		logger.Info("Reverse WebSocket client disconnected")
	}()

	// Heartbeat: respond to ping, and respect the peer's close/ping timeouts.
	conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		return nil
	})

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				logger.Warn("WebSocket read error: %v", err)
			}
			return
		}
		if len(data) == 0 {
			continue
		}
		// Reverse-WS frames are either events (post_type) or API responses
		// (echo) to our send_msg calls.
		var msg map[string]interface{}
		if err := json.Unmarshal(data, &msg); err != nil {
			logger.Warn("WebSocket message not JSON: %v", err)
			continue
		}
		if _, hasPost := msg["post_type"]; hasPost {
			a.handleEvent(msg)
			continue
		}
		if _, hasEcho := msg["echo"]; hasEcho {
			logger.Debug("OneBot API response: %v", msg)
		}
	}
}

func (a *Adapter) addConn(c *websocket.Conn) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conns[c] = struct{}{}
}

func (a *Adapter) removeConn(c *websocket.Conn) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.conns, c)
}

// handleEvent processes a OneBot v11 event.
func (a *Adapter) handleEvent(raw map[string]interface{}) {
	postType, _ := raw["post_type"].(string)
	if postType != "message" {
		return
	}

	// Track the bot's own ID from the event's self field so @-mentions of the
	// bot can be detected by WakingCheckStage.
	if a.SelfID == "" {
		if self, ok := raw["self"].(map[string]interface{}); ok {
			if id, ok := self["user_id"]; ok {
				a.SelfID = fmt.Sprintf("%v", id)
			}
		}
	}

	messageType, _ := raw["message_type"].(string)
	isGroup := messageType == "group"

	var senderID, senderName, convID string
	if sender, ok := raw["sender"].(map[string]interface{}); ok {
		senderID = fmt.Sprintf("%v", sender["user_id"])
		if name, ok := sender["card"].(string); ok && name != "" {
			senderName = name
		} else if nick, ok := sender["nickname"].(string); ok {
			senderName = nick
		}
	}
	if senderID == "" {
		senderID = fmt.Sprintf("%v", raw["user_id"])
	}

	if isGroup {
		convID = fmt.Sprintf("%v", raw["group_id"])
	} else {
		convID = senderID
	}
	a.mu.Lock()
	a.groupConvs[convID] = isGroup
	a.mu.Unlock()

	// Convert message segments
	msgChain := a.convertFromCQFormat(raw)

	// Publish event
	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   "aiocqhttp",
			SelfID:     a.SelfID,
			SenderID:   senderID,
			SenderName: senderName,
			ConvID:     convID,
			IsGroup:    isGroup,
		},
		Message:    msgChain,
		MessageStr: extractPlainText(msgChain),
		Timestamp:  time.Now(),
		Metadata:   make(map[string]interface{}),
	}

	if a.EventBus == nil {
		logger.Error("aiocqhttp event bus not configured; cannot publish")
		return
	}
	if err := a.EventBus.Publish(event); err != nil {
		logger.Error("Failed to publish event: %v", err)
	}
}

// convertFromCQFormat converts OneBot v11 message segments to MessageChain.
func (a *Adapter) convertFromCQFormat(raw map[string]interface{}) *message.MessageChain {
	chain := &message.MessageChain{Chain: []message.Component{}}

	segments, ok := raw["message"].([]interface{})
	if !ok {
		return chain
	}

	for _, seg := range segments {
		segMap, ok := seg.(map[string]interface{})
		if !ok {
			continue
		}
		segType, _ := segMap["type"].(string)
		data, _ := segMap["data"].(map[string]interface{})

		switch segType {
		case "text":
			text, _ := data["text"].(string)
			chain.Chain = append(chain.Chain, &message.Plain{Text: text})
		case "at":
			qq, _ := data["qq"].(string)
			if qq == "" {
				if qqFloat, ok := data["qq"].(float64); ok {
					qq = fmt.Sprintf("%v", int64(qqFloat))
				}
			}
			name, _ := data["name"].(string)
			if qq == "all" {
				chain.Chain = append(chain.Chain, &message.AtAll{})
			} else {
				chain.Chain = append(chain.Chain, &message.At{TargetID: qq, Name: name})
			}
		case "reply":
			id, _ := data["id"].(string)
			chain.Chain = append(chain.Chain, &message.Reply{MessageID: id})
		case "image":
			url, _ := data["url"].(string)
			file, _ := data["file"].(string)
			chain.Chain = append(chain.Chain, &message.Image{URL: url, File: file})
		case "record":
			url, _ := data["url"].(string)
			file, _ := data["file"].(string)
			chain.Chain = append(chain.Chain, &message.Record{URL: url, File: file})
		case "face":
			id, _ := data["id"].(string)
			chain.Chain = append(chain.Chain, &message.Face{ID: id})
		case "file":
			url, _ := data["url"].(string)
			name, _ := data["name"].(string)
			if name == "" {
				name, _ = data["file"].(string)
			}
			chain.Chain = append(chain.Chain, &message.File{URL: url, Name: name})
		case "video":
			url, _ := data["url"].(string)
			file, _ := data["file"].(string)
			chain.Chain = append(chain.Chain, &message.Video{URL: url, FileID: file})
		case "json":
			jsonStr, _ := data["data"].(string)
			var jsonData map[string]interface{}
			if jsonStr != "" {
				_ = json.Unmarshal([]byte(jsonStr), &jsonData)
			}
			if jsonData == nil {
				jsonData = make(map[string]interface{})
			}
			chain.Chain = append(chain.Chain, &message.Json{Data: jsonData})
		case "poke":
			id, _ := data["id"].(string)
			chain.Chain = append(chain.Chain, &message.Poke{Target: id})
		}
	}

	return chain
}

// convertToCQFormat converts a MessageChain to OneBot v11 message segments.
func (a *Adapter) convertToCQFormat(mc *message.MessageChain) []map[string]interface{} {
	if mc == nil {
		return nil
	}
	segments := []map[string]interface{}{}
	for _, comp := range mc.Chain {
		switch c := comp.(type) {
		case *message.Plain:
			segments = append(segments, map[string]interface{}{
				"type": "text",
				"data": map[string]interface{}{"text": c.Text},
			})
		case *message.At:
			segments = append(segments, map[string]interface{}{
				"type": "at",
				"data": map[string]interface{}{"qq": c.TargetID, "name": c.Name},
			})
		case *message.AtAll:
			segments = append(segments, map[string]interface{}{
				"type": "at",
				"data": map[string]interface{}{"qq": "all"},
			})
		case *message.Reply:
			segments = append(segments, map[string]interface{}{
				"type": "reply",
				"data": map[string]interface{}{"id": c.MessageID},
			})
		case *message.Image:
			segments = append(segments, map[string]interface{}{
				"type": "image",
				"data": map[string]interface{}{"url": c.URL, "file": c.URL},
			})
		case *message.Record:
			segments = append(segments, map[string]interface{}{
				"type": "record",
				"data": map[string]interface{}{"url": c.URL, "file": c.URL},
			})
		case *message.Face:
			segments = append(segments, map[string]interface{}{
				"type": "face",
				"data": map[string]interface{}{"id": c.ID},
			})
		case *message.File:
			segments = append(segments, map[string]interface{}{
				"type": "file",
				"data": map[string]interface{}{"url": c.URL, "name": c.Name},
			})
		case *message.Video:
			segments = append(segments, map[string]interface{}{
				"type": "video",
				"data": map[string]interface{}{"url": c.URL, "file": c.URL},
			})
		case *message.Json:
			segments = append(segments, map[string]interface{}{
				"type": "json",
				"data": map[string]interface{}{"data": c.Data},
			})
		}
	}
	return segments
}

// extractPlainText extracts plain text from a message chain.
func extractPlainText(mc *message.MessageChain) string {
	if mc == nil {
		return ""
	}
	var result string
	for _, comp := range mc.Chain {
		if plain, ok := comp.(*message.Plain); ok {
			result += plain.Text
		}
	}
	return result
}
