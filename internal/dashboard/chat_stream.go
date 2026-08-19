// SSE chat streaming for the WebUI /chat page.
//
// The WebUI chat page sends a message via POST /api/v1/chat and expects a
// text/event-stream response (events: session_id -> user_message_saved ->
// run_started -> plain (streaming text) -> complete -> end). This mirrors
// astrbot/dashboard/services/chat_service.py build_chat_stream.
//
// To reuse the existing message pipeline we run the user message through the
// "default" pipeline scheduler as a webchat-platform event and register a
// lightweight platform adapter ("dashboard_chat") that captures the reply
// chain sent by the pipeline's RespondStage / streamSender and forwards it to
// a per-session SSE subscriber.
package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
	"github.com/gorilla/websocket"
)

// chatStreamAdapter captures reply chains from the pipeline for a chat session.
// It implements platform.PlatformAdapter so platformMgr.Send("dashboard_chat",
// sessionID, chain) lands here and fans out to SSE subscribers.
//
// 同一 session 的多次并发发送可能同时存在多个订阅者。pipeline 的 Send 契约
// 只携带 sessionID、无法区分 reply 属于哪一次 run，因此按会话内注册顺序
// （seq）路由：reply 一定产生在对应 run 的 dispatch 过程中，而同一会话的
// run 在事件总线上串行分发，故取"仍存活且 seq 最小"的订阅者即当前正在
// dispatch 的 run。订阅者在收到 done 后立即退订，防止下一个 run 的回复
// 落进自己的 channel。
type chatStreamAdapter struct {
	mu          sync.Mutex
	seq         uint64
	subscribers map[string]map[uint64]chan *message.MessageChain // sessionID -> seq -> ch
}

func newChatStreamAdapter() *chatStreamAdapter {
	return &chatStreamAdapter{subscribers: make(map[string]map[uint64]chan *message.MessageChain)}
}

func (a *chatStreamAdapter) ID() string   { return "dashboard_chat" }
func (a *chatStreamAdapter) Type() string { return "dashboard_chat" }

// Start/Stop are no-ops: the adapter is purely a reply sink.
func (a *chatStreamAdapter) Start(ctx context.Context) error { return nil }
func (a *chatStreamAdapter) Stop() error                     { return nil }

// Send forwards a reply chain to the earliest-registered still-active
// subscriber of the session. The channel set is copied under the lock so a
// concurrent unsubscribe (delete) can never race the map iteration.
func (a *chatStreamAdapter) Send(sessionID string, chain *message.MessageChain) error {
	a.mu.Lock()
	var bestSeq uint64
	var target chan *message.MessageChain
	for seq, ch := range a.subscribers[sessionID] {
		if target == nil || seq < bestSeq {
			bestSeq = seq
			target = ch
		}
	}
	a.mu.Unlock()
	if target == nil {
		return nil
	}
	select {
	case target <- chain:
	default:
		// Subscriber not draining; drop rather than block the pipeline.
	}
	return nil
}

func (a *chatStreamAdapter) subscribe(sessionID string) (chan *message.MessageChain, uint64) {
	ch := make(chan *message.MessageChain, 64)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.subscribers[sessionID] == nil {
		a.subscribers[sessionID] = make(map[uint64]chan *message.MessageChain)
	}
	a.seq++
	a.subscribers[sessionID][a.seq] = ch
	return ch, a.seq
}

func (a *chatStreamAdapter) unsubscribe(sessionID string, seq uint64, ch chan *message.MessageChain) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if subs, ok := a.subscribers[sessionID]; ok {
		if subs[seq] == ch {
			delete(subs, seq)
		}
		if len(subs) == 0 {
			delete(a.subscribers, sessionID)
		}
	}
}

