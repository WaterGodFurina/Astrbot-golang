// Package weixin_official_account implements a WeChat Official Account
// (公众号) adapter. Ported 1:1 from
// astrbot/core/platform/sources/weixin_official_account/ (Python, wechatpy),
// built on the github.com/blusewang/wx SDK (MpAccount.ReadMessage + NewMpReq).
package weixin_official_account

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	wx "github.com/blusewang/wx"
	wxmp "github.com/blusewang/wx/mp_api"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

var logger = log.GetDefault().WithComponent("WeixinOffAcc")

// Adapter implements the WeChat Official Account adapter.
type Adapter struct {
	config   map[string]interface{}
	settings map[string]interface{}

	EventBus *core.EventBus

	appid      string
	secret     string
	apiBase    string
	port       int
	host       string
	webhookID  string
	activeSend bool

	account *wx.MpAccount

	httpClient *http.Client
	stopCh     chan struct{}
	once       sync.Once
}

// New creates the adapter.
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		config:     config,
		settings:   settings,
		EventBus:   eventBus,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		stopCh:     make(chan struct{}),
	}
	a.appid, _ = config["appid"].(string)
	a.secret, _ = config["secret"].(string)
	a.apiBase, _ = config["api_base_url"].(string)
	if a.apiBase == "" {
		a.apiBase = "https://api.weixin.qq.com/cgi-bin/"
	}
	if !strings.HasSuffix(a.apiBase, "/") {
		a.apiBase += "/"
	}
	a.host, _ = config["callback_server_host"].(string)
	if a.host == "" {
		a.host = "0.0.0.0"
	}
	if v, ok := config["port"].(float64); ok {
		a.port = int(v)
	}
	if a.port == 0 {
		a.port = 6194
	}
	a.webhookID, _ = config["webhook_uuid"].(string)
	if v, ok := config["active_send_mode"].(bool); ok {
		a.activeSend = v
	}

	token, _ := config["token"].(string)
	aesKey, _ := config["encoding_aes_key"].(string)
	a.account = &wx.MpAccount{
		AppId:          a.appid,
		AppSecret:      a.secret,
		PrivateToken:   token,
		EncodingAESKey: aesKey,
		ServerHost:     "api.weixin.qq.com",
	}
	// TokenGuard refreshes access_token via gettoken before requests.
	a.account.TokenGuard = func(ctx context.Context) error {
		return a.refreshAccessToken(ctx)
	}
	return a
}

// refreshAccessToken fetches a fresh access token and stores it on the
// account (the SDK's TokenGuard is invoked before each request).
func (a *Adapter) refreshAccessToken(ctx context.Context) error {
	if a.account.AccessToken != "" && time.Now().Before(a.account.ExpireAt) {
		return nil
	}
	u := fmt.Sprintf("%sgettoken?grant_type=client_credential&appid=%s&secret=%s",
		a.apiBase, url.QueryEscape(a.appid), url.QueryEscape(a.secret))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if result.AccessToken == "" {
		return fmt.Errorf("weixin: gettoken failed(%d): %s", result.ErrCode, result.ErrMsg)
	}
	exp := result.ExpiresIn
	if exp <= 0 {
		exp = 7200
	}
	a.account.AccessToken = result.AccessToken
	a.account.ExpireAt = time.Now().Add(time.Duration(exp) * time.Second).Add(-time.Minute)
	return nil
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
	return "weixin_official_account"
}

// Type returns the platform type.
func (a *Adapter) Type() string { return "weixin_official_account" }

// Start registers the webhook server (or the unified webhook entry).
func (a *Adapter) Start(ctx context.Context) error {
	if a.webhookID != "" {
		logger.I18nInfo("微信公众号 webhook 模式已启用, webhook_uuid=%s", a.webhookID)
		return nil
	}
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/callback/command", a.handleCallback)
		srv := &http.Server{Addr: fmt.Sprintf("%s:%d", a.host, a.port), Handler: mux}
		_ = srv.ListenAndServe()
	}()
	logger.I18nInfo("微信公众号 webhook 服务器已启动 :%d/callback/command", a.port)
	return nil
}

// Stop stops the adapter.
func (a *Adapter) Stop() error {
	a.once.Do(func() { close(a.stopCh) })
	logger.I18nInfo("微信公众号适配器已关闭")
	return nil
}

// WebhookUUID returns the unified-webhook uuid.
func (a *Adapter) WebhookUUID() string { return a.webhookID }

// WebhookCallback is the unified webhook entry.
func (a *Adapter) WebhookCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		a.verify(w, r)
		return
	}
	a.callbackCommand(w, r)
}

// handleCallback is the standalone server entry (GET verify + POST callback).
func (a *Adapter) handleCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		a.verify(w, r)
		return
	}
	a.callbackCommand(w, r)
}

// verify handles the URL verification (echostr). The SDK's ReadMessage skips
// signature validation when EchoStr is present, so we validate manually
// (mirrors wechatpy check_signature).
func (a *Adapter) verify(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := (&wxmp.MessageQuery{
		Signature: q.Get("signature"),
		Timestamp: q.Get("timestamp"),
		Nonce:     q.Get("nonce"),
		EchoStr:   q.Get("echostr"),
	}).Validate(a.account.PrivateToken); err != nil {
		http.Error(w, "err", http.StatusBadRequest)
		return
	}
	_, _ = io.WriteString(w, q.Get("echostr"))
}

