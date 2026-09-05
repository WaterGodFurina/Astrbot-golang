// Package weixin_oc implements a WeChat Open Platform (微信开放平台) adapter.
// Ported 1:1 from astrbot/core/platform/sources/weixin_oc/ (Python), built on
// the github.com/dobest1024/go-weixin-ilink SDK (iLink protocol: QR login +
// getupdates long-polling + sendmessage, with token/context/sync persistence).
package weixin_oc

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	ilink "github.com/dobest1024/go-weixin-ilink"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

var logger = log.GetDefault().WithComponent("WeixinOC")

// sdkCallTimeout bounds every SDK call (media download / upload / send). The
// ilink SDK's default HTTP client has no Timeout of its own — it relies on the
// passed context — so context.Background() would let a stuck CDN/API hang the
// message handler forever.
const sdkCallTimeout = 30 * time.Second

// maxMediaDownloadSize 限制发送时下载外部媒体的大小上限。

const maxMediaDownloadSize = 20 << 20

// Adapter implements the Weixin OC adapter.
type Adapter struct {
	config   map[string]interface{}
	settings map[string]interface{}

	EventBus *core.EventBus

	bot      *ilink.Bot
	botType  string
	dataDir  string
	stopCh   chan struct{}
	once     sync.Once
	lastQR   string
	lastQRMu sync.Mutex

	// typing 状态管理（对齐本体 _typing_states + start/stop_typing）。
	typing *typingManagerAdapter

	// 最近消息缓存（引用回复时间窗匹配，对齐本体 _recent_messages）。
	recentMu           sync.Mutex
	recentMessages     map[string]*recentSessionCache
	recentCacheSize    int
	replyMatchWindowMs int64
	recentSessionTTL   time.Duration
	maxRecentSessions  int
}

// New creates the adapter.
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		config:   config,
		settings: settings,
		EventBus: eventBus,
		stopCh:   make(chan struct{}),
	}
	a.typing = newTypingManagerAdapter(typingKeepaliveIntervalFromConfig(config))
	a.recentMessages = make(map[string]*recentSessionCache)
	a.recentCacheSize = intConfig(config, "weixin_oc_recent_message_cache_size", defaultRecentMessageCacheSize, 1)
	a.replyMatchWindowMs = int64(intConfig(config, "weixin_oc_reply_match_window_ms", defaultReplyMatchWindowMs, 1))
	a.recentSessionTTL = time.Duration(intConfig(config, "weixin_oc_recent_session_cache_ttl_s", int(defaultRecentSessionCacheTTL/time.Second), 60)) * time.Second
	a.maxRecentSessions = intConfig(config, "weixin_oc_max_recent_message_sessions", defaultMaxRecentMessageSessions, 1)
	a.botType, _ = config["weixin_oc_bot_type"].(string)
	if a.botType == "" {
		a.botType = "3"
	}
	// Persistence directory for the iLink SDK (token/context/syncbuf).
	a.dataDir, _ = config["weixin_oc_data_dir"].(string)
	if a.dataDir == "" {
		wd, _ := os.Getwd()
		a.dataDir = filepath.Join(wd, "data", "weixin_oc")
	}
	_ = os.MkdirAll(a.dataDir, 0o755)

	baseURL, _ := config["weixin_oc_base_url"].(string)
	cdnBase, _ := config["weixin_oc_cdn_base_url"].(string)
	opts := []ilink.Option{
		ilink.WithBotType(a.botType),
		ilink.WithTokenFile(filepath.Join(a.dataDir, "account.json")),
		ilink.WithContextTokenDir(filepath.Join(a.dataDir, "ctx")),
		ilink.WithSyncBufFile(filepath.Join(a.dataDir, "syncbuf")),
	}
	if baseURL != "" {
		opts = append(opts, ilink.WithBaseURL(baseURL))
	}
	if cdnBase != "" {
		opts = append(opts, ilink.WithCDNBaseURL(cdnBase))
	}
	// 配置中的 weixin_oc_token（来自前端一键注册扫码成功写回）优先于 SDK 持久化：
	// 预写入 token store，使 Resume 能直接恢复，无需重新扫码。
	if tok, _ := config["weixin_oc_token"].(string); tok != "" {
		tokenFile := filepath.Join(a.dataDir, "account.json")
		if err := os.MkdirAll(filepath.Dir(tokenFile), 0o755); err == nil {
			store := ilink.NewFileTokenStore(tokenFile)
			_ = store.Save(tok, baseURL)
		}
		logger.I18nInfo("微信开放平台已读取配置中的 weixin_oc_token，无需重新扫码")
	}
	a.bot = ilink.NewBot(opts...)
	return a
}

