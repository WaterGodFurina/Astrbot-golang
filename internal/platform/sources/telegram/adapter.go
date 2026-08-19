// Package telegram implements a Telegram Bot platform adapter.
// Ported from astrbot/core/platform/sources/telegram/
package telegram

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
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
	stopOnce sync.Once
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

// SetEventBus injects the event bus (implements platform.EventBusSetter).
func (a *Adapter) SetEventBus(bus platform.EventBus) {
	if eb, ok := bus.(*core.EventBus); ok {
		a.EventBus = eb
	}
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
	a.stopOnce.Do(func() { close(a.stopCh) })
	return nil
}

// ID returns the adapter ID.
func (a *Adapter) ID() string { return "telegram" }

// Type returns the platform type.
func (a *Adapter) Type() string { return "telegram" }

// Send sends a message chain to a Telegram chat. Supports text, images
// (sendPhoto), voice (sendVoice), documents (sendDocument) and video
// (sendVideo). FileID / public https URLs are passed through directly;
// local paths and base64 payloads are uploaded as multipart/form-data.
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	if chain == nil {
		return nil
	}
	var textParts []string
	for _, comp := range chain.Chain {
		switch c := comp.(type) {
		case *message.Plain:
			textParts = append(textParts, c.Text)
		case *message.Image:
			if err := a.sendMedia(sessionID, "sendPhoto", "photo", c.FileID, c.URL, c.Path, c.File, c.Base64); err != nil {
				return err
			}
		case *message.Record:
			if err := a.sendMedia(sessionID, "sendVoice", "voice", c.FileID, c.URL, c.Path, c.File, c.Base64); err != nil {
				return err
			}
		case *message.File:
			if err := a.sendMedia(sessionID, "sendDocument", "document", c.FileID, c.URL, c.Path, "", ""); err != nil {
				return err
			}
		case *message.Video:
			if err := a.sendMedia(sessionID, "sendVideo", "video", c.FileID, c.URL, c.Path, "", ""); err != nil {
				return err
			}
		}
	}
	text := strings.Join(textParts, "")
	if text == "" {
		return nil
	}
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

// React adds an emoji reaction to a message (Telegram Bot API setMessageReaction).
func (a *Adapter) React(sessionID, messageID, emoji string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	params := map[string]interface{}{
		"chat_id":    sessionID,
		"message_id": messageID,
		"reaction": []map[string]interface{}{
			{"type": "emoji", "emoji": emoji},
		},
	}
	_, err := a.apiCall(ctx, "setMessageReaction", params)
	return err
}

// sendMedia sends a single media component via the given Telegram method.
func (a *Adapter) sendMedia(sessionID, method, field, fileID, url, path, file, b64 string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 1. file_id: reuse an already-uploaded Telegram file.
	if fileID != "" {
		_, err := a.apiCall(ctx, method, map[string]interface{}{
			"chat_id": sessionID, field: fileID,
		})
		return err
	}
	// 2. public https URL: Telegram fetches it server-side.
	if url != "" {
		_, err := a.apiCall(ctx, method, map[string]interface{}{
			"chat_id": sessionID, field: url,
		})
		return err
	}
	// 3. base64 payload: decode to a temp file and upload.
	if b64 != "" {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil {
			return fmt.Errorf("decode base64 media: %w", err)
		}
		tmp, err := os.CreateTemp("", "astrbot-tg-*")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		defer os.Remove(tmpName)
		if _, err := tmp.Write(raw); err != nil {
			tmp.Close()
			return err
		}
		tmp.Close()
		return a.sendMediaUpload(ctx, sessionID, method, field, tmpName)
	}
	// 4. local path / file.
	localPath := path
	if localPath == "" {
		localPath = file
	}
	if localPath == "" {
		return fmt.Errorf("%s: media has no file_id/url/path", method)
	}
	return a.sendMediaUpload(ctx, sessionID, method, field, localPath)
}

// sendMediaUpload uploads a local file as multipart/form-data.
func (a *Adapter) sendMediaUpload(ctx context.Context, sessionID, method, field, filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open media %s: %w", filePath, err)
	}
	defer f.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile(field, filepath.Base(filePath))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	if err := writer.WriteField("chat_id", sessionID); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", a.apiBase+"/"+method, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode %s response: %w", method, err)
	}
	if ok, _ := result["ok"].(bool); !ok {
		if desc, _ := result["description"].(string); desc != "" {
			return fmt.Errorf("%s failed: %s", method, desc)
		}
		return fmt.Errorf("%s failed: %v", method, result)
	}
	return nil
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
			PlatformID: a.ID(),
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
