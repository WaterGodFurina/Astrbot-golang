package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/internal/utils"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
	"github.com/gorilla/websocket"
)

var logger = log.GetDefault().WithComponent("DingTalk")

// 钉钉会话 id 前缀 (对应 Python dingtalk_adapter.py 的 _id_to_sid: prefix = "$:LWCP_v1:$")。
const dingtalkIDPrefix = "$:LWCP_v1:$"

// accessTokenCache 缓存钉钉 access_token (对应 Python 中 SDK 的 token 缓存)。
type accessTokenCache struct {
	mu       sync.Mutex
	token    string
	expireAt time.Time
}

// Adapter 实现钉钉机器人官方 API 适配器。
// 对应 Python dingtalk_adapter.py 的 DingtalkPlatformAdapter。
type Adapter struct {
	config   map[string]interface{}
	settings map[string]interface{}

	EventBus *core.EventBus

	clientID     string
	clientSecret string

	httpClient *http.Client
	tokenCache accessTokenCache

	// Stream 长连接
	wsMu   sync.Mutex
	wsConn *websocket.Conn

	// 运行控制
	running atomic.Bool
	ctx     context.Context
	cancel  context.CancelFunc

	// 记录群聊的 openConversationId, 用于发送时区分群聊/私聊
	convMu      sync.Mutex
	knownGroups map[string]bool

	// 私聊会话的 senderId -> staffId 映射 (对应 Python sp.put_async/sp.get_async)
	staffMu    sync.Mutex
	staffIDMap map[string]string

	// 回调消息串行处理队列 (先 ack 再异步处理, 避免下载/转码阻塞 WS 读循环)
	msgCh chan map[string]interface{}

	// 会话映射持久化目录 (staffIDMap/knownGroups 落盘 JSON, 重启后恢复)
	dataDir string
}

// dingtalkState 会话映射的持久化结构。
type dingtalkState struct {
	StaffIDMap  map[string]string `json:"staff_id_map"`
	KnownGroups []string          `json:"known_groups"`
}

// New 创建钉钉适配器 (对应 Python __init__)。
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		config:      config,
		settings:    settings,
		EventBus:    eventBus,
		httpClient:  &http.Client{Timeout: 60 * time.Second},
		knownGroups: make(map[string]bool),
		staffIDMap:  make(map[string]string),
	}
	a.clientID, _ = config["client_id"].(string)
	a.clientSecret, _ = config["client_secret"].(string)
	a.dataDir, _ = config["dingtalk_data_dir"].(string)
	if a.dataDir == "" {
		wd, _ := os.Getwd()
		a.dataDir = filepath.Join(wd, "data", "dingtalk")
	}
	a.loadState()
	return a
}

// SetEventBus 注入事件总线 (实现 platform.EventBusSetter)。
func (a *Adapter) SetEventBus(bus platform.EventBus) {
	if eb, ok := bus.(*core.EventBus); ok {
		a.EventBus = eb
	}
}

// ID 返回适配器实例 id。
func (a *Adapter) ID() string {
	if id, ok := a.config["id"].(string); ok {
		return id
	}
	return "dingtalk"
}

// Type 返回平台类型名。
func (a *Adapter) Type() string { return "dingtalk" }

// Start 启动适配器 (对应 Python run)。
func (a *Adapter) Start(ctx context.Context) error {
	a.running.Store(true)
	a.ctx, a.cancel = context.WithCancel(ctx)
	a.msgCh = make(chan map[string]interface{}, 64)
	go a.msgLoop()
	go a.runStream()
	return nil
}

// Stop 停止适配器。
func (a *Adapter) Stop() error {
	a.running.Store(false)
	if a.cancel != nil {
		a.cancel()
	}
	a.wsMu.Lock()
	ws := a.wsConn
	a.wsConn = nil
	a.wsMu.Unlock()
	if ws != nil {
		_ = ws.Close()
	}
	logger.I18nInfo("钉钉适配器已关闭")
	return nil
}