// SetEventBus injects the event bus.
func (a *Adapter) SetEventBus(bus platform.EventBus) {
	if eb, ok := bus.(*core.EventBus); ok {
		a.EventBus = eb
	}
}

// ID returns the adapter instance id.
func (a *Adapter) ID() string {
	if id, ok := a.config["id"].(string); ok {
		return id
	}
	return "weixin_oc"
}

// Type returns the platform type.
func (a *Adapter) Type() string { return "weixin_oc" }

// Start boots the bot: resume from persisted token or QR login, then run.
func (a *Adapter) Start(ctx context.Context) error {
	logger.I18nInfo("微信开放平台适配器启动...")

	// OnBody receives every inbound message and feeds the pipeline.
	a.bot.OnBody(func(c *ilink.Context) {
		a.handleMessage(c)
	})

	go func() {
		// Try to resume an existing session first.
		if err := a.bot.Resume(); err != nil {
			logger.I18nInfo("微信开放平台无已保存会话，请扫码登录")
			if err := a.bot.Login(ctx, func(qr string) {
				a.lastQRMu.Lock()
				a.lastQR = qr
				a.lastQRMu.Unlock()
				logger.I18nInfo("微信开放平台请扫码登录 (二维码 URL 已记录)")
			}); err != nil {
				logger.I18nError("微信开放平台登录失败: %v", err)
				return
			}
		}
		if err := a.bot.Run(ctx); err != nil {
			logger.I18nError("微信开放平台运行结束: %v", err)
		}
	}()
	return nil
}

// Stop stops the adapter.
func (a *Adapter) Stop() error {
	a.once.Do(func() { close(a.stopCh) })
	if a.typing != nil {
		a.typing.stopAll()
	}
	if a.bot != nil {
		a.bot.Stop()
	}
	logger.I18nInfo("微信开放平台适配器已关闭")
	return nil
}

