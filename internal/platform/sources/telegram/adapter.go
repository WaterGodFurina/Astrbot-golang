// Package telegram implements a Telegram Bot platform adapter.
// Ported from astrbot/core/platform/sources/telegram/
package telegram

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/internal/star"
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

	// config 保存平台配置（start_message / telegram_command_register 等）。
	config map[string]interface{}
	// selfUsername 是 getMe 返回的 bot 用户名（不带 @），用于 /cmd@bot 命令
	// 剥离、@bot 唤醒识别与指令注册。
	selfUsername string
	// startMessage 是配置的 /start 欢迎语（本体 tg_adapter.py:413-422）。
	startMessage string
	// ctx 是 Start 注入的运行上下文，相册合并等异步任务复用。
	ctx context.Context

	// enableCmdRegister / enableCmdRefresh 对齐本体的
	// telegram_command_register / telegram_command_auto_refresh 配置。
	enableCmdRegister bool
	enableCmdRefresh  bool
	// starMgr 是宿主注入的 star 处理器注册表（指令注册来源）。
	starMgr *star.Manager
	// commandsMu 保护 lastCommandHash（本体 last_command_hash 的去重语义）。
	commandsMu      sync.Mutex
	lastCommandHash uint32

	// mediaGroups 缓存进行中的相册合并（media_group_id → 已到达的消息项）。
	mediaMu     sync.Mutex
	mediaGroups map[string]*mediaGroupEntry

	// lastTyping 记录流式输出期间各会话上次发送 typing 的时间（节流）。
	streamMu   sync.Mutex
	lastTyping map[string]time.Time

	// workerMu 保护 workers；workers 按 chat_id 串行处理 update 的队列
	//（见 dispatchUpdate）。
	workerMu sync.Mutex
	workers  map[string]chan map[string]interface{}

	// _forum_topic_names caches Telegram forum topic names keyed by
	// (chat_id, thread_id). Aligned with Python tg_adapter _forum_topic_names.
	forumTopicMu    sync.Mutex
	forumTopicNames map[string]string
	forumTopicOrder []string
}

// New creates a Telegram adapter.
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		EventBus: eventBus,
		config:   config,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
		stopCh: make(chan struct{}),
	}
	a.forumTopicNames = make(map[string]string)
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
	// 指令注册开关（本体 tg_adapter.py:73-80）。
	a.enableCmdRegister = true
	if v, ok := config["telegram_command_register"].(bool); ok {
		a.enableCmdRegister = v
	}
	a.enableCmdRefresh = true
	if v, ok := config["telegram_command_auto_refresh"].(bool); ok {
		a.enableCmdRefresh = v
	}
	a.startMessage, _ = config["start_message"].(string)
	return a
}

// SetEventBus injects the event bus (implements platform.EventBusSetter).
func (a *Adapter) SetEventBus(bus platform.EventBus) {
	if eb, ok := bus.(*core.EventBus); ok {
		a.EventBus = eb
	}
}

// SetStarManager injects the star handler registry (command registration
// source). Implements platform.StarManagerSetter，由宿主 lifecycle 统一注入。
func (a *Adapter) SetStarManager(mgr interface{}) {
	if m, ok := mgr.(*star.Manager); ok {
		a.starMgr = m
	}
}

// ID returns the adapter instance id (config.id，对齐本体 meta())。
func (a *Adapter) ID() string {
	if id, ok := a.config["id"].(string); ok && id != "" {
		return id
	}
	return "telegram"
}

// Type returns the platform type.
func (a *Adapter) Type() string { return "telegram" }

// Start begins long-polling for updates.
func (a *Adapter) Start(ctx context.Context) error {
	a.ctx = ctx
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
			if name, ok := result["username"].(string); ok {
				a.selfUsername = name
			}
		}
	}

	logger.I18nInfo("Telegram 机器人已连接, self_id=%s username=%s", a.SelfID, a.selfUsername)

	// 指令注册（本体 tg_adapter.py:152-153, 321-341）：启动时从宿主 star
	// 注册表收集指令注册到 Telegram；开启自动刷新时按间隔周期重注册。
	if a.enableCmdRegister {
		if err := a.registerCommands(); err != nil {
			logger.Warn("向 Telegram 注册指令时发生错误: %v", err)
		}
		if a.enableCmdRefresh {
			go a.commandRefreshLoop(ctx)
		}
	}

	go a.pollLoop(ctx)

	return nil
}

// Stop stops the adapter. 关闭时尽力清除已注册指令（对齐本体 terminate()，
// tg_adapter.py:807-817 的 delete_commands=True）。
func (a *Adapter) Stop() error {
	if a.enableCmdRegister {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := a.apiCall(ctx, "deleteMyCommands", map[string]interface{}{})
		cancel()
		if err != nil {
			logger.Debug("Telegram deleteMyCommands 失败（忽略）: %v", err)
		}
	}
	a.stopOnce.Do(func() { close(a.stopCh) })
	return nil
}

// React sets a message reaction (Telegram Bot API setMessageReaction)，
// 对齐本体 tg_event.py:350-380 react() 的能力面：
//   - emoji 为空：移除本 bot 已设置的反应（reaction 传空列表）；
//   - emoji 为纯数字：视为自定义表情的 custom_emoji_id；
//   - 其余：普通 emoji reaction。
func (a *Adapter) React(sessionID, messageID, emoji string) error {
	// 解析 chat_id（去掉超级群的 "#<thread_id>" 片段，本体 :357-361）。
	chatID, _ := splitThreadID(sessionID)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var reaction interface{}
	switch {
	case emoji == "":
		reaction = []interface{}{}
	case isAllDigits(emoji):
		reaction = []interface{}{
			map[string]interface{}{"type": "custom_emoji", "custom_emoji_id": emoji},
		}
	default:
		reaction = []interface{}{
			map[string]interface{}{"type": "emoji", "emoji": emoji},
		}
	}
	params := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
		"reaction":   reaction,
	}
	_, err := a.apiCall(ctx, "setMessageReaction", params)
	return err
}

// resolveForumTopicName discovers and caches Telegram forum topic names
// (aligned with Python tg_adapter _forum_topic_names cache + "title-topic" format).
func (a *Adapter) resolveForumTopicName(chatID string, threadID int64, isForum bool, msg map[string]interface{}) string {
	a.forumTopicMu.Lock()
	defer a.forumTopicMu.Unlock()

	var topicKey string
	if threadID > 0 {
		topicKey = fmt.Sprintf("%s#%d", chatID, threadID)
	} else if isForum {
		topicKey = chatID
	}
	if topicKey == "" {
		return ""
	}

	discovered := ""
	if ft, ok := msg["forum_topic_created"].(map[string]interface{}); ok {
		if n, _ := ft["name"].(string); n != "" {
			discovered = n
		}
	}
	if discovered == "" {
		if ft, ok := msg["forum_topic_edited"].(map[string]interface{}); ok {
			if n, _ := ft["name"].(string); n != "" {
				discovered = n
			}
		}
	}
	if discovered == "" {
		if replyRaw, ok := msg["reply_to_message"].(map[string]interface{}); ok {
			if ft, ok := replyRaw["forum_topic_created"].(map[string]interface{}); ok {
				if n, _ := ft["name"].(string); n != "" {
					discovered = n
				}
			}
		}
	}

	if discovered != "" {
		delete(a.forumTopicNames, topicKey)
		a.forumTopicNames[topicKey] = discovered
		a.forumTopicOrder = append(a.forumTopicOrder, topicKey)
		for len(a.forumTopicNames) > 1000 && len(a.forumTopicOrder) > 0 {
			oldest := a.forumTopicOrder[0]
			a.forumTopicOrder = a.forumTopicOrder[1:]
			delete(a.forumTopicNames, oldest)
		}
	}

	if name, ok := a.forumTopicNames[topicKey]; ok {
		return name
	}
	return ""
}

