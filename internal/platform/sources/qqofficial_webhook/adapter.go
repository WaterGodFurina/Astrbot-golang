// Package qqofficial_webhook implements the QQ Official Bot Webhook platform
// adapter（QQ 官方开放平台 webhook 回调模式）。
// 1:1 移植自 astrbot/core/platform/sources/qqofficial_webhook/（Python）：
//   - Webhook 回调处理（签名校验/验证请求/事件分发/去重）对齐 qo_webhook_server.py
//   - 消息解析复用 qqofficial 的 _parse_from_qqofficial 逻辑
//   - 支持统一 Webhook 模式（WebhookPlatform 接口）与独立 HTTP 服务器两种模式
package qqofficial_webhook

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

var logger = log.GetDefault().WithComponent("QQOfficialWebhook")

// QQ 官方开放平台常量
const (
	// wsDispatchEvent Webhook 载荷中事件分发的 opcode（对齐 botpy 的 WS_DISPATCH_EVENT）
	wsDispatchEvent = 0
	// wsValidation 验证请求的 opcode（URL 验证）
	wsValidation = 13
	// apiDomain QQ 开放平台 API 域名
	apiDomain = "https://api.sgroup.qq.com"
	// tokenEndpoint 获取 access_token 的接口
	tokenEndpoint = "https://bots.qq.com/app/getAppAccessToken"
	// 媒体文件类型（对齐 qqofficial）
	fileTypeImage = 1
	fileTypeVideo = 2
	fileTypeVoice = 3
	fileTypeFile  = 4
	// dedupTTL 事件去重窗口（秒，对齐 Python _dedup_ttl = 60）
	dedupTTL = 60 * time.Second
	// webhookPath 独立服务器的回调路径（对齐 Python 的 /astrbot-qo-webhook/callback）
	webhookPath = "/astrbot-qo-webhook/callback"
	// defaultPort 默认回调端口（对齐 Python port = 6196）
	defaultPort = 6196
	// validationWindow 平台 URL 验证窗口：适配器启动后仅在该时长内接受 op=13
	// 验证请求（平台完成 URL 验证后不会再发送 op=13）。
	validationWindow = 10 * time.Minute
	// validationEventTSSkew op=13 验证请求 event_ts 与当前时间允许的最大偏差
	validationEventTSSkew = 5 * time.Minute
	// validationMaxRatePerMin op=13 验证端点每分钟允许的最大请求数（限速）
	validationMaxRatePerMin = 5
	// signatureTimestampSkew 正常事件回调 X-Signature-Timestamp 与当前时间允许的最大偏差
	signatureTimestampSkew = 5 * time.Minute
)

// Adapter 是 QQ 官方 webhook 平台适配器。
type Adapter struct {
	platform.BaseAdapter
	config   map[string]interface{}
	settings map[string]interface{}

	// EventBus 由 lifecycle 通过 SetEventBus 注入
	EventBus platform.EventBus

	appid              string
	secret             string
	unifiedWebhookMode bool   // 是否使用统一 webhook 入口
	webhookUUID        string // 统一 webhook 的 uuid
	port               int
	isSandbox          bool
	callbackServerHost string

	mu             sync.Mutex
	sessionScene   map[string]string                 // convID -> group/friend/channel
	sessionLastMsg map[string]string                 // convID -> 最后一条消息 id
	extraDataCache map[string]map[string]interface{} // message_id -> 载荷附加字段
	seenEventIDs   map[string]time.Time              // 事件去重缓存
	accessToken    string
	tokenExpiresAt time.Time
	server         *http.Server
	httpClient     *http.Client
	stopCh         chan struct{}
	started        bool
	startedAt      time.Time   // 适配器启动时间（op=13 验证窗口起点）
	validationTS   []time.Time // op=13 请求时间戳（滑动窗口限速）
}

