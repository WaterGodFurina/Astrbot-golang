package discord

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/internal/star"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

var logger = log.GetDefault().WithComponent("Discord")

// slashCommandNamePattern mirrors the Python discord name validation.
var slashCommandNamePattern = regexp.MustCompile(`^[-_'\w]{1,32}$`)

// Adapter implements the Discord bot adapter.
type Adapter struct {
	config   map[string]interface{}
	settings map[string]interface{}

	EventBus *core.EventBus

	token             string
	proxy             string
	enableCmdRegister bool
	activityName      string
	guildID           string
	allowBotMessages  bool
	botSelfID         string

	starMgr *star.Manager

	session *discordgo.Session
	stopCh  chan struct{}
	once    sync.Once

	// registeredCommands tracks the last registered slash commands so
	// terminate() can clean them up.
	registeredCommands []*discordgo.ApplicationCommand
	commandsMu         sync.Mutex

	// followups maps channelID -> pending slash-command interaction so the
	// reply is delivered through the interaction followup webhook (mirrors
	// DiscordPlatformEvent.interaction_followup_webhook).
	followupsMu sync.Mutex
	followups   map[string]followupEntry
}

// followupEntry records a pending slash-command interaction together with the
// time it was registered so a stale entry (whose interaction token expires
// after ~15 minutes) is not used to reply via an invalid webhook.
type followupEntry struct {
	interaction  *discordgo.Interaction
	registeredAt time.Time
}

// followupMaxAge bounds how long a slash-command interaction may be replied
// to via its followup webhook. Older entries fall back to a normal channel
// message (Discord interaction tokens expire after roughly 15 minutes).
const followupMaxAge = 5 * time.Minute

// New creates a Discord adapter.
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		config:   config,
		settings: settings,
		EventBus: eventBus,
		stopCh:   make(chan struct{}),
	}
	a.token, _ = config["discord_token"].(string)
	a.proxy, _ = config["discord_proxy"].(string)
	a.enableCmdRegister = true
	if v, ok := config["discord_command_register"].(bool); ok {
		a.enableCmdRegister = v
	}
	a.activityName, _ = config["discord_activity_name"].(string)
	a.guildID, _ = config["discord_guild_id_for_debug"].(string)
	a.allowBotMessages, _ = config["discord_allow_bot_messages"].(bool)
	a.followups = make(map[string]followupEntry)
	return a
}

// SetEventBus injects the event bus (implements platform.EventBusSetter).
func (a *Adapter) SetEventBus(bus platform.EventBus) {
	if eb, ok := bus.(*core.EventBus); ok {
		a.EventBus = eb
	}
}

// SetStarManager injects the star handler registry (slash command source).
func (a *Adapter) SetStarManager(mgr interface{}) {
	if m, ok := mgr.(*star.Manager); ok {
		a.starMgr = m
	}
}

// ID returns the adapter instance id.
func (a *Adapter) ID() string {
	if id, ok := a.config["id"].(string); ok {
		return id
	}
	return "discord"
}

// Type returns the platform type.
func (a *Adapter) Type() string { return "discord" }

// Start connects to the Discord gateway and registers handlers.
func (a *Adapter) Start(ctx context.Context) error {
	if a.token == "" {
		return fmt.Errorf("discord: bot token is not configured, please set a valid token in the config file")
	}

	session, err := discordgo.New("Bot " + a.token)
	if err != nil {
		return err
	}
	a.session = session

	// HTTP proxy support (discord_proxy).
	if a.proxy != "" {
		proxyURL, err := url.Parse(a.proxy)
		if err == nil {
			session.Client = &http.Client{
				Timeout: 30 * time.Second,
				Transport: &http.Transport{
					Proxy: http.ProxyURL(proxyURL),
				},
			}
		}
	}

	// Gateway intents: guilds + guild messages + direct messages + message
	// content (privileged intent; mirrors the Python client).
	session.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsMessageContent

	// Message handler (mirrors DiscordBotClient.on_message).
	session.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		a.handleMessage(s, m)
	})

	// Slash command interaction handler (mirrors the dynamic callbacks
	// registered by _collect_and_register_commands).
	session.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		a.handleInteraction(s, i)
	})

	// on_ready_once callback: register slash commands + set activity.
	session.AddHandlerOnce(func(s *discordgo.Session, r *discordgo.Ready) {
		a.botSelfID = s.State.User.ID
		logger.I18nInfo("Discord 机器人已连接, self_id=%s", a.botSelfID)
		if a.enableCmdRegister {
			if err := a.registerCommands(s); err != nil {
				logger.I18nWarn("Discord 指令注册失败: %v", err)
			}
		}
		if a.activityName != "" {
			usd := &discordgo.UpdateStatusData{
				Status:     "online",
				Activities: []*discordgo.Activity{{Type: discordgo.ActivityTypeCustom, Name: a.activityName}},
			}
			if err := s.UpdateStatusComplex(*usd); err != nil {
				logger.I18nWarn("Discord 设置活动状态失败: %v", err)
			}
		}
	})

	logger.I18nInfo("Discord 适配器启动中...")
	if err := session.Open(); err != nil {
		if isLoginFailure(err) {
			return fmt.Errorf("discord: login failed, please check whether the bot token is correct")
		}
		return err
	}
	return nil
}

