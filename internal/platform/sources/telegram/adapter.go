// Package telegram implements a Telegram Bot platform adapter.
// Ported from astrbot/core/platform/sources/telegram/
package telegram

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/internal/utils"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

var logger = log.GetDefault().WithComponent("Telegram")

// Adapter implements a Telegram Bot adapter using long polling.
type Adapter struct {
	Token    string
	apiBase  string
	fileBase string
	client   *http.Client
	EventBus *core.EventBus
	SelfID   string
	stopCh   chan struct{}
	stopOnce sync.Once

	// workerMu 保护 workers；workers 按 chat_id 串行处理 update 的队列
	//（见 dispatchUpdate）。
	workerMu sync.Mutex
	workers  map[string]chan map[string]interface{}
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
	if base, ok := config["telegram_api_base_url"].(string); ok && base != "" {
		a.apiBase = base + a.Token
	} else {
		a.apiBase = "https://api.telegram.org/bot" + a.Token
	}
	if base, ok := config["telegram_file_base_url"].(string); ok && base != "" {
		a.fileBase = base + a.Token
	} else {
		a.fileBase = "https://api.telegram.org/file/bot" + a.Token
	}
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
// (sendPhoto), voice (sendVoice) / audio (sendAudio, decided by Record.Format),
// documents (sendDocument) and video (sendVideo). FileID / public https URLs
// are passed through directly; local paths and base64 payloads are uploaded as
// multipart/form-data.
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
			// 语音（ogg/opus）走 sendVoice，其余音频格式（mp3/m4a/flac/wav）走 sendAudio。
			method, field := "sendVoice", "voice"
			if !isVoiceFormat(c.Format) {
				method, field = "sendAudio", "audio"
			}
			if err := a.sendMedia(sessionID, method, field, c.FileID, c.URL, c.Path, c.File, c.Base64); err != nil {
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
			_ = tmp.Close()
			return err
		}
		_ = tmp.Close()
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

	// io.Pipe 流式构造 multipart 请求体，避免整个文件读入内存。
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	go func() {
		defer f.Close()
		defer pw.Close()
		part, werr := writer.CreateFormFile(field, filepath.Base(filePath))
		if werr != nil {
			return
		}
		if _, werr = io.Copy(part, f); werr != nil {
			return
		}
		if werr = writer.WriteField("chat_id", sessionID); werr != nil {
			return
		}
		_ = writer.Close()
	}()

	req, err := http.NewRequestWithContext(ctx, "POST", a.apiBase+"/"+method, pr)
	if err != nil {
		_ = pr.Close()
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := a.client.Do(req)
	_ = pr.Close()
	if err != nil {
		return fmt.Errorf("telegram %s upload request failed: %s", method, sanitizeURLErr(err))
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
			// 响应异常/空 result：短暂退避再轮询，避免空转轰炸。
			time.Sleep(5 * time.Second)
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
			a.dispatchUpdate(ctx, updateMap)
		}
	}
}

// dispatchUpdate 把 update 交给对应会话的 worker goroutine 串行处理
// （每 chat_id 一个 channel + worker）：语音下载等慢操作不再阻塞轮询，
// 同时保持同一会话内消息的处理顺序。
func (a *Adapter) dispatchUpdate(ctx context.Context, update map[string]interface{}) {
	chatID := ""
	if msg, ok := update["message"].(map[string]interface{}); ok {
		if chat, ok := msg["chat"].(map[string]interface{}); ok {
			if id, ok := chat["id"].(float64); ok {
				chatID = fmt.Sprintf("%d", int64(id))
			}
		}
	}
	if chatID == "" {
		chatID = "unknown"
	}
	a.workerMu.Lock()
	if a.workers == nil {
		a.workers = make(map[string]chan map[string]interface{})
	}
	ch, ok := a.workers[chatID]
	if !ok {
		ch = make(chan map[string]interface{}, 64)
		a.workers[chatID] = ch
		go func(c chan map[string]interface{}) {
			for {
				select {
				case <-ctx.Done():
					return
				case u, ok := <-c:
					if !ok {
						return
					}
					a.handleUpdate(ctx, u)
				}
			}
		}(ch)
	}
	a.workerMu.Unlock()
	select {
	case ch <- update:
	default:
		logger.Warn("Telegram chat %s 的 update 队列已满，丢弃 update_id=%v", chatID, update["update_id"])
	}
}

