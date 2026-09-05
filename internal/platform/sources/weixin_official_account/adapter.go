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
	"os"
	"path/filepath"
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
	srv        *http.Server
	stopCh     chan struct{}
	once       sync.Once
	// tokenMu 串行化 access_token 的检查与刷新，避免并发回复多个用户时
	// 重复请求 gettoken（微信对 gettoken 有频率限制，且重复获取会互相作废 token）。
	tokenMu sync.Mutex

	// 被动回复 5 秒窗口状态（对齐本体 user_buffer + wexin_event_workers）：
	// userBuffer: from_user → 被动回复缓冲状态；workers: msg_id → 排重 worker。
	userMu     sync.Mutex
	userBuffer map[string]*userState
	workersMu  sync.Mutex
	workers    map[string]*msgWorker
}

// New creates the adapter.
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		config:     config,
		settings:   settings,
		EventBus:   eventBus,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		stopCh:     make(chan struct{}),
		userBuffer: make(map[string]*userState),
		workers:    make(map[string]*msgWorker),
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
	mux := http.NewServeMux()
	mux.HandleFunc("/callback/command", a.handleCallback)
	a.srv = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", a.host, a.port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		if err := a.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.I18nError("微信公众号 webhook 服务器运行异常: %v", err)
		}
	}()
	logger.I18nInfo("微信公众号 webhook 服务器已启动 :%d/callback/command", a.port)
	return nil
}

// Stop stops the adapter.
func (a *Adapter) Stop() error {
	a.once.Do(func() { close(a.stopCh) })
	if a.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.srv.Shutdown(ctx)
	}
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
	// 显式 text/plain：避免 Go 对 echostr 做内容嗅探（若含 HTML 会按
	// text/html 响应，构成反射型 XSS）；签名已在上面用 PrivateToken 校验。
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, q.Get("echostr")) // nosemgrep: go.lang.security.audit.xss.no-io-writestring-to-responsewriter.no-io-writestring-to-responsewriter
}

// callbackCommand handles the POST callback. Signature validation, timestamp
// freshness, ciphertext structure checks and appId verification are done here
// (the SDK only validates sha1(token,timestamp,nonce) and never uses
// msg_signature, and its decryption can panic on malformed input).
//
// 被动回复全链路（对齐本体 handle_callback:128-298）：
//  1. 同 msg_id 排重（微信失败重推 3 次），180s 内等待同一 worker 结果；
//  2. user_buffer 命中：弹出缓存回复或返回【正在思考…】占位符；
//  3. 新消息：创建缓冲状态、发布事件进入管线，4.0s 窗口内等待回复 XML
//     （有 EncodingAESKey 时以安全模式加密回包），超时回占位符。
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

	if a.activeSend {
		// 主动发送模式：旁路被动回复逻辑，直接进入管线异步处理（本体 active_send_mode 分支）。
		abm := a.convertMessage(&msg)
		if abm == nil {
			_, _ = io.WriteString(w, "success")
			return
		}
		a.publishEvent(abm, nil)
		_, _ = io.WriteString(w, "success")
		return
	}

	a.handlePassiveReply(&msg, w, r)
}