// handleMessage converts an inbound iLink message to an AstrBotMessage and
// publishes it.
func (a *Adapter) handleMessage(c *ilink.Context) {
	msg := c.Message
	if msg == nil {
		return
	}
	fromUser := msg.FromUserID
	if fromUser == "" {
		return
	}

	// typing 状态：消息进入处理链即开启"对方正在输入"（对齐本体 LLM 请求前
	// event.send_typing() 的时机）；owner id 每条消息唯一，管线结束（回复发出或
	// 未唤醒）时 stop，避免 LLM 处理期间状态丢失。
	ownerID := strconv.FormatInt(msg.MessageID, 10) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	a.typing.start(fromUser, c, ownerID)

	components := []message.Component{}

	// Quoted/reply reference first.
	if c.HasQuote() {
		quoted := c.QuotedText()
		refID := ""
		for _, item := range msg.ItemList {
			if item.RefMsg != nil && item.RefMsg.MessageItem != nil {
				refID = item.RefMsg.MessageItem.MsgID
				break
			}
		}
		chain := []message.Component{}
		if quoted != "" {
			chain = append(chain, &message.Plain{Text: quoted})
		}
		// 引用回复解析：内嵌文本直接匹配 → 时间窗就近匹配回填 → ref title 兜底
		// （对齐本体 _build_reply_component_from_ref 的 direct-ref-msg /
		// nearest-message-by-timestamp 两级策略）。
		reply := a.buildReplyFromRef(fromUser, c)
		if reply != nil {
			if reply.MessageID == "" && refID != "" {
				reply.MessageID = refID
			}
			components = append(components, reply)
		} else {
			components = append(components, &message.Reply{MessageID: refID, MessageStr: quoted, Chain: chain})
		}
	}

	// Text / voice-transcript body.
	body := c.Body()
	if body != "" {
		components = append(components, &message.Plain{Text: body})
	}

	// Media items: image / voice / file / video. The SDK downloads via
	// http.NewRequestWithContext and its default client has no Timeout, so a
	// stuck CDN must be bounded here.
	ctx, cancel := context.WithTimeout(context.Background(), sdkCallTimeout)
	defer cancel()
	for _, item := range msg.ItemList {
		switch item.Type {
		case ilink.ItemTypeImage:
			if item.ImageItem != nil && item.ImageItem.Media != nil {
				data, err := a.bot.DownloadImage(ctx, item.ImageItem)
				if err != nil {
					a.logMediaError("image", msg.MessageID, err)
				} else if len(data) > 0 {
					if path := a.saveMedia(data, "image", ".png"); path != "" {
						components = append(components, &message.Image{Path: path, File: path})
					}
				}
			}
		case ilink.ItemTypeVoice:
			if item.VoiceItem != nil && item.VoiceItem.Media != nil {
				data, _, err := a.bot.DownloadVoice(ctx, item.VoiceItem)
				if err != nil {
					a.logMediaError("voice", msg.MessageID, err)
				} else if len(data) > 0 {
					if path := a.saveMedia(data, "audio", ".wav"); path != "" {
						components = append(components, &message.Record{Path: path, File: path, URL: path})
					}
				}
			}
		case ilink.ItemTypeFile:
			if item.FileItem != nil && item.FileItem.Media != nil {
				name := item.FileItem.FileName
				data, _, err := a.bot.DownloadFile(ctx, item.FileItem)
				if err != nil {
					a.logMediaError("file", msg.MessageID, err)
				} else if len(data) > 0 {
					if path := a.saveMedia(data, "file", filepath.Ext(name)); path != "" {
						components = append(components, &message.File{Name: name, Path: path})
					}
				}
			}
		case ilink.ItemTypeVideo:
			if item.VideoItem != nil && item.VideoItem.Media != nil {
				data, err := a.bot.Download(ctx, item.VideoItem.Media.EncryptQueryParam, item.VideoItem.Media.AESKey)
				if err != nil {
					a.logMediaError("video", msg.MessageID, err)
				} else if len(data) > 0 {
					if path := a.saveMedia(data, "video", ".mp4"); path != "" {
						components = append(components, &message.Video{Path: path, FileID: path})
					}
				}
			}
		}
	}

	if len(components) == 0 {
		a.typing.stop(fromUser, ownerID)
		return
	}

	// 缓存入站最近消息（引用回复时间窗匹配的数据源）。
	createTimeMs := msg.CreateTimeMs
	if createTimeMs <= 0 {
		createTimeMs = time.Now().UnixMilli()
	}
	a.cacheRecentMessage(fromUser, recentMessage{
		messageID:   strconv.FormatInt(msg.MessageID, 10),
		senderID:    fromUser,
		senderNick:  fromUser,
		timestamp:   createTimeMs / 1000,
		timestampMs: createTimeMs,
		components:  append([]message.Component{}, components...),
		messageStr:  messageTextOf(components),
	})

	if a.EventBus == nil {
		a.typing.stop(fromUser, ownerID)
		return
	}
	ev := messageToEvent(msg, fromUser, components)
	// 覆盖为 config.id（适配器实例 id），messageToEvent 为纯函数无 a 访问权。
	ev.Source.PlatformID = a.ID()
	// 管线完成信号：结束后回收该消息的 typing owner（对齐本体 finally 中
	// event.stop_typing() —— 无论是否发出回复都停止）。
	done := core.NewPipelineDone()
	ev.Metadata[core.MetadataPipelineDone] = done
	go func() {
		<-done.Done()
		a.typing.stop(fromUser, ownerID)
	}()
	if err := a.EventBus.Publish(ev); err != nil {
		logger.Error("发布事件失败: %v", err)
		a.typing.stop(fromUser, ownerID)
	}
}

