// Package wecom implements the WeCom (企业微信应用 & 微信客服) platform adapter.
// 1:1 移植自 astrbot/core/platform/sources/wecom/：
//   - 企业微信应用（corpid/secret/token/encoding_aes_key）回调消息接收与发送；
//   - 微信客服（kf_name）消息接收（kf/sync_msg）与发送（kf/send_msg）；
//   - AES-256-CBC 加解密与 SHA1 签名（见 wxcrypt.go）；
//   - 独立回调服务器（/callback/command）与统一 webhook 模式（WebhookPlatform）。
package wecom

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

var logger = log.GetDefault().WithComponent("Wecom")

const (
	// defaultAPiBaseURL 默认企业微信 API 地址
	defaultAPiBaseURL = "https://qyapi.weixin.qq.com/cgi-bin/"
	// kfTextDedupTTL 微信客服文本消息去重窗口（对应 WECHAT_KF_TEXT_CONTENT_DEDUP_TTL_SECONDS = 15 秒）
	kfTextDedupTTL = 15 * time.Second
)

// Adapter 企业微信（应用 & 微信客服）平台适配器。
type Adapter struct {
	config   map[string]interface{}
	settings map[string]interface{}

	// EventBus 事件总线（lifecycle 通过 SetEventBus 注入）
	EventBus platform.EventBus

	id                 string
	corpID             string
	secret             string
	token              string
	encodingAESKey     string
	kfName             string
	apiBaseURL         string
	unifiedWebhookMode bool
	webhookUUID        string
	callbackServerHost string
	port               int

	client *WeChatClient
	crypto *WXBizMsgCrypt
	server *WecomServer

	mu         sync.Mutex
	agentID    string // 最近一次收到的消息的 AgentID（用于回复）
	seenKFText map[string]time.Time
	seenAppMsg map[string]time.Time
	stopCh     chan struct{}
}

// New 构造企业微信适配器。
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		config:     config,
		settings:   settings,
		EventBus:   eventBus,
		seenKFText: make(map[string]time.Time),
		seenAppMsg: make(map[string]time.Time),
		stopCh:     make(chan struct{}),
	}
	a.id, _ = config["id"].(string)
	if a.id == "" {
		a.id = "wecom"
	}
	a.corpID = strings.TrimSpace(configString(config, "corpid"))
	a.secret = strings.TrimSpace(configString(config, "secret"))
	a.token = strings.TrimSpace(configString(config, "token"))
	a.encodingAESKey = strings.TrimSpace(configString(config, "encoding_aes_key"))
	a.kfName = configString(config, "kf_name")

	// api_base_url 归一化：与 Python 一致，确保以 /cgi-bin/ 结尾
	a.apiBaseURL = configString(config, "api_base_url")
	if a.apiBaseURL == "" {
		a.apiBaseURL = defaultAPiBaseURL
	}
	a.apiBaseURL = strings.TrimSuffix(a.apiBaseURL, "/")
	if !strings.HasSuffix(a.apiBaseURL, "/cgi-bin") {
		a.apiBaseURL += "/cgi-bin"
	}
	a.apiBaseURL += "/"

	a.unifiedWebhookMode = configBool(config, "unified_webhook_mode")
	a.webhookUUID = configString(config, "webhook_uuid")
	a.callbackServerHost = configString(config, "callback_server_host")
	if a.callbackServerHost == "" {
		a.callbackServerHost = "0.0.0.0"
	}
	a.port = configInt(config, "port", 6195)

	a.client = NewWeChatClient(a.corpID, a.secret, a.apiBaseURL)
	// crypto 在 Start 中初始化（token/encoding_aes_key 校验失败时给出明确错误）
	return a
}

// SetEventBus 注入事件总线（实现 platform.EventBusSetter，lifecycle 在构造后调用）。
func (a *Adapter) SetEventBus(bus platform.EventBus) {
	a.EventBus = bus
}

// ID 返回适配器实例 ID。
func (a *Adapter) ID() string { return a.id }

// Type 返回平台类型。
func (a *Adapter) Type() string { return "wecom" }

// IsKFMode 是否启用了微信客服（配置了 kf_name）。
func (a *Adapter) IsKFMode() bool { return a.kfName != "" }

