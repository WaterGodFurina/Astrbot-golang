// Package webchat implements a built-in web chat platform adapter.
// Ported from astrbot/core/platform/sources/webchat/
package webchat

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

var logger = log.GetDefault().WithComponent("WebChat")

// Adapter provides a web-based chat interface.
type Adapter struct {
	Host      string
	Port      int
	AuthToken string
	server    *http.Server
	EventBus  *core.EventBus
	mu        sync.Mutex
	clients   map[string]chan *message.MessageChain
	// pollClients 是 /poll 长轮询专用通道（与 /chat 的同步回复通道分离），
	// 避免同一 session 的回复被 /chat 与 /poll 两个消费者竞争抢走。
	pollClients map[string]chan *message.MessageChain
	// pollRefs 记录每个会话当前在等待的 /poll 长轮询数量，用于轮询结束后
	// 引用计数归零时清理 pollClients 通道，防止通道永久泄漏。
	pollRefs map[string]int
}

// New creates a WebChat adapter.
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		EventBus:    eventBus,
		clients:     make(map[string]chan *message.MessageChain),
		pollClients: make(map[string]chan *message.MessageChain),
		pollRefs:    make(map[string]int),
	}
	a.Host, _ = config["host"].(string)
	if a.Host == "" {
		a.Host = "0.0.0.0"
	}
	if port, ok := config["port"].(float64); ok {
		a.Port = int(port)
	}
	if a.Port == 0 {
		a.Port = 6195
	}
	a.AuthToken, _ = config["auth_token"].(string)
	return a
}

// SetEventBus injects the event bus (implements platform.EventBusSetter so the
// lifecycle wires it after construction, since the factory passes nil).
func (a *Adapter) SetEventBus(bus platform.EventBus) {
	if be, ok := bus.(*core.EventBus); ok {
		a.EventBus = be
	}
}

// Start starts the web chat server.
func (a *Adapter) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/chat", a.handleChat)
	mux.HandleFunc("/poll", a.handlePoll)

	a.server = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", a.Host, a.Port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.I18nInfo("WebChat 适配器正在监听 %s:%d", a.Host, a.Port)
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("WebChat server error: %v", err)
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

// ID returns the adapter ID.
func (a *Adapter) ID() string { return "webchat" }

// Type returns the platform type.
func (a *Adapter) Type() string { return "webchat" }

// Send sends a message to a web chat client.
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	a.mu.Lock()
	ch := a.clients[sessionID]
	pch := a.pollClients[sessionID]
	a.mu.Unlock()
	if ch == nil && pch == nil {
		return fmt.Errorf("client %s not connected", sessionID)
	}
	// 优先投递给 /chat 的同步请求（该请求持有自己的专用回复通道，不会被
	// /poll 抢走）；若 /chat 无人在等待则退化为投递给 /poll 长轮询端。
	// 两个通道均为非阻塞写入：通道满/无人消费时丢弃，绝不阻塞回复链路。
	if ch != nil {
		select {
		case ch <- chain:
			return nil
		default:
		}
	}
	if pch != nil {
		select {
		case pch <- chain:
			return nil
		default:
		}
	}
	return fmt.Errorf("send timeout")
}

// isAuthorized 校验 /chat 与 /poll 的访问身份。配置了 auth_token 时要求
// `Authorization: Bearer <token>` 或 `?token=`；未配置时仅允许本机回环来源
// 访问，避免 /chat 被任意来源盗刷 LLM 额度、/poll 窃取他人会话回复。
func (a *Adapter) isAuthorized(r *http.Request) bool {
	if token := strings.TrimSpace(a.AuthToken); token != "" {
		provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if provided == "" {
			provided = strings.TrimSpace(r.URL.Query().Get("token"))
		}
		return subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// handleChat handles incoming chat messages.
func (a *Adapter) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAuthorized(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
		Message   string `json:"message"`
		SenderID  string `json:"sender_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	if req.SessionID == "" {
		req.SessionID = fmt.Sprintf("webchat_%d", time.Now().UnixNano())
	}
	if req.SenderID == "" {
		req.SenderID = req.SessionID
	}

	// Create message chain
	chain := &message.MessageChain{
		Chain: []message.Component{&message.Plain{Text: req.Message}},
	}

	// 每个 /chat 请求使用独立的回复通道（先注册再发布事件，避免回复在
	// 注册前到达导致丢失），请求结束即移除，防止旧回复残留串扰后续请求。
	replyCh := make(chan *message.MessageChain, 1)
	a.mu.Lock()
	a.clients[req.SessionID] = replyCh
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if a.clients[req.SessionID] == replyCh {
			delete(a.clients, req.SessionID)
		}
		a.mu.Unlock()
	}()

	// Publish event
	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   "webchat",
			PlatformID: a.ID(),
			SelfID:     "webchat",
			SenderID:   req.SenderID,
			SenderName: req.SenderID,
			ConvID:     req.SessionID,
			IsGroup:    false,
		},
		Message:    chain,
		MessageStr: req.Message,
		Timestamp:  time.Now(),
		Metadata:   make(map[string]interface{}),
	}

	if a.EventBus == nil {
		logger.Error("webchat event bus not configured; cannot publish")
		http.Error(w, "Event bus not configured", http.StatusInternalServerError)
		return
	}
	if err := a.EventBus.Publish(event); err != nil {
		logger.Error("Failed to publish event: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	// Wait for the reply on the request-owned channel.
	select {
	case resp := <-replyCh:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"session_id": req.SessionID,
			"reply":      extractPlainText(resp),
		})
	case <-time.After(60 * time.Second):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"session_id": req.SessionID,
			"reply":      "Timeout waiting for response",
		})
	}
}

// handlePoll handles long-polling for responses.
func (a *Adapter) handlePoll(w http.ResponseWriter, r *http.Request) {
	if !a.isAuthorized(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		http.Error(w, "Missing session_id", http.StatusBadRequest)
		return
	}

	// /poll 使用独立的轮询通道（pollClients），与 /chat 的回复通道分离，
	// 二者互不抢数据。
	a.mu.Lock()
	if _, ok := a.pollClients[sessionID]; !ok {
		a.pollClients[sessionID] = make(chan *message.MessageChain, 10)
	}
	a.pollRefs[sessionID]++
	ch := a.pollClients[sessionID]
	a.mu.Unlock()
	// 轮询结束（收到回复、超时或客户端断开）后引用计数归零时清理通道，
	// 防止 pollClients 随任意 session_id 无限增长。
	defer func() {
		a.mu.Lock()
		a.pollRefs[sessionID]--
		if a.pollRefs[sessionID] <= 0 {
			delete(a.pollRefs, sessionID)
			delete(a.pollClients, sessionID)
		}
		a.mu.Unlock()
	}()

	select {
	case resp := <-ch:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"reply": extractPlainText(resp),
		})
	case <-r.Context().Done():
		// 客户端断开连接：直接返回，由 defer 清理轮询通道。
	case <-time.After(30 * time.Second):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"reply": nil,
		})
	}
}

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