// messageTextOf 拼接组件中的 Plain 文本。
func messageTextOf(components []message.Component) string {
	text := ""
	for _, comp := range components {
		if plain, ok := comp.(*message.Plain); ok {
			text += plain.Text
		}
	}
	return strings.TrimSpace(text)
}

// resolveRefItemComponents 解析被引用消息内嵌的媒体项为组件
// （引用媒体在 ref_msg.message_item 中复用 MessageItem 结构，可复用入站下载逻辑）。
func (a *Adapter) resolveRefItemComponents(mi *ilink.MessageItem) []message.Component {
	if mi == nil || !mi.IsMedia() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), sdkCallTimeout)
	defer cancel()
	var comps []message.Component
	switch mi.Type {
	case ilink.ItemTypeImage:
		if mi.ImageItem != nil && mi.ImageItem.Media != nil {
			if data, err := a.bot.DownloadImage(ctx, mi.ImageItem); err == nil && len(data) > 0 {
				if path := a.saveMedia(data, "image", ".png"); path != "" {
					comps = append(comps, &message.Image{Path: path, File: path})
				}
			}
		}
	case ilink.ItemTypeVoice:
		if mi.VoiceItem != nil && mi.VoiceItem.Media != nil {
			if data, _, err := a.bot.DownloadVoice(ctx, mi.VoiceItem); err == nil && len(data) > 0 {
				if path := a.saveMedia(data, "audio", ".wav"); path != "" {
					comps = append(comps, &message.Record{Path: path, File: path, URL: path})
				}
			}
		}
	case ilink.ItemTypeFile:
		if mi.FileItem != nil && mi.FileItem.Media != nil {
			if data, _, err := a.bot.DownloadFile(ctx, mi.FileItem); err == nil && len(data) > 0 {
				ext := filepath.Ext(mi.FileItem.FileName)
				if path := a.saveMedia(data, "file", ext); path != "" {
					comps = append(comps, &message.File{Name: mi.FileItem.FileName, Path: path})
				}
			}
		}
	case ilink.ItemTypeVideo:
		if mi.VideoItem != nil && mi.VideoItem.Media != nil {
			if data, err := a.bot.Download(ctx, mi.VideoItem.Media.EncryptQueryParam, mi.VideoItem.Media.AESKey); err == nil && len(data) > 0 {
				if path := a.saveMedia(data, "video", ".mp4"); path != "" {
					comps = append(comps, &message.Video{Path: path, FileID: path})
				}
			}
		}
	}
	return comps
}

// messageToEvent converts an iLink message to a core.Event (pure function,
// testable without a bus).
func messageToEvent(msg *ilink.Message, fromUser string, components []message.Component) *core.Event {
	text := ""
	for _, comp := range components {
		if plain, ok := comp.(*message.Plain); ok {
			text += plain.Text
		}
	}
	createTime := time.Now().Unix()
	if msg.CreateTimeMs > 0 {
		createTime = msg.CreateTimeMs / 1000
	}
	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   "weixin_oc",
			PlatformID: "weixin_oc", // 默认平台实例 id，调用处会用 a.ID()（config.id）覆盖
			SelfID:     msg.ToUserID,
			SenderID:   fromUser,
			SenderName: fromUser,
			ConvID:     fromUser,
			IsGroup:    msg.IsGroup(),
		},
		Message:    &message.MessageChain{Chain: components},
		MessageStr: text,
		Timestamp:  time.Unix(createTime, 0),
		MessageObj: &core.MessageObj{MessageID: strconv.FormatInt(msg.MessageID, 10), SelfID: msg.ToUserID},
		Metadata:   map[string]interface{}{},
	}
	if msg.IsGroup() && msg.GroupID != "" {
		event.Source.ConvID = msg.GroupID
	}
	return event
}