// Start 启动适配器：
//  1. 微信客服模式下获取客服帐号列表并打印客服链接（对应 Python run() 的 kf 初始化）；
//  2. 统一 webhook 模式下不启动独立服务器；
//  3. 否则在 callback_server_host:port 启动回调服务器。
func (a *Adapter) Start(ctx context.Context) error {
	crypto, err := NewWXBizMsgCrypt(a.token, a.encodingAESKey, a.corpID)
	if err != nil {
		return fmt.Errorf("初始化企业微信加解密失败: %w", err)
	}
	a.crypto = crypto

	// 微信客服：获取 open_kfid 并生成客服链接
	if a.kfName != "" {
		a.initKF(ctx)
	}

	if a.unifiedWebhookMode && a.webhookUUID != "" {
		logger.I18nInfo("企业微信(wecom) 已启用统一 Webhook 模式, webhook_uuid=%s", a.webhookUUID)
		return nil
	}

	a.server = &WecomServer{adapter: a}
	if err := a.server.Start(ctx, a.callbackServerHost, a.port); err != nil {
		return err
	}
	logger.I18nInfo("企业微信 适配器将在 %s:%d 端口启动回调服务器", a.callbackServerHost, a.port)
	return nil
}

// initKF 初始化微信客服：获取客服帐号列表，查找 kf_name 对应的 open_kfid 并生成客服链接。
func (a *Adapter) initKF(ctx context.Context) {
	data, err := a.client.KFGetAccountList(ctx)
	if err != nil {
		logger.I18nError("获取微信客服列表失败: %v", err)
		return
	}
	accList, _ := data["account_list"].([]interface{})
	for _, item := range accList {
		acc, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := acc["name"].(string)
		if name != a.kfName {
			continue
		}
		openKFID, _ := acc["open_kfid"].(string)
		if openKFID == "" {
			logger.I18nError("获取微信客服失败，open_kfid 为空。")
			continue
		}
		logger.Debug("Found open_kfid: %s", openKFID)
		kfURL, err := a.client.KFAddContactWay(ctx, openKFID, "astrbot_placeholder")
		if err != nil {
			logger.I18nError("获取客服链接失败: %v", err)
			return
		}
		logger.I18nInfo("请打开以下链接，在微信扫码以获取客服微信: https://api.cl2wm.cn/api/qrcode/code?text=%s", kfURL)
		return
	}
}

// Stop 关闭适配器。
func (a *Adapter) Stop() error {
	select {
	case <-a.stopCh:
	default:
		close(a.stopCh)
	}
	if a.server != nil {
		a.server.Shutdown()
	}
	logger.I18nInfo("企业微信 适配器已被关闭")
	return nil
}

// WebhookUUID 返回统一 Webhook 模式的标识（实现 platform.WebhookPlatform）。
func (a *Adapter) WebhookUUID() string { return a.webhookUUID }

// WebhookCallback 统一 Webhook 回调入口（实现 platform.WebhookPlatform）：
// GET 处理 URL 验证，POST 处理消息回调。
func (a *Adapter) WebhookCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		a.handleVerify(w, r)
		return
	}
	a.handleCallback(w, r)
}

// handleVerify 处理 URL 验证请求（GET），返回解密后的 echostr。
func (a *Adapter) handleVerify(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	logger.I18nInfo("验证请求有效性: %v", query)
	if a.crypto == nil {
		http.Error(w, "verify fail", http.StatusInternalServerError)
		return
	}
	echoStr, err := a.crypto.CheckSignature(
		query.Get("msg_signature"),
		query.Get("timestamp"),
		query.Get("nonce"),
		query.Get("echostr"),
	)
	if err != nil {
		logger.I18nError("验证请求有效性失败，签名异常，请检查配置: %v", err)
		http.Error(w, "verify fail", http.StatusBadRequest)
		return
	}
	logger.I18nInfo("验证请求有效性成功。")
	w.Header().Set("Content-Type", "text/plain")
	// #nosec no-io-writestring-to-responsewriter -- 企业微信 URL 验证回调：回显解密后的 echostr
	//（签名已用 msg_signature 校验），Content-Type 为 text/plain，响应对象是企业微信服务器而非浏览器。
	_, _ = io.WriteString(w, echoStr) // nosemgrep: go.lang.security.audit.xss.no-io-writestring-to-responsewriter.no-io-writestring-to-responsewriter
}