// GetGroupInfo enriches Telegram group metadata via Bot API get_chat +
// get_chat_member_count + get_chat_administrators (Python tg_event.py get_group).
func (a *Adapter) GetGroupInfo(ctx context.Context, groupID string) (*platform.Group, error) {
	if groupID == "" {
		return nil, nil
	}
	group := &platform.Group{GroupID: groupID}

	apiChatID, threadID := splitThreadID(groupID)
	numID, err := strconv.ParseInt(apiChatID, 10, 64)
	if err != nil {
		logger.Debug("[Telegram] Invalid group ID for GetGroupInfo: %s", groupID)
		return group, nil
	}

	resp, err := a.apiCall(ctx, "getChat", map[string]interface{}{"chat_id": numID})
	if err != nil {
		logger.Debug("[Telegram] getChat failed for %s: %v", apiChatID, err)
	} else {
		if result, ok := resp["result"].(map[string]interface{}); ok {
			if title, _ := result["title"].(string); title != "" {
				if threadID != "" {
					topicName := a.getCachedTopicName(apiChatID, threadID)
					if topicName != "" {
						title = title + "-" + topicName
					}
				}
				group.GroupName = title
			}
			if photo, ok := result["photo"].(map[string]interface{}); ok {
				if fileID, _ := photo["big_file_id"].(string); fileID != "" {
					if filePath := a.getFileFilePath(ctx, fileID); filePath != "" {
						group.GroupAvatar = filePath
					}
				}
			}
		}
	}

	countResp, err := a.apiCall(ctx, "getChatMemberCount", map[string]interface{}{"chat_id": numID})
	if err == nil {
		if result, ok := countResp["result"].(float64); ok {
			c := int(result)
			group.MemberCount = &c
		}
	}

	adminResp, err := a.apiCall(ctx, "getChatAdministrators", map[string]interface{}{"chat_id": numID})
	if err == nil {
		if result, ok := adminResp["result"].([]interface{}); ok {
			for _, admin := range result {
				am, ok := admin.(map[string]interface{})
				if !ok {
					continue
				}
				user, ok := am["user"].(map[string]interface{})
				if !ok {
					continue
				}
				userID, _ := user["id"].(float64)
				if userID == 0 {
					continue
				}
				userIDStr := strconv.FormatFloat(userID, 'f', 0, 64)
				status, _ := am["status"].(string)
				if status == "creator" {
					group.GroupOwner = userIDStr
				} else if status == "administrator" {
					group.GroupAdmins = append(group.GroupAdmins, userIDStr)
				}
			}
		}
	}

	return group, nil
}

// getCachedTopicName looks up a cached forum topic name.
func (a *Adapter) getCachedTopicName(chatID, threadID string) string {
	a.forumTopicMu.Lock()
	defer a.forumTopicMu.Unlock()
	topicKey := fmt.Sprintf("%s#%s", chatID, threadID)
	if name, ok := a.forumTopicNames[topicKey]; ok {
		return name
	}
	return ""
}

// getFileFilePath retrieves a file's download path via getFile API.
func (a *Adapter) getFileFilePath(ctx context.Context, fileID string) string {
	resp, err := a.apiCall(ctx, "getFile", map[string]interface{}{"file_id": fileID})
	if err != nil {
		return ""
	}
	if result, ok := resp["result"].(map[string]interface{}); ok {
		if path, _ := result["file_path"].(string); path != "" {
			return a.fileBase + "/" + path
		}
	}
	return ""
}

// ---------- 流式输出（审计项 12） ----------

// StreamStart 发送一条真实消息作为流式载体并返回其 message_id。
// 对齐本体 tg_event.py:633-713 _send_streaming_edit（send_message +
// edit_message_text 节流编辑）：首片段 sendMessage，后续片段由宿主
// streamSender 以 ~2x/s 的频率调用 StreamUpdate 编辑同一条消息。
// 本体私聊用的 sendMessageDraft 为 Bot API 非公开接口，Go 侧统一采用
// edit_message_text 方案（宿主已做 500ms 节流，效果对齐）。
func (a *Adapter) StreamStart(sessionID, text string) (string, error) {
	chatID, threadID := splitThreadID(sessionID)
	a.sendChatAction(chatID, threadID, chatActionTyping)
	payload := a.sendPayload(chatID, threadID, "")
	payload["text"] = truncateRunes(text, maxMessageLength)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := a.apiCall(ctx, "sendMessage", payload)
	if err != nil {
		return "", err
	}
	result, _ := resp["result"].(map[string]interface{})
	msgID := ""
	if id, ok := result["message_id"].(float64); ok {
		msgID = fmt.Sprintf("%d", int64(id))
	}
	return msgID, nil
}

// StreamUpdate edits an in-progress streaming message（edit_message_text）。
func (a *Adapter) StreamUpdate(sessionID, msgID, text string) error {
	chatID, threadID := splitThreadID(sessionID)
	// typing 状态节流重发（本体 chat_action_interval=0.5s，:687-689）。
	a.throttledTyping(chatID, threadID)
	return a.editStreamText(chatID, threadID, msgID, text)
}

// StreamEnd finalizes the streaming message with its final text.
func (a *Adapter) StreamEnd(sessionID, msgID, text string) error {
	chatID, threadID := splitThreadID(sessionID)
	return a.editStreamText(chatID, threadID, msgID, text)
}

// editStreamText edits the streaming carrier message with the accumulated text.
func (a *Adapter) editStreamText(chatID, threadID, msgID, text string) error {
	if msgID == "" {
		return fmt.Errorf("telegram edit_message_text: message_id 为空")
	}
	if text == "" {
		return nil
	}
	payload := a.sendPayload(chatID, threadID, "")
	payload["text"] = truncateRunes(text, maxMessageLength)
	payload["message_id"] = msgID
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := a.apiCall(ctx, "edit_message_text", payload)
	return err
}

// streamTypingInterval 是流式输出期间 typing 的重发间隔（本体 :647）。
const streamTypingInterval = 500 * time.Millisecond

// throttledTyping 以 streamTypingInterval 节流重发 typing 状态。
func (a *Adapter) throttledTyping(chatID, threadID string) {
	key := chatID + "#" + threadID
	a.streamMu.Lock()
	if a.lastTyping == nil {
		a.lastTyping = make(map[string]time.Time)
	}
	if time.Since(a.lastTyping[key]) < streamTypingInterval {
		a.streamMu.Unlock()
		return
	}
	a.lastTyping[key] = time.Now()
	a.streamMu.Unlock()
	a.sendChatAction(chatID, threadID, chatActionTyping)
}

// ---------- 消息发送（本体 tg_event.py send_with_client:268-341） ----------

// maxMessageLength 是 Telegram 单条消息的最大文本长度（本体 :40）。
const maxMessageLength = 4096

// chat action 常量（本体 ChatAction 映射，tg_event.py:64-70）。
const (
	chatActionTyping         = "typing"
	chatActionUploadVoice    = "upload_voice"
	chatActionUploadVideo    = "upload_video"
	chatActionUploadDocument = "upload_document"
	chatActionUploadPhoto    = "upload_photo"
)