// Stop closes the gateway connection and cleans up slash commands.
func (a *Adapter) Stop() error {
	a.once.Do(func() {
		close(a.stopCh)
	})
	if a.session == nil {
		return nil
	}
	if a.enableCmdRegister && a.session.State != nil && a.session.State.User != nil {
		// Clean up registered commands (bulk overwrite with empty list).
		appID := a.session.State.User.ID
		if a.guildID != "" {
			_, _ = a.session.ApplicationCommandBulkOverwrite(appID, a.guildID, []*discordgo.ApplicationCommand{})
		} else {
			_, _ = a.session.ApplicationCommandBulkOverwrite(appID, "", []*discordgo.ApplicationCommand{})
		}
	}
	if err := a.session.Close(); err != nil {
		logger.I18nWarn("Discord 连接关闭失败: %v", err)
	}
	logger.I18nInfo("Discord 适配器已关闭")
	return nil
}

// handleMessage converts an incoming message into an AstrBot event
// (mirrors on_received + convert_message + handle_msg).
func (a *Adapter) handleMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Message == nil {
		return
	}
	// Ignore messages from other bots unless allow_bot_messages.
	if m.Author.Bot && !a.allowBotMessages {
		return
	}

	abm := a.convertMessage(s, m.Message)
	if abm == nil {
		return
	}
	a.handleMsg(abm)
}

// convertMessage builds an AstrBotMessage (mirrors _convert_message_to_abm).
func (a *Adapter) convertMessage(s *discordgo.Session, msg *discordgo.Message) *platform.AstrBotMessage {
	content := msg.Content

	// Strip user mentions of the bot.
	if s.State != nil && s.State.User != nil {
		mentionStr := fmt.Sprintf("<@%s>", s.State.User.ID)
		mentionStrNick := fmt.Sprintf("<@!%s>", s.State.User.ID)
		if strings.HasPrefix(content, mentionStr) {
			content = strings.TrimLeft(content[len(mentionStr):], " ")
		} else if strings.HasPrefix(content, mentionStrNick) {
			content = strings.TrimLeft(content[len(mentionStrNick):], " ")
		}
	}

	isGroup := msg.GuildID != ""
	abm := platform.NewAstrBotMessage()
	if isGroup {
		abm.Type = platform.GroupMessage
	} else {
		abm.Type = platform.FriendMessage
	}
	abm.Group = &platform.Group{GroupID: msg.ChannelID}
	abm.MessageStr = content
	abm.Sender = platform.MessageMember{
		UserID:   msg.Author.ID,
		Nickname: msg.Author.Username,
	}

	chain := []message.Component{}
	if abm.MessageStr != "" {
		chain = append(chain, &message.Plain{Text: abm.MessageStr})
	}
	// Attachments: image/audio/other (mirrors Python attachment handling).
	for _, att := range msg.Attachments {
		ct := att.ContentType
		switch {
		case strings.HasPrefix(ct, "image/"):
			chain = append(chain, &message.Image{URL: att.URL})
		case strings.HasPrefix(ct, "audio/"):
			chain = append(chain, &message.Record{URL: att.URL, File: att.URL})
		default:
			chain = append(chain, &message.File{Name: att.Filename, URL: att.URL})
		}
	}
	abm.Message = chain
	abm.RawMessage = msg
	abm.SelfID = a.botSelfID
	abm.SessionID = msg.ChannelID
	abm.MessageID = msg.ID
	return abm
}

