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
	// callbackTimestampTolerance 回调时间戳新鲜度窗口。Python 原版完全没有
	// 该校验（wecom_adapter.py 直接解密），过严的 ±300s 会让宿主时钟偏差
	// >5 分钟的部署所有回调都 400；这里放宽到 1 小时并保留签名校验。
	callbackTimestampTolerance = time.Hour
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

	mu             sync.Mutex
	agentBySession map[string]agentBinding // 会话 -> 发送方标识（应用 agent_id / 客服 open_kfid）
	seenKFText     map[string]time.Time
	seenAppMsg     map[string]time.Time
	stopCh         chan struct{}
}

// agentBinding 记录某会话的发送方标识及其最近写入时间。用于按会话精确回复，
// 避免多会话并发时用"最近一次"的 agent_id 回复到错误的应用/客服账号。
type agentBinding struct {
	id string
	at time.Time
}

// agentBindingTTL 会话发送方绑定的存活时间：超时后淘汰，防止无界增长。
const agentBindingTTL = 24 * time.Hour

// New 构造企业微信适配器。
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		config:         config,
		settings:       settings,
		EventBus:       eventBus,
		seenKFText:     make(map[string]time.Time),
		seenAppMsg:     make(map[string]time.Time),
		agentBySession: make(map[string]agentBinding),
		stopCh:         make(chan struct{}),
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
	// 时间戳新鲜度校验：对齐 Python（原版无此校验），窗口放宽到
	// callbackTimestampTolerance 以容忍宿主时钟偏差；偏差过大时打日志而非
	// 静默拒绝。签名校验仍由 DecryptMessage 完成，防重放不依赖此项。
	if ts, err := strconv.ParseInt(timestamp, 10, 64); err != nil {
		logger.I18nWarn("企业微信回调 timestamp 非数字: %q，交由签名校验: %v", timestamp, err)
	} else if skew := time.Duration(time.Now().Unix()-ts) * time.Second; skew > callbackTimestampTolerance || skew < -callbackTimestampTolerance {
		logger.I18nWarn("企业微信回调 timestamp 偏差过大(%s)，已拒绝: ts=%d", skew, ts)
		http.Error(w, "timestamp 过期", http.StatusBadRequest)
		return
	}
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

	// 先回包 success（微信要求 5 秒内响应），耗时操作异步执行，避免媒体下载等超时被重推。
	//
	// 【有意偏离 py，且 Go 行为更正确】证据：
	//   - py wecom_adapter.py:129-154 handle_callback 在 `await self.callback(msg)`
	//     完成后才 `return "success"`，即处理完再 ack。
	//   - 但企业微信回调要求 5s 内返回，否则重试投递；Go 侧消息处理含媒体下载/
	//     LLM/转码，可能远超 5s。若照搬 py 先处理再 ack，会触发大量重复回调
	//     （应用消息虽有 MsgId 去重，客服消息仍会重复拉取）。故此处有意改为
	//     先 ack 再异步处理，配合 isDuplicateAppMessage / kf 去重保证幂等。
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, "success")

	if msg.IsKFMsgOrEvent() {
		go a.handleKFMsgOrEvent(msg)
	} else if !a.isDuplicateAppMessage(msg) {
		go a.convertMessage(msg)
	}
}