// handleUpdate processes a single Telegram update.
func (a *Adapter) handleUpdate(ctx context.Context, update map[string]interface{}) {
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
	chain := &message.MessageChain{Chain: []message.Component{}}
	if text != "" {
		chain.Chain = append(chain.Chain, &message.Plain{Text: text})
	}

	// 语音/音频消息：下载文件并按内容识别真实格式，转换为 Record 组件。
	// 语音无文本；音频可携带 caption（作为 Plain 追加，对齐 4.27.4 的 _apply_caption）。
	if _, ok := msg["voice"]; ok {
		if record := a.handleAudioMessage(ctx, msg, "voice"); record != nil {
			chain.Chain = append(chain.Chain, record)
		}
	} else if _, ok := msg["audio"]; ok {
		if record := a.handleAudioMessage(ctx, msg, "audio"); record != nil {
			chain.Chain = append(chain.Chain, record)
		}
		if caption, ok := msg["caption"].(string); ok && caption != "" {
			chain.Chain = append(chain.Chain, &message.Plain{Text: caption})
		}
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

// handleAudioMessage 处理 Telegram 语音/音频消息：下载文件后优先使用 mime_type 识别格式，
// 缺失/不可靠（如 application/octet-stream）时按文件头 magic bytes 判断真实格式，
// 构造带正确扩展名/format 的 message.Record。下载失败时仅告警并返回 nil，不阻断消息处理。
func (a *Adapter) handleAudioMessage(ctx context.Context, msg map[string]interface{}, field string) *message.Record {
	info, ok := msg[field].(map[string]interface{})
	if !ok {
		return nil
	}
	fileID, _ := info["file_id"].(string)
	if fileID == "" {
		logger.Warn("Telegram %s 消息缺少 file_id", field)
		return nil
	}
	mimeType, _ := info["mime_type"].(string)

	resp, err := a.apiCall(ctx, "getFile", map[string]interface{}{"file_id": fileID})
	if err != nil {
		logger.Warn("Telegram getFile 失败 (%s): %v", field, err)
		return nil
	}
	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		logger.Warn("Telegram getFile 返回异常 (%s)", field)
		return nil
	}
	filePath, _ := result["file_path"].(string)
	if filePath == "" {
		logger.Warn("Telegram getFile 缺少 file_path (%s)", field)
		return nil
	}

	tmp, err := os.CreateTemp("", "astrbot-tg-"+field+"-*")
	if err != nil {
		logger.Warn("创建 Telegram 音频临时文件失败 (%s): %v", field, err)
		return nil
	}
	tmpName := tmp.Name()
	_ = tmp.Close()

	if err := utils.DownloadFile(ctx, a.fileBase+"/"+filePath, tmpName); err != nil {
		logger.Warn("Telegram 音频文件下载失败 (%s): %v", field, err)
		_ = os.Remove(tmpName)
		return nil
	}

	// 识别音频格式：优先已有 mime_type，缺失/不可靠时按文件内容 magic bytes 判断。
	format := formatFromMime(mimeType)
	if format == "" {
		format = utils.DetectAudioFormat(tmpName)
	}
	if format == "" {
		// 识别失败回退：使用默认 ogg。
		format = "ogg"
		logger.Warn("Telegram %s 音频格式识别失败 (mime=%s)，回退为 ogg", field, mimeType)
	}

	// 补上正确扩展名，便于 Telegram 识别语音/音频格式。
	finalName := tmpName + audioExt(format)
	if finalName != tmpName {
		if err := os.Rename(tmpName, finalName); err != nil {
			logger.Warn("重命名 Telegram 音频临时文件失败 (%s): %v", field, err)
			finalName = tmpName
		}
	}
	scheduleAudioCleanup(finalName)

	return &message.Record{
		File:   finalName,
		URL:    finalName,
		Path:   finalName,
		Format: format,
		Mime:   mimeType,
	}
}

// tempAudioCleanupDelay 是临时音频文件的清理延迟：消息在事件总线中异步处理，
// 延迟清理保证文件在消费/转发期间可用。
const tempAudioCleanupDelay = 30 * time.Minute

// scheduleAudioCleanup 在延迟后删除临时音频文件。
func scheduleAudioCleanup(path string) {
	if path == "" {
		return
	}
	time.AfterFunc(tempAudioCleanupDelay, func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			logger.Debug("清理 Telegram 临时音频文件失败 %s: %v", path, err)
		}
	})
}

// isVoiceFormat 判断音频格式是否作为语音（sendVoice）发送。Telegram 语音仅支持
// ogg/opus；格式为空时保持向后兼容（默认按语音处理）。
func isVoiceFormat(format string) bool {
	switch strings.ToLower(format) {
	case "", "ogg", "opus":
		return true
	}
	return false
}

// formatFromMime 从 mime_type 推断音频格式；mime 缺失或不可靠（如
// application/octet-stream）时返回空字符串，交由文件内容识别。
func formatFromMime(mime string) string {
	switch strings.ToLower(mime) {
	case "audio/ogg", "application/ogg", "audio/opus":
		return "ogg"
	case "audio/mpeg", "audio/mp3", "audio/x-mp3":
		return "mp3"
	case "audio/mp4", "audio/x-m4a", "audio/m4a", "video/mp4":
		return "m4a"
	case "audio/flac", "audio/x-flac":
		return "flac"
	case "audio/wav", "audio/x-wav", "audio/wave", "audio/vnd.wave":
		return "wav"
	}
	return ""
}

// audioExt 返回音频格式对应的文件扩展名；opus 使用 ogg 容器。
func audioExt(format string) string {
	switch strings.ToLower(format) {
	case "ogg", "opus":
		return ".ogg"
	case "m4a", "mp4":
		return ".m4a"
	default:
		return "." + format
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
		return nil, fmt.Errorf("telegram %s request failed: %s", method, sanitizeURLErr(err))
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

	// HTTP 401/409/429 等错误也可能带 200 状态码：ok=false 时按失败处理，
	// 否则 Send 会把失败响应当成功，pollLoop 也会对空 result 立即重发轰炸。
	if ok, _ := result["ok"].(bool); !ok {
		desc, _ := result["description"].(string)
		return nil, fmt.Errorf("telegram %s failed: %s", method, desc)
	}

	return result, nil
}

// sanitizeURLErr 去除 *url.Error 中的完整 URL（bot token 内嵌于 URL），
// 仅保留底层错误与操作名，避免 token 泄漏进日志。
func sanitizeURLErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Sprintf("%s: %v", ue.Err, ue.Op)
	}
	return err.Error()
}