// handlePassiveReply 实现被动回复 5 秒窗口的全链路等待与缓冲（本体 :164-298）。
func (a *Adapter) handlePassiveReply(msg *wxmp.MessageData, w http.ResponseWriter, r *http.Request) {
	nonce := r.URL.Query().Get("nonce")
	timestamp := r.URL.Query().Get("timestamp")
	msgID := messageKey(msg)

	// 1) 消息排重：同 msg_id（微信重试）在 180s 内等待同一 future（本体 :355-386）。
	a.workersMu.Lock()
	wk := a.workers[msgID]
	a.workersMu.Unlock()
	if wk != nil {
		logger.Debug("duplicate message id checked: %s", msgID)
		select {
		case <-wk.done:
		case <-time.After(workerWaitTTL):
		}
		if xml, ok, empty := wk.st.takeReply(); ok {
			if empty {
				a.deleteUserState(wk.st)
			}
			a.writeReply(w, xml, nonce, timestamp)
			return
		}
		a.writeReply(w, a.maybeEncrypt(textReplyXML(msg.ToUserName, msg.FromUserName, workerTimeoutReply), nonce, timestamp), nonce, timestamp)
		return
	}

	// 2) 用户缓冲命中：思考中/残留缓存（本体 :174-244）。
	a.userMu.Lock()
	st := a.userBuffer[msg.FromUserName]
	a.userMu.Unlock()
	if st != nil {
		a.replyFromState(st, msg, w, nonce, timestamp, msgID)
		return
	}

	// 3) 新消息：创建缓冲状态并发布事件（本体 :246-298）。
	preview := msgPreview(msg)
	logger.Info("wx start task: user=%s msg_id=%s preview=%s", msg.FromUserName, msgID, preview)
	st = newUserState(msg, msgID, preview)

	a.userMu.Lock()
	if exist, ok := a.userBuffer[msg.FromUserName]; ok {
		// 并发同用户：并入既有状态的处理分支。
		a.userMu.Unlock()
		st = exist
		a.replyFromState(st, msg, w, nonce, timestamp, msgID)
		return
	}
	a.userBuffer[msg.FromUserName] = st
	a.userMu.Unlock()

	abm := a.convertMessage(msg)
	if abm == nil || a.EventBus == nil {
		// 无事件总线（无法进入管线）或未支持的消息类型：清理状态后确认。
		if a.EventBus == nil {
			logger.Warn("EventBus 未注入，跳过被动回复等待")
		}
		st.finish()
		a.deleteUserState(st)
		_, _ = io.WriteString(w, "success")
		return
	}

	done := core.NewPipelineDone()
	a.publishEvent(abm, done)
	a.launchWorker(msgID, st, done)

	// 4.0s 窗口内等待管线结果。
	st.waitDone(wxMsgTimeOut)
	if xml, ok, empty := st.takeReply(); ok {
		if empty {
			a.deleteUserState(st)
		}
		logger.Info("wx finished in window: user=%s msg_id=%s", msg.FromUserName, msgID)
		a.writeReply(w, xml, nonce, timestamp)
		return
	}
	logger.Info("wx first window timeout: user=%s msg_id=%s", msg.FromUserName, msgID)
	a.writePlaceholder(w, st, msg, nonce, timestamp)
}

// replyFromState 处理既有缓冲状态的回复弹出/重试等待/占位符（本体 :174-244）。
func (a *Adapter) replyFromState(st *userState, msg *wxmp.MessageData, w http.ResponseWriter, nonce, timestamp, msgID string) {
	// 缓存弹出优先：每次触发弹出一条（本体 cached_xml 分支）。
	if xml, ok, empty := st.takeReply(); ok {
		logger.Info("wx buffer hit on trigger: user=%s", st.fromUser)
		if empty {
			a.deleteUserState(st)
		}
		a.writeReply(w, xml, nonce, timestamp)
		return
	}

	// 同 msg_id => 微信重试：在窗口内继续等待；新 msg_id => 用户触发：直接占位符。
	if st.msgID == msgID && st.isActive() {
		st.waitDone(wxMsgTimeOut)
		if xml, ok, empty := st.takeReply(); ok {
			if empty {
				a.deleteUserState(st)
			}
			a.writeReply(w, xml, nonce, timestamp)
			return
		}
	}
	logger.Debug("wx trigger while thinking: user=%s", st.fromUser)
	a.writePlaceholder(w, st, msg, nonce, timestamp)
}

// writePlaceholder 输出【正在思考…】占位符回复（本体 placeholder 文案）。
func (a *Adapter) writePlaceholder(w http.ResponseWriter, st *userState, msg *wxmp.MessageData, nonce, timestamp string) {
	elapsed := int(time.Since(st.startedAt).Seconds())
	placeholder := fmt.Sprintf("【正在思考'%s'中，已思考%ds，回复任意文字尝试获取回复】", st.preview, elapsed)
	a.writeReply(w, a.maybeEncrypt(textReplyXML(msg.ToUserName, msg.FromUserName, placeholder), nonce, timestamp), nonce, timestamp)
}

// writeReply 以 text/xml 输出被动回复（内容已就绪，含加密与否）。
func (a *Adapter) writeReply(w http.ResponseWriter, replyXML, nonce, timestamp string) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	_, _ = io.WriteString(w, replyXML)
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
	// IV 与协议一致（密钥前 16 字节），与 msg.ShouldDecode 的 iv=key[:16] 对齐
	cipher.NewCBCDecrypter(block, key[:aes.BlockSize]).CryptBlocks(pt, raw)
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
	// #nosec G401 -- sha1 为微信公众号/服务号签名协议要求（check_signature 校验），非密码学哈希用途
	sum := sha1.Sum([]byte(strings.Join(cp, ""))) // nosemgrep: go.lang.security.audit.crypto.use_of_weak_crypto.use-of-sha1
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
		// 语音接收：下载临时素材并用宿主 ffmpeg 转 wav（本体 :460-484）。
		// 转换失败时跳过该消息（本体转换失败直接 return，不进入管线）。
		path, ok := a.fetchVoiceMessage(msg)
		if !ok {
			return nil
		}
		abm.MessageStr = ""
		abm.Message = []message.Component{&message.Record{File: path, URL: path}}
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