// handleCallback 处理消息回调（POST）：解密 → 解析 → 分发。
func (a *Adapter) handleCallback(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return
	}
	query := r.URL.Query()
	msgSignature := query.Get("msg_signature")
	timestamp := query.Get("timestamp")
	nonce := query.Get("nonce")
	if a.crypto == nil {
		http.Error(w, "verify fail", http.StatusInternalServerError)
		return
	}
	xmlBody, err := a.crypto.DecryptMessage(body, msgSignature, timestamp, nonce)
	if err != nil {
		logger.I18nError("解密失败，签名异常，请检查配置: %v", err)
		http.Error(w, "解密失败", http.StatusBadRequest)
		return
	}
	msg, err := ParseWecomMessage(xmlBody)
	if err != nil {
		logger.I18nError("解析企业微信消息失败: %v", err)
		http.Error(w, "解析失败", http.StatusBadRequest)
		return
	}
	logger.I18nInfo("解析成功: type=%s msgid=%s agent=%s", msg.Type, msg.ID, msg.Agent)

	// 先回包 success（微信要求 5 秒内响应），耗时操作异步执行，避免媒体下载等超时被重推
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, "success")

	if msg.IsKFMsgOrEvent() {
		go a.handleKFMsgOrEvent(msg)
	} else if !a.isDuplicateAppMessage(msg) {
		go a.convertMessage(msg)
	}
}

// handleKFMsgOrEvent 处理 kf_msg_or_event 回调：通过 kf/sync_msg 拉取最新客服消息。
func (a *Adapter) handleKFMsgOrEvent(msg *WecomMessage) {
	ctx := context.Background()
	cursor := ""
	hasMore := true
	for hasMore {
		ret, err := a.client.KFSyncMsg(ctx, msg.Token, msg.OpenKfID, cursor, 1000)
		if err != nil {
			logger.I18nError("同步微信客服消息失败: %v", err)
			return
		}
		if nc, ok := ret["next_cursor"].(string); ok && nc != "" {
			cursor = nc
		}
		if hm, ok := ret["has_more"].(float64); ok {
			hasMore = int(hm) != 0
		} else {
			hasMore = false
		}
		msgList, _ := ret["msg_list"].([]interface{})
		for _, item := range msgList {
			if m, ok := item.(map[string]interface{}); ok {
				a.convertKFMessage(m)
			}
		}
	}
}

// convertMessage 转换普通消息（文本/图片/语音）为 AstrBotMessage 并提交事件。
// 对应 Python convert_message。
func (a *Adapter) convertMessage(msg *WecomMessage) {
	abm := platform.NewAstrBotMessage()
	switch msg.Type {
	case "text":
		abm.MessageStr = msg.Content
		abm.SelfID = msg.Agent
		abm.Message = []message.Component{&message.Plain{Text: msg.Content}}
		abm.Type = platform.FriendMessage
		abm.Sender = platform.MessageMember{UserID: msg.Source, Nickname: msg.Source}
		abm.MessageID = msg.ID
		abm.Timestamp = msg.Time
		abm.SessionID = msg.Source
		abm.RawMessage = msg
	case "image":
		abm.MessageStr = "[图片]"
		abm.SelfID = msg.Agent
		abm.Message = []message.Component{&message.Image{File: msg.PicURL, URL: msg.PicURL}}
		abm.Type = platform.FriendMessage
		abm.Sender = platform.MessageMember{UserID: msg.Source, Nickname: msg.Source}
		abm.MessageID = msg.ID
		abm.Timestamp = msg.Time
		abm.SessionID = msg.Source
		abm.RawMessage = msg
	case "voice":
		data, _, err := a.client.DownloadMedia(context.Background(), msg.MediaID)
		if err != nil {
			logger.I18nError("下载企业微信语音素材失败: %v", err)
			return
		}
		path := filepath.Join(os.TempDir(), fmt.Sprintf("wecom_%s.amr", msg.MediaID))
		if err := os.WriteFile(path, data, 0600); err != nil {
			logger.I18nError("保存企业微信语音素材失败: %v", err)
			return
		}
		// Python 此处使用 ffmpeg 将 amr 转为 wav；Go 端不内置 ffmpeg，
		// 直接保留原始 amr 文件，其余字段与 Python 一致。
		abm.MessageStr = ""
		abm.SelfID = msg.Agent
		abm.Message = []message.Component{&message.Record{File: path, URL: path}}
		abm.Type = platform.FriendMessage
		abm.Sender = platform.MessageMember{UserID: msg.Source, Nickname: msg.Source}
		abm.MessageID = msg.ID
		abm.Timestamp = msg.Time
		abm.SessionID = msg.Source
		abm.RawMessage = msg
	default:
		logger.I18nWarn("暂未实现的事件: %s", msg.Type)
		return
	}

	a.setAgentID(abm.SelfID)
	logger.I18nInfo("abm: %s", abm.MessageStr)
	a.handleMsg(abm)
}

