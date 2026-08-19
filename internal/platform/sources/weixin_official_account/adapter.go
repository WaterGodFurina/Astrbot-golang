// Package weixin_official_account implements a WeChat Official Account
// (公众号) adapter. Ported 1:1 from
// astrbot/core/platform/sources/weixin_official_account/ (Python, wechatpy),
// built on the github.com/blusewang/wx SDK (MpAccount.ReadMessage + NewMpReq).
package weixin_official_account

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
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
	// tokenMu 串行化 access_token 的检查与刷新，避免并发回复多个用户时
	// 重复请求 gettoken（微信对 gettoken 有频率限制，且重复获取会互相作废 token）。
	tokenMu sync.Mutex
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
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
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

// callbackCommand handles the POST callback. Signature validation, timestamp
// freshness, ciphertext structure checks and appId verification are done here
// (the SDK only validates sha1(token,timestamp,nonce) and never uses
// msg_signature, and its decryption can panic on malformed input).
func (a *Adapter) callbackCommand(w http.ResponseWriter, r *http.Request) {
	msg, err := a.readCallbackMessage(r)
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

// readCallbackMessage 读取并校验回调消息：
//   - 校验时间戳新鲜度；
//   - 密文模式（报文体含 Encrypt）：校验 msg_signature = sha1(sort(token,timestamp,nonce,Encrypt))，
//     校验密文结构合法后调用 SDK 解密，并比对解密后的 AppId；
//   - 明文模式：校验 signature = sha1(sort(token,timestamp,nonce))。
func (a *Adapter) readCallbackMessage(r *http.Request) (wxmp.MessageData, error) {
	var msg wxmp.MessageData
	q := r.URL.Query()
	timestamp := q.Get("timestamp")
	nonce := q.Get("nonce")
	if timestamp == "" || nonce == "" {
		return msg, errors.New("缺少 timestamp/nonce 参数")
	}
	if err := checkTimestampFreshness(timestamp); err != nil {
		return msg, err
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return msg, err
	}
	if err := xml.Unmarshal(body, &msg); err != nil {
		return msg, err
	}
	token := a.account.PrivateToken
	if msg.Encrypt != "" {
		expected := computeSHA1(token, timestamp, nonce, msg.Encrypt)
		provided := q.Get("msg_signature")
		if provided == "" || subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) != 1 {
			return msg, errors.New("msg_signature 校验失败")
		}
		if err := validateCiphertext(msg.Encrypt, a.account.EncodingAESKey); err != nil {
			return msg, err
		}
		if err := msg.ShouldDecode(a.account.EncodingAESKey); err != nil {
			return msg, err
		}
		if a.account.AppId != "" && msg.AppId != a.account.AppId {
			return msg, errors.New("解密后的 AppId 与账号 AppId 不一致")
		}
		return msg, nil
	}
	expected := computeSHA1(token, timestamp, nonce)
	provided := q.Get("signature")
	if provided == "" || subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) != 1 {
		return msg, errors.New("signature 校验失败")
	}
	return msg, nil
}

// validateCiphertext 在调用 SDK 解密前校验密文结构（长度、padding、长度字段范围），
// 避免 SDK ShouldDecode 对畸形密文 slice 越界 panic。
func validateCiphertext(encrypt, encodingAESKey string) error {
	key, err := base64.StdEncoding.DecodeString(encodingAESKey + "=")
	if err != nil {
		return errors.New("无效的 EncodingAESKey")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return errors.New("无效的 EncodingAESKey")
	}
	raw, err := base64.StdEncoding.DecodeString(encrypt)
	if err != nil {
		return errors.New("无效的密文 base64")
	}
	if len(raw) < aes.BlockSize || len(raw)%aes.BlockSize != 0 {
		return errors.New("无效的密文长度")
	}
	pt := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, raw[:aes.BlockSize]).CryptBlocks(pt, raw)
	if len(pt) < 20 {
		return errors.New("密文结构不完整")
	}
	pad := int(pt[len(pt)-1])
	if pad < 1 || pad > 32 || pad > len(pt) {
		return errors.New("无效的填充长度")
	}
	bodyEnd := len(pt) - pad
	if bodyEnd < 20 {
		return errors.New("密文结构不完整")
	}
	msgLen := int(binary.BigEndian.Uint32(pt[16:20]))
	if msgLen <= 0 || msgLen > bodyEnd-20 {
		return errors.New("无效的消息长度字段")
	}
	return nil
}

// checkTimestampFreshness 校验回调时间戳在 ±5 分钟窗口内（timestamp 为秒）。
func checkTimestampFreshness(timestamp string) error {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("无效时间戳")
	}
	diff := time.Now().Unix() - ts
	if diff < -300 || diff > 300 {
		return errors.New("时间戳超出新鲜度窗口")
	}
	return nil
}

// computeSHA1 计算微信签名：对参数排序后拼接并做 SHA1。
func computeSHA1(parts ...string) string {
	cp := make([]string, len(parts))
	copy(cp, parts)
	sort.Strings(cp)
	sum := sha1.Sum([]byte(strings.Join(cp, "")))
	return hex.EncodeToString(sum[:])
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
			PlatformID: a.ID(),
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