// fetchVoiceMessage 下载语音临时素材并转为 wav，返回文件路径。
// 对应本体 convert_message 的 voice 分支（media.download + ffmpeg 转 wav）。
func (a *Adapter) fetchVoiceMessage(msg *wxmp.MessageData) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	data, err := a.downloadMediaByID(ctx, msg.MediaId)
	if err != nil {
		logger.Error("下载公众号语音素材失败: %v", err)
		return "", false
	}
	ext := mediaExtByFormat(msg.Format)
	if sniffed := sniffAudioExt(data, ""); sniffed != "" {
		ext = sniffed
	}
	path := filepath.Join(os.TempDir(), fmt.Sprintf("weixin_offacc_%s%s", msg.MediaId, ext))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		logger.Error("保存公众号语音素材失败: %v", err)
		return "", false
	}
	// ffmpeg 转 wav（amr 等 → wav），失败时保留原文件（宿主 wecom 先例：降级不丢消息）。
	path = convertAudioToWavPath(path)
	return path, true
}

// processEvent publishes the event into the pipeline.
// done 非空时（被动回复模式）在 Metadata 上挂管线完成信号，
// 供被动回复窗口等待管线结束（对应本体 future/task 完成语义）。
func (a *Adapter) publishEvent(abm *platform.AstrBotMessage, done *core.PipelineDone) {
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
	if done != nil {
		event.Metadata[core.MetadataPipelineDone] = done
	}
	if err := a.EventBus.Publish(event); err != nil {
		logger.Error("发布事件失败: %v", err)
	}
}

// Send 发送消息链。
//   - active_send_mode：通过客服消息接口发送（send_text/send_image/send_voice，
//     对齐本体 weixin_offacc_event.py 的 active_send_mode 分支）；
//   - 被动模式：写入用户缓冲——文本按 1024 分段进 cached（本体 send() 的
//     message_out["cached_xml"]），图片/语音上传素材后生成被动回复 XML
//     等待被动窗口返回（本体 future.set_result）。
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	if a.activeSend {
		return a.sendActive(sessionID, chain)
	}
	return a.bufferPassive(sessionID, chain)
}

// bufferPassive 被动模式发送：写入用户缓冲状态。
func (a *Adapter) bufferPassive(sessionID string, chain *message.MessageChain) error {
	st := a.getUserState(sessionID)
	if st == nil {
		return fmt.Errorf("微信公众号不支持发送主动消息（被动模式且无回复缓冲）")
	}
	for _, comp := range chain.Chain {
		switch c := comp.(type) {
		case *message.Plain:
			// 长文本 1024 分段（本体 split_plain），暂存待管线结束后统一入缓存。
			st.appendPending(c.Text)
		case *message.Image:
			mediaID, err := a.uploadImageComponent(c)
			if err != nil {
				logger.Error("微信公众平台上传图片失败: %v", err)
				st.appendPending(fmt.Sprintf("微信公众平台上传图片失败: %v", err))
				continue
			}
			// 被动 ImageReply XML（本体 ImageReply(media_id).render() + future.set_result）。
			st.setFutureXML(imageReplyXML(st.toUser, st.fromUser, mediaID))
		case *message.Record:
			mediaID, err := a.uploadVoiceComponent(c)
			if err != nil {
				logger.Error("微信公众平台上传语音失败: %v", err)
				st.appendPending(fmt.Sprintf("微信公众平台上传语音失败: %v", err))
				continue
			}
			// 被动 VoiceReply XML（本体 VoiceReply(media_id).render() + future.set_result）。
			st.setFutureXML(voiceReplyXML(st.toUser, st.fromUser, mediaID))
		default:
			logger.Warn("还没实现这个消息类型的发送逻辑: %T。", comp)
		}
	}
	return nil
}