// downloadMedia 下载媒体 URL 并返回内容（经 SSRF 校验，上限 maxMediaDownloadSize）。
func downloadMedia(rawURL string) ([]byte, error) {
	return platform.SafeDownloadBytes(context.Background(), rawURL, maxMediaDownloadSize)
}

// saveMedia writes inbound media bytes to a temp file.
func (a *Adapter) saveMedia(data []byte, kind, suffix string) string {
	tmp, err := os.CreateTemp("", "weixin_oc_"+kind+"_*"+suffix)
	if err != nil {
		return ""
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return ""
	}
	name := tmp.Name()
	_ = tmp.Close()
	return name
}

// Send sends a message chain to a user.
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	// 回复发出：结束该会话的 typing 状态（对齐本体 finally 中 event.stop_typing()）。
	a.typing.stopAllOwners(sessionID)

	// SDK upload/send calls use http.NewRequestWithContext with a client that
	// has no Timeout — always bound them so a stuck connection cannot hang the
	// whole send path.
	ctx, cancel := context.WithTimeout(context.Background(), sdkCallTimeout)
	defer cancel()
	var pendingText string
	flushText := func() error {
		if strings.TrimSpace(pendingText) == "" {
			return nil
		}
		if err := a.bot.SendText(ctx, sessionID, strings.TrimSpace(pendingText)); err != nil {
			return err
		}
		// 缓存出站消息（引用回复时间窗匹配的数据源，对齐本体发送后 _cache_recent_message）。
		text := strings.TrimSpace(pendingText)
		a.cacheRecentMessage(sessionID, recentMessage{
			messageID:   recentOutboundID(),
			senderID:    a.ID(),
			senderNick:  a.ID(),
			timestamp:   time.Now().Unix(),
			timestampMs: time.Now().UnixMilli(),
			components:  []message.Component{&message.Plain{Text: text}},
			messageStr:  text,
		})
		pendingText = ""
		return nil
	}
	sentComponents := []message.Component{}
	for _, comp := range chain.Chain {
		switch c := comp.(type) {
		case *message.Plain:
			pendingText += c.Text
			sentComponents = append(sentComponents, comp)
		case *message.Image:
			if err := flushText(); err != nil {
				return err
			}
			if err := a.sendImage(ctx, sessionID, c); err != nil {
				return err
			}
			sentComponents = append(sentComponents, comp)
		case *message.File:
			if err := flushText(); err != nil {
				return err
			}
			if err := a.sendFile(ctx, sessionID, c); err != nil {
				return err
			}
			sentComponents = append(sentComponents, comp)
		case *message.Video:
			if err := flushText(); err != nil {
				return err
			}
			if err := a.sendVideo(ctx, sessionID, c); err != nil {
				return err
			}
			sentComponents = append(sentComponents, comp)
		case *message.Record:
			if err := flushText(); err != nil {
				return err
			}
			if err := a.sendRecord(ctx, sessionID, c); err != nil {
				return err
			}
			sentComponents = append(sentComponents, comp)
		}
	}
	if strings.TrimSpace(pendingText) != "" {
		if err := flushText(); err != nil {
			return err
		}
	}
	// 媒体类出站消息同样入缓存（按类型生成占位文本，对齐本体 _message_text_from_item_list）。
	a.cacheOutboundMedia(sessionID, sentComponents)
	return nil
}

