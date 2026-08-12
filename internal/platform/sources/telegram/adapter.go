// Package telegram implements a Telegram Bot platform adapter.
// Ported from astrbot/core/platform/sources/telegram/
package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

var logger = log.GetDefault().WithComponent("Telegram")

// Adapter implements a Telegram Bot adapter using long polling.
type Adapter struct {
	Token    string
	apiBase  string
	client   *http.Client
	EventBus *core.EventBus
	SelfID   string
	stopCh   chan struct{}
}

// New creates a Telegram adapter.
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		EventBus: eventBus,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
		stopCh: make(chan struct{}),
	}
	a.Token, _ = config["token"].(string)
	a.apiBase = "https://api.telegram.org/bot" + a.Token
	return a
}

// Start begins long-polling for updates.
func (a *Adapter) Start(ctx context.Context) error {
	// Get bot info
	resp, err := a.apiCall(ctx, "getMe", nil)
	if err != nil {
		return fmt.Errorf("telegram getMe failed: %w", err)
	}

	if ok, _ := resp["ok"].(bool); ok {
		if result, ok := resp["result"].(map[string]interface{}); ok {
			if id, ok := result["id"].(float64); ok {
				a.SelfID = fmt.Sprintf("%d", int64(id))
			}
		}
	}

	logger.I18nInfo("Telegram 机器人已连接, self_id=%s", a.SelfID)

	go a.pollLoop(ctx)

	return nil
}

// Stop stops the adapter.
func (a *Adapter) Stop() error {
	close(a.stopCh)
	return nil
}

// ID returns the adapter ID.
func (a *Adapter) ID() string { return "telegram" }

// Type returns the platform type.
func (a *Adapter) Type() string { return "telegram" }

// Send sends a message chain to a Telegram chat.
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	text := extractPlainText(chain)
	params := map[string]interface{}{
		"chat_id": sessionID,
		"text":    text,
	}
	// 用带超时的上下文发送，避免网络卡死时 sendMessage 无限期挂起。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := a.apiCall(ctx, "sendMessage", params)
	return err
}

// pollLoop continuously polls Telegram for updates.
func (a *Adapter) pollLoop(ctx context.Context) {
	offset := 0
	for {
		select {
		case <-a.stopCh:
			return
		case <-ctx.Done():
			return
		default:
		}

		params := map[string]interface{}{
			"timeout": 30,
		}
		if offset > 0 {
			params["offset"] = offset
		}

		resp, err := a.apiCall(ctx, "getUpdates", params)
		if err != nil {
			logger.Error("Telegram poll error: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		updates, ok := resp["result"].([]interface{})
		if !ok {
			continue
		}

		for _, update := range updates {
			updateMap, ok := update.(map[string]interface{})
			if !ok {
				continue
			}
			if updateID, ok := updateMap["update_id"].(float64); ok {
				offset = int(updateID) + 1
			}
			a.handleUpdate(updateMap)
		}
	}
}

// handleUpdate processes a single Telegram update.
func (a *Adapter) handleUpdate(update map[string]interface{}) {
	msg, ok := update["message"].(map[string]interface{})
	if !ok {
		return
	}

	var senderID, senderName, convID string
	var isGroup bool

	if from, ok := msg["from"].(map[string]interface{}); ok {
		if id, ok := from["id"].(float64); ok {
			senderID = fmt.Sprintf("%d", int64(id))
		}
		if first, ok := from["first_name"].(string); ok {
			senderName = first
			if last, ok := from["last_name"].(string); ok && last != "" {
				senderName += " " + last
			}
		}
	}

	if chat, ok := msg["chat"].(map[string]interface{}); ok {
		if id, ok := chat["id"].(float64); ok {
			convID = fmt.Sprintf("%d", int64(id))
		}
		if chatType, ok := chat["type"].(string); ok {
			isGroup = chatType == "group" || chatType == "supergroup"
		}
	}

	// Extract text
	text, _ := msg["text"].(string)

	// Check for @bot mention
	isAtBot := false
	if text != "" {
		if strings.Contains(text, "@"+a.SelfID) || strings.HasPrefix(text, "/") {
			isAtBot = true
		}
	}

	// Build message chain
	chain := &message.MessageChain{
		Chain: []message.Component{&message.Plain{Text: text}},
	}

	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   "telegram",
			SelfID:     a.SelfID,
			SenderID:   senderID,
			SenderName: senderName,
			ConvID:     convID,
			IsGroup:    isGroup,
			IsAtBot:    isAtBot,
		},
		Message:           chain,
		MessageStr:        text,
		IsAtOrWakeCommand: isAtBot,
		Timestamp:         time.Now(),
		Metadata:          make(map[string]interface{}),
	}

	if err := a.EventBus.Publish(event); err != nil {
		logger.Error("Failed to publish event: %v", err)
	}
}

// apiCall makes a Telegram Bot API call.
func (a *Adapter) apiCall(ctx context.Context, method string, params map[string]interface{}) (map[string]interface{}, error) {
	url := a.apiBase + "/" + method

	var bodyReader io.Reader
	if len(params) > 0 {
		data, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		bodyReader = strings.NewReader(string(data))
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return result, nil
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