// convertKFMessage 转换微信客服消息为 AstrBotMessage 并提交事件。
// 对应 Python convert_wechat_kf_message。
func (a *Adapter) convertKFMessage(msg map[string]interface{}) {
	msgtype, _ := msg["msgtype"].(string)
	externalUserID, _ := msg["external_userid"].(string)
	openKFID, _ := msg["open_kfid"].(string)

	abm := platform.NewAstrBotMessage()
	abm.RawMessage = msg
	abm.SelfID = openKFID
	abm.Sender = platform.MessageMember{UserID: externalUserID, Nickname: externalUserID}
	abm.SessionID = externalUserID
	abm.Type = platform.FriendMessage
	abm.MessageID = mapString(msg, "msgid")
	if abm.MessageID == "" {
		abm.MessageID = randomHex(4)
	}
	abm.MessageStr = ""
	ctx := context.Background()

	switch msgtype {
	case "text":
		text := strings.TrimSpace(mapNestedString(msg, "text", "content"))
		if a.isDuplicateKFText(abm.SessionID, text) {
			logger.Debug("忽略 15 秒内重复微信客服文本消息 session_id=%s text=%s", abm.SessionID, text)
			return
		}
		abm.Message = []message.Component{&message.Plain{Text: text}}
		abm.MessageStr = text
	case "image":
		mediaID := mapNestedString(msg, "image", "media_id")
		data, _, err := a.client.DownloadMedia(ctx, mediaID)
		if err != nil {
			logger.I18nError("下载微信客服图片素材失败: %v", err)
			return
		}
		mimeType := http.DetectContentType(data)
		suffix := mimeExt(mimeType)
		path := filepath.Join(os.TempDir(), fmt.Sprintf("weixinkefu_%s%s", mediaID, suffix))
		if err := os.WriteFile(path, data, 0600); err != nil {
			logger.I18nError("保存微信客服图片素材失败: %v", err)
			return
		}
		abm.Message = []message.Component{&message.Image{File: path, URL: path}}
	case "voice":
		mediaID := mapNestedString(msg, "voice", "media_id")
		data, _, err := a.client.DownloadMedia(ctx, mediaID)
		if err != nil {
			logger.I18nError("下载微信客服语音素材失败: %v", err)
			return
		}
		path := filepath.Join(os.TempDir(), fmt.Sprintf("weixinkefu_%s.amr", mediaID))
		if err := os.WriteFile(path, data, 0600); err != nil {
			logger.I18nError("保存微信客服语音素材失败: %v", err)
			return
		}
		// Python 此处用 ffmpeg 转 wav；Go 端保留原始 amr。
		abm.Message = []message.Component{&message.Record{File: path, URL: path}}
	case "file":
		mediaID := mapNestedString(msg, "file", "media_id")
		if mediaID == "" {
			logger.I18nWarn("微信客服文件消息缺少 media_id: %v", msg)
			return
		}
		data, header, err := a.client.DownloadMedia(ctx, mediaID)
		if err != nil {
			logger.I18nError("下载微信客服文件素材失败: %v", err)
			return
		}
		fileName := ExtractWecomMediaFilename(header.Get("Content-Disposition"))
		if fileName == "" {
			fileName = fmt.Sprintf("weixinkefu_%s.bin", mediaID)
		}
		path := filepath.Join(os.TempDir(), fmt.Sprintf("weixinkefu_%s_%s", randomHex(16), fileName))
		if err := os.WriteFile(path, data, 0600); err != nil {
			logger.I18nError("保存微信客服文件素材失败: %v", err)
			return
		}
		abm.Message = []message.Component{&message.File{Name: fileName, Path: path}}
	default:
		logger.I18nWarn("未实现的微信客服消息事件: %v", msg)
		return
	}

	a.setAgentID(abm.SelfID)
	a.handleMsg(abm)
}