// handleMsg publishes the event with wake detection (mirrors handle_msg:
// user mention or bot role mention sets is_at_or_wake_command).
func (a *Adapter) handleMsg(abm *platform.AstrBotMessage) {
	if a.EventBus == nil {
		return
	}
	rawMsg, _ := abm.RawMessage.(*discordgo.Message)

	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   "discord",
			PlatformID: a.ID(),
			SelfID:     a.botSelfID,
			SenderID:   abm.Sender.UserID,
			SenderName: abm.Sender.Nickname,
			ConvID:     abm.SessionID,
			IsGroup:    abm.Type == platform.GroupMessage,
		},
		Message:    &message.MessageChain{Chain: abm.Message},
		MessageStr: abm.MessageStr,
		Timestamp:  time.Unix(abm.Timestamp, 0),
		MessageObj: &core.MessageObj{
			MessageID: abm.MessageID,
			SelfID:    a.botSelfID,
		},
		Metadata: map[string]interface{}{},
	}

	// Wake detection: the bot is mentioned directly (or one of its roles is).
	isMention := false
	if rawMsg != nil && a.session != nil && a.session.State != nil && a.session.State.User != nil {
		botID := a.session.State.User.ID
		for _, mention := range rawMsg.Mentions {
			if mention.ID == botID {
				isMention = true
				break
			}
		}
		if !isMention && len(rawMsg.MentionRoles) > 0 {
			if guild, err := a.session.State.Guild(rawMsg.GuildID); err == nil && guild != nil {
				botRoles := map[string]bool{}
				if member, err := a.session.State.Member(guild.ID, botID); err == nil && member != nil {
					for _, roleID := range member.Roles {
						botRoles[roleID] = true
					}
				}
				for _, roleID := range rawMsg.MentionRoles {
					if botRoles[roleID] {
						isMention = true
						break
					}
				}
			}
		}
	}
	if isMention {
		event.IsAtOrWakeCommand = true
		event.Source.IsAtBot = true
	}

	if err := a.EventBus.Publish(event); err != nil {
		logger.Error("Failed to publish event: %v", err)
	}
}

// Send sends a message chain to a Discord channel (mirrors DiscordPlatformEvent
// _parse_to_discord + send). Text over 2000 chars is truncated.
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	if a.session == nil || a.session.State == nil || a.session.State.User == nil {
		return fmt.Errorf("discord: Client is not ready; message send skipped")
	}
	channelID := sessionID
	if strings.Contains(channelID, "_") {
		channelID = channelID[strings.Index(channelID, "_")+1:]
	}

	contentParts := []string{}
	files := []*discordgo.File{}
	var reference *discordgo.MessageReference

	for _, comp := range chain.Chain {
		switch c := comp.(type) {
		case *message.Plain:
			contentParts = append(contentParts, c.Text)
		case *message.Reply:
			reference = &discordgo.MessageReference{MessageID: c.MessageID}
		case *message.At:
			contentParts = append(contentParts, "<@"+c.TargetID+">")
		case *message.Image:
			f := a.mediaFile(c.Path, c.File, c.Base64, c.URL, "image")
			if f != nil {
				files = append(files, f)
			}
		case *message.Record:
			f := a.fileFromMedia(c.URL, c.Path, "audio")
			if f != nil {
				files = append(files, f)
			}
		case *message.File:
			name := c.Name
			if name == "" {
				name = "file"
			}
			f := a.fileFromMedia(c.URL, c.Path, name)
			if f != nil {
				files = append(files, f)
			}
		case *message.Video:
			f := a.fileFromMedia(c.URL, c.Path, "video")
			if f != nil {
				files = append(files, f)
			}
		}
	}

	content := strings.Join(contentParts, "")
	if utf8.RuneCountInString(content) > 2000 {
		logger.I18nWarn("Discord 消息内容超过 2000 字符，将被截断")
		content = string([]rune(content)[:2000])
	}
	if content == "" && len(files) == 0 {
		logger.Debug("Discord 尝试发送空消息，已忽略")
		return nil
	}

	// Slash-command replies go through the interaction followup webhook. Only
	// a matching, not-yet-expired interaction is used; stale entries fall back
	// to a normal channel message so replies are not routed to an invalid
	// webhook (interaction tokens expire after ~15 minutes).
	a.followupsMu.Lock()
	entry, ok := a.followups[channelID]
	delete(a.followups, channelID)
	a.followupsMu.Unlock()
	if ok && time.Since(entry.registeredAt) < followupMaxAge {
		params := &discordgo.WebhookParams{Content: content, Files: files}
		_, err := a.session.FollowupMessageCreate(entry.interaction, false, params)
		return err
	}

	data := &discordgo.MessageSend{
		Content: content,
		Files:   files,
	}
	if reference != nil && reference.MessageID != "" {
		data.Reference = reference
	}
	_, err := a.session.ChannelMessageSendComplex(channelID, data)
	return err
}