// runStream 钉钉 Stream 长连接主循环, 带指数退避重连 (对应 Python run 中的重连逻辑)。
func (a *Adapter) runStream() {
	retryCount := 0
	for {
		if a.ctx.Err() != nil {
			return
		}
		runStart := time.Now()
		err := a.startStream(a.ctx)
		if a.ctx.Err() != nil {
			// 正常关闭
			return
		}
		if err == nil {
			// 收到 disconnect 指令后的正常断开, 重新连接
			logger.I18nInfo("钉钉长连接已断开, 准备重连")
		} else {
			logger.I18nError("钉钉长连接异常退出: %v", err)
		}
		// 若本次运行时长超过稳定阈值, 重置重连次数
		if time.Since(runStart) >= dingtalkReconnectStableSeconds {
			retryCount = 0
		}
		retryCount++
		delay := dingtalkReconnectDelay(retryCount)
		logger.I18nInfo("钉钉适配器将在 %d 秒后重连 (第 %d 次)...", int(delay.Seconds()), retryCount)
		select {
		case <-a.ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// idToSid 去除钉钉 id 前缀, 得到会话用户 id (对应 Python _id_to_sid)。
func idToSid(dingtalkID string) string {
	if dingtalkID == "" {
		return "unknown"
	}
	if strings.HasPrefix(dingtalkID, dingtalkIDPrefix) {
		return strings.TrimPrefix(dingtalkID, dingtalkIDPrefix)
	}
	return dingtalkID
}

// convertMsg 将钉钉机器人消息转换为 AstrBotMessage (对应 Python convert_msg)。
func (a *Adapter) convertMsg(msg *ChatbotMessage) *platform.AstrBotMessage {
	abm := platform.NewAstrBotMessage()
	abm.Message = []message.Component{}
	abm.MessageStr = ""
	abm.Timestamp = msg.CreateAt / 1000
	if msg.ConversationType == "2" {
		abm.Type = platform.GroupMessage
	} else {
		abm.Type = platform.FriendMessage
	}
	abm.Sender = platform.MessageMember{
		UserID:   idToSid(msg.SenderID),
		Nickname: msg.SenderNick,
	}
	abm.SelfID = idToSid(msg.ChatbotUserID)
	abm.MessageID = msg.MsgID
	abm.RawMessage = msg

	leadingAtIsSelf := false
	if abm.Type == platform.GroupMessage {
		// 处理所有被 @ 的用户 (包括机器人自己, 因 at_users 已包含)
		if len(msg.AtUsers) > 0 {
			for index, user := range msg.AtUsers {
				sid := idToSid(user.DingtalkID)
				abm.Message = append(abm.Message, &message.At{TargetID: sid})
				if index == 0 && sid == abm.SelfID {
					leadingAtIsSelf = true
				}
			}
		}
		abm.Group = &platform.Group{GroupID: msg.ConversationID}
		// 对齐 Python dingtalk_adapter.py #9808：补全群聊名称
		if msg.ConversationTitle != "" {
			abm.Group.GroupName = msg.ConversationTitle
		}
		abm.SessionID = msg.ConversationID
		a.rememberGroup(msg.ConversationID)
	} else {
		abm.SessionID = abm.Sender.UserID
	}

	robotCode := msg.RobotCode
	switch msg.MessageType {
	case "text":
		abm.MessageStr = strings.TrimSpace(msg.TextContent)
		abm.Message = append(abm.Message, &message.Plain{Text: abm.MessageStr})
	case "picture":
		if robotCode == "" {
			logger.I18nError("钉钉图片消息解析失败: 回调中缺少 robotCode")
			a.rememberSenderBinding(msg, abm)
			return abm
		}
		downloadCode := msg.DownloadCode
		if downloadCode == "" {
			logger.I18nWarn("钉钉图片消息缺少 downloadCode，已跳过")
		} else {
			fPath := a.downloadDingFile(downloadCode, robotCode, "jpg")
			if fPath != "" {
				abm.Message = append(abm.Message, &message.Image{File: fPath, Path: fPath})
			} else {
				logger.I18nWarn("钉钉图片消息下载失败，无法解析为图片")
			}
		}
	case "richText":
		plainParts := []string{}
		for index, content := range msg.RichText {
			if textVal, ok := content["text"].(string); ok {
				plainText := textVal
				if plainText != "" {
					// HarmonyOS 会把开头的机器人 @ 也作为文本段重复出现,
					// 即使 atUsers 已经表达了它
					if index == 0 && leadingAtIsSelf && strings.HasPrefix(strings.TrimSpace(plainText), "@") {
						continue
					}
					plainParts = append(plainParts, plainText)
					abm.Message = append(abm.Message, &message.Plain{Text: plainText})
				}
			} else if content["type"] == "picture" {
				downloadCode := getString(content, "downloadCode")
				if downloadCode == "" {
					logger.I18nWarn("钉钉富文本图片消息缺少 downloadCode，已跳过")
					continue
				}
				if robotCode == "" {
					logger.I18nError("钉钉富文本图片消息解析失败: 回调中缺少 robotCode")
					continue
				}
				fPath := a.downloadDingFile(downloadCode, robotCode, "jpg")
				if fPath != "" {
					abm.Message = append(abm.Message, &message.Image{File: fPath, Path: fPath})
				}
			}
		}
		abm.MessageStr = strings.TrimSpace(strings.Join(plainParts, ""))
	case "audio", "voice":
		downloadCode := getString(msg.Content, "downloadCode")
		if downloadCode == "" {
			logger.I18nWarn("钉钉语音消息缺少 downloadCode，已跳过")
		} else if robotCode == "" {
			logger.I18nError("钉钉语音消息解析失败: 回调中缺少 robotCode")
		} else {
			voiceExt := getString(msg.Content, "fileExtension")
			if voiceExt == "" {
				voiceExt = "amr"
			}
			voiceExt = strings.TrimPrefix(voiceExt, ".")
			fPath := a.downloadDingFile(downloadCode, robotCode, voiceExt)
			if fPath != "" {
				// Python 使用 MediaResolver 将语音转为 wav; Go 侧 EnsureWAV 为
				// 占位实现 (保持原文件), 这里直接使用下载的文件
				abm.Message = append(abm.Message, &message.Record{File: fPath, URL: fPath})
			}
		}
	case "file":
		downloadCode := getString(msg.Content, "downloadCode")
		if downloadCode == "" {
			logger.I18nWarn("钉钉文件消息缺少 downloadCode，已跳过")
		} else if robotCode == "" {
			logger.I18nError("钉钉文件消息解析失败: 回调中缺少 robotCode")
		} else {
			fileName := getString(msg.Content, "fileName")
			fileExt := ""
			if fileName != "" {
				fileExt = strings.TrimPrefix(filepath.Ext(fileName), ".")
			}
			if fileExt == "" {
				fileExt = getString(msg.Content, "fileExtension")
			}
			if fileExt == "" {
				fileExt = "file"
			}
			fPath := a.downloadDingFile(downloadCode, robotCode, fileExt)
			if fPath != "" {
				if fileName == "" {
					fileName = filepath.Base(fPath)
				}
				abm.Message = append(abm.Message, &message.File{Name: fileName, Path: fPath})
			}
		}
	}

	a.rememberSenderBinding(msg, abm)
	return abm
}

// rememberGroup 记录群聊的 openConversationId。
func (a *Adapter) rememberGroup(convID string) {
	if convID == "" {
		return
	}
	a.convMu.Lock()
	if a.knownGroups[convID] {
		a.convMu.Unlock()
		return
	}
	a.knownGroups[convID] = true
	a.convMu.Unlock()
	a.persistState()
}

// isKnownGroup 判断会话是否为已接收过消息的群聊。
func (a *Adapter) isKnownGroup(convID string) bool {
	a.convMu.Lock()
	defer a.convMu.Unlock()
	return a.knownGroups[convID]
}

// rememberSenderBinding 记录私聊会话的 senderId -> senderStaffId 映射
// (对应 Python _remember_sender_binding, 存储于全局 sp)。
func (a *Adapter) rememberSenderBinding(msg *ChatbotMessage, abm *platform.AstrBotMessage) {
	if abm.Type != platform.FriendMessage {
		return
	}
	senderID := abm.Sender.UserID
	senderStaffID := msg.SenderStaffID
	if senderStaffID == "" {
		return
	}
	key := a.staffIDKey(senderID)
	a.staffMu.Lock()
	if a.staffIDMap[key] == senderStaffID {
		a.staffMu.Unlock()
		return
	}
	a.staffIDMap[key] = senderStaffID
	a.staffMu.Unlock()
	a.persistState()
}

// staffIDKey 构造私聊会话映射的 key (对应 Python MessageSesion 字符串)。
func (a *Adapter) staffIDKey(senderID string) string {
	return a.ID() + ":FriendMessage:" + senderID
}

// getSenderStaffID 读取私聊会话的 staff_id (对应 Python _get_sender_staff_id)。
func (a *Adapter) getSenderStaffID(senderID string) string {
	a.staffMu.Lock()
	defer a.staffMu.Unlock()
	return a.staffIDMap[a.staffIDKey(senderID)]
}

// stateFile 返回会话映射持久化文件路径。
func (a *Adapter) stateFile() string {
	return filepath.Join(a.dataDir, "state.json")
}

// loadState 从磁盘加载会话映射, 使重启后私聊/群聊发送不再失效。
func (a *Adapter) loadState() {
	data, err := os.ReadFile(a.stateFile())
	if err != nil {
		return
	}
	var st dingtalkState
	if err := json.Unmarshal(data, &st); err != nil {
		logger.I18nWarn("解析钉钉会话状态文件失败: %v", err)
		return
	}
	a.staffMu.Lock()
	for k, v := range st.StaffIDMap {
		a.staffIDMap[k] = v
	}
	a.staffMu.Unlock()
	a.convMu.Lock()
	for _, g := range st.KnownGroups {
		a.knownGroups[g] = true
	}
	a.convMu.Unlock()
}

// persistState 将会话映射落盘为 JSON (仅在新映射产生时调用, 频率低)。
func (a *Adapter) persistState() {
	st := dingtalkState{
		StaffIDMap:  make(map[string]string),
		KnownGroups: make([]string, 0),
	}
	a.staffMu.Lock()
	for k, v := range a.staffIDMap {
		st.StaffIDMap[k] = v
	}
	a.staffMu.Unlock()
	a.convMu.Lock()
	for g := range a.knownGroups {
		st.KnownGroups = append(st.KnownGroups, g)
	}
	a.convMu.Unlock()

	if err := os.MkdirAll(a.dataDir, 0o755); err != nil {
		logger.I18nWarn("创建钉钉数据目录失败: %v", err)
		return
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := a.stateFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		logger.I18nWarn("写入钉钉会话状态失败: %v", err)
		return
	}
	if err := os.Rename(tmp, a.stateFile()); err != nil {
		logger.I18nWarn("保存钉钉会话状态失败: %v", err)
	}
}

// downloadDingFile 下载钉钉消息中的文件 (对应 Python download_ding_file)。
func (a *Adapter) downloadDingFile(downloadCode, robotCode, ext string) string {
	accessToken := a.getAccessToken()
	if accessToken == "" {
		return ""
	}
	payload := map[string]interface{}{
		"downloadCode": downloadCode,
		"robotCode":    robotCode,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(a.dingtalkCtx(), http.MethodPost,
		dingtalkOpenAPI+"/v1.0/robot/messageFiles/download", bytes.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-acs-dingtalk-access-token", accessToken)
	resp, err := a.httpClient.Do(req)
	if err != nil {
		logger.I18nError("下载钉钉文件失败: %v", err)
		return ""
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		logger.I18nError("下载钉钉文件失败: %d, %s", resp.StatusCode, string(respBody))
		return ""
	}
	var respData map[string]interface{}
	if err := json.Unmarshal(respBody, &respData); err != nil {
		logger.I18nError("下载钉钉文件失败: 响应格式错误 %s", string(respBody))
		return ""
	}
	downloadURL := getString(respData, "downloadUrl")
	if downloadURL == "" {
		if data, ok := respData["data"].(map[string]interface{}); ok {
			downloadURL = getString(data, "downloadUrl")
		}
	}
	if downloadURL == "" {
		logger.I18nError("下载钉钉文件失败: 未找到 downloadUrl, 响应: %s", string(respBody))
		return ""
	}
	fPath := filepath.Join(os.TempDir(), fmt.Sprintf("dingtalk_%d.%s", time.Now().UnixNano(), ext))
	if err := utils.DownloadFile(a.dingtalkCtx(), downloadURL, fPath); err != nil {
		logger.I18nError("下载钉钉文件失败: %v", err)
		return ""
	}
	return fPath
}

// dingtalkCtx 返回适配器的运行上下文; 未启动时回退到 background。
func (a *Adapter) dingtalkCtx() context.Context {
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

// getAccessToken 获取钉钉 access_token (带缓存, 对应 Python get_access_token)。
func (a *Adapter) getAccessToken() string {
	a.tokenCache.mu.Lock()
	defer a.tokenCache.mu.Unlock()
	if a.tokenCache.token != "" && time.Now().Before(a.tokenCache.expireAt) {
		return a.tokenCache.token
	}
	payload := map[string]interface{}{
		"appKey":    a.clientID,
		"appSecret": a.clientSecret,
	}
	body, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(a.dingtalkCtx(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		dingtalkOpenAPI+"/v1.0/oauth2/accessToken", bytes.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		logger.I18nError("获取钉钉机器人 access_token 失败: %v", err)
		return ""
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		logger.I18nError("获取钉钉机器人 access_token 失败: %d, %s", resp.StatusCode, string(respBody))
		return ""
	}
	var data map[string]interface{}
	if err := json.Unmarshal(respBody, &data); err != nil {
		return ""
	}
	token := getString(data, "accessToken")
	if token == "" {
		if inner, ok := data["data"].(map[string]interface{}); ok {
			token = getString(inner, "accessToken")
		}
	}
	if token == "" {
		return ""
	}
	// 提前 5 分钟过期 (对应 SDK 的 expireIn - 5min 缓冲)
	expireIn := 7200
	if v, ok := data["expireIn"].(float64); ok {
		expireIn = int(v)
	} else if inner, ok := data["data"].(map[string]interface{}); ok {
		if v, ok := inner["expireIn"].(float64); ok {
			expireIn = int(v)
		}
	}
	a.tokenCache.token = token
	a.tokenCache.expireAt = time.Now().Add(time.Duration(expireIn-300) * time.Second)
	return token
}

// uploadMedia 上传媒体文件到钉钉 (对应 Python upload_media)。
func (a *Adapter) uploadMedia(filePath, mediaType string) string {
	accessToken := a.getAccessToken()
	if accessToken == "" {
		logger.I18nError("钉钉媒体上传失败: access_token 为空")
		return ""
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		logger.I18nError("钉钉媒体上传失败: 读取文件失败 %v", err)
		return ""
	}
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("media", filepath.Base(filePath))
	if err != nil {
		return ""
	}
	if _, err := part.Write(data); err != nil {
		return ""
	}
	if err := writer.Close(); err != nil {
		return ""
	}
	url := fmt.Sprintf("%s/media/upload?access_token=%s&type=%s", dingtalkOAPI, accessToken, mediaType)
	ctx, cancel := context.WithTimeout(a.dingtalkCtx(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := a.httpClient.Do(req)
	if err != nil {
		logger.I18nError("钉钉媒体上传失败: %v", err)
		return ""
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		logger.I18nError("钉钉媒体上传失败: %d, %s", resp.StatusCode, string(respBody))
		return ""
	}
	var data2 map[string]interface{}
	if err := json.Unmarshal(respBody, &data2); err != nil {
		return ""
	}
	if errcode, ok := data2["errcode"].(float64); ok && int(errcode) != 0 {
		logger.I18nError("钉钉媒体上传失败: %s", string(respBody))
		return ""
	}
	mediaID := getString(data2, "media_id")
	if mediaID == "" {
		logger.I18nError("钉钉媒体上传失败: 未找到 media_id, %s", string(respBody))
	}
	return mediaID
}

// sendGroupMessage 发送群聊消息 (对应 Python _send_group_message)。
func (a *Adapter) sendGroupMessage(openConversationID, robotCode, msgKey string, msgParam map[string]interface{}) error {
	accessToken := a.getAccessToken()
	if accessToken == "" {
		logger.I18nError("钉钉群消息发送失败: access_token 为空")
		return fmt.Errorf("钉钉群消息发送失败: access_token 为空")
	}
	paramJSON, _ := json.Marshal(msgParam)
	payload := map[string]interface{}{
		"msgKey":             msgKey,
		"msgParam":           string(paramJSON),
		"openConversationId": openConversationID,
		"robotCode":          robotCode,
	}
	body, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(a.dingtalkCtx(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		dingtalkOpenAPI+"/v1.0/robot/groupMessages/send", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("钉钉群消息发送失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-acs-dingtalk-access-token", accessToken)
	resp, err := a.httpClient.Do(req)
	if err != nil {
		logger.I18nError("钉钉群消息发送失败: %v", err)
		return fmt.Errorf("钉钉群消息发送失败: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		logger.I18nError("钉钉群消息发送失败: %d, %s", resp.StatusCode, string(respBody))
		return fmt.Errorf("钉钉群消息发送失败: HTTP %d", resp.StatusCode)
	}
	return nil
}

// sendPrivateMessage 发送私聊消息 (对应 Python _send_private_message)。
func (a *Adapter) sendPrivateMessage(staffID, robotCode, msgKey string, msgParam map[string]interface{}) error {
	accessToken := a.getAccessToken()
	if accessToken == "" {
		logger.I18nError("钉钉私聊消息发送失败: access_token 为空")
		return fmt.Errorf("钉钉私聊消息发送失败: access_token 为空")
	}
	paramJSON, _ := json.Marshal(msgParam)
	payload := map[string]interface{}{
		"robotCode": robotCode,
		"userIds":   []string{staffID},
		"msgKey":    msgKey,
		"msgParam":  string(paramJSON),
	}
	body, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(a.dingtalkCtx(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		dingtalkOpenAPI+"/v1.0/robot/oToMessages/batchSend", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("钉钉私聊消息发送失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-acs-dingtalk-access-token", accessToken)
	resp, err := a.httpClient.Do(req)
	if err != nil {
		logger.I18nError("钉钉私聊消息发送失败: %v", err)
		return fmt.Errorf("钉钉私聊消息发送失败: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		logger.I18nError("钉钉私聊消息发送失败: %d, %s", resp.StatusCode, string(respBody))
		return fmt.Errorf("钉钉私聊消息发送失败: HTTP %d", resp.StatusCode)
	}
	return nil
}

// sendMessageChain 发送消息链 (对应 Python _send_message_chain)。
func (a *Adapter) sendMessageChain(targetType, targetID, robotCode string, chain *message.MessageChain, atStr string) error {
	var firstErr error
	sendMessage := func(msgKey string, msgParam map[string]interface{}) {
		var err error
		if targetType == "group" {
			err = a.sendGroupMessage(targetID, robotCode, msgKey, msgParam)
		} else {
			err = a.sendPrivateMessage(targetID, robotCode, msgKey, msgParam)
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	for _, comp := range chain.Chain {
		switch c := comp.(type) {
		case *message.Plain:
			text := strings.TrimSpace(c.Text)
			if text == "" && atStr == "" {
				continue
			}
			text = strings.TrimSpace(atStr + " " + text)
			if chain.UseMarkdown != nil && !*chain.UseMarkdown {
				sendMessage("sampleText", map[string]interface{}{"content": text})
			} else {
				sendMessage("sampleMarkdown", map[string]interface{}{"title": "AstrBot", "text": text})
			}
		case *message.Image:
			photoURL := c.File
			if photoURL == "" {
				photoURL = c.Path
			}
			if photoURL == "" {
				photoURL = c.URL
			}
			if photoURL == "" && c.Base64 != "" {
				photoURL = saveBase64TempFile(c.Base64)
			}
			if !strings.HasPrefix(photoURL, "http://") && !strings.HasPrefix(photoURL, "https://") {
				photoURL = a.uploadMedia(photoURL, "image")
			}
			if photoURL == "" {
				continue
			}
			sendMessage("sampleImageMsg", map[string]interface{}{"photoURL": photoURL})
		case *message.Record:
			convertedAudio := false
			audioPath := c.File
			if audioPath == "" {
				audioPath = c.Path
			}
			if audioPath == "" {
				audioPath = c.URL
			}
			if audioPath == "" && c.Base64 != "" {
				audioPath = saveBase64TempFile(c.Base64)
			}
			if audioPath == "" {
				continue
			}
			var converted string
			converted, convertedAudio = prepareVoiceForDingtalk(audioPath)
			audioPath = converted
			mediaID := a.uploadMedia(audioPath, "voice")
			if mediaID == "" {
				if convertedAudio {
					safeRemoveFile(audioPath)
				}
				continue
			}
			durationMS := getMediaDuration(audioPath)
			if durationMS == 0 {
				durationMS = 1000
			}
			sendMessage("sampleAudio", map[string]interface{}{
				"mediaId":  mediaID,
				"duration": fmt.Sprintf("%d", durationMS),
			})
			if convertedAudio {
				safeRemoveFile(audioPath)
			}
		case *message.Video:
			convertedVideo := false
			coverPath := ""
			videoPath := c.Path
			if videoPath == "" {
				videoPath = c.URL
			}
			if videoPath == "" {
				continue
			}
			var converted string
			converted, convertedVideo = convertVideoToMP4(videoPath)
			videoPath = converted
			coverPath = extractVideoCover(videoPath)
			videoMediaID := a.uploadMedia(videoPath, "file")
			picMediaID := ""
			if coverPath != "" {
				picMediaID = a.uploadMedia(coverPath, "image")
			}
			if videoMediaID == "" || picMediaID == "" {
				if coverPath != "" {
					safeRemoveFile(coverPath)
				}
				if convertedVideo {
					safeRemoveFile(videoPath)
				}
				continue
			}
			durationMS := getMediaDuration(videoPath)
			if durationMS == 0 {
				durationMS = 1000
			}
			durationSec := durationMS / 1000
			if durationSec < 1 {
				durationSec = 1
			}
			sendMessage("sampleVideo", map[string]interface{}{
				"duration":     fmt.Sprintf("%d", durationSec),
				"videoMediaId": videoMediaID,
				"videoType":    "mp4",
				"picMediaId":   picMediaID,
			})
			safeRemoveFile(coverPath)
			if convertedVideo {
				safeRemoveFile(videoPath)
			}
		case *message.File:
			filePath := c.Path
			if filePath == "" {
				filePath = c.URL
			}
			if filePath == "" {
				logger.I18nWarn("钉钉文件发送失败: 无法解析文件路径")
				continue
			}
			mediaID := a.uploadMedia(filePath, "file")
			if mediaID == "" {
				continue
			}
			fileName := c.Name
			if fileName == "" {
				fileName = filepath.Base(filePath)
			}
			fileType := strings.TrimPrefix(filepath.Ext(fileName), ".")
			if fileType == "" {
				fileType = "file"
			}
			sendMessage("sampleFile", map[string]interface{}{
				"mediaId":  mediaID,
				"fileName": fileName,
				"fileType": fileType,
			})
		}
	}
	return firstErr
}

// prepareVoiceForDingtalk 优先转换为 OGG(Opus), 不可用时回退 AMR。
// 对应 Python _prepare_voice_for_dingtalk。
func prepareVoiceForDingtalk(inputPath string) (string, bool) {
	lower := strings.ToLower(inputPath)
	if strings.HasSuffix(lower, ".amr") || strings.HasSuffix(lower, ".ogg") {
		return inputPath, false
	}
	converted, isConverted := convertAudioFormat(inputPath, "ogg")
	if isConverted {
		return converted, true
	}
	// OGG 转换失败, 回退 AMR
	converted, isConverted = convertAudioFormat(inputPath, "amr")
	return converted, isConverted
}

// saveBase64TempFile 将 base64 数据保存为临时文件。
func saveBase64TempFile(b64 string) string {
	data, err := utils.Base64ToBytes(b64)
	if err != nil {
		logger.I18nWarn("钉钉 base64 解码失败: %v", err)
		return ""
	}
	tmp, err := os.CreateTemp("", "astrbot-dingtalk-*")
	if err != nil {
		return ""
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return ""
	}
	_ = tmp.Close()
	return name
}

// Send 发送消息链 (对应 Python send_by_session + send_message_chain_to_*)。
// 群聊的 sessionID 为 openConversationId; 私聊的 sessionID 为发送者的 sid,
// 私聊发送时通过 staff_id 映射找到用户的 staffId, 缺失时回退使用 session_id。
// 发送失败时返回 error, 不再静默吞掉 (对应 L-27)。
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	if chain == nil {
		return nil
	}
	robotCode := a.clientID
	if a.isKnownGroup(sessionID) {
		return a.sendMessageChain("group", sessionID, robotCode, chain, "")
	}
	staffID := a.getSenderStaffID(sessionID)
	if staffID == "" {
		logger.I18nWarn("钉钉私聊会话缺少 staff_id 映射，回退使用 session_id 作为 userId 发送")
		staffID = sessionID
	}
	return a.sendMessageChain("user", staffID, robotCode, chain, "")
}

// handleMsg 发布消息事件到事件总线。
func (a *Adapter) handleMsg(abm *platform.AstrBotMessage) {
	if a.EventBus == nil {
		return
	}
	isGroup := abm.Type == platform.GroupMessage
	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   "dingtalk",
			PlatformID: a.ID(),
			SelfID:     abm.SelfID,
			SenderID:   abm.Sender.UserID,
			SenderName: abm.Sender.Nickname,
			ConvID:     abm.SessionID,
			IsGroup:    isGroup,
		},
		Message:    &message.MessageChain{Chain: abm.Message},
		MessageStr: abm.MessageStr,
		Timestamp:  time.Unix(abm.Timestamp, 0),
		MessageObj: &core.MessageObj{
			MessageID:   abm.MessageID,
			SelfID:      abm.SelfID,
			SessionID:   abm.SessionID,
			MessageType: string(abm.Type),
			Platform:    "dingtalk",
			MessageStr:  abm.MessageStr,
			RawMessage:  abm.RawMessage,
		},
		Metadata: map[string]interface{}{
			"dingtalk_staff_id": msgStaffID(abm.RawMessage),
		},
	}
	if err := a.EventBus.Publish(event); err != nil {
		logger.I18nError("发布钉钉消息事件失败: %v", err)
	}
	a.removeMsgTempFiles(abm)
}

// removeMsgTempFiles 清理本条消息下载到临时目录的媒体文件
// (downloadDingFile 落盘为 dingtalk_* 文件, 发布后无其他消费方)。
func (a *Adapter) removeMsgTempFiles(abm *platform.AstrBotMessage) {
	for _, comp := range abm.Message {
		var p string
		switch c := comp.(type) {
		case *message.Image:
			p = c.Path
		case *message.Record:
			p = c.File
		case *message.File:
			p = c.Path
		}
		removeDingtalkTemp(p)
	}
}

func removeDingtalkTemp(p string) {
	if p != "" && strings.HasPrefix(filepath.Base(p), "dingtalk_") {
		_ = os.Remove(p)
	}
}

// msgStaffID 从原始消息中提取 senderStaffId (用于会话映射保留)。
func msgStaffID(raw interface{}) string {
	if msg, ok := raw.(*ChatbotMessage); ok {
		return msg.SenderStaffID
	}
	return ""
}