// handleKFMsgOrEvent 处理 kf_msg_or_event 回调：通过 kf/sync_msg 拉取最新客服消息。
//
// 【有意修正性偏离 Python，非等价移植，请勿按 Python 回改】
// Python 权威参考 wecom_adapter.py:221-239 的 get_latest_msg_item：
//
//	has_more = 1
//	while has_more:
//	    ret = self.wechat_kf_api.sync_msg(token, kfid)   # cursor 恒为默认 ""
//	    has_more = ret["has_more"]
//	msg_list = ret.get("msg_list", [])
//	return msg_list[-1] if msg_list else None            # 只取最后一页最后一条
//
// Python 的缺陷：循环翻页用同一 cursor 反复拉取，翻页结果被丢弃，最终只把
// 最后一页的末条消息交给 convert_wechat_kf_message，同一回调批次里除末条
// 之外的客服消息全部丢失（用户连发多条只处理最后一条）。
// Go 的实现：每轮回传上一轮的 next_cursor 正确翻页，并逐条 convertKFMessage
// 处理全部消息（adapter.go:302-327），不再丢消息。这是有意修正性偏离，
// 以“不丢客服消息”为正确基线；仅在明确需要复刻 Python 缺陷时才改动。
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
		cursorAdvanced := false
		if nc, ok := ret["next_cursor"].(string); ok && nc != "" && nc != cursor {
			cursor = nc
			cursorAdvanced = true
		}
		if hm, ok := ret["has_more"].(float64); ok {
			hasMore = int(hm) != 0 && cursorAdvanced
		} else {
			hasMore = false
		}
		if hm, ok := ret["has_more"].(float64); ok && int(hm) == 1 && !cursorAdvanced {
			logger.I18nWarn("kf/sync_msg has_more=1 但游标未推进，终止同步")
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
		// 对齐 Python：amr 语音经 ffmpeg 转为 wav（MediaResolver
		// target_format="wav"），供下游语音识别；转码产物为内存数据后
		// 落盘为 .wav，其余字段与 Python 一致。
		// 对齐 Python wecom_adapter.py:396-404：转码失败时记录错误并丢弃
		// 该语音消息（不投递原 amr），否则 ASR 会对 amr 产生行为漂移。
		wavData, ok := convertAudioToWav(data)
		if !ok {
			logger.I18nError("转换企业微信语音失败，已丢弃该语音消息。如果没有安装 ffmpeg 请先安装。")
			_ = os.Remove(path)
			return
		}
		wavPath := path + ".wav"
		if err := os.WriteFile(wavPath, wavData, 0600); err != nil {
			logger.I18nError("保存企业微信语音 wav 失败: %v", err)
			return
		}
		// 转码成功后清理原始 amr 临时文件（对齐本体临时目录保留 wav 即可）。
		_ = os.Remove(path)
		abm.MessageStr = ""
		abm.SelfID = msg.Agent
		abm.Message = []message.Component{&message.Record{File: wavPath, URL: wavPath}}
		abm.Type = platform.FriendMessage
		abm.Sender = platform.MessageMember{UserID: msg.Source, Nickname: msg.Source}
		abm.MessageID = msg.ID
		abm.Timestamp = msg.Time
		abm.SessionID = msg.Source
		abm.RawMessage = msg
	default:
		// py 同行为：Python wecom_adapter.py:419-421 同样只处理 text/image/voice，
		// 其它类型仅告警并丢弃（无对应实现）。
		logger.I18nWarn("暂未实现的事件: %s", msg.Type)
		return
	}

	// 按会话记录发送方标识（应用模式为 agent_id），而非全局"最近一次"。
	a.setAgentID(abm.SessionID, abm.SelfID)
	logger.I18nInfo("abm: %s", abm.MessageStr)
	a.handleMsg(abm)
}