// isDuplicateKFText 判断是否为 15 秒内重复的微信客服文本消息。
func (a *Adapter) isDuplicateKFText(sessionID, text string) bool {
	normalized := strings.TrimSpace(text)
	if normalized == "" {
		return false
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, expiresAt := range a.seenKFText {
		if expiresAt.Before(now) {
			delete(a.seenKFText, key)
		}
	}
	dedupKey := sessionID + ":" + normalized
	if _, ok := a.seenKFText[dedupKey]; ok {
		return true
	}
	a.seenKFText[dedupKey] = now.Add(kfTextDedupTTL)
	return false
}

// isDuplicateAppMessage 判断是否为短时间窗口内重复推送的应用消息（按 MsgId 去重，
// 微信在回调超时或异常时会重推同一条消息）。
func (a *Adapter) isDuplicateAppMessage(msg *WecomMessage) bool {
	if msg == nil || msg.ID == "" {
		return false
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, expiresAt := range a.seenAppMsg {
		if expiresAt.Before(now) {
			delete(a.seenAppMsg, key)
		}
	}
	if _, ok := a.seenAppMsg[msg.ID]; ok {
		return true
	}
	a.seenAppMsg[msg.ID] = now.Add(kfTextDedupTTL)
	return false
}

// setAgentID 记录最近一次消息对应的 AgentID（发送回复时使用）。
func (a *Adapter) setAgentID(agentID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if agentID != "" {
		a.agentID = agentID
	}
}

// getAgentID 获取最近一次消息的 AgentID。
func (a *Adapter) getAgentID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.agentID
}

// handleMsg 将 AstrBotMessage 发布为 core.Event（对应 Python commit_event）。
func (a *Adapter) handleMsg(abm *platform.AstrBotMessage) {
	if a.EventBus == nil {
		logger.I18nError("企业微信适配器尚未注入事件总线，消息被丢弃")
		return
	}
	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   "wecom",
			PlatformID: a.ID(),
			SelfID:     abm.SelfID,
			SenderID:   abm.Sender.UserID,
			SenderName: abm.Sender.Nickname,
			ConvID:     abm.SessionID,
			IsGroup:    abm.Type == platform.GroupMessage,
		},
		Message:    &message.MessageChain{Chain: abm.Message},
		MessageStr: abm.MessageStr,
		Timestamp:  time.Unix(abm.Timestamp, 0),
		MessageObj: &core.MessageObj{
			MessageID:   abm.MessageID,
			SelfID:      abm.SelfID,
			SessionID:   abm.SessionID,
			MessageType: string(abm.Type),
			Platform:    "wecom",
			MessageStr:  abm.MessageStr,
			RawMessage:  abm.RawMessage,
		},
		Metadata: map[string]interface{}{},
	}
	if err := a.EventBus.Publish(event); err != nil {
		logger.I18nError("发布企业微信事件失败: %v", err)
	}
}

// Send 发送消息到会话（对应 Python send_by_session）：
//   - 微信客服模式不支持主动发送，返回错误；
//   - 应用模式使用最近一次收到的 AgentID 作为发送方。
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	if chain == nil {
		return nil
	}
	if a.kfName != "" {
		return fmt.Errorf("企业微信客服模式不支持 send_by_session 主动发送")
	}
	agentID := a.getAgentID()
	if agentID == "" {
		return fmt.Errorf("send_by_session 失败：无法为会话 %s 推断 agent_id", sessionID)
	}
	return a.sendChain(chain, agentID, sessionID)
}

// WecomServer 独立回调服务器（/callback/command）。
type WecomServer struct {
	adapter *Adapter
	httpSrv *http.Server
}

// Start 启动独立回调服务器。
func (s *WecomServer) Start(ctx context.Context, host string, port int) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/callback/command", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			s.adapter.handleVerify(w, r)
			return
		}
		s.adapter.handleCallback(w, r)
	})
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return fmt.Errorf("企业微信回调服务器监听 %s:%d 失败: %w", host, port, err)
	}
	s.httpSrv = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", host, port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	logger.I18nInfo("企业微信回调服务器开始监听 %s:%d", host, port)
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.I18nError("企业微信回调服务器运行异常: %v", err)
		}
	}()
	return nil
}

// Shutdown 关闭回调服务器。
func (s *WecomServer) Shutdown() {
	if s.httpSrv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.httpSrv.Shutdown(ctx)
}

// ---------- 工具函数 ----------

// configString 读取字符串配置。
func configString(config map[string]interface{}, key string) string {
	if v, ok := config[key].(string); ok {
		return v
	}
	return ""
}

// configBool 读取布尔配置。
func configBool(config map[string]interface{}, key string) bool {
	if v, ok := config[key].(bool); ok {
		return v
	}
	return false
}

// configInt 读取整型配置（支持 JSON 的 float64 与字符串）。
func configInt(config map[string]interface{}, key string, def int) int {
	switch v := config[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

// mapString 读取 map 中的字符串字段。
func mapString(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// mapNestedString 读取嵌套 map 中的字符串字段。
func mapNestedString(m map[string]interface{}, outer, inner string) string {
	if sub, ok := m[outer].(map[string]interface{}); ok {
		return mapString(sub, inner)
	}
	return ""
}

// mimeExt 根据 MIME 类型返回文件后缀（默认 .jpg，对应 MEDIA_MIME_EXTENSIONS）。
func mimeExt(mimeType string) string {
	switch mimeType {
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/bmp":
		return ".bmp"
	case "image/heic":
		return ".heic"
	default:
		return ".jpg"
	}
}

// randomHex 生成 n 字节随机数的十六进制字符串。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
