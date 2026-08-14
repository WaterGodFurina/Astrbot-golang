// Package weixin_oc implements a WeChat Open Platform (微信开放平台) adapter.
// Ported 1:1 from astrbot/core/platform/sources/weixin_oc/ (Python), built on
// the github.com/dobest1024/go-weixin-ilink SDK (iLink protocol: QR login +
// getupdates long-polling + sendmessage, with token/context/sync persistence).
package weixin_oc

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
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

// maxMediaDownloadSize 限制发送时下载外部媒体的大小上限。
const maxMediaDownloadSize = 20 << 20

// mediaHTTPClient 用于发送时下载外部媒体（带超时，避免 DefaultClient 永久挂起）。
var mediaHTTPClient = &http.Client{Timeout: 30 * time.Second}

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
}

// New creates the adapter.
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		config:   config,
		settings: settings,
		EventBus: eventBus,
		stopCh:   make(chan struct{}),
	}
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
		components = append(components, &message.Reply{MessageID: refID, MessageStr: quoted, Chain: chain})
	}

	// Text / voice-transcript body.
	body := c.Body()
	if body != "" {
		components = append(components, &message.Plain{Text: body})
	}

	// Media items: image / voice / file / video.
	for _, item := range msg.ItemList {
		switch item.Type {
		case ilink.ItemTypeImage:
			if item.ImageItem != nil && item.ImageItem.Media != nil {
				if data, err := a.bot.DownloadImage(context.Background(), item.ImageItem); err == nil && len(data) > 0 {
					if path := a.saveMedia(data, "image", ".png"); path != "" {
						components = append(components, &message.Image{Path: path, File: path})
					}
				}
			}
		case ilink.ItemTypeVoice:
			if item.VoiceItem != nil && item.VoiceItem.Media != nil {
				if data, _, err := a.bot.DownloadVoice(context.Background(), item.VoiceItem); err == nil && len(data) > 0 {
					if path := a.saveMedia(data, "audio", ".wav"); path != "" {
						components = append(components, &message.Record{Path: path, File: path, URL: path})
					}
				}
			}
		case ilink.ItemTypeFile:
			if item.FileItem != nil && item.FileItem.Media != nil {
				name := item.FileItem.FileName
				if data, _, err := a.bot.DownloadFile(context.Background(), item.FileItem); err == nil && len(data) > 0 {
					if path := a.saveMedia(data, "file", filepath.Ext(name)); path != "" {
						components = append(components, &message.File{Name: name, Path: path})
					}
				}
			}
		case ilink.ItemTypeVideo:
			if item.VideoItem != nil && item.VideoItem.Media != nil {
				if data, err := a.bot.Download(context.Background(), item.VideoItem.Media.EncryptQueryParam, item.VideoItem.Media.AESKey); err == nil && len(data) > 0 {
					if path := a.saveMedia(data, "video", ".mp4"); path != "" {
						components = append(components, &message.Video{Path: path, FileID: path})
					}
				}
			}
		}
	}

	if len(components) == 0 {
		return
	}

	if a.EventBus == nil {
		return
	}
	if err := a.EventBus.Publish(messageToEvent(msg, fromUser, components)); err != nil {
		logger.Error("发布事件失败: %v", err)
	}
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

// downloadMedia 下载媒体 URL 并返回内容（上限 maxMediaDownloadSize）。
func downloadMedia(rawURL string) ([]byte, error) {
	resp, err := mediaHTTPClient.Get(rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("下载媒体失败: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMediaDownloadSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxMediaDownloadSize {
		return nil, fmt.Errorf("媒体文件超过大小上限 %d 字节", maxMediaDownloadSize)
	}
	return data, nil
}

// saveMedia writes inbound media bytes to a temp file.
func (a *Adapter) saveMedia(data []byte, kind, suffix string) string {
	tmp, err := os.CreateTemp("", "weixin_oc_"+kind+"_*"+suffix)
	if err != nil {
		return ""
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return ""
	}
	name := tmp.Name()
	tmp.Close()
	return name
}

// Send sends a message chain to a user.
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	ctx := context.Background()
	var pendingText string
	for _, comp := range chain.Chain {
		switch c := comp.(type) {
		case *message.Plain:
			pendingText += c.Text
		case *message.Image:
			if err := a.sendImage(ctx, sessionID, c); err != nil {
				return err
			}
			pendingText = ""
		case *message.File:
			if err := a.sendFile(ctx, sessionID, c); err != nil {
				return err
			}
			pendingText = ""
		case *message.Video:
			if err := a.sendVideo(ctx, sessionID, c); err != nil {
				return err
			}
			pendingText = ""
		case *message.Record:
			if err := a.sendRecord(ctx, sessionID, c); err != nil {
				return err
			}
			pendingText = ""
		}
	}
	if strings.TrimSpace(pendingText) != "" {
		return a.bot.SendText(ctx, sessionID, strings.TrimSpace(pendingText))
	}
	return nil
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