// convertKFMessage 转换微信客服消息为 AstrBotMessage 并提交事件。
// 对应 Python convert_wechat_kf_message。
func (a *Adapter) convertKFMessage(msg map[string]interface{}) {
	msgtype, _ := msg["msgtype"].(string)
	externalUserID, _ := msg["external_userid"].(string)
	// open_kfid 仅用于客服回复路由（kf/send_msg 必填字段）；同步消息缺失/为空时
	// 降级处理：仅告警并跳过客服回复路由，消息仍正常进入消息管线（对齐 4.27.4）。
	openKFID, _ := msg["open_kfid"].(string)
	if openKFID == "" {
		if v, ok := msg["OpenKfId"].(string); ok {
			openKFID = v
		}
	}
	if openKFID == "" {
		logger.I18nWarn("微信客服同步消息缺少 open_kfid，已跳过客服回复路由，消息仍将正常进入消息管线。external_userid=%s", externalUserID)
	}

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
		// 对齐 Python：amr 语音经 ffmpeg 转 wav 后投递（MediaResolver
		// target_format="wav"）。对齐 wecom_adapter.py:477-484 的 try/except：
		// 转码失败时记录错误并丢弃该语音消息（不投递原 amr）。
		wavData, ok := convertAudioToWav(data)
		if !ok {
			logger.I18nError("转换微信客服语音失败，已丢弃该语音消息。如果没有安装 ffmpeg 请先安装。")
			_ = os.Remove(path)
			return
		}
		wavPath := path + ".wav"
		if err := os.WriteFile(wavPath, wavData, 0600); err != nil {
			logger.I18nError("保存微信客服语音 wav 失败: %v", err)
			return
		}
		_ = os.Remove(path)
		abm.Message = []message.Component{&message.Record{File: wavPath, URL: wavPath}}
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
		// py 同行为：Python wecom_adapter.py:513-515 的微信客服同样只处理
		// text/image/voice/file，其它 msgtype 仅告警并丢弃。
		logger.I18nWarn("未实现的微信客服消息事件: %v", msg)
		return
	}

	// 客服模式按会话记录 open_kfid（abm.SelfID），供回复路由使用。
	a.setAgentID(abm.SessionID, abm.SelfID)
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
//
// 【有意偏离 py，且 Go 行为更正确】证据：
//   - py wecom_adapter.py:415 对应用消息只设置 `abm.message_id = str(msg.id)`，
//     全文没有按 msg.id/MsgId 去重；去重仅针对客服文本（:441
//     `_is_duplicate_wechat_kf_text_message`，按 session_id+文本 15s 窗口）。
//   - 企业微信应用消息回调在应用未及时返回 success 时会重试投递同一条消息。
//     若按 py 不去重，重推会触发重复的 LLM 调用与重复回复。Go 侧按 MsgId
//     做 15s 窗口去重可避免该问题，故有意保留并偏离 py。
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

// setAgentID 记录某会话对应的发送方标识（应用模式为 agent_id，客服模式为
// open_kfid）。按会话存储而非全局"最近一次"，避免并发会话互相串号。
func (a *Adapter) setAgentID(sessionID, agentID string) {
	if sessionID == "" || agentID == "" {
		return
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, b := range a.agentBySession {
		if now.Sub(b.at) > agentBindingTTL {
			delete(a.agentBySession, key)
		}
	}
	a.agentBySession[sessionID] = agentBinding{id: agentID, at: now}
}

// getAgentID 获取指定会话的发送方标识，未命中或已过期返回空串。
func (a *Adapter) getAgentID(sessionID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.agentBySession[sessionID]
	if !ok || time.Since(b.at) > agentBindingTTL {
		return ""
	}
	return b.id
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

// Send 发送消息到会话（对应 Python send_by_session + 事件 send 的统一入口，
// 管线回复 RespondStage 与主动发送均经由本方法）：
//   - 微信客服模式：带会话上下文的发送（含回复）走 kf/send_msg，接收者为
//     会话 external_userid，发送方为最近一条客服消息的 open_kfid（对齐
//     Python wecom_event.send 的客服回复路径）；仅禁止无会话上下文的主动
//     群发，对齐 Python send_by_session 的限制语义（wecom_adapter.py:273-276）；
//   - 应用模式使用最近一次收到的 AgentID 作为发送方。
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	if chain == nil {
		return nil
	}
	if a.kfName != "" {
		// 对齐 Python：客服模式仅禁止无会话上下文的主动发送；管线回复携带
		// 会话 external_userid，必须走客服消息 API 回复，否则客服收到的
		// 消息将永远无法得到回应。
		if sessionID == "" {
			return fmt.Errorf("企业微信客服模式不支持无会话上下文的主动发送")
		}
		openKFID := a.getAgentID(sessionID)
		if openKFID == "" {
			return fmt.Errorf("send_by_session 失败：无法为会话 %s 推断 open_kfid", sessionID)
		}
		return a.sendChain(chain, openKFID, sessionID)
	}
	agentID := a.getAgentID(sessionID)
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
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
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