// Send sends a message chain to a Telegram chat. 对齐本体
// tg_event.py send_with_client：
//   - Reply 组件 → 所有消息附带 reply_to_message_id（:301-302）；
//   - At 组件 → 首段 Plain 文本前置 "@username "（:307-309）；
//   - 发送前按链内容发送 chat action（:293-295）；
//   - Image 按扩展名/魔数识别 GIF，用 sendAnimation（:313-318）；
//   - Record 语音在 Voice_messages_forbidden 时回退 sendDocument（:184-244）；
//   - 文本按 4096 长度切分发送（_split_message）。
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	if chain == nil {
		return nil
	}
	chatID, threadID := splitThreadID(sessionID)

	// 先扫描链收集 Reply（被引用消息 id）与 At（@前置文本），本体 :276-284。
	replyID, atName, atPrefixed := "", "", false
	for _, comp := range chain.Chain {
		switch c := comp.(type) {
		case *message.Reply:
			if c.MessageID != "" {
				replyID = c.MessageID
			}
		case *message.At:
			if c.Name != "" && atName == "" {
				atName = c.Name
			}
		}
	}

	// 按消息链确定合适的 chat action 并发送（本体 :293-295）。
	a.sendChatAction(chatID, threadID, chatActionForChain(chain.Chain))

	for _, comp := range chain.Chain {
		switch c := comp.(type) {
		case *message.Plain:
			text := c.Text
			if atName != "" && !atPrefixed {
				// At 组件前置文本（本体 :307-309）。
				text = "@" + atName + " " + text
				atPrefixed = true
			}
			if err := a.sendTextChunks(chatID, threadID, replyID, text); err != nil {
				return err
			}
		case *message.Image:
			// GIF 用 sendAnimation，否则 sendPhoto（本体 :313-318）。
			method, field := "sendPhoto", "photo"
			if isGIFComponent(c) {
				method, field = "sendAnimation", "animation"
			}
			if err := a.sendMedia(chatID, method, field, tgMedia{
				fileID: c.FileID, url: c.URL, path: c.Path, file: c.File, b64: c.Base64,
			}, a.sendPayload(chatID, threadID, replyID)); err != nil {
				return err
			}
		case *message.Record:
			// 语音（ogg/opus）走 sendVoice，其余音频格式（mp3/m4a/flac/wav）走 sendAudio。
			method, field := "sendVoice", "voice"
			if !isVoiceFormat(c.Format) {
				method, field = "sendAudio", "audio"
			}
			err := a.sendMedia(chatID, method, field, tgMedia{
				fileID: c.FileID, url: c.URL, path: c.Path, file: c.File, b64: c.Base64,
			}, a.sendPayload(chatID, threadID, replyID))
			if err != nil && method == "sendVoice" && isVoiceForbiddenErr(err) {
				// 对端隐私设置禁收语音时回退为文件发送（本体 tg_event.py:184-244）。
				logger.Warn("对方隐私设置禁止接收语音消息，回退为发送音频文件。如需语音消息，请前往 Telegram 设置 → 隐私与安全 → 语音消息 → 设为“所有人”。")
				payload := a.sendPayload(chatID, threadID, replyID)
				if c.Text != "" {
					payload["caption"] = c.Text
				}
				err = a.sendMedia(chatID, "sendDocument", "document", tgMedia{
					fileID: c.FileID, url: c.URL, path: c.Path, file: c.File, b64: c.Base64,
				}, payload)
			}
			if err != nil {
				return err
			}
		case *message.File:
			if err := a.sendMedia(chatID, "sendDocument", "document", tgMedia{
				fileID: c.FileID, url: c.URL, path: c.Path, uploadName: c.Name,
			}, a.sendPayload(chatID, threadID, replyID)); err != nil {
				return err
			}
		case *message.Video:
			if err := a.sendMedia(chatID, "sendVideo", "video", tgMedia{
				fileID: c.FileID, url: c.URL, path: c.Path,
			}, a.sendPayload(chatID, threadID, replyID)); err != nil {
				return err
			}
		}
	}
	return nil
}

// tgMedia 汇聚一个待发送媒体组件的取值来源（file_id / url / 本地路径 /
// base64）与上传展示名。
type tgMedia struct {
	fileID     string
	url        string
	path       string
	file       string
	b64        string
	uploadName string
}

// sendPayload 组装一次发送的公共参数：chat_id / message_thread_id /
// reply_to_message_id（对齐本体 tg_event.py:298-304 的 payload 构造）。
func (a *Adapter) sendPayload(chatID, threadID, replyID string) map[string]interface{} {
	payload := map[string]interface{}{
		"chat_id": chatID,
	}
	if threadID != "" {
		payload["message_thread_id"] = threadID
	}
	if replyID != "" {
		payload["reply_to_message_id"] = replyID
	}
	return payload
}