// sendActive 主动发送模式：客服消息接口发送文本/图片/语音。
func (a *Adapter) sendActive(sessionID string, chain *message.MessageChain) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, comp := range chain.Chain {
		switch c := comp.(type) {
		case *message.Plain:
			// 长文本 1024 分段逐条发送（本体 active 分支逐 chunk send_text）。
			for _, chunk := range splitPlain(c.Text, plainMaxLength) {
				if err := a.sendCustomText(sessionID, chunk); err != nil {
					return err
				}
			}
		case *message.Image:
			mediaID, err := a.uploadImageComponent(c)
			if err != nil {
				logger.Error("微信公众平台上传图片失败: %v", err)
				if err := a.sendCustomText(sessionID, fmt.Sprintf("微信公众平台上传图片失败: %v", err)); err != nil {
					return err
				}
				continue
			}
			data := wxmp.MessageCustomSendData{
				ToUser:  sessionID,
				MsgType: wxmp.MessageCustomSendTypeImage,
				Image:   wxmp.MessageCustomSendMsgImage{MediaId: mediaID},
			}
			if err := a.sendCustom(ctx, &data); err != nil {
				return fmt.Errorf("weixin: custom send image failed: %w", err)
			}
		case *message.Record:
			mediaID, err := a.uploadVoiceComponent(c)
			if err != nil {
				logger.Error("微信公众平台上传语音失败: %v", err)
				if err := a.sendCustomText(sessionID, fmt.Sprintf("微信公众平台上传语音失败: %v", err)); err != nil {
					return err
				}
				continue
			}
			data := wxmp.MessageCustomSendData{
				ToUser:  sessionID,
				MsgType: "voice",
				Voice:   wxmp.MessageCustomSendMsgVoice{MediaId: mediaID},
			}
			if err := a.sendCustom(ctx, &data); err != nil {
				return fmt.Errorf("weixin: custom send voice failed: %w", err)
			}
		default:
			logger.Warn("还没实现这个消息类型的发送逻辑: %T。", comp)
		}
	}
	return nil
}

// sendCustom 通过客服消息接口发送（message/custom/send）。
func (a *Adapter) sendCustom(ctx context.Context, data *wxmp.MessageCustomSendData) error {
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := a.account.NewMpReq(wxmp.MessageCustomSend).SendData(data).Do(ctx2); err != nil {
		return fmt.Errorf("weixin: custom send failed: %w", err)
	}
	return nil
}

// uploadImageComponent 解析图片组件内容并上传为临时素材，返回 media_id。
func (a *Adapter) uploadImageComponent(img *message.Image) (string, error) {
	data, err := a.resolveComponentMedia(img.Path, img.File, img.Base64, img.URL)
	if err != nil {
		return "", err
	}
	ext := ".jpg"
	if p := pickFirst(img.Path, img.File); p != "" {
		if e := strings.ToLower(filepath.Ext(p)); e != "" {
			ext = e
		}
	}
	if len(data) >= 4 && string(data[:4]) == "\x89PNG" {
		ext = ".png"
	} else if len(data) >= 3 && string(data[:3]) == "GIF" {
		ext = ".gif"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return a.uploadMedia(ctx, "image", data, ext)
}

// uploadVoiceComponent 解析语音组件内容并转为 amr 后上传，返回 media_id。
// 对齐本体 send() 的 Record 分支（convert_audio_to_amr → media.upload("voice")）。
func (a *Adapter) uploadVoiceComponent(rec *message.Record) (string, error) {
	data, err := a.resolveComponentMedia(rec.Path, rec.File, rec.Base64, rec.URL)
	if err != nil {
		return "", err
	}
	// 先落盘再转码（ffmpeg 需要文件输入）。
	ext := rec.Format
	if ext == "" {
		ext = strings.TrimPrefix(sniffAudioExt(data, ".amr"), ".")
	}
	inPath := filepath.Join(os.TempDir(), fmt.Sprintf("weixin_offacc_send_%d.%s", time.Now().UnixNano(), ext))
	if err := os.WriteFile(inPath, data, 0o600); err != nil {
		return "", err
	}
	defer os.Remove(inPath)

	amrPath := convertAudioToAmrPath(inPath)
	if amrPath == "" {
		// 转码失败：已是 amr 时可直接上传，否则报错（对齐本体转换失败抛错的语义）。
		if strings.HasSuffix(inPath, ".amr") {
			amrPath = inPath
		} else {
			return "", fmt.Errorf("语音转 amr 失败：请确认宿主已安装 ffmpeg")
		}
	}
	amrData, err := os.ReadFile(amrPath)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return a.uploadMedia(ctx, "voice", amrData, ".amr")
}

// resolveComponentMedia 解析消息组件的二进制内容：优先本地文件，其次 base64，
// 最后经 SSRF 校验下载 URL（宿主 SafeDownloadBytes 先例）。
func (a *Adapter) resolveComponentMedia(path, file, b64, url string) ([]byte, error) {
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
		return platform.SafeDownloadBytes(context.Background(), url, 20<<20)
	}
	return nil, fmt.Errorf("媒体组件缺少可用的内容（path/file/base64/url 均为空）")
}

// pickFirst 返回第一个非空字符串。
func pickFirst(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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