// callbackCommand handles the POST callback using the SDK (decrypt + parse).
func (a *Adapter) callbackCommand(w http.ResponseWriter, r *http.Request) {
	_, msg, err := a.account.ReadMessage(r)
	if err != nil {
		logger.I18nWarn("读取回调失败: %v", err)
		http.Error(w, "err", http.StatusBadRequest)
		return
	}

	// Events (subscribe/unsubscribe etc.) are logged and ignored.
	if msg.MsgType == "event" {
		logger.Debug("微信公众号事件: %s %s", msg.Event, msg.EventKey)
		_, _ = io.WriteString(w, "success")
		return
	}

	abm := a.convertMessage(&msg)
	if abm == nil {
		_, _ = io.WriteString(w, "success")
		return
	}
	a.processEvent(abm)
	_, _ = io.WriteString(w, "success")
}

// convertMessage maps a MessageData to an AstrBotMessage.
func (a *Adapter) convertMessage(msg *wxmp.MessageData) *platform.AstrBotMessage {
	abm := platform.NewAstrBotMessage()
	abm.Type = platform.FriendMessage
	abm.Sender = platform.MessageMember{UserID: msg.FromUserName, Nickname: msg.FromUserName}
	abm.SessionID = msg.FromUserName
	abm.SelfID = msg.ToUserName
	if msg.MsgId > 0 {
		abm.MessageID = fmt.Sprintf("%d", msg.MsgId)
	}
	abm.Timestamp = msg.CreateTime
	abm.RawMessage = msg

	switch msg.MsgType {
	case "text":
		abm.MessageStr = msg.Content
		abm.Message = []message.Component{&message.Plain{Text: msg.Content}}
	case "image":
		abm.MessageStr = "[图片]"
		abm.Message = []message.Component{&message.Image{URL: msg.PicUrl, File: msg.MediaId}}
	case "voice":
		abm.MessageStr = ""
		abm.Message = []message.Component{&message.Record{File: msg.MediaId, URL: msg.MediaId}}
	case "video", "shortvideo":
		abm.MessageStr = "[视频]"
		abm.Message = []message.Component{&message.Video{FileID: msg.MediaId, Path: msg.ThumbMediaId}}
	case "link":
		text := strings.Join([]string{msg.Title, msg.Description, msg.Url}, " ")
		abm.MessageStr = text
		abm.Message = []message.Component{&message.Plain{Text: text}}
	case "location":
		text := fmt.Sprintf("位置: %s, %d (经度 %v, 纬度 %v)", msg.Label, msg.Scale, msg.LocationX, msg.LocationY)
		abm.MessageStr = text
		abm.Message = []message.Component{&message.Plain{Text: text}}
	default:
		abm.MessageStr = "[" + msg.MsgType + "]"
		abm.Message = []message.Component{&message.Plain{Text: abm.MessageStr}}
	}
	return abm
}

// processEvent publishes the event into the pipeline.
func (a *Adapter) processEvent(abm *platform.AstrBotMessage) {
	if a.EventBus == nil {
		return
	}
	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   "weixin_official_account",
			SelfID:     abm.SelfID,
			SenderID:   abm.Sender.UserID,
			SenderName: abm.Sender.Nickname,
			ConvID:     abm.SessionID,
			IsGroup:    false,
		},
		Message:    &message.MessageChain{Chain: abm.Message},
		MessageStr: abm.MessageStr,
		Timestamp:  time.Unix(abm.Timestamp, 0),
		MessageObj: &core.MessageObj{MessageID: abm.MessageID, SelfID: abm.SelfID},
		Metadata:   map[string]interface{}{},
	}
	if err := a.EventBus.Publish(event); err != nil {
		logger.Error("发布事件失败: %v", err)
	}
}

// Send sends a message chain via the custom message API. The Official
// Account does not support proactive messages; this path is used in
// active_send_mode replies (mirrors Python's send_by_session which raises).
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	if !a.activeSend {
		return fmt.Errorf("微信公众号不支持发送主动消息（请使用被动回复模式）")
	}
	text := ""
	for _, comp := range chain.Chain {
		if plain, ok := comp.(*message.Plain); ok {
			text += plain.Text
		}
	}
	if text == "" {
		return nil
	}
	return a.sendCustomText(sessionID, text)
}

// sendCustomText sends a text message via message/custom/send.
func (a *Adapter) sendCustomText(openID, text string) error {
	data := wxmp.MessageCustomSendData{
		ToUser:  openID,
		MsgType: wxmp.MessageCustomSendTypeText,
		Text:    wxmp.MessageCustomSendMsgText{Content: text},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.account.NewMpReq(wxmp.MessageCustomSend).SendData(&data).Do(ctx); err != nil {
		return fmt.Errorf("weixin: custom send failed: %w", err)
	}
	return nil
}
