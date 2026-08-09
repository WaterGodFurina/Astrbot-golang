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
	"strings"
	"sync"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/core"
	"github.com/AstrBotDevs/AstrBot/internal/platform"
	"github.com/AstrBotDevs/AstrBot/pkg/message"
	"github.com/gorilla/websocket"
)

// chatStreamAdapter captures reply chains from the pipeline for a chat session.
// It implements platform.PlatformAdapter so platformMgr.Send("dashboard_chat",
// sessionID, chain) lands here and fans out to SSE subscribers.
type chatStreamAdapter struct {
	mu          sync.Mutex
	subscribers map[string]map[chan *message.MessageChain]struct{}
}

func newChatStreamAdapter() *chatStreamAdapter {
	return &chatStreamAdapter{subscribers: make(map[string]map[chan *message.MessageChain]struct{})}
}

func (a *chatStreamAdapter) ID() string   { return "dashboard_chat" }
func (a *chatStreamAdapter) Type() string { return "dashboard_chat" }

// Start/Stop are no-ops: the adapter is purely a reply sink.
func (a *chatStreamAdapter) Start(ctx context.Context) error { return nil }
func (a *chatStreamAdapter) Stop() error                     { return nil }

// Send forwards a reply chain to all subscribers of the session.
func (a *chatStreamAdapter) Send(sessionID string, chain *message.MessageChain) error {
	a.mu.Lock()
	subs := a.subscribers[sessionID]
	a.mu.Unlock()
	if len(subs) == 0 {
		return nil
	}
	for ch := range subs {
		select {
		case ch <- chain:
		default:
			// Subscriber not draining; drop rather than block the pipeline.
		}
	}
	return nil
}

func (a *chatStreamAdapter) subscribe(sessionID string) chan *message.MessageChain {
	ch := make(chan *message.MessageChain, 64)
	a.mu.Lock()
	if a.subscribers[sessionID] == nil {
		a.subscribers[sessionID] = make(map[chan *message.MessageChain]struct{})
	}
	a.subscribers[sessionID][ch] = struct{}{}
	a.mu.Unlock()
	return ch
}

func (a *chatStreamAdapter) unsubscribe(sessionID string, ch chan *message.MessageChain) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if subs, ok := a.subscribers[sessionID]; ok {
		delete(subs, ch)
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
		"id":         savedUserID,
		"session_id": sessionID,
		"sender_id":  "dashboard",
		"sender_name": "dashboard",
		"role":       "user",
		"type":       "user",
		"content":    map[string]interface{}{"type": "user", "message": parts},
		"created_at": time.Now().Format(time.RFC3339Nano),
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
			"id":               savedUserID,
			"created_at":       time.Now().Format(time.RFC3339Nano),
			"llm_checkpoint_id": fmt.Sprintf("c_%d", time.Now().UnixNano()),
		},
	})
	sendSSE(w, flusher, map[string]interface{}{
		"type": "run_started",
		"data": map[string]interface{}{"run_id": fmt.Sprintf("r_%d", time.Now().UnixNano())},
	})

	// Subscribe to the pipeline reply for this session.
	ch := s.chatAdapter.subscribe(sessionID)
	defer s.chatAdapter.unsubscribe(sessionID, ch)

	// Run the user message through the pipeline; done is closed when the
	// event finishes processing.
	done := s.processChatEvent(r.Context(), sessionID, text, body.SelectedProvider, body.SelectedModel, body.Flags)
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
			// Pipeline finished; flush any pending text then complete.
			for {
				select {
				case chain := <-ch:
					if t := chainPlainText(chain); t != "" {
						full.WriteString(t)
						sendSSE(w, flusher, map[string]interface{}{"type": "plain", "data": t, "chain_type": "text"})
					}
				default:
					goto done
				}
			}
		done:
			if full.Len() > 0 {
				// Persist the bot reply into the chat session store.
				botID := fmt.Sprintf("b_%d", time.Now().UnixNano())
				botRecord := map[string]interface{}{
					"id":         botID,
					"session_id": sessionID,
					"sender_id":  "bot",
					"sender_name": "bot",
					"role":       "assistant",
					"type":       "bot",
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
			if t := chainPlainText(chain); t != "" {
				full.WriteString(t)
				sendSSE(w, flusher, map[string]interface{}{"type": "plain", "data": t, "chain_type": "text"})
			}
		}
	}
}

// processChatEvent runs the message through the "default" pipeline scheduler
// as a webchat-platform event in a goroutine and returns a channel closed
// when processing completes (nil if the pipeline is unavailable).
func (s *Server) processChatEvent(ctx context.Context, sessionID, text, providerID, model string, flags map[string]interface{}) <-chan struct{} {
	bus, ok := s.eventBus.(*core.EventBus)
	if !ok || bus == nil {
		return nil
	}
	scheduler := bus.GetScheduler("default")
	if scheduler == nil {
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
		Message:           chain,
		MessageStr:        text,
		PlainText:         text,
		Timestamp:         time.Now(),
		IsAtOrWakeCommand: true,
		CallLLM:           true,
		Metadata:          map[string]interface{}{},
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

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := scheduler.Process(ctx, event); err != nil {
			logger.Error("dashboard chat pipeline failed: %v", err)
		}
	}()
	return done
}