// sendTextChunks 按 Telegram 4096 长度限制切分文本后逐段发送。
// 对齐本体 _send_text_chunks（:108-130）；Go 侧无 telegramify_markdown
// 等价物，直接以纯文本发送（对齐本体的 Markdown 失败回退路径）。
func (a *Adapter) sendTextChunks(chatID, threadID, replyID, text string) error {
	if text == "" {
		return nil
	}
	for _, chunk := range splitMessage(text) {
		payload := a.sendPayload(chatID, threadID, replyID)
		payload["text"] = chunk
		// 用带超时的上下文发送，避免网络卡死时 sendMessage 无限期挂起。
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := a.apiCall(ctx, "sendMessage", payload)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

// splitPatterns 对齐本体 SPLIT_PATTERNS 的切分优先级（:42-47）。
var splitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\n\n`),     // paragraph
	regexp.MustCompile(`\n`),       // line
	regexp.MustCompile(`[.!?。！？]`), // sentence
	regexp.MustCompile(`\s`),       // word
}

// splitMessage 按 Telegram 限制切分长文本，优先在段落/行/句/词边界断开，
// 对齐本体 _split_message（:84-106）。以 rune 计数对齐 Python 的 len 语义。
func splitMessage(text string) []string {
	runes := []rune(text)
	if len(runes) <= maxMessageLength {
		return []string{text}
	}
	var chunks []string
	for len(runes) > 0 {
		if len(runes) <= maxMessageLength {
			chunks = append(chunks, string(runes))
			break
		}
		segment := string(runes[:maxMessageLength])
		splitPoint := maxMessageLength
		for _, pattern := range splitPatterns {
			if matches := pattern.FindAllStringIndex(segment, -1); len(matches) > 0 {
				splitPoint = matches[len(matches)-1][1]
				break
			}
		}
		chunks = append(chunks, segment[:splitPoint])
		rest := string(runes[splitPoint:])
		rest = strings.TrimLeftFunc(rest, unicode.IsSpace) // 本体 lstrip()
		runes = []rune(rest)
	}
	return chunks
}

// truncateRunes 按 rune 截断文本到 max 长度（edit/send 超 4096 会被
// Telegram 拒绝，流式编辑时对齐本体的 MAX_MESSAGE_LENGTH 上限）。
func truncateRunes(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max])
}

// chatActionForChain 根据消息链中的组件类型确定合适的 chat action
// （按本体 ACTION_BY_TYPE 的优先级，tg_event.py:64-70, 149-155）。
func chatActionForChain(chain []message.Component) string {
	for _, comp := range chain {
		switch comp.(type) {
		case *message.Record:
			return chatActionUploadVoice
		}
	}
	for _, comp := range chain {
		switch comp.(type) {
		case *message.Video:
			return chatActionUploadVideo
		}
	}
	for _, comp := range chain {
		switch comp.(type) {
		case *message.File:
			return chatActionUploadDocument
		}
	}
	for _, comp := range chain {
		switch comp.(type) {
		case *message.Image:
			return chatActionUploadPhoto
		}
	}
	return chatActionTyping
}

// sendChatAction 发送聊天状态动作，失败仅告警不阻断发送（本体 :132-147）。
func (a *Adapter) sendChatAction(chatID, threadID, action string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	payload := map[string]interface{}{
		"chat_id": chatID,
		"action":  action,
	}
	if threadID != "" {
		payload["message_thread_id"] = threadID
	}
	if _, err := a.apiCall(ctx, "sendChatAction", payload); err != nil {
		logger.Warn("[Telegram] 发送 chat action 失败: %v", err)
	}
}

// isGIFComponent 判断图片组件是否应作为 GIF 用 sendAnimation 发送。
// 对齐本体 _is_gif（:28-35）：扩展名或文件头魔数 GIF87a/GIF89a；
// file_id 场景无法本地判断，按普通图片处理。
func isGIFComponent(img *message.Image) bool {
	p := img.Path
	if p == "" {
		p = img.File
	}
	if p != "" && isGIFPath(p) {
		return true
	}
	if img.URL != "" {
		clean := strings.ToLower(img.URL)
		if i := strings.IndexByte(clean, '?'); i >= 0 {
			clean = clean[:i]
		}
		if strings.HasSuffix(clean, ".gif") {
			return true
		}
	}
	return false
}

// isGIFPath 按扩展名或文件头魔数判断本地文件是否为 GIF。
func isGIFPath(path string) bool {
	if strings.HasSuffix(strings.ToLower(path), ".gif") {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 6)
	if _, err := io.ReadFull(f, buf); err != nil {
		return false
	}
	return string(buf) == "GIF87a" || string(buf) == "GIF89a"
}

// isVoiceForbiddenErr 识别 sendVoice 返回的 Voice_messages_forbidden 错误
// （本体 tg_event.py:217-221）。
func isVoiceForbiddenErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "voice_messages_forbidden") ||
		strings.Contains(msg, "voice messages forbidden")
}

// isAllDigits 判断字符串是否全为数字（自定义表情 custom_emoji_id 语义）。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// sendMedia sends a single media component via the given Telegram method.
// payload 携带 chat_id 与公共附加参数（message_thread_id /
// reply_to_message_id / caption 等）。FileID / public https URLs are passed
// through directly; local paths and base64 payloads are uploaded as
// multipart/form-data.
func (a *Adapter) sendMedia(chatID, method, field string, m tgMedia, payload map[string]interface{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 1. file_id: reuse an already-uploaded Telegram file.
	if m.fileID != "" {
		params := cloneParams(payload)
		params[field] = m.fileID
		_, err := a.apiCall(ctx, method, params)
		return err
	}
	// 2. public https URL: Telegram fetches it server-side.
	if m.url != "" {
		params := cloneParams(payload)
		params[field] = m.url
		_, err := a.apiCall(ctx, method, params)
		return err
	}
	// 3. base64 payload: decode to a temp file and upload.
	if m.b64 != "" {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(m.b64))
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
		return a.sendMediaUpload(ctx, chatID, method, field, tmpName, m, payload)
	}
	// 4. local path / file.
	localPath := m.path
	if localPath == "" {
		localPath = m.file
	}
	if localPath == "" {
		return fmt.Errorf("%s: media has no file_id/url/path", method)
	}
	return a.sendMediaUpload(ctx, chatID, method, field, localPath, m, payload)
}

// cloneParams 复制公共参数 map，避免多个发送分支相互污染。
func cloneParams(payload map[string]interface{}) map[string]interface{} {
	params := make(map[string]interface{}, len(payload)+1)
	for k, v := range payload {
		params[k] = v
	}
	return params
}

// sendMediaUpload uploads a local file as multipart/form-data. 除 chat_id 外
// 还写入 payload 中的字符串附加参数（message_thread_id 等）；上传展示名优先
// 使用组件显式指定的名字（如 File.Name）。
func (a *Adapter) sendMediaUpload(ctx context.Context, chatID, method, field, filePath string, m tgMedia, payload map[string]interface{}) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open media %s: %w", filePath, err)
	}

	displayName := m.uploadName
	if displayName == "" {
		displayName = filepath.Base(filePath)
	}

	// io.Pipe 流式构造 multipart 请求体，避免整个文件读入内存。
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	go func() {
		defer f.Close()
		defer pw.Close()
		part, werr := writer.CreateFormFile(field, displayName)
		if werr != nil {
			return
		}
		if _, werr = io.Copy(part, f); werr != nil {
			return
		}
		if werr = writer.WriteField("chat_id", chatID); werr != nil {
			return
		}
		for k, v := range payload {
			if k == "chat_id" {
				continue
			}
			if werr = writer.WriteField(k, fmt.Sprint(v)); werr != nil {
				return
			}
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

// ---------- 指令注册（本体 tg_adapter.py:152-153, 196-209, 321-341） ----------

// tgBotCommand 对齐 telegram.BotCommand。
type tgBotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// tgCommandNamePattern 是 Telegram 命令名的合法性规则（本体 :400）。
var tgCommandNamePattern = regexp.MustCompile(`^[a-z0-9_]+$`)

// commandRegisterIntervalDefault 是指令自动刷新的默认间隔秒数（本体 :205）。
const commandRegisterIntervalDefault = 300

// registerCommands 收集所有注册的指令并注册到 Telegram，对齐本体
// register_commands（:321-341）：内容未变化时跳过，变化时先删后设。
func (a *Adapter) registerCommands() error {
	commands := a.collectCommands()
	if len(commands) == 0 {
		return nil
	}

	// 内容 hash 去重（本体 :327-332）。
	h := fnv.New32a()
	for _, cmd := range commands {
		h.Write([]byte(cmd.Command))
		h.Write([]byte{0})
		h.Write([]byte(cmd.Description))
		h.Write([]byte{0})
	}
	sum := h.Sum32()
	a.commandsMu.Lock()
	if sum == a.lastCommandHash {
		a.commandsMu.Unlock()
		return nil
	}
	a.commandsMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := a.apiCall(ctx, "deleteMyCommands", map[string]interface{}{}); err != nil {
		logger.Warn("Telegram deleteMyCommands 失败: %v", err)
	}
	list := make([]map[string]interface{}, 0, len(commands))
	names := make([]string, 0, len(commands))
	for _, cmd := range commands {
		list = append(list, map[string]interface{}{
			"command":     cmd.Command,
			"description": cmd.Description,
		})
		names = append(names, cmd.Command)
	}
	if _, err := a.apiCall(ctx, "setMyCommands", map[string]interface{}{"commands": list}); err != nil {
		return err
	}
	a.commandsMu.Lock()
	a.lastCommandHash = sum
	a.commandsMu.Unlock()
	logger.I18nInfo("Telegram 指令注册完成: %s", strings.Join(names, ", "))
	return nil
}

// collectCommands 从宿主 star 处理器注册表收集所有顶层指令（含别名与命令
// 组），对齐本体 collect_commands/_extract_command_info（:339-411）：
// 跳过 "start" 与不符合 ^[a-z0-9_]{1,32}$ 的名称；插件命令同样覆盖。
func (a *Adapter) collectCommands() []tgBotCommand {
	commandDict := make(map[string]string)
	if a.starMgr == nil {
		return nil
	}
	registry := a.starMgr.Handlers()
	if registry == nil {
		return nil
	}
	for _, handler := range registry.GetFilterHandlers() {
		if !handler.Enabled {
			continue
		}
		for _, filter := range handler.EventFilters {
			names, isGroup := extractTgCommandNames(filter)
			for _, name := range names {
				if name == "" || name == "start" {
					continue
				}
				if !tgCommandNamePattern.MatchString(name) || len(name) > 32 {
					continue
				}
				if _, dup := commandDict[name]; dup {
					logger.Warn("命令名 '%s' 重复注册，将使用首次注册的定义", name)
					continue
				}
				commandDict[name] = commandDescription(handler, name, isGroup)
			}
		}
	}
	names := make([]string, 0, len(commandDict))
	for name := range commandDict {
		names = append(names, name)
	}
	sort.Strings(names)
	commands := make([]tgBotCommand, 0, len(names))
	for _, name := range names {
		commands = append(commands, tgBotCommand{Command: name, Description: commandDict[name]})
	}
	return commands
}

// extractTgCommandNames 从事件过滤器提取指令名（含别名）；子命令/子命令组
// 不注册（对齐本体 _extract_command_info，:371-394）。isGroup 表示命令组。
func extractTgCommandNames(filter star.HandlerFilter) ([]string, bool) {
	switch f := filter.(type) {
	case *star.CommandFilter:
		if f.HasParentCommand() {
			return nil, false
		}
		names := []string{f.CommandName()}
		return append(names, f.Aliases()...), false
	case *star.CommandGroupFilter:
		if f.ParentGroupName() != "" {
			return nil, false
		}
		return []string{f.GroupName()}, true
	}
	return nil, false
}

// commandDescription 构造命令描述：优先 handler 描述，缺省用
// "Command: X" / "Command group: X"；超过 30 字符截断加省略号（本体 :403-408）。
func commandDescription(handler *star.StarHandlerMetadata, name string, isGroup bool) string {
	desc := handler.Desc
	if desc == "" {
		if isGroup {
			desc = "Command group: " + name
		} else {
			desc = "Command: " + name
		}
	}
	if runes := []rune(desc); len(runes) > 30 {
		desc = string(runes[:30]) + "..."
	}
	return desc
}

// commandRefreshLoop 周期性重新注册指令（插件热加载后内容变化时生效），
// 对齐本体 _start_command_scheduler 的 interval 任务（:196-209）。
func (a *Adapter) commandRefreshLoop(ctx context.Context) {
	interval := commandRegisterIntervalDefault
	if v, ok := a.config["telegram_command_register_interval"].(float64); ok && v > 0 {
		interval = int(v)
	}
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.registerCommands(); err != nil {
				logger.Warn("向 Telegram 注册指令时发生错误: %v", err)
			}
		}
	}
}

// ---------- 消息接收（本体 tg_adapter.py convert_message:439-670） ----------

// tgMsg 承载一次 update.message 的转换结果，字段对应本体 AstrBotMessage
// 的构造（tg_adapter.py:469-494），并按 core.Event/MessageObj 的需求供给。
type tgMsg struct {
	MessageID  string
	ChatID     string
	SessionID  string
	GroupID    string
	IsGroup    bool
	SenderID   string
	SenderName string
	GroupName  string
	MessageStr string
	Chain      []message.Component
	RawMessage map[string]interface{}
	TopicName  string
	Skip       bool
}

// handleUpdate processes a single Telegram update.
func (a *Adapter) handleUpdate(ctx context.Context, update map[string]interface{}) {
	msg, ok := update["message"].(map[string]interface{})
	if !ok {
		return
	}

	// 相册消息走 debounce 合并（本体 tg_adapter.py:424-437, 672-782）。
	if mgid, _ := msg["media_group_id"].(string); mgid != "" {
		a.enqueueMediaGroup(mgid, msg)
		return
	}

	m := a.convertMessage(ctx, msg, true)
	if m == nil || m.Skip {
		return
	}
	a.publishMsg(m)
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
			// 空闲超时自动退出并从 map 删除，避免常驻 goroutine 只增不减。
			idle := time.NewTicker(workerIdleTimeout)
			defer idle.Stop()
			for {
				select {
				case <-idle.C:
					a.workerMu.Lock()
					if len(c) == 0 {
						delete(a.workers, chatID)
						a.workerMu.Unlock()
						return
					}
					a.workerMu.Unlock()
				case u, ok := <-c:
					if !ok {
						return
					}
					idle.Reset(workerIdleTimeout)
					a.handleUpdate(ctx, u)
				case <-ctx.Done():
					return
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

// publishMsg 为转换结果构造 core.Event 并发布到事件总线：
//   - 补全 MessageObj（审计项 1，对齐本体 tg_adapter.py:469-494 的
//     AstrBotMessage 构造，供 pre_ack_emoji 等读取 MessageObj.MessageID）；
//   - 处理入口发送一次 chat action（审计项 8，本体 tg_event.py:132-155）。
func (a *Adapter) publishMsg(m *tgMsg) {
	msgType := "FriendMessage"
	if m.IsGroup {
		msgType = "GroupMessage"
	}

	// @bot / 命令唤醒提示（供宿主 WakingCheck 使用）：命令前缀，或链中
	// At 组件指向本 bot（bot mention 已从 MessageStr 移除，见
	// appendEntityComponents），或数字 self_id 出现在文本中。
	isAtBot := strings.HasPrefix(m.MessageStr, "/")
	if !isAtBot && a.selfUsername != "" {
		for _, comp := range m.Chain {
			if at, ok := comp.(*message.At); ok {
				if strings.EqualFold(at.Name, a.selfUsername) ||
					strings.EqualFold(at.TargetID, a.selfUsername) {
					isAtBot = true
					break
				}
			}
		}
	}
	if !isAtBot && a.SelfID != "" && strings.Contains(m.MessageStr, "@"+a.SelfID) {
		isAtBot = true
	}

	// 处理入口发送一次 chat action，按链内容选择动作类型（审计项 8）。
	_, threadID := splitThreadID(m.SessionID)
	a.sendChatAction(m.ChatID, threadID, chatActionForChain(m.Chain))

	rawJSONStr := ""
	if b, err := json.Marshal(m.RawMessage); err == nil {
		rawJSONStr = string(b)
	}

	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   "telegram",
			PlatformID: a.ID(),
			SelfID:     a.SelfID,
			SenderID:   m.SenderID,
			SenderName: m.SenderName,
			ConvID:     m.SessionID,
			IsGroup:    m.IsGroup,
			IsAtBot:    isAtBot,
		},
		Message:           &message.MessageChain{Chain: m.Chain},
		MessageStr:        m.MessageStr,
		RawMessage:        rawJSONStr,
		IsAtOrWakeCommand: isAtBot,
		Timestamp:         time.Now(),
		Metadata:          make(map[string]interface{}),
		// 审计项 1：补全 MessageObj（对齐本体 AstrBotMessage 构造，
		// message_id/self_id/session_id/group_id/type/sender/message_str）。
		MessageObj: &core.MessageObj{
			MessageID:   m.MessageID,
			SelfID:      a.SelfID,
			SessionID:   m.SessionID,
			MessageType: msgType,
			Platform:    "telegram",
			MessageStr:  m.MessageStr,
			RawMessage:  m.RawMessage,
			Timestamp:   time.Now(),
		},
	}

	if m.IsGroup && m.GroupID != "" {
		event.MessageObj.Group = &core.Group{
			GroupID:   m.GroupID,
			GroupName: m.GroupName,
		}
	}

	if err := a.EventBus.Publish(event); err != nil {
		logger.Error("Failed to publish event: %v", err)
	}
}

// convertMessage 将 Telegram update.message 转换为内部消息对象。
// 对齐本体 tg_adapter.py:439-670 convert_message；getReply=false 用于
// 引用递归与相册合并场景，防止多层嵌套。
func (a *Adapter) convertMessage(ctx context.Context, msg map[string]interface{}, getReply bool) *tgMsg {
	if msg == nil {
		logger.Warn("收到不含 message 的 update")
		return nil
	}
	m := &tgMsg{RawMessage: msg}

	// 会话/群组信息（本体 :470-481）。
	chat, _ := msg["chat"].(map[string]interface{})
	chatID := ""
	chatType := ""
	if chat != nil {
		if id := int64FromAny(chat["id"]); id != 0 {
			chatID = fmt.Sprintf("%d", id)
		}
		chatType, _ = chat["type"].(string)
	}
	m.ChatID = chatID
	m.SessionID = chatID
	if chatType == "private" {
		m.IsGroup = false
	} else {
		m.IsGroup = true
		groupID := chatID
		isForum, _ := chat["is_forum"].(bool)
		var threadID int64
		if tid := int64FromAny(msg["message_thread_id"]); tid > 0 {
			threadID = tid
		}
		if !(isForum && threadID == 1) && threadID > 0 {
			groupID = fmt.Sprintf("%s#%d", chatID, threadID)
			m.SessionID = groupID
		}
		m.GroupID = groupID

		chatTitle, _ := chat["title"].(string)
		topicName := a.resolveForumTopicName(chatID, threadID, isForum, msg)
		if chatTitle != "" && topicName != "" {
			chatTitle = chatTitle + "-" + topicName
		} else if topicName != "" {
			chatTitle = topicName
		}
		if chatTitle != "" {
			m.GroupName = chatTitle
		}
		m.TopicName = topicName
	}
	if id := int64FromAny(msg["message_id"]); id != 0 {
		m.MessageID = fmt.Sprintf("%d", id)
	}

	// 发送者（本体 :483-490）：nickname 取 username；Go 侧补充
	// first+last 展示名回退，均缺失时用 "Unknown"。
	from, ok := msg["from"].(map[string]interface{})
	if !ok || from == nil {
		logger.Warn("[Telegram] 收到不含 from_user 的消息")
		return nil
	}
	if id := int64FromAny(from["id"]); id != 0 {
		m.SenderID = fmt.Sprintf("%d", id)
	}
	if username, _ := from["username"].(string); username != "" {
		m.SenderName = username
	} else if m.SenderName = joinName(from); m.SenderName == "" {
		m.SenderName = "Unknown"
	}

	m.Chain = make([]message.Component, 0, 4)

	// 引用消息（审计项 2，本体 :496-527）。
	if getReply {
		a.appendReplyComponent(ctx, msg, m)
	}

	// 各消息类型分支（本体 :529-670 的 if/elif 链）。
	switch {
	case msg["text"] != nil:
		a.convertTextMessage(msg, m)
	case msg["voice"] != nil:
		if record := a.handleAudioMessage(ctx, msg, "voice"); record != nil {
			m.Chain = append(m.Chain, record)
		}
	case msg["audio"] != nil:
		// 音频走独立字段，不回退 document（本体 :591-608）。
		if record := a.handleAudioMessage(ctx, msg, "audio"); record != nil {
			m.Chain = append(m.Chain, record)
		}
		a.applyCaption(msg, m)
	case msg["photo"] != nil:
		if img := a.handlePhotoMessage(ctx, msg); img != nil {
			m.Chain = append(m.Chain, img)
		}
		a.applyCaption(msg, m)
	case msg["sticker"] != nil:
		if img := a.handleStickerMessage(ctx, msg); img != nil {
			m.Chain = append(m.Chain, img)
		}
		if st, ok := msg["sticker"].(map[string]interface{}); ok {
			if emoji, _ := st["emoji"].(string); emoji != "" {
				// 贴纸 emoji 以文本形式附在 message_str（本体 :628-631）。
				stickerText := "Sticker: " + emoji
				m.MessageStr = stickerText
				m.Chain = append(m.Chain, &message.Plain{Text: stickerText})
			}
		}
	case msg["document"] != nil:
		if f := a.handleDocumentMessage(ctx, msg); f != nil {
			m.Chain = append(m.Chain, f)
		}
		a.applyCaption(msg, m)
	case msg["video"] != nil:
		if v := a.handleVideoMessage(ctx, msg); v != nil {
			m.Chain = append(m.Chain, v)
		}
		a.applyCaption(msg, m)
	case msg["video_note"] != nil:
		// 视频笔记无 file_name 也没有 caption（本体 :659-668）。
		if v := a.handleVideoNoteMessage(ctx, msg); v != nil {
			m.Chain = append(m.Chain, v)
		}
	}
	return m
}

// convertTextMessage 处理文本消息：群聊回复机器人时前置 /@bot、/cmd@bot
// 命令后缀剥离、entities → At、/start 欢迎语（本体 :529-572）。
func (a *Adapter) convertTextMessage(msg map[string]interface{}, m *tgMsg) {
	text, _ := msg["text"].(string)
	plain := text

	// 群聊回复机器人消息时前置 "/@bot "，使命令解析可见（本体 :532-540）。
	if m.IsGroup && a.selfUsername != "" && a.isReplyToBot(msg) {
		plain = "/@" + a.selfUsername + " " + plain
	}

	// /cmd@bot 命令后缀剥离（审计项 9，本体 :543-551）。
	plain = a.stripCommandTarget(plain)

	// entities → At 组件（审计项 4，本体 :552-564）。
	plain = a.appendEntityComponents(msg["entities"], plain, &m.Chain, true)

	if plain != "" {
		m.Chain = append(m.Chain, &message.Plain{Text: plain})
	}
	m.MessageStr = plain

	// /start 欢迎语（审计项 10，本体 :570-572 → start :413-422）：
	// 回复欢迎语后不产生事件（本体 return None）。
	if strings.TrimSpace(plain) == "/start" {
		a.sendStartMessage(m.ChatID)
		m.Skip = true
		return
	}
}

// appendReplyComponent 处理引用消息（审计项 2，本体 tg_adapter.py:496-527）：
// reply_to_message 递归转换（get_reply=False 防嵌套）生成 Reply 组件；
// 本体 quote.text（引用面板展示的片段文本）存在时以其替换被引用链文本
// （对齐 quote.text 截断语义）；话题群回复"创建话题"消息时跳过。
func (a *Adapter) appendReplyComponent(ctx context.Context, msg map[string]interface{}, m *tgMsg) {
	replyRaw, ok := msg["reply_to_message"].(map[string]interface{})
	if !ok || replyRaw == nil {
		return
	}
	// 话题群的建话题消息不是真实引用（本体 :497-500）。
	if isTopic, _ := msg["is_topic_message"].(bool); isTopic {
		if threadID := int64FromAny(msg["message_thread_id"]); threadID > 0 &&
			threadID == int64FromAny(replyRaw["message_id"]) {
			return
		}
	}
	reply := a.convertMessage(ctx, replyRaw, false)
	if reply == nil {
		return
	}
	replyChain := make([]message.Component, len(reply.Chain))
	copy(replyChain, reply.Chain)
	replyStr := reply.MessageStr
	if quote, ok := msg["quote"].(map[string]interface{}); ok {
		if quoteText, _ := quote["text"].(string); quoteText != "" {
			// 引用片段优先（本体 :509-514）。
			replyChain = []message.Component{&message.Plain{Text: quoteText}}
			replyStr = quoteText
		}
	}
	m.Chain = append(m.Chain, &message.Reply{
		MessageID:  reply.MessageID,
		SenderID:   reply.SenderID,
		SenderNick: reply.SenderName,
		Chain:      replyChain,
		MessageStr: replyStr,
		CreatedAt:  time.Unix(int64FromAny(replyRaw["date"]), 0),
	})
}

// applyCaption 处理媒体 caption（本体 _apply_caption，tg_adapter.py:455-467）：
// caption 文本作为 message_str 与 Plain 追加；caption_entities 中的
// mention/text_mention 追加为 At 组件（不移除文本）。
func (a *Adapter) applyCaption(msg map[string]interface{}, m *tgMsg) {
	caption, _ := msg["caption"].(string)
	if caption != "" {
		m.MessageStr = caption
		m.Chain = append(m.Chain, &message.Plain{Text: caption})
	}
	a.appendEntityComponents(msg["caption_entities"], caption, &m.Chain, false)
}

// appendEntityComponents 将消息 entities 中的 mention/text_mention 解析为
// At 组件（审计项 4；对齐本体 tg_adapter.py:461-467, 552-564，text_mention
// 为审计项 4 补齐的能力面）。removeBotMention 为 true 时（文本消息场景），
// 指向当前 bot 的 @mention 从文本中移除（本体 :559-564）。
// entity.offset/length 以 UTF-16 code units 计，需换算为 Go 字符串字节下标。
func (a *Adapter) appendEntityComponents(entities interface{}, text string, chain *[]message.Component, removeBotMention bool) string {
	arr, ok := entities.([]interface{})
	if !ok || text == "" {
		return text
	}
	for _, e := range arr {
		em, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		entityType, _ := em["type"].(string)
		offset := int(int64FromAny(em["offset"]))
		length := int(int64FromAny(em["length"]))
		if length <= 0 || offset < 0 {
			continue
		}
		start := utf16IndexToByteIndex(text, offset)
		end := utf16IndexToByteIndex(text, offset+length)
		if end <= start || end > len(text) {
			continue
		}
		switch entityType {
		case "mention": // @username
			name := strings.TrimPrefix(text[start:end], "@")
			if name == "" {
				continue
			}
			*chain = append(*chain, &message.At{TargetID: name, Name: name})
			// mention 指向当前 bot 时从文本移除（本体 :559-564）。
			if removeBotMention && a.selfUsername != "" &&
				strings.EqualFold(name, a.selfUsername) {
				text = text[:start] + text[end:]
			}
		case "text_mention": // 内联用户提及（无 @username 的用户）
			user, _ := em["user"].(map[string]interface{})
			if user == nil {
				continue
			}
			at := &message.At{}
			if id := int64FromAny(user["id"]); id != 0 {
				at.TargetID = fmt.Sprintf("%d", id)
			}
			if uname, _ := user["username"].(string); uname != "" {
				at.Name = uname
			} else {
				at.Name = joinName(user)
			}
			if at.TargetID == "" && at.Name == "" {
				continue
			}
			*chain = append(*chain, at)
		}
	}
	return text
}

// isReplyToBot 判断群聊消息是否回复了机器人自身的消息（本体 :532-538）。
func (a *Adapter) isReplyToBot(msg map[string]interface{}) bool {
	replyRaw, ok := msg["reply_to_message"].(map[string]interface{})
	if !ok || replyRaw == nil {
		return false
	}
	from, _ := replyRaw["from"].(map[string]interface{})
	if from == nil {
		return false
	}
	return a.SelfID != "" && fmt.Sprintf("%d", int64FromAny(from["id"])) == a.SelfID
}

// stripCommandTarget 剥离 "/cmd@botname" 命令中的 @bot 后缀（审计项 9，
// 本体 tg_adapter.py:543-551），仅当 botname 与本 bot 用户名一致时剥离。
func (a *Adapter) stripCommandTarget(text string) string {
	if !strings.HasPrefix(text, "/") {
		return text
	}
	parts := strings.SplitN(text, " ", 2)
	if !strings.Contains(parts[0], "@") {
		return text
	}
	cmdBits := strings.SplitN(parts[0], "@", 2)
	if a.selfUsername != "" && cmdBits[1] == a.selfUsername {
		if len(parts) > 1 {
			return cmdBits[0] + " " + parts[1]
		}
		return cmdBits[0]
	}
	return text
}

// sendStartMessage 回复配置的 start_message 欢迎语（审计项 10，
// 本体 tg_adapter.py:413-422 start()）。
func (a *Adapter) sendStartMessage(chatID string) {
	if a.startMessage == "" {
		logger.Warn("收到 /start 但未配置 start_message，跳过欢迎语回复")
		return
	}
	payload := map[string]interface{}{
		"chat_id": chatID,
		"text":    a.startMessage,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := a.apiCall(ctx, "sendMessage", payload); err != nil {
		logger.Warn("发送 /start 欢迎语失败: %v", err)
	}
}

// ---------- 相册合并（审计项 7，本体 tg_adapter.py:424-437, 672-782） ----------

// mediaGroupDebounce 是相册合并的 debounce 窗口：首图到达后启动计时，
// 窗口内到达的后续图片并入同一事件。本体默认 2.5s debounce（media_group_timeout），
// Telegram 相册消息几乎同时到达，按审计要求取 500ms 更快完成合并。
const mediaGroupDebounce = 500 * time.Millisecond

// mediaGroupMaxWait 是相册合并的硬上限，防止无限延迟
// （对齐本体 media_group_max_wait，:708-714 达到上限立即处理）。
const mediaGroupMaxWait = 10 * time.Second

// mediaGroupEntry 缓存一个相册（media_group_id）已到达的消息项。
type mediaGroupEntry struct {
	items     []map[string]interface{}
	createdAt time.Time
	timer     *time.Timer
}

// enqueueMediaGroup 把相册成员加入缓存并在 debounce 后合并处理
// （对齐本体 handle_media_group_message，:672-731）。
func (a *Adapter) enqueueMediaGroup(mediaGroupID string, msg map[string]interface{}) {
	a.mediaMu.Lock()
	if a.mediaGroups == nil {
		a.mediaGroups = make(map[string]*mediaGroupEntry)
	}
	entry, ok := a.mediaGroups[mediaGroupID]
	if !ok {
		entry = &mediaGroupEntry{createdAt: time.Now()}
		entry.timer = time.AfterFunc(mediaGroupDebounce, func() {
			a.processMediaGroup(mediaGroupID)
		})
		a.mediaGroups[mediaGroupID] = entry
		logger.Debug("创建相册合并缓存: %s", mediaGroupID)
	}
	entry.items = append(entry.items, msg)
	logger.Debug("相册 %s 已累计 %d 项", mediaGroupID, len(entry.items))
	expired := time.Since(entry.createdAt) >= mediaGroupMaxWait
	a.mediaMu.Unlock()

	if expired {
		// 达到硬上限：立即合并处理，避免无限延迟（本体 :708-714）。
		entry.timer.Stop()
		a.processMediaGroup(mediaGroupID)
	}
}

// processMediaGroup 合并相册全部消息：首条消息完整转换（含引用/caption），
// 其余消息仅把组件列表补入同一事件（get_reply=False，对齐本体
// process_media_group，:733-782）。
func (a *Adapter) processMediaGroup(mediaGroupID string) {
	a.mediaMu.Lock()
	entry, ok := a.mediaGroups[mediaGroupID]
	if ok {
		delete(a.mediaGroups, mediaGroupID)
	}
	a.mediaMu.Unlock()
	if !ok || len(entry.items) == 0 {
		return
	}

	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	logger.Info("处理相册 %s，共 %d 项", mediaGroupID, len(entry.items))

	first := a.convertMessage(ctx, entry.items[0], true)
	if first == nil {
		logger.Warn("相册 %s 首条消息转换失败", mediaGroupID)
		return
	}
	for _, item := range entry.items[1:] {
		extra := a.convertMessage(ctx, item, false)
		if extra == nil {
			continue
		}
		first.Chain = append(first.Chain, extra.Chain...)
	}
	a.publishMsg(first)
}

// ---------- 工具函数 ----------

// int64FromAny 从 JSON 数值取 int64（encoding/json 数字默认 float64）。
func int64FromAny(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

// joinName 拼接 first_name/last_name 为展示名。
func joinName(user map[string]interface{}) string {
	first, _ := user["first_name"].(string)
	last, _ := user["last_name"].(string)
	return strings.TrimSpace(strings.TrimSpace(first) + " " + strings.TrimSpace(last))
}

// utf16IndexToByteIndex 把 Telegram entity 的 UTF-16 偏移换算为 Go 字符串的
// 字节下标（entity.offset/length 按 UTF-16 code units 计，增补平面字符占
// 2 个 code unit）。
func utf16IndexToByteIndex(s string, u16 int) int {
	if u16 <= 0 {
		return 0
	}
	units := 0
	for i, r := range s {
		if units >= u16 {
			return i
		}
		if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	return len(s)
}

// splitThreadID 把 "chatid#thread_id" 形式的会话 id 拆为 chat_id 与
// thread_id（超级群话题，本体 send_with_client :288-291）。
func splitThreadID(sessionID string) (chatID, threadID string) {
	if idx := strings.IndexByte(sessionID, '#'); idx >= 0 {
		return sessionID[:idx], sessionID[idx+1:]
	}
	return sessionID, ""
}

// ---------- 媒体下载与格式识别（原实现保持不变） ----------

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

	tmpName := a.handleMediaDownload(ctx, fileID, field)
	if tmpName == "" {
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

// handleMediaDownload 经 getFile 获取 Telegram 文件信息并下载到本地临时文件，
// 返回临时文件路径；失败时返回空字符串。临时文件由 scheduleAudioCleanup 延迟清理。
func (a *Adapter) handleMediaDownload(ctx context.Context, fileID, field string) string {
	if fileID == "" {
		logger.Warn("Telegram %s 消息缺少 file_id", field)
		return ""
	}
	resp, err := a.apiCall(ctx, "getFile", map[string]interface{}{"file_id": fileID})
	if err != nil {
		logger.Warn("Telegram getFile 失败 (%s): %v", field, err)
		return ""
	}
	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		logger.Warn("Telegram getFile 返回异常 (%s)", field)
		return ""
	}
	filePath, _ := result["file_path"].(string)
	if filePath == "" {
		logger.Warn("Telegram getFile 缺少 file_path (%s)", field)
		return ""
	}

	tmp, err := os.CreateTemp("", "astrbot-tg-"+field+"-*")
	if err != nil {
		logger.Warn("创建 Telegram %s 临时文件失败: %v", field, err)
		return ""
	}
	tmpName := tmp.Name()
	_ = tmp.Close()

	if err := utils.DownloadFile(ctx, a.fileBase+"/"+filePath, tmpName); err != nil {
		logger.Warn("Telegram %s 文件下载失败: %v", field, err)
		_ = os.Remove(tmpName)
		return ""
	}
	scheduleAudioCleanup(tmpName)
	return tmpName
}

// handlePhotoMessage 处理 Telegram 图片消息：取数组最后一个（最大尺寸）下载为 Image。
func (a *Adapter) handlePhotoMessage(ctx context.Context, msg map[string]interface{}) *message.Image {
	sizes, ok := msg["photo"].([]interface{})
	if !ok || len(sizes) == 0 {
		return nil
	}
	s, ok := sizes[len(sizes)-1].(map[string]interface{})
	if !ok {
		return nil
	}
	fileID, _ := s["file_id"].(string)
	path := a.handleMediaDownload(ctx, fileID, "photo")
	if path == "" {
		return nil
	}
	return &message.Image{File: path, Path: path}
}

// handleStickerMessage 处理 Telegram 贴纸消息：动图/视频贴纸回退缩略图，附 emoji 文本。
func (a *Adapter) handleStickerMessage(ctx context.Context, msg map[string]interface{}) *message.Image {
	st, ok := msg["sticker"].(map[string]interface{})
	if !ok {
		return nil
	}
	fileID := ""
	if animated, _ := st["is_animated"].(bool); animated {
		if thumb, ok := st["thumbnail"].(map[string]interface{}); ok {
			fileID, _ = thumb["file_id"].(string)
		}
	} else if video, _ := st["is_video"].(bool); video {
		if thumb, ok := st["thumbnail"].(map[string]interface{}); ok {
			fileID, _ = thumb["file_id"].(string)
		}
	} else {
		fileID, _ = st["file_id"].(string)
	}
	path := a.handleMediaDownload(ctx, fileID, "sticker")
	if path == "" {
		return nil
	}
	return &message.Image{File: path, Path: path}
}

// handleDocumentMessage 处理 Telegram 文件消息，构造 File 组件。
func (a *Adapter) handleDocumentMessage(ctx context.Context, msg map[string]interface{}) *message.File {
	doc, ok := msg["document"].(map[string]interface{})
	if !ok {
		return nil
	}
	fileID, _ := doc["file_id"].(string)
	name, _ := doc["file_name"].(string)
	if name == "" {
		name = fileID
	}
	path := a.handleMediaDownload(ctx, fileID, "document")
	if path == "" {
		return nil
	}
	return &message.File{Path: path, Name: name}
}

// handleVideoMessage 处理 Telegram 视频消息，构造 Video 组件。
func (a *Adapter) handleVideoMessage(ctx context.Context, msg map[string]interface{}) *message.Video {
	vid, ok := msg["video"].(map[string]interface{})
	if !ok {
		return nil
	}
	fileID, _ := vid["file_id"].(string)
	path := a.handleMediaDownload(ctx, fileID, "video")
	if path == "" {
		return nil
	}
	return &message.Video{Path: path}
}

// handleVideoNoteMessage 处理 Telegram 视频笔记消息，构造 Video 组件。
func (a *Adapter) handleVideoNoteMessage(ctx context.Context, msg map[string]interface{}) *message.Video {
	note, ok := msg["video_note"].(map[string]interface{})
	if !ok {
		return nil
	}
	fileID, _ := note["file_id"].(string)
	path := a.handleMediaDownload(ctx, fileID, "video_note")
	if path == "" {
		return nil
	}
	return &message.Video{Path: path}
}

// tempAudioCleanupDelay 是临时音频文件的清理延迟：消息在事件总线中异步处理，
// 延迟清理保证文件在消费/转发期间可用。
const tempAudioCleanupDelay = 30 * time.Minute

// workerIdleTimeout 是每 chat worker 的空闲回收超时：超过该时间没有新 update
// 且队列为空时，worker 自动退出并从 map 中删除，避免常驻 goroutine 累积。
const workerIdleTimeout = 30 * time.Minute

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