// New 根据平台配置创建 QQ 官方 webhook 适配器。
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		BaseAdapter:        *platform.NewBaseAdapter(configID(config), "qq_official_webhook"),
		config:             config,
		settings:           settings,
		sessionScene:       make(map[string]string),
		sessionLastMsg:     make(map[string]string),
		extraDataCache:     make(map[string]map[string]interface{}),
		seenEventIDs:       make(map[string]time.Time),
		httpClient:         &http.Client{Timeout: 30 * time.Second},
		stopCh:             make(chan struct{}),
		startedAt:          time.Now(),
		port:               defaultPort,
		callbackServerHost: "0.0.0.0",
	}
	a.appid, _ = config["appid"].(string)
	a.secret, _ = config["secret"].(string)
	if v, ok := config["unified_webhook_mode"].(bool); ok {
		a.unifiedWebhookMode = v
	}
	a.webhookUUID, _ = config["webhook_uuid"].(string)
	if v, ok := configInt(config, "port"); ok && v > 0 {
		a.port = v
	}
	if v, ok := config["is_sandbox"].(bool); ok {
		a.isSandbox = v
	}
	if v, ok := config["callback_server_host"].(string); ok && v != "" {
		a.callbackServerHost = v
	}
	if eventBus != nil {
		a.SetEventBus(eventBus)
	}
	return a
}

func configID(config map[string]interface{}) string {
	if id, ok := config["id"].(string); ok && id != "" {
		return id
	}
	return "qq_official_webhook"
}

// configInt 读取整数配置（JSON 数字可能以 float64 或 int 形式出现）。
func configInt(config map[string]interface{}, key string) (int, bool) {
	switch v := config[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	case string:
		n := 0
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

// SetEventBus 注入事件总线（实现 platform.EventBusSetter）。
func (a *Adapter) SetEventBus(bus platform.EventBus) {
	a.EventBus = bus
	a.BaseAdapter.SetEventBus(bus)
}

// ID 返回平台实例 id。
func (a *Adapter) ID() string { return a.BaseAdapter.ID() }

// Type 返回平台类型名。
func (a *Adapter) Type() string { return "qq_official_webhook" }

// Start 启动适配器：
//   - 统一 webhook 模式：不启动独立服务器，等待 dashboard 注册统一回调入口
//   - 独立服务器模式：监听 callback_server_host:port 上的 /astrbot-qo-webhook/callback
func (a *Adapter) Start(ctx context.Context) error {
	a.mu.Lock()
	a.started = true
	a.startedAt = time.Now()
	a.mu.Unlock()

	if a.unifiedWebhookMode {
		if a.webhookUUID != "" {
			logger.I18nInfo("%s(QQ 官方机器人 Webhook) 已启用统一 Webhook 模式, webhook_uuid=%s", a.ID(), a.webhookUUID)
		} else {
			logger.I18nWarn("QQ 官方 Webhook 已启用统一 Webhook 模式，但未配置 webhook_uuid")
		}
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc(webhookPath, a.WebhookCallback)
	a.server = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", a.callbackServerHost, a.port),
		Handler: mux,
	}
	go func() {
		if err := a.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.I18nError("QQ 官方 webhook 服务器启动失败: %v", err)
		}
	}()
	logger.I18nInfo("将在 %s:%d 端口启动 QQ 官方机器人 webhook 适配器", a.callbackServerHost, a.port)
	return nil
}

// Stop 关闭适配器。
func (a *Adapter) Stop() error {
	a.mu.Lock()
	a.started = false
	server := a.server
	a.mu.Unlock()
	if server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			logger.I18nWarn("QQ 官方 webhook 服务器关闭异常: %v", err)
		}
	}
	select {
	case <-a.stopCh:
	default:
		close(a.stopCh)
	}
	logger.I18nInfo("QQ 官方机器人 Webhook 适配器已经被关闭")
	return nil
}

// ---------------------------------------------------------------------------
// Webhook 回调（可被统一 webhook 入口复用）
// ---------------------------------------------------------------------------

// WebhookUUID 返回统一 webhook 的 uuid（实现 platform.WebhookPlatform）。
func (a *Adapter) WebhookUUID() string {
	return a.webhookUUID
}

// WebhookCallback 是统一 webhook 的回调入口（实现 platform.WebhookPlatform）。
func (a *Adapter) WebhookCallback(w http.ResponseWriter, r *http.Request) {
	a.handleCallback(w, r)
}