// cacheOutboundMedia 缓存出站媒体消息（文本已在 flushText 中缓存，此处仅补媒体）。
func (a *Adapter) cacheOutboundMedia(sessionID string, components []message.Component) {
	hasMedia := false
	for _, comp := range components {
		switch comp.(type) {
		case *message.Image, *message.Record, *message.File, *message.Video:
			hasMedia = true
		}
	}
	if !hasMedia {
		return
	}
	now := time.Now()
	a.cacheRecentMessage(sessionID, recentMessage{
		messageID:   recentOutboundID(),
		senderID:    a.ID(),
		senderNick:  a.ID(),
		timestamp:   now.Unix(),
		timestampMs: now.UnixMilli(),
		components:  append([]message.Component{}, components...),
		messageStr:  messageTextOf(components),
	})
}

// resolveMedia 解析媒体组件的二进制内容：优先本地文件，其次 base64，最后下载 URL。
func resolveMedia(path, file, b64, url string) ([]byte, error) {
	if path == "" {
		path = file
	}
	if path != "" {
		return os.ReadFile(path)
	}
	if b64 != "" {
		return base64.StdEncoding.DecodeString(b64)
	}
	if url != "" {
		return downloadMedia(url)
	}
	return nil, fmt.Errorf("媒体组件缺少可用的内容（path/file/base64/url 均为空）")
}

func (a *Adapter) sendImage(ctx context.Context, userID string, img *message.Image) error {
	data, err := resolveMedia(img.Path, img.File, img.Base64, img.URL)
	if err != nil {
		return err
	}
	up, err := a.bot.Upload(ctx, data, userID, "image")
	if err != nil {
		return err
	}
	return a.bot.SendImage(ctx, userID, &ilink.ImageItem{
		Media: &ilink.CDNMedia{
			EncryptQueryParam: up.EncryptedParam,
			AESKey:            up.AESKey,
		},
	})
}

func (a *Adapter) sendFile(ctx context.Context, userID string, f *message.File) error {
	data, err := resolveMedia(f.Path, "", "", f.URL)
	if err != nil {
		return err
	}
	up, err := a.bot.Upload(ctx, data, userID, "file")
	if err != nil {
		return err
	}
	return a.bot.SendFile(ctx, userID, &ilink.FileItem{
		Media:    &ilink.CDNMedia{EncryptQueryParam: up.EncryptedParam, AESKey: up.AESKey},
		FileName: f.Name,
		Length:   strconv.Itoa(len(data)),
	})
}

func (a *Adapter) sendVideo(ctx context.Context, userID string, v *message.Video) error {
	data, err := resolveMedia(v.Path, "", "", v.URL)
	if err != nil {
		return err
	}
	up, err := a.bot.Upload(ctx, data, userID, "video")
	if err != nil {
		return err
	}
	return a.bot.SendVideo(ctx, userID, &ilink.VideoItem{
		Media: &ilink.CDNMedia{EncryptQueryParam: up.EncryptedParam, AESKey: up.AESKey},
	})
}

func (a *Adapter) sendRecord(ctx context.Context, userID string, rec *message.Record) error {
	data, err := resolveMedia(rec.Path, rec.File, rec.Base64, rec.URL)
	if err != nil {
		return err
	}
	up, err := a.bot.Upload(ctx, data, userID, "voice")
	if err != nil {
		return err
	}
	return a.bot.SendVoice(ctx, userID, &ilink.VoiceItem{
		Media: &ilink.CDNMedia{
			EncryptQueryParam: up.EncryptedParam,
			AESKey:            up.AESKey,
		},
	})
}

// logMediaError logs an inbound media download failure, calling out timeouts
// explicitly since the SDK's default HTTP client has no built-in deadline.
func (a *Adapter) logMediaError(kind string, msgID int64, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		logger.Warn("媒体下载超时（kind=%s message_id=%d）: %v", kind, msgID, err)
		return
	}
	logger.Debug("媒体下载失败（kind=%s message_id=%d）: %v", kind, msgID, err)
}