// sendSSE writes one SSE data frame.
func sendSSE(w http.ResponseWriter, flusher http.Flusher, payload map[string]interface{}) {
	data, _ := json.Marshal(payload)
	fmt.Fprintf(w, "data: %s\n\n", data)
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
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

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
		logger.Warn("websocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
		return nil
	})

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
				s.wsSend(conn, map[string]interface{}{
					"ct": "chat", "t": "error", "data": "session_id is required",
					"code": "INVALID_MESSAGE_FORMAT",
				})
				continue
			}
			s.wsSend(conn, map[string]interface{}{
				"ct": "chat", "type": "session_bound", "session_id": msg.SessionID,
				"message_id": fmt.Sprintf("ws_sub_%d", time.Now().UnixNano()),
			})

		case "interrupt":
			// Best-effort: cancel the current pipeline run for the session.
			s.wsSend(conn, map[string]interface{}{
				"ct": "chat", "t": "error", "data": "INTERRUPTED",
				"code": "INTERRUPTED",
				"message_id": msg.MessageID,
			})

		case "send":
			if len(msg.Message) == 0 {
				s.wsSend(conn, map[string]interface{}{
					"ct": "chat", "t": "error", "data": "Message content is empty",
					"code": "INVALID_MESSAGE_FORMAT", "message_id": msg.MessageID,
				})
				continue
			}
			if msg.SessionID == "" {
				s.wsSend(conn, map[string]interface{}{
					"ct": "chat", "t": "error", "data": "session_id is required",
					"code": "INVALID_MESSAGE_FORMAT", "message_id": msg.MessageID,
				})
				continue
			}
			go s.handleWSMessageSend(conn, msg)
		}
	}
}

// wsSend writes one JSON frame to the websocket.
func (s *Server) wsSend(conn *websocket.Conn, payload map[string]interface{}) {
	if err := conn.WriteJSON(payload); err != nil {
		logger.Debug("ws send failed: %v", err)
	}
}

// handleWSMessageSend runs a chat message through the pipeline and streams the
// reply events back over the websocket (per message_id).
func (s *Server) handleWSMessageSend(conn *websocket.Conn, msg struct {
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
		"id":         fmt.Sprintf("u_%d", time.Now().UnixNano()),
		"session_id": sessionID,
		"sender_id":  "dashboard",
		"sender_name": "dashboard",
		"role":       "user",
		"type":       "user",
		"content":    map[string]interface{}{"type": "user", "message": msg.Message},
		"created_at": time.Now().Format(time.RFC3339Nano),
	}
	s.chat.appendMessage(sessionID, userRecord)

	llmCheckpointID := fmt.Sprintf("c_%d", time.Now().UnixNano())
	s.wsSend(conn, map[string]interface{}{
		"ct": "chat", "type": "user_message_saved",
		"data":       map[string]interface{}{"id": userRecord["id"], "created_at": userRecord["created_at"], "llm_checkpoint_id": llmCheckpointID},
		"message_id": messageID,
	})
	s.wsSend(conn, map[string]interface{}{
		"ct": "chat", "type": "run_started",
		"data":       map[string]interface{}{"run_id": messageID},
		"message_id": messageID,
	})

	ch := s.chatAdapter.subscribe(sessionID)
	defer s.chatAdapter.unsubscribe(sessionID, ch)

	done := s.processChatEvent(context.Background(), sessionID, text, msg.SelectedProvider, msg.SelectedModel, msg.Flags)
	if done == nil {
		s.wsSend(conn, map[string]interface{}{"ct": "chat", "type": "error", "data": "对话管道不可用", "message_id": messageID})
		s.wsSend(conn, map[string]interface{}{"ct": "chat", "type": "end", "data": nil, "message_id": messageID})
		return
	}

	var full strings.Builder
	deadline := time.After(300 * time.Second)
	for {
		select {
		case <-deadline:
			s.wsSend(conn, map[string]interface{}{"ct": "chat", "type": "end", "data": nil, "message_id": messageID})
			return
		case <-done:
			for {
				select {
				case chain := <-ch:
					if t := chainPlainText(chain); t != "" {
						full.WriteString(t)
						s.wsSend(conn, map[string]interface{}{"ct": "chat", "type": "plain", "chain_type": "text", "data": t, "message_id": messageID})
					}
				default:
					goto done
				}
			}
		done:
			if full.Len() > 0 {
				botRecord := map[string]interface{}{
					"id":         fmt.Sprintf("b_%d", time.Now().UnixNano()),
					"session_id": sessionID,
					"sender_id":  "bot",
					"sender_name": "bot",
					"role":       "assistant",
					"type":       "bot",
					"content": map[string]interface{}{
						"type":    "bot",
						"message": []map[string]interface{}{{"type": "plain", "text": full.String()}},
					},
					"created_at": time.Now().Format(time.RFC3339Nano),
				}
				s.chat.appendMessage(sessionID, botRecord)
				s.wsSend(conn, map[string]interface{}{"ct": "chat", "type": "message_saved", "data": map[string]interface{}{"id": botRecord["id"], "created_at": botRecord["created_at"]}, "message_id": messageID})
				s.wsSend(conn, map[string]interface{}{"ct": "chat", "type": "complete", "data": full.String(), "message_id": messageID})
			}
			s.wsSend(conn, map[string]interface{}{"ct": "chat", "type": "end", "data": nil, "message_id": messageID})
			return
		case chain := <-ch:
			if t := chainPlainText(chain); t != "" {
				full.WriteString(t)
				s.wsSend(conn, map[string]interface{}{"ct": "chat", "type": "plain", "chain_type": "text", "data": t, "message_id": messageID})
			}
		}
	}
}