// handleCallback 处理 webhook 回调（对齐 Python qo_webhook_server.handle_callback）。
func (a *Adapter) handleCallback(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeWebhookResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}

	var msg map[string]interface{}
	if err := json.Unmarshal(body, &msg); err != nil || msg == nil {
		logger.I18nWarn("qq_official_webhook callback body is not valid JSON.")
		writeWebhookResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}

	logger.Debug("收到 qq_official_webhook 回调: %s", string(body))

	event, _ := msg["t"].(string)
	opcode, _ := msg["op"].(float64)
	data, _ := msg["d"].(map[string]interface{})

	// URL 验证请求（opcode == 13）：返回对 plain_token 的签名。
	// 官方协议中该验证请求不携带 X-Signature-* 签名头，无法像事件回调那样
	// 验签，故仅在平台"尚未完成验证"的窗口期内接受，并对 event_ts 做新鲜度
	// 校验与端点限速，防止验证端点被用作签名预言机。
	if int(opcode) == wsValidation {
		if !a.allowValidation() {
			logger.I18nWarn("qq_official_webhook validation request rejected (outside window or rate limited).")
			writeWebhookResponse(w, http.StatusUnauthorized, map[string]interface{}{"error": "Invalid validation request"})
			return
		}
		signed, ok := a.webhookValidation(data)
		if !ok {
			logger.I18nWarn("qq_official_webhook validation request rejected (invalid event_ts).")
			writeWebhookResponse(w, http.StatusUnauthorized, map[string]interface{}{"error": "Invalid validation request"})
			return
		}
		logger.Debug("webhook validation response: %s", toJSON(signed))
		writeWebhookResponse(w, http.StatusOK, signed)
		return
	}

	// 时间戳新鲜度校验：拒绝时间戳与当前时间偏差超过 5 分钟的请求（防重放）。
	if !isFreshTimestamp(r.Header.Get(signatureTimestampHeader), signatureTimestampSkew) {
		logger.I18nWarn("qq_official_webhook callback timestamp is invalid or stale.")
		writeWebhookResponse(w, http.StatusUnauthorized, map[string]interface{}{"error": "Invalid timestamp"})
		return
	}

	// 签名校验
	if !verifyQQWebhookSignature(
		a.secret,
		r.Header.Get(signatureTimestampHeader),
		r.Header.Get(signatureHeader),
		body,
	) {
		logger.I18nWarn("qq_official_webhook signature verification failed.")
		writeWebhookResponse(w, http.StatusUnauthorized, map[string]interface{}{"error": "Invalid signature"})
		return
	}

	// 事件去重（重试回调可能在短时间内重复推送）
	eventID, _ := msg["id"].(string)
	if eventID != "" {
		if !a.markSeenEvent(eventID) {
			logger.Debug("Duplicate webhook event %q, skipping.", eventID)
			writeWebhookResponse(w, http.StatusOK, map[string]interface{}{"opcode": 12})
			return
		}
	}

	if event != "" && int(opcode) == wsDispatchEvent {
		event = strings.ToLower(event)

		// 在 botpy 解析前提取载荷中的附加字段（union_openid / message_scene）
		if data != nil {
			msgID, _ := data["id"].(string)
			if msgID != "" {
				extra := map[string]interface{}{}
				if author, ok := data["author"].(map[string]interface{}); ok {
					if unionOpenID, ok := author["union_openid"].(string); ok && unionOpenID != "" {
						extra["union_openid"] = unionOpenID
					}
				}
				if messageScene, ok := data["message_scene"].(string); ok && messageScene != "" {
					extra["message_scene"] = messageScene
				}
				if len(extra) > 0 {
					a.storeExtraData(msgID, extra)
				}
			}
		}

		// 事件分发（对齐 botpy 的 parser dispatch）
		switch event {
		case "group_at_message_create":
			a.onGroupAtMessageCreate(data)
		case "group_message_create":
			a.onGroupMessageCreate(data)
		case "at_message_create":
			a.onAtMessageCreate(data)
		case "c2c_message_create":
			a.onC2CMessageCreate(data)
		case "direct_message_create":
			a.onDirectMessageCreate(data)
		case "message_create":
			// botpy Client.on_message_create 为空实现，忽略
			logger.Debug("qq_official_webhook 忽略频道消息 message_create")
		default:
			logger.I18nError("qq_official_webhook 未知事件 %s", event)
			if data != nil {
				a.popExtraData(strOf(data["id"]))
			}
		}
	}

	writeWebhookResponse(w, http.StatusOK, map[string]interface{}{"opcode": 12})
}