// React adds an emoji reaction to a message.
func (a *Adapter) React(sessionID, messageID, emoji string) error {
	if a.session == nil {
		return fmt.Errorf("discord: client not ready")
	}
	channelID := sessionID
	if strings.Contains(channelID, "_") {
		channelID = channelID[strings.Index(channelID, "_")+1:]
	}
	return a.session.MessageReactionAdd(channelID, messageID, emoji)
}

// handleInteraction handles slash-command interactions (mirrors the dynamic
// callbacks created by _create_dynamic_callback).
func (a *Adapter) handleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	data := i.ApplicationCommandData()
	cmdName := data.Name

	// 1. Defer the interaction so the command can run asynchronously
	// (mirrors ctx.defer() with a 2.5s timeout; the response is immediate).
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		logger.I18nWarn("Discord defer 指令 %q 失败: %v", cmdName, err)
		return
	}

	// Build the command string: "<cmd_name> <params>".
	messageStr := cmdName
	for _, opt := range data.Options {
		if opt.Name == "params" {
			if v, ok := opt.Value.(string); ok && v != "" {
				messageStr += " " + v
			}
		}
	}
	logger.Debug("Discord 斜杠指令 %q 触发: %q", cmdName, messageStr)

	channelID := i.ChannelID
	abm := platform.NewAstrBotMessage()
	if i.GuildID != "" {
		abm.Type = platform.GroupMessage
	} else {
		abm.Type = platform.FriendMessage
	}
	abm.Group = &platform.Group{GroupID: channelID}
	abm.MessageStr = messageStr
	userID, username := "", ""
	if i.Member != nil && i.Member.User != nil {
		userID = i.Member.User.ID
		username = i.Member.User.Username
	} else if i.User != nil {
		userID = i.User.ID
		username = i.User.Username
	}
	abm.Sender = platform.MessageMember{
		UserID:   userID,
		Nickname: username,
	}

	abm.Message = []message.Component{&message.Plain{Text: messageStr}}
	abm.RawMessage = i.Interaction
	abm.SelfID = a.botSelfID
	abm.SessionID = channelID
	abm.MessageID = i.ID
	if a.EventBus == nil {
		return
	}

	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   "discord",
			PlatformID: a.ID(),
			SelfID:     a.botSelfID,
			SenderID:   abm.Sender.UserID,
			SenderName: abm.Sender.Nickname,
			ConvID:     channelID,
			IsGroup:    i.GuildID != "",
		},
		Message:           &message.MessageChain{Chain: abm.Message},
		MessageStr:        messageStr,
		Timestamp:         time.Now(),
		IsAtOrWakeCommand: true,
		MessageObj: &core.MessageObj{
			MessageID: i.ID,
			SelfID:    a.botSelfID,
		},
		Metadata: map[string]interface{}{
			// The reply must go through the interaction followup webhook
			// instead of a normal channel message.
			"discord_interaction_id": i.ID,
			"discord_followup_token": i.Token,
		},
	}
	a.followupsMu.Lock()
	a.pruneFollowupsLocked(time.Now())
	a.followups[channelID] = followupEntry{interaction: i.Interaction, registeredAt: time.Now()}
	a.followupsMu.Unlock()
	if err := a.EventBus.Publish(event); err != nil {
		logger.Error("Failed to publish slash-command event: %v", err)
	}
}