// handleChatSend streams a chat reply over SSE.
// POST /api/v1/chat  body: {session_id, message:[parts], selected_provider, selected_model, flags}
func (s *Server) handleChatSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID        string                   `json:"session_id"`
		ConversationID   string                   `json:"conversation_id"`
		Message          []map[string]interface{} `json:"message"`
		Files            []interface{}            `json:"files"`
		SelectedProvider string                   `json:"selected_provider"`
		SelectedModel    string                   `json:"selected_model"`
		Flags            map[string]interface{}   `json:"flags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("Invalid JSON body"))
		return
	}

	sessionID := body.SessionID
	if sessionID == "" {
		sessionID = body.ConversationID
	}
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, apiError("Missing session_id"))
		return
	}
	parts := body.Message
	if len(parts) == 0 {
		parts = filePartsToMessage(body.Files)
	}
	text := plainTextFromParts(parts)
	if text == "" {
		writeJSON(w, http.StatusBadRequest, apiError("Message content is empty"))
		return
	}

	// Persist the user message into the chat session store (WebUI history).
	savedUserID := fmt.Sprintf("u_%d", time.Now().UnixNano())
	userRecord := map[string]interface{}{
		"id":          savedUserID,
		"session_id":  sessionID,
		"sender_id":   "dashboard",
		"sender_name": "dashboard",
		"role":        "user",
		"type":        "user",
		"content":     map[string]interface{}{"type": "user", "message": parts},
		"created_at":  time.Now().Format(time.RFC3339Nano),
	}
	s.chat.appendMessage(sessionID, userRecord)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, apiError("streaming not supported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// session_id event first (matches Python build_chat_stream).
	sendSSE(w, flusher, map[string]interface{}{
		"type":       "session_id",
		"data":       nil,
		"session_id": sessionID,
	})
	sendSSE(w, flusher, map[string]interface{}{
		"type": "user_message_saved",
		"data": map[string]interface{}{
			"id":                savedUserID,
			"created_at":        time.Now().Format(time.RFC3339Nano),
			"llm_checkpoint_id": fmt.Sprintf("c_%d", time.Now().UnixNano()),
		},
	})
	runID := fmt.Sprintf("r_%d", time.Now().UnixNano())
	sendSSE(w, flusher, map[string]interface{}{
		"type": "run_started",
		"data": map[string]interface{}{"run_id": runID},
	})

	// Subscribe to the pipeline reply for this session. runID 标识本次 run，
	// 即使同一 session 并发发送也不会把别的请求的回复累积进来。
	ch, subSeq := s.chatAdapter.subscribe(sessionID)
	defer s.chatAdapter.unsubscribe(sessionID, subSeq, ch)

	// Run the user message through the pipeline; done is closed when the
	// event finishes processing.
	done := s.processChatEvent(r.Context(), sessionID, runID, text, body.SelectedProvider, body.SelectedModel, body.Flags)
	if done == nil {
		sendSSE(w, flusher, map[string]interface{}{"type": "error", "data": "对话管道不可用"})
		sendSSE(w, flusher, map[string]interface{}{"type": "end", "data": nil})
		return
	}

	// Stream reply chains until the pipeline finishes or the client disconnects.
	var full strings.Builder
	deadline := time.After(300 * time.Second)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline:
			sendSSE(w, flusher, map[string]interface{}{"type": "end", "data": nil})
			return
		case <-done:
			// 本 run 的回复在 done 触发前已全部送入 ch；先退订再排空，
			// 避免同一 session 下一个 run 的回复落到本 channel。
			s.chatAdapter.unsubscribe(sessionID, subSeq, ch)
			for {
				select {
				case chain := <-ch:
					emitChainSSE(w, flusher, &full, chain)
				default:
					goto done
				}
			}
		done:
			if full.Len() > 0 {
				// Persist the bot reply into the chat session store.
				botID := fmt.Sprintf("b_%d", time.Now().UnixNano())
				botRecord := map[string]interface{}{
					"id":          botID,
					"session_id":  sessionID,
					"sender_id":   "bot",
					"sender_name": "bot",
					"role":        "assistant",
					"type":        "bot",
					"content": map[string]interface{}{
						"type":    "bot",
						"message": []map[string]interface{}{{"type": "plain", "text": full.String()}},
					},
					"created_at": time.Now().Format(time.RFC3339Nano),
				}
				s.chat.appendMessage(sessionID, botRecord)
				sendSSE(w, flusher, map[string]interface{}{
					"type": "message_saved",
					"data": map[string]interface{}{"id": botID, "created_at": time.Now().Format(time.RFC3339Nano)},
				})
				sendSSE(w, flusher, map[string]interface{}{"type": "complete", "data": full.String()})
			}
			sendSSE(w, flusher, map[string]interface{}{"type": "end", "data": nil})
			return
		case chain := <-ch:
			emitChainSSE(w, flusher, &full, chain)
		}
	}
}

// emitChainSSE sends SSE frames for a reply chain: media components first
// (images as data URLs), then the plain text.
func emitChainSSE(w http.ResponseWriter, flusher http.Flusher, full *strings.Builder, chain *message.MessageChain) {
	for _, data := range chainImageDataURLs(chain) {
		sendSSE(w, flusher, map[string]interface{}{"type": "image", "data": data})
	}
	if t := chainPlainText(chain); t != "" {
		full.WriteString(t)
		sendSSE(w, flusher, map[string]interface{}{"type": "plain", "data": t, "chain_type": "text"})
	}
}

// processChatEvent runs the message through the "default" pipeline scheduler
// as a webchat-platform event and returns a channel closed when processing
// completes (nil if the pipeline is unavailable).
//
// Preferred path: enqueue on the event bus so dashboard chat shares the same
// single-goroutine pipeline as platform messages (the bus never runs two
// ProcessStage invocations concurrently). Completion is observed via a
// core.PipelineDone signal that the bus closes once the event is dispatched.
// Fallback: when the bus is unavailable (queue full / no scheduler), run the
// event through the scheduler directly in a goroutine.
func (s *Server) processChatEvent(ctx context.Context, sessionID, runID, text, providerID, model string, flags map[string]interface{}) <-chan struct{} {
	bus, ok := s.eventBus.(*core.EventBus)
	if !ok || bus == nil {
		return nil
	}
	chain := message.NewMessageChain(&message.Plain{Text: text})
	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   "dashboard_chat",
			SelfID:     "dashboard_chat",
			SenderID:   "dashboard",
			SenderName: "dashboard",
			ConvID:     sessionID,
			IsGroup:    false,
		},
		Message:    chain,
		MessageStr: text,
		PlainText:  text,
		Timestamp:  time.Now(),
		// 不要预置 IsAtOrWakeCommand=true：WakingCheckStage 对已置位的
		// 事件直接跳过前缀剥离，导致 "/am_status" 的 "/" 不被剥掉、命令
		// handler 匹配失败（走向 LLM 闲聊）。置 false 让 WakingCheck 正常
		// 处理："/" 前缀剥离后命中命令；普通文本经好友自动唤醒（wakeByFriend）
		// 触发 LLM（CallLLM=true 兜底）。
		IsAtOrWakeCommand: false,
		CallLLM:           true,
		Metadata:          map[string]interface{}{},
		Ctx:               ctx,
	}
	if runID != "" {
		event.Metadata["run_id"] = runID
	}
	if providerID != "" {
		event.Metadata["selected_provider"] = providerID
	}
	if model != "" {
		event.Metadata["selected_model"] = model
	}
	if len(flags) > 0 {
		event.Metadata["flags"] = flags
	}

	done := core.NewPipelineDone()
	event.Metadata[core.MetadataPipelineDone] = done
	if err := bus.Publish(event); err == nil {
		return done.Done()
	}

	// Bus unavailable (queue full) or scheduler missing: fall back to running
	// the event through the scheduler synchronously in a goroutine.
	scheduler := bus.GetScheduler("default")
	if scheduler == nil {
		return nil
	}
	go func() {
		defer done.Signal()
		if _, err := scheduler.Process(ctx, event); err != nil {
			logger.Error("dashboard chat pipeline failed: %v", err)
		}
	}()
	return done.Done()
}

// sendSSE writes one SSE data frame.
func sendSSE(w http.ResponseWriter, flusher http.Flusher, payload map[string]interface{}) {
	data, _ := json.Marshal(payload)
	// #nosec no-fprintf-to-responsewriter -- SSE 流：data 帧内容为 JSON 序列化负载，
	// Content-Type 为 text/event-stream，客户端按 JSON 解析，非 HTML 渲染上下文，无 XSS。
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data) // nosemgrep: go.lang.security.audit.xss.no-fprintf-to-responsewriter.no-fprintf-to-responsewriter
	flusher.Flush()
}

// chainPlainText extracts the plain text of a message chain.
func chainPlainText(chain *message.MessageChain) string {
	if chain == nil {
		return ""
	}
	var b strings.Builder
	for _, comp := range chain.Chain {
		if plain, ok := comp.(*message.Plain); ok {
			b.WriteString(plain.Text)
		}
	}
	return b.String()
}

// chainImageDataURLs returns the displayable image sources in a chain: base64
// payloads become data URLs (no file service needed), https URLs pass through.
func chainImageDataURLs(chain *message.MessageChain) []string {
	if chain == nil {
		return nil
	}
	var out []string
	for _, comp := range chain.Chain {
		img, ok := comp.(*message.Image)
		if !ok {
			continue
		}
		switch {
		case img.Base64 != "":
			out = append(out, "data:image/png;base64,"+img.Base64)
		case img.URL != "":
			out = append(out, img.URL)
		}
	}
	return out
}

// plainTextFromParts joins the plain text of message parts.
func plainTextFromParts(parts []map[string]interface{}) string {
	var b strings.Builder
	for _, p := range parts {
		switch t, _ := p["type"].(string); t {
		case "plain", "":
			text, _ := p["text"].(string)
			b.WriteString(text)
		}
	}
	return b.String()
}

// filePartsToMessage converts a files array (path strings) into plain text
// placeholders (best effort; attachment parsing is out of scope here).
func filePartsToMessage(files []interface{}) []map[string]interface{} {
	if len(files) == 0 {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(files))
	for _, f := range files {
		if s, ok := f.(string); ok {
			out = append(out, map[string]interface{}{"type": "plain", "text": "[FILE] " + s})
		}
	}
	return out
}

// compile-time interface check: chatStreamAdapter must satisfy
// platform.PlatformAdapter.
var _ platform.PlatformAdapter = (*chatStreamAdapter)(nil)

// wsUpgrader upgrades HTTP connections to WebSocket.
var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Origin 白名单：仅允许同源连接（浏览器 WebSocket 无法设置自定义
	// Authorization 头，token 只能走 query，故用 Origin 校验防跨站 CSWSH）。
	// 无 Origin 头的非浏览器客户端（curl/脚本）放行。
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		o, err := url.Parse(origin)
		if err != nil {
			return false
		}
		return o.Host == r.Host || o.Host == "localhost:6185" || o.Host == "127.0.0.1:6185"
	},
}

// wsClient wraps a live websocket connection. gorilla websocket forbids
// concurrent writers, so every frame is serialized behind writeMu; ctx is
// cancelled when the connection closes (or fails to write) so in-flight
// pipeline runs stop instead of living until the 300s deadline.
type wsClient struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc

	// runs tracks the cancel function of each in-flight chat run keyed by
	// message_id, so an interrupt frame can cancel a specific (or all) run.
	runMu sync.Mutex
	runs  map[string]context.CancelFunc
}

// wsPingInterval is how often the server sends a WS Ping to keep idle
// connections within the 10-minute read deadline (browsers only reply pong to
// a received ping). Exposed as a var so tests can shorten it.
var wsPingInterval = 4 * time.Minute

// handleUnifiedChatWS serves the WebUI's websocket chat transport
// (GET /api/v1/unified-chat/ws?token=...). The client sends messages shaped
// like {ct:"chat", t:"send", session_id, message_id, message:[parts], flags,
// selected_provider, selected_model} and receives JSON frames with the same
// event types as the SSE transport (session_id / user_message_saved /
// run_started / plain / complete / end), each carrying the message_id.
func (s *Server) handleUnifiedChatWS(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if s.auth == nil || !s.auth.IsAuthenticated(token) {
		writeJSON(w, http.StatusUnauthorized, apiError("未认证"))
		return
	}
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.I18nWarn("WebSocket 升级失败: %v", err)
		return
	}
	wsCtx, wsCancel := context.WithCancel(context.Background())
	client := &wsClient{conn: conn, ctx: wsCtx, cancel: wsCancel, runs: make(map[string]context.CancelFunc)}
	defer wsCancel()
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
		return nil
	})

	// 服务端每 4 分钟主动发一次 Ping，否则空闲连接收不到任何帧、读侧
	// 10 分钟 ReadDeadline 一到就被强制断开。
	stopPing := make(chan struct{})
	defer close(stopPing)
	go func() {
		ticker := time.NewTicker(wsPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-ticker.C:
				client.writeMu.Lock()
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
					client.cancel()
				}
				client.writeMu.Unlock()
			}
		}
	}()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var msg struct {
			CT               string                   `json:"ct"`
			T                string                   `json:"t"`
			SessionID        string                   `json:"session_id"`
			MessageID        string                   `json:"message_id"`
			Message          []map[string]interface{} `json:"message"`
			Flags            map[string]interface{}   `json:"flags"`
			SelectedProvider string                   `json:"selected_provider"`
			SelectedModel    string                   `json:"selected_model"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		if msg.CT != "chat" {
			continue
		}

		switch msg.T {
		case "bind":
			// Acknowledge the session bind (Python sends session_bound).
			if msg.SessionID == "" {
				s.wsSend(client, map[string]interface{}{
					"ct": "chat", "t": "error", "data": "session_id is required",
					"code": "INVALID_MESSAGE_FORMAT",
				})
				continue
			}
			s.wsSend(client, map[string]interface{}{
				"ct": "chat", "type": "session_bound", "session_id": msg.SessionID,
				"message_id": fmt.Sprintf("ws_sub_%d", time.Now().UnixNano()),
			})

		case "interrupt":
			// Cancel the in-flight pipeline run(s) for this connection: the
			// matching message_id run if provided, otherwise every active run.
			// The run's context is derived from this connection, so cancelling
			// it propagates to the LLM provider call and stops it early.
			client.runMu.Lock()
			if msg.MessageID != "" {
				if fn, ok := client.runs[msg.MessageID]; ok {
					fn()
				}
			} else {
				for _, fn := range client.runs {
					fn()
				}
			}
			client.runMu.Unlock()
			s.wsSend(client, map[string]interface{}{
				"ct": "chat", "t": "error", "data": "INTERRUPTED",
				"code":       "INTERRUPTED",
				"message_id": msg.MessageID,
			})

		case "send":
			if len(msg.Message) == 0 {
				s.wsSend(client, map[string]interface{}{
					"ct": "chat", "t": "error", "data": "Message content is empty",
					"code": "INVALID_MESSAGE_FORMAT", "message_id": msg.MessageID,
				})
				continue
			}
			if msg.SessionID == "" {
				s.wsSend(client, map[string]interface{}{
					"ct": "chat", "t": "error", "data": "session_id is required",
					"code": "INVALID_MESSAGE_FORMAT", "message_id": msg.MessageID,
				})
				continue
			}
			go s.handleWSMessageSend(client, msg)
		}
	}
}

