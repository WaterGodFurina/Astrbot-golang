// Package webchat implements a built-in web chat platform adapter.
// Ported from astrbot/core/platform/sources/webchat/
package webchat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/core"
	"github.com/AstrBotDevs/AstrBot/internal/log"
	"github.com/AstrBotDevs/AstrBot/pkg/message"
)

var logger = log.GetDefault().WithComponent("WebChat")

// Adapter provides a web-based chat interface.
type Adapter struct {
	Host     string
	Port     int
	server   *http.Server
	EventBus *core.EventBus
	mu       sync.Mutex
	clients  map[string]chan *message.MessageChain
}

// New creates a WebChat adapter.
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		EventBus: eventBus,
		clients:  make(map[string]chan *message.MessageChain),
	}
	a.Host, _ = config["host"].(string)
	if a.Host == "" {
		a.Host = "0.0.0.0"
	}
	if port, ok := config["port"].(float64); ok {
		a.Port = int(port)
	}
	if a.Port == 0 {
		a.Port = 6185
	}
	return a
}

// Start starts the web chat server.
func (a *Adapter) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/chat", a.handleChat)
	mux.HandleFunc("/poll", a.handlePoll)

	a.server = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", a.Host, a.Port),
		Handler: mux,
	}

	go func() {
		logger.Info("WebChat adapter listening on %s:%d", a.Host, a.Port)
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
	ch, ok := a.clients[sessionID]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("client %s not connected", sessionID)
	}
	select {
	case ch <- chain:
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("send timeout")
	}
}

// handleChat handles incoming chat messages.
func (a *Adapter) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
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

	// Publish event
	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   "webchat",
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

	if err := a.EventBus.Publish(event); err != nil {
		logger.Error("Failed to publish event: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	// Register client for response
	a.mu.Lock()
	if _, ok := a.clients[req.SessionID]; !ok {
		a.clients[req.SessionID] = make(chan *message.MessageChain, 10)
	}
	ch := a.clients[req.SessionID]
	a.mu.Unlock()

	// Wait for response
	select {
	case resp := <-ch:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"session_id": req.SessionID,
			"reply":      extractPlainText(resp),
		})
	case <-time.After(60 * time.Second):
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"session_id": req.SessionID,
			"reply":      "Timeout waiting for response",
		})
	}
}

// handlePoll handles long-polling for responses.
func (a *Adapter) handlePoll(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		http.Error(w, "Missing session_id", http.StatusBadRequest)
		return
	}

	a.mu.Lock()
	if _, ok := a.clients[sessionID]; !ok {
		a.clients[sessionID] = make(chan *message.MessageChain, 10)
	}
	ch := a.clients[sessionID]
	a.mu.Unlock()

	select {
	case resp := <-ch:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"reply": extractPlainText(resp),
		})
	case <-time.After(30 * time.Second):
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
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