// pruneFollowupsLocked removes followup entries that are older than
// followupMaxAge so the map does not accumulate entries for slash commands
// that never produce a reply. Caller must hold followupsMu.
func (a *Adapter) pruneFollowupsLocked(now time.Time) {
	for ch, entry := range a.followups {
		if now.Sub(entry.registeredAt) >= followupMaxAge {
			delete(a.followups, ch)
		}
	}
}

// mediaFile builds a discordgo.File from image component fields. Base64 data
// is decoded before upload and a filename extension is derived from the media
// bytes. It returns nil when the media cannot be read so the caller skips the
// attachment instead of uploading a broken or typed-nil reader.
func (a *Adapter) mediaFile(path, file, b64, url, name string) *discordgo.File {
	if path != "" {
		return openFile(path, name)
	}
	if file != "" {
		return openFile(file, name)
	}
	if b64 != "" {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			logger.I18nWarn("Discord 解码 base64 媒体失败: %v", err)
			return nil
		}
		return &discordgo.File{Name: mediaFilename(name, sniffMediaExtension(data)), Reader: bytes.NewReader(data)}
	}
	if url != "" {
		return fetchFile(url, name)
	}
	return nil
}

// fileFromMedia builds a discordgo.File from a URL or a local path source.
func (a *Adapter) fileFromMedia(url, path, name string) *discordgo.File {
	if url != "" {
		return fetchFile(url, name)
	}
	if path != "" {
		return openFile(path, name)
	}
	return nil
}

// maxMediaBytes bounds the download size of remote media so an oversized or
// malicious source cannot exhaust memory. It aligns with Discord's standard
// attachment upload limit.
const maxMediaBytes = 25 << 20

// openFile opens a local file for upload (nil on failure). The upload name is
// given a file extension when the source path carries one.
func openFile(path, name string) *discordgo.File {
	f, err := os.Open(path)
	if err != nil {
		logger.I18nWarn("打开文件失败: %v", err)
		return nil
	}
	return &discordgo.File{Name: mediaFilename(name, filepath.Ext(path)), Reader: f}
}

// fetchFile downloads a URL body for upload (nil on failure). The download is
// bounded by a context timeout, rejects non-200 responses and caps the body
// size so a bad or hostile URL cannot hang the sender or exhaust memory.
func fetchFile(rawURL, name string) *discordgo.File {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		logger.I18nWarn("下载 URL 失败: %v", err)
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.I18nWarn("下载 URL 失败: %v", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.I18nWarn("下载 URL 失败: HTTP %d (%s)", resp.StatusCode, rawURL)
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMediaBytes+1))
	if err != nil {
		logger.I18nWarn("读取 URL 内容失败: %v", err)
		return nil
	}
	if len(body) > maxMediaBytes {
		logger.I18nWarn("下载 URL 失败: 媒体大小超过 %d 字节上限", maxMediaBytes)
		return nil
	}
	ext := extensionFromContentType(resp.Header.Get("Content-Type"))
	if ext == "" {
		ext = filepath.Ext(req.URL.Path)
	}
	if ext == "" {
		ext = sniffMediaExtension(body)
	}
	return &discordgo.File{Name: mediaFilename(name, ext), Reader: bytes.NewReader(body)}
}

// mediaFilename ensures the upload filename carries an extension. A name that
// already has one (e.g. a real filename) is kept as-is; otherwise the
// detected ext is appended, defaulting to a per-kind extension.
func mediaFilename(name, ext string) string {
	if filepath.Ext(name) != "" {
		return name
	}
	if ext != "" {
		return name + ext
	}
	switch name {
	case "image":
		return name + ".png"
	case "audio":
		return name + ".mp3"
	case "video":
		return name + ".mp4"
	}
	return name + ".bin"
}

// sniffMediaExtension guesses a file extension from media magic bytes.
func sniffMediaExtension(data []byte) string {
	if len(data) < 4 {
		return ""
	}
	switch {
	case bytes.Equal(data[:4], []byte("\x89PNG")):
		return ".png"
	case data[0] == 0xFF && data[1] == 0xD8:
		return ".jpg"
	case bytes.Equal(data[:4], []byte("GIF8")):
		return ".gif"
	case bytes.Equal(data[:4], []byte("RIFF")) && len(data) >= 12 && bytes.Equal(data[8:12], []byte("WEBP")):
		return ".webp"
	case bytes.Equal(data[:3], []byte("ID3")):
		return ".mp3"
	case len(data) >= 8 && bytes.Equal(data[4:8], []byte("ftyp")):
		return ".mp4"
	}
	return ""
}