// wsSend writes one JSON frame to the websocket. Writes are serialized per
// connection (gorilla forbids concurrent writers); a failed write cancels the
// connection lifecycle so pending pipeline runs are torn down.
func (s *Server) wsSend(c *wsClient, payload map[string]interface{}) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.WriteJSON(payload); err != nil {
		c.cancel()
		logger.Debug("ws send failed: %v", err)
	}
}

// handleWSMessageSend runs a chat message through the pipeline and streams the
// reply events back over the websocket (per message_id).
func (s *Server) handleWSMessageSend(c *wsClient, msg struct {
	CT               string                   `json:"ct"`
	T                string                   `json:"t"`
	SessionID        string                   `json:"session_id"`
	MessageID        string                   `json:"message_id"`
	Message          []map[string]interface{} `json:"message"`
	Flags            map[string]interface{}   `json:"flags"`
	SelectedProvider string                   `json:"selected_provider"`
	SelectedModel    string                   `json:"selected_model"`
}) {
	sessionID := msg.SessionID
	text := plainTextFromParts(msg.Message)
	messageID := msg.MessageID
	if messageID == "" {
		messageID = fmt.Sprintf("r_%d", time.Now().UnixNano())
	}

	// Persist the user message.
	userRecord := map[string]interface{}{
		"id":          fmt.Sprintf("u_%d", time.Now().UnixNano()),
		"session_id":  sessionID,
		"sender_id":   "dashboard",
		"sender_name": "dashboard",
		"role":        "user",
		"type":        "user",
		"content":     map[string]interface{}{"type": "user", "message": msg.Message},
		"created_at":  time.Now().Format(time.RFC3339Nano),
	}
	s.chat.appendMessage(sessionID, userRecord)

	llmCheckpointID := fmt.Sprintf("c_%d", time.Now().UnixNano())
	s.wsSend(c, map[string]interface{}{
		"ct": "chat", "type": "user_message_saved",
		"data":       map[string]interface{}{"id": userRecord["id"], "created_at": userRecord["created_at"], "llm_checkpoint_id": llmCheckpointID},
		"message_id": messageID,
	})
	s.wsSend(c, map[string]interface{}{
		"ct": "chat", "type": "run_started",
		"data":       map[string]interface{}{"run_id": messageID},
		"message_id": messageID,
	})

	ch, subSeq := s.chatAdapter.subscribe(sessionID)
	defer s.chatAdapter.unsubscribe(sessionID, subSeq, ch)

	// Derive a per-run context from the connection so an interrupt can cancel
	// this specific run (registered by message_id) without tearing down the
	// whole connection.
	runCtx, runCancel := context.WithCancel(c.ctx)
	c.runMu.Lock()
	c.runs[messageID] = runCancel
	c.runMu.Unlock()
	defer func() {
		c.runMu.Lock()
		delete(c.runs, messageID)
		c.runMu.Unlock()
		runCancel()
	}()

	done := s.processChatEvent(runCtx, sessionID, messageID, text, msg.SelectedProvider, msg.SelectedModel, msg.Flags)
	if done == nil {
		s.wsSend(c, map[string]interface{}{"ct": "chat", "type": "error", "data": "对话管道不可用", "message_id": messageID})
		s.wsSend(c, map[string]interface{}{"ct": "chat", "type": "end", "data": nil, "message_id": messageID})
		return
	}

	var full strings.Builder
	deadline := time.After(300 * time.Second)
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-deadline:
			s.wsSend(c, map[string]interface{}{"ct": "chat", "type": "end", "data": nil, "message_id": messageID})
			return
		case <-done:
			s.chatAdapter.unsubscribe(sessionID, subSeq, ch)
			for {
				select {
				case chain := <-ch:
					s.emitChainWS(c, &full, chain, messageID)
				default:
					goto done
				}
			}
		done:
			if full.Len() > 0 {
				botRecord := map[string]interface{}{
					"id":          fmt.Sprintf("b_%d", time.Now().UnixNano()),
					"session_id":  sessionID,
					"sender_id":   "bot",
					"sender_name": "bot",
					"role":        "assistant",
					"type":        "bot",
					"content": map[string]interface{}{
						"type":    "bot",
						"message": []map[string]interface{}{{"type": "plain", "text": full.String()}},
					},
					"created_at": time.Now().Format(time.RFC3339Nano),
				}
				s.chat.appendMessage(sessionID, botRecord)
				s.wsSend(c, map[string]interface{}{"ct": "chat", "type": "message_saved", "data": map[string]interface{}{"id": botRecord["id"], "created_at": botRecord["created_at"]}, "message_id": messageID})
				s.wsSend(c, map[string]interface{}{"ct": "chat", "type": "complete", "data": full.String(), "message_id": messageID})
			}
			s.wsSend(c, map[string]interface{}{"ct": "chat", "type": "end", "data": nil, "message_id": messageID})
			return
		case chain := <-ch:
			s.emitChainWS(c, &full, chain, messageID)
		}
	}
}

// emitChainWS sends WebSocket frames for a reply chain: media components first
// (images as data URLs), then the plain text.
func (s *Server) emitChainWS(c *wsClient, full *strings.Builder, chain *message.MessageChain, messageID string) {
	for _, data := range chainImageDataURLs(chain) {
		s.wsSend(c, map[string]interface{}{"ct": "chat", "type": "image", "data": data, "message_id": messageID})
	}
	if t := chainPlainText(chain); t != "" {
		full.WriteString(t)
		s.wsSend(c, map[string]interface{}{"ct": "chat", "type": "plain", "chain_type": "text", "data": t, "message_id": messageID})
	}
}