// isFreshTimestamp 判断回调时间戳是否为 Unix 秒且与当前时间偏差不超过 maxSkew。
func isFreshTimestamp(timestamp string, maxSkew time.Duration) bool {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || ts <= 0 {
		return false
	}
	return time.Since(time.Unix(ts, 0)).Abs() <= maxSkew
}

// webhookValidation 处理 QQ 官方的 URL 验证请求
// （对齐 Python webhook_validation：签名 event_ts + plain_token）。
// event_ts 必须为近期数字时间戳（±validationEventTSSkew 内），否则返回失败。
func (a *Adapter) webhookValidation(validationPayload map[string]interface{}) (map[string]interface{}, bool) {
	eventTS := strOf(validationPayload["event_ts"])
	ts, err := strconv.ParseInt(eventTS, 10, 64)
	if err != nil || ts <= 0 || time.Since(time.Unix(ts, 0)).Abs() > validationEventTSSkew {
		return nil, false
	}
	msg := eventTS + strOf(validationPayload["plain_token"])
	seed, err := buildEd25519Seed(a.secret)
	if err != nil {
		logger.I18nError("webhook 验证签名失败: %v", err)
		return nil, false
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	signature := hex.EncodeToString(ed25519.Sign(privateKey, []byte(msg)))
	return map[string]interface{}{
		"plain_token": validationPayload["plain_token"],
		"signature":   signature,
	}, true
}

// allowValidation 判断当前是否允许接受 op=13 URL 验证请求：
// 仅当适配器处于启动后的验证窗口期内，且请求频率未超过限速阈值。
func (a *Adapter) allowValidation() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.startedAt.IsZero() || time.Since(a.startedAt) > validationWindow {
		return false
	}
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	kept := a.validationTS[:0]
	for _, t := range a.validationTS {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	a.validationTS = kept
	if len(a.validationTS) >= validationMaxRatePerMin {
		return false
	}
	a.validationTS = append(a.validationTS, now)
	return true
}

// markSeenEvent 记录并检查事件 id（60 秒 TTL，惰性淘汰）。
func (a *Adapter) markSeenEvent(eventID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for id, ts := range a.seenEventIDs {
		if now.Sub(ts) > dedupTTL {
			delete(a.seenEventIDs, id)
		}
	}
	if _, seen := a.seenEventIDs[eventID]; seen {
		return false
	}
	a.seenEventIDs[eventID] = now
	return true
}

// storeExtraData 缓存载荷附加字段（按消息 id）。
func (a *Adapter) storeExtraData(messageID string, extra map[string]interface{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.extraDataCache[messageID] = extra
}

// popExtraData 取出并删除消息的附加字段（对齐 Python pop_extra_data）。
func (a *Adapter) popExtraData(messageID string) (map[string]interface{}, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	extra, ok := a.extraDataCache[messageID]
	if ok {
		delete(a.extraDataCache, messageID)
	}
	return extra, ok
}

// ---------------------------------------------------------------------------
// 消息处理（对齐 qo_webhook_adapter.py 的 botClient 事件回调）
// ---------------------------------------------------------------------------

// onGroupAtMessageCreate 收到群 @ 消息。
func (a *Adapter) onGroupAtMessageCreate(d map[string]interface{}) {
	abm := parseFromQQOfficial(d, platform.GroupMessage, kindGroup, true)
	groupOpenID, _ := d["group_openid"].(string)
	abm.Group = &platform.Group{GroupID: groupOpenID}
	abm.SessionID = groupOpenID
	a.rememberSessionScene(abm.SessionID, "group")
	a.commit(abm)
}

// onGroupMessageCreate 收到群消息。
func (a *Adapter) onGroupMessageCreate(d map[string]interface{}) {
	abm := parseFromQQOfficial(d, platform.GroupMessage, kindGroup, false)
	groupOpenID, _ := d["group_openid"].(string)
	abm.Group = &platform.Group{GroupID: groupOpenID}
	abm.SessionID = groupOpenID
	a.rememberSessionScene(abm.SessionID, "group")
	a.commit(abm)
}

// onAtMessageCreate 收到频道 @ 消息。
func (a *Adapter) onAtMessageCreate(d map[string]interface{}) {
	abm := parseFromQQOfficial(d, platform.GroupMessage, kindChannel, false)
	channelID, _ := d["channel_id"].(string)
	abm.Group = &platform.Group{GroupID: channelID}
	abm.SessionID = channelID
	a.rememberSessionScene(abm.SessionID, "channel")
	a.commit(abm)
}

// onC2CMessageCreate 收到 C2C 单聊消息。
func (a *Adapter) onC2CMessageCreate(d map[string]interface{}) {
	abm := parseFromQQOfficial(d, platform.FriendMessage, kindC2C, false)
	abm.SessionID = abm.Sender.UserID
	a.rememberSessionScene(abm.SessionID, "friend")
	a.commit(abm)
}

// onDirectMessageCreate 收到频道私聊消息。
func (a *Adapter) onDirectMessageCreate(d map[string]interface{}) {
	abm := parseFromQQOfficial(d, platform.FriendMessage, kindDirect, false)
	abm.SessionID = abm.Sender.UserID
	a.rememberSessionScene(abm.SessionID, "friend")
	a.commit(abm)
}

// rememberSessionMessageID 记录会话最后一条消息 id（对齐 Python）。
func (a *Adapter) rememberSessionMessageID(sessionID, messageID string) {
	if sessionID == "" || messageID == "" {
		return
	}
	a.mu.Lock()
	a.sessionLastMsg[sessionID] = messageID
	a.mu.Unlock()
}

// rememberSessionScene 记录会话场景（对齐 Python）。
func (a *Adapter) rememberSessionScene(sessionID, scene string) {
	if sessionID == "" || scene == "" {
		return
	}
	a.mu.Lock()
	a.sessionScene[sessionID] = scene
	a.mu.Unlock()
}

// commit 将消息发布到事件总线（对齐 Python 的 _commit + create_event：
// 附加字段随消息 id 注入事件 Metadata）。
func (a *Adapter) commit(abm *platform.AstrBotMessage) {
	a.rememberSessionMessageID(abm.SessionID, abm.MessageID)

	if a.EventBus == nil {
		logger.I18nWarn("QQ 官方 Webhook 事件总线未注入，丢弃消息")
		return
	}

	isGroup := abm.Type == platform.GroupMessage
	event := &core.Event{
		Type:       core.EventMessage,
		Message:    &message.MessageChain{Chain: abm.Message},
		MessageStr: abm.MessageStr,
		MessageObj: &core.MessageObj{
			MessageID:   abm.MessageID,
			SelfID:      abm.SelfID,
			SessionID:   abm.SessionID,
			MessageType: string(abm.Type),
			Platform:    a.Type(),
			MessageStr:  abm.MessageStr,
			RawMessage:  abm.RawMessage,
		},
		Source: core.EventSource{
			Platform:   a.Type(),
			SelfID:     abm.SelfID,
			SenderID:   abm.Sender.UserID,
			SenderName: abm.Sender.Nickname,
			ConvID:     abm.SessionID,
			IsGroup:    isGroup,
		},
		Timestamp: time.Now(),
		Metadata:  map[string]interface{}{},
	}
	// webhook 载荷附加字段注入（union_openid / message_scene）
	if extra, ok := a.popExtraData(abm.MessageID); ok {
		for k, v := range extra {
			event.Metadata[k] = v
		}
	}
	if err := a.EventBus.Publish(event); err != nil {
		logger.Error("Failed to publish event: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 发送（复用 qqofficial 的 REST 接口）
// ---------------------------------------------------------------------------

// Send 向指定会话发送消息链（对齐 Python _send_by_session_common 与 qqofficial）。
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	if chain == nil || len(chain.Chain) == 0 {
		return nil
	}
	a.mu.Lock()
	scene := a.sessionScene[sessionID]
	lastMsgID := a.sessionLastMsg[sessionID]
	a.mu.Unlock()

	plainText, imageRef, fileRef, fileName, fileType := extractSendParts(chain)

	switch scene {
	case "friend":
		return a.sendC2C(sessionID, plainText, imageRef, fileRef, fileName, fileType, lastMsgID)
	case "group":
		return a.sendGroup(sessionID, plainText, imageRef, fileRef, fileName, fileType, lastMsgID)
	case "channel":
		return a.sendChannel(sessionID, plainText, imageRef)
	default:
		// 兜底：按 C2C（单聊）发送
		return a.sendC2C(sessionID, plainText, imageRef, fileRef, fileName, fileType, lastMsgID)
	}
}

// extractSendParts 从消息链中提取文本与媒体引用（对齐 qqofficial）。
// fileType 保留媒体组件类型（Record→语音、Video→视频、File→文件），
// 避免仅凭 fileName 区分而把语音误判为视频。
func extractSendParts(chain *message.MessageChain) (plain string, imageRef string, fileRef string, fileName string, fileType int) {
	fileType = fileTypeFile
	for _, c := range chain.Chain {
		switch comp := c.(type) {
		case *message.Plain:
			plain += comp.Text
		case *message.Image:
			if comp.Base64 != "" {
				imageRef = comp.Base64
			} else if comp.Path != "" {
				imageRef = readFileBase64(comp.Path)
			} else if comp.File != "" {
				imageRef = readFileBase64(comp.File)
			} else if comp.URL != "" {
				imageRef = comp.URL
			}
		case *message.File:
			if fileRef == "" {
				if comp.Path != "" {
					fileRef = comp.Path
				} else {
					fileRef = comp.URL
				}
				fileName = comp.Name
				fileType = fileTypeFile
			}
		case *message.Video:
			if fileRef == "" {
				if comp.Path != "" {
					fileRef = comp.Path
				} else {
					fileRef = comp.URL
				}
				fileType = fileTypeVideo
			}
		case *message.Record:
			if fileRef == "" {
				if comp.Path != "" {
					fileRef = comp.Path
				} else {
					fileRef = comp.URL
				}
				fileType = fileTypeVoice
			}
		}
	}
	return plain, imageRef, fileRef, fileName, fileType
}

func readFileBase64(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(data)
}

// fetchAccessToken 用 appid/secret 换取 access_token（对齐 qqofficial）。
func (a *Adapter) fetchAccessToken() (string, error) {
	body, _ := json.Marshal(map[string]string{
		"appId":        a.appid,
		"clientSecret": a.secret,
	})
	resp, err := a.httpClient.Post(tokenEndpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("获取 access_token 失败: %v", err)
	}
	defer resp.Body.Close()
	dataBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var data map[string]interface{}
	if err := json.Unmarshal(dataBytes, &data); err != nil {
		return "", fmt.Errorf("获取 access_token 响应解析失败: %v", err)
	}
	token, _ := data["access_token"].(string)
	expiresIn, _ := data["expires_in"].(float64)
	if token == "" {
		return "", fmt.Errorf("获取 access_token 失败，请检查 appid/secret 是否正确: %s", string(dataBytes))
	}
	a.mu.Lock()
	a.accessToken = token
	a.tokenExpiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
	a.mu.Unlock()
	return token, nil
}

// getAccessToken 返回有效的 access_token（过期时自动刷新）。
func (a *Adapter) getAccessToken() (string, error) {
	a.mu.Lock()
	token := a.accessToken
	expires := a.tokenExpiresAt
	a.mu.Unlock()
	if token == "" || time.Now().After(expires) {
		return a.fetchAccessToken()
	}
	return token, nil
}

// apiRequest 发起带鉴权的 QQ 开放平台 API 请求（对齐 qqofficial）。
func (a *Adapter) apiRequest(method, path string, payload map[string]interface{}) (map[string]interface{}, error) {
	token, err := a.getAccessToken()
	if err != nil {
		return nil, err
	}
	var body io.Reader
	if payload != nil {
		data, _ := json.Marshal(payload)
		body = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, apiDomain+path, body)
	req.Header.Set("Authorization", "QQBot "+token)
	req.Header.Set("X-Union-Appid", a.appid)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	dataBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	var data map[string]interface{}
	if len(dataBytes) > 0 {
		_ = json.Unmarshal(dataBytes, &data)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := data["message"].(string)
		if msg == "" {
			msg = string(dataBytes)
		}
		return nil, fmt.Errorf("QQ 接口错误 (%d): %s", resp.StatusCode, msg)
	}
	return data, nil
}

// uploadFile 上传媒体文件（对齐 qqofficial 的 C2C/群上传接口）。
func (a *Adapter) uploadFile(kind, targetID string, fileData string, fileType int, fileName string) (map[string]interface{}, error) {
	var path string
	payload := map[string]interface{}{
		"file_type":    fileType,
		"srv_send_msg": false,
	}
	if fileName != "" {
		payload["file_name"] = fileName
	}
	if strings.HasPrefix(fileData, "data:") {
		parts := strings.SplitN(fileData, ",", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("无效的 data: URI 文件数据")
		}
		fileData = parts[1]
	}
	if kind == "friend" {
		payload["openid"] = targetID
		path = "/v2/users/" + targetID + "/files"
	} else {
		payload["group_openid"] = targetID
		path = "/v2/groups/" + targetID + "/files"
	}
	if strings.HasPrefix(fileData, "http://") || strings.HasPrefix(fileData, "https://") {
		payload["url"] = fileData
	} else {
		payload["file_data"] = fileData
	}
	return a.apiRequest(http.MethodPost, path, payload)
}

// sendC2C 向 C2C 用户发送消息。
func (a *Adapter) sendC2C(openID, plainText, imageRef, fileRef, fileName string, fileType int, msgID string) error {
	payload := map[string]interface{}{"content": plainText}
	payload["msg_seq"] = rand.Intn(10000) + 1
	if msgID != "" {
		payload["msg_id"] = msgID
	}
	if imageRef != "" {
		media, err := a.uploadFile("friend", openID, imageRef, fileTypeImage, "")
		if err != nil {
			return err
		}
		payload["media"] = media
		payload["msg_type"] = 7
	} else if fileRef != "" {
		media, err := a.uploadFile("friend", openID, fileRef, fileType, fileName)
		if err != nil {
			return err
		}
		payload["media"] = media
		payload["msg_type"] = 7
	}
	return a.postMessage("/v2/users/"+openID+"/messages", payload)
}

// sendGroup 向群发送消息。
func (a *Adapter) sendGroup(groupOpenID, plainText, imageRef, fileRef, fileName string, fileType int, msgID string) error {
	payload := map[string]interface{}{"content": plainText}
	if msgID != "" {
		payload["msg_id"] = msgID
	}
	payload["msg_seq"] = rand.Intn(10000) + 1
	if imageRef != "" {
		media, err := a.uploadFile("group", groupOpenID, imageRef, fileTypeImage, "")
		if err != nil {
			return err
		}
		payload["media"] = media
		payload["msg_type"] = 7
	} else if fileRef != "" {
		media, err := a.uploadFile("group", groupOpenID, fileRef, fileType, fileName)
		if err != nil {
			return err
		}
		payload["media"] = media
		payload["msg_type"] = 7
	}
	return a.postMessage("/v2/groups/"+groupOpenID+"/messages", payload)
}

// sendChannel 向频道发送消息。
func (a *Adapter) sendChannel(channelID, plainText, imageRef string) error {
	payload := map[string]interface{}{"content": plainText}
	if imageRef != "" {
		payload["file_image"] = imageRef
	}
	return a.postMessage("/channels/"+channelID+"/messages", payload)
}

func (a *Adapter) postMessage(path string, payload map[string]interface{}) error {
	_, err := a.apiRequest(http.MethodPost, path, payload)
	return err
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// writeWebhookResponse 写出 JSON 响应。
func writeWebhookResponse(w http.ResponseWriter, code int, data map[string]interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}

// toJSON 序列化 map 为 JSON 字符串（仅用于日志）。
func toJSON(v interface{}) string {
	data, _ := json.Marshal(v)
	return string(data)
}