// extensionFromContentType maps a media Content-Type to a file extension.
func extensionFromContentType(ct string) string {
	if ct == "" {
		return ""
	}
	if idx := strings.Index(ct, ";"); idx >= 0 {
		ct = ct[:idx]
	}
	switch strings.TrimSpace(strings.ToLower(ct)) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/ogg":
		return ".ogg"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	}
	return ""
}

// registerCommands collects all star-registered commands and registers them
// as Discord slash commands (mirrors _collect_and_register_commands). Only
// commands present in the star registry are registered: whatever the user
// enabled (plugins/built-ins) appears in Discord, nothing else.
func (a *Adapter) registerCommands(s *discordgo.Session) error {
	logger.I18nInfo("Discord 正在收集并注册斜杠指令...")
	commands := []*discordgo.ApplicationCommand{}
	registered := []string{}

	if a.starMgr != nil {
		registry := a.starMgr.Handlers()
		if registry != nil {
			for _, handler := range registry.GetFilterHandlers() {
				if !handler.Enabled {
					continue
				}
				for _, filter := range handler.EventFilters {
					cmdInfo := extractCommandInfo(filter, handler)
					if cmdInfo == nil {
						continue
					}
					name, desc := cmdInfo[0], cmdInfo[1]
					commands = append(commands, &discordgo.ApplicationCommand{
						Name:        name,
						Description: desc,
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type:        discordgo.ApplicationCommandOptionString,
								Name:        "params",
								Description: "指令的所有参数",
								Required:    false,
							},
						},
					})
					registered = append(registered, name)
				}
			}
		}
	}

	if len(registered) > 0 {
		logger.I18nInfo("Discord 待同步 %d 个指令: %s", len(registered), strings.Join(registered, ", "))
	} else {
		logger.I18nInfo("Discord 未找到可注册的指令")
	}

	a.commandsMu.Lock()
	a.registeredCommands = commands
	a.commandsMu.Unlock()

	// Sync commands (guild-scoped when discord_guild_id_for_debug is set).
	_, err := s.ApplicationCommandBulkOverwrite(s.State.User.ID, a.guildID, commands)
	if err != nil && isDailyQuotaError(err) {
		logger.I18nWarn("Discord 每日指令创建配额已达上限(30034)，跳过指令同步")
		return nil
	}
	if err != nil {
		logger.I18nWarn("Discord 同步指令失败: %v", err)
		return err
	}
	logger.I18nInfo("Discord 指令同步完成")
	return nil
}

// extractCommandInfo mirrors _extract_command_info: only top-level commands
// with a Discord-valid name are registered.
func extractCommandInfo(filter star.HandlerFilter, handler *star.StarHandlerMetadata) []string {
	cmdFilter, ok := filter.(*star.CommandFilter)
	if !ok {
		return nil
	}
	// Sub-commands are not supported as slash commands.
	if cmdFilter.HasParentCommand() {
		return nil
	}
	cmdName := cmdFilter.CommandName()
	if cmdName == "" {
		return nil
	}
	// Discord slash-command name rules: lowercase + ^[-_'\w]{1,32}$.
	if cmdName != strings.ToLower(cmdName) || !slashCommandNamePattern.MatchString(cmdName) {
		logger.Debug("Discord 跳过非法斜杠指令名: %s", cmdName)
		return nil
	}
	desc := handler.Desc
	if desc == "" {
		desc = "Command: " + cmdName
	}
	if len(desc) > 100 {
		desc = desc[:97] + "..."
	}
	return []string{cmdName, desc}
}

// isLoginFailure detects a login failure.
func isLoginFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "401") || strings.Contains(msg, "Unauthorized") ||
		strings.Contains(msg, "Incorrect login details")
}

// isDailyQuotaError detects Discord error code 30034.
func isDailyQuotaError(err error) bool {
	if err == nil {
		return false
	}
	if restErr, ok := err.(*discordgo.RESTError); ok && restErr.Message != nil {
		return restErr.Message.Code == 30034
	}
	return strings.Contains(err.Error(), "30034")
}
