// Package aiocqhttp implements the OneBot v11 platform adapter.
// Ported from astrbot/core/platform/sources/aiocqhttp/
package aiocqhttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

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
	mu       sync.Mutex
}

// New creates an aiocqhttp adapter from config.
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		EventBus: eventBus,
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
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	// Convert message chain to OneBot v11 format
	segments := a.convertToCQFormat(chain)
	msg := map[string]interface{}{
		"action": "send_group_msg",
		"params": map[string]interface{}{
			"group_id": sessionID,
			"message":  segments,
		},
	}
	// In a full implementation, this would send via the WebSocket connection
	_ = msg
	return nil
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

// handleWebSocket handles WebSocket connections (placeholder for full WS support).
func (a *Adapter) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Full WebSocket implementation would use gorilla/websocket
	// For now, this is a placeholder
	http.Error(w, "WebSocket not yet implemented", http.StatusNotImplemented)
}

// handleEvent processes a OneBot v11 event.
func (a *Adapter) handleEvent(raw map[string]interface{}) {
	postType, _ := raw["post_type"].(string)
	if postType != "message" {
		return
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
