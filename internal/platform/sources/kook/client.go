package kook

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/gorilla/websocket"
)

var logger = log.GetDefault().WithComponent("KOOK")

// KookClient 对应 Python kook_client.py 的 KookClient。
// 负责 REST 调用 (机器人信息/网关地址/消息发送/文件上传) 与 WebSocket 长连接
// (消息接收 + 心跳 + 断线重连状态维护)。
type KookClient struct {
	config *KookConfig

	// REST 客户端, 统一携带 Authorization: Bot <token> 请求头
	httpClient *http.Client

	// 事件回调, 用于处理接收到的事件 (对应 Python 的 event_callback)
	eventCallback func(data *kookMessageEventData)

	// 机器人账号信息
	botID       string
	botUsername string
	botNickname string

	// WebSocket 资源
	wsMu sync.Mutex
	ws   *websocket.Conn

	// 状态/计算字段
	runningMu sync.RWMutex
	running   bool

	sessionID            string // 当前会话 id
	lastSN               int64  // 记录最后处理的消息序号
	lastHeartbeatTime    time.Time
	heartbeatFailedCount int

	// 通知连接结束 (对应 Python 的 _stop_event)
	stopEvent chan struct{}
	stopOnce  sync.Once

	heartbeatCancel context.CancelFunc
}

// NewKookClient 创建 KOOK 客户端。
func NewKookClient(config *KookConfig, eventCallback func(data *kookMessageEventData)) *KookClient {
	c := &KookClient{
		config:        config,
		eventCallback: eventCallback,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
		stopEvent: make(chan struct{}),
	}
	return c
}

// BotID 返回机器人账号 id。
func (c *KookClient) BotID() string { return c.botID }

// BotNickname 返回机器人昵称。
func (c *KookClient) BotNickname() string { return c.botNickname }

// BotUsername 返回机器人名称。
func (c *KookClient) BotUsername() string { return c.botUsername }

// IsRunning 返回连接是否存活。
func (c *KookClient) IsRunning() bool {
	c.runningMu.RLock()
	defer c.runningMu.RUnlock()
	return c.running
}

func (c *KookClient) setRunning(running bool) {
	c.runningMu.Lock()
	c.running = running
	c.runningMu.Unlock()
}

// newRequest 构造带鉴权头的 HTTP 请求。
func (c *KookClient) newRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bot "+c.config.Token)
	return req, nil
}

// GetBotInfo 获取机器人账号信息 (对应 Python get_bot_info)。
func (c *KookClient) GetBotInfo(ctx context.Context) {
	req, err := c.newRequest(ctx, http.MethodGet, apiUserMe, nil)
	if err != nil {
		logger.I18nError("[KOOK] 获取机器人账号信息异常: %v", err)
		return
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		logger.I18nError("[KOOK] 获取机器人账号信息异常: %v", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		logger.I18nError("[KOOK] 获取机器人账号信息失败，状态码: %d, %s", resp.StatusCode, string(body))
		return
	}
	var apiResp struct {
		Code int            `json:"code"`
		Data kookUserMeData `json:"data"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		logger.I18nError("[KOOK] 获取机器人账号信息失败, 响应数据格式错误: %v", err)
		logger.I18nError("[KOOK] 响应内容: %s", string(body))
		return
	}
	if apiResp.Code != 0 {
		logger.I18nError("[KOOK] 获取机器人账号信息失败: %d %s", apiResp.Code, string(body))
		return
	}
	c.botID = apiResp.Data.ID
	logger.I18nInfo("[KOOK] 获取机器人账号ID成功: %s", c.botID)
	c.botNickname = apiResp.Data.Nickname
	c.botUsername = apiResp.Data.Username
	logger.I18nInfo("[KOOK] 获取机器人名称成功: %s", c.botNickname)
}

// GetGatewayURL 获取网关连接地址 (对应 Python get_gateway_url)。
func (c *KookClient) GetGatewayURL(ctx context.Context, resume bool, sn int64, sessionID string) (string, error) {
	url := apiGatewayIndex
	// 构建连接参数
	params := ""
	if resume {
		params = fmt.Sprintf("?resume=1&sn=%d", sn)
		if sessionID != "" {
			params += "&session_id=" + sessionID
		}
	}
	req, err := c.newRequest(ctx, http.MethodGet, url+params, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		logger.I18nError("[KOOK] 获取gateway异常: %v", err)
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		logger.I18nError("[KOOK] 获取gateway失败，状态码: %d", resp.StatusCode)
		return "", fmt.Errorf("获取gateway失败，状态码: %d", resp.StatusCode)
	}
	var apiResp struct {
		Code int                  `json:"code"`
		Data kookGatewayIndexData `json:"data"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		logger.I18nError("[KOOK] 获取gateway失败, 响应数据格式错误: %v", err)
		logger.I18nError("[KOOK] 原始响应内容: %s", string(body))
		return "", err
	}
	if apiResp.Code != 0 {
		logger.I18nError("[KOOK] 获取gateway失败: %s", string(body))
		return "", fmt.Errorf("获取gateway失败: code=%d", apiResp.Code)
	}
	gatewayURL := apiResp.Data.URL
	// 日志中隐藏 query 中的 token
	if idx := strings.Index(gatewayURL, "?"); idx >= 0 {
		logger.I18nInfo("[KOOK] 获取gateway成功: %s", gatewayURL[:idx])
	} else {
		logger.I18nInfo("[KOOK] 获取gateway成功: %s", gatewayURL)
	}
	return gatewayURL, nil
}

// closeWS 关闭当前 WebSocket 连接 (幂等)。
func (c *KookClient) closeWS() {
	c.wsMu.Lock()
	ws := c.ws
	c.ws = nil
	c.wsMu.Unlock()
	if ws != nil {
		_ = ws.Close()
	}
}

// Connect 连接 WebSocket 并阻塞监听, 直到连接断开。
// 返回 true 表示成功建立过连接 (对应 Python connect)。
func (c *KookClient) Connect(ctx context.Context) bool {
	c.closeWS()
	gatewayURL, err := c.GetGatewayURL(ctx, false, c.lastSN, c.sessionID)
	if err != nil || gatewayURL == "" {
		return false
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, gatewayURL, nil)
	if err != nil {
		logger.I18nError("[KOOK] WebSocket 连接失败: %v", err)
		c.closeWS()
		return false
	}
	c.wsMu.Lock()
	c.ws = conn
	c.wsMu.Unlock()
	c.setRunning(true)
	logger.I18nInfo("[KOOK] WebSocket 连接成功")

	// 启动心跳任务
	hbCtx, cancel := context.WithCancel(context.Background())
	c.heartbeatCancel = cancel
	go c.heartbeatLoop(hbCtx)

	// 开始监听消息 (阻塞)
	c.listen(ctx)
	return true
}

// listen 监听 WebSocket 消息 (对应 Python listen)。
func (c *KookClient) listen(ctx context.Context) {
	defer func() {
		c.setRunning(false)
		c.stopOnce.Do(func() { close(c.stopEvent) })
	}()
	for c.IsRunning() {
		conn := c.currentWS()
		if conn == nil {
			logger.I18nError("[KOOK] WebSocket 对象丢失，结束监听流程。")
			break
		}
		// 10 秒读超时, 用于周期性检查 running 状态 (对应 Python wait_for timeout=10)
		if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			break
		}
		msgType, msg, err := conn.ReadMessage()
		if err != nil {
			if netErr, ok := err.(interface{ Timeout() bool }); ok && netErr.Timeout() {
				// 正常超时, 继续下一轮循环检查
				continue
			}
			logger.I18nWarn("[KOOK] WebSocket连接已关闭: %v", err)
			break
		}

		// 二进制消息为 zlib 压缩数据, 解压后按文本解析 (对应 Python zlib.decompress)
		if msgType == websocket.BinaryMessage {
			msg, err = zlibUncompress(msg)
			if err != nil {
				logger.I18nError("[KOOK] 解压消息失败: %v", err)
				continue
			}
		}

		var frame kookWSFrame
		if err := json.Unmarshal(msg, &frame); err != nil {
			logger.I18nError("[KOOK] 解析WebSocket事件数据格式失败: %v", err)
			logger.I18nError("[KOOK] 原始响应内容: %s", string(msg))
			continue
		}
		c.handleSignal(ctx, &frame)
	}
}

// currentWS 返回当前 WebSocket 连接。
func (c *KookClient) currentWS() *websocket.Conn {
	c.wsMu.Lock()
	defer c.wsMu.Unlock()
	return c.ws
}

// zlibUncompress 解压 zlib 数据。
func zlibUncompress(data []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// handleSignal 处理不同类型的信令 (对应 Python _handle_signal)。
func (c *KookClient) handleSignal(ctx context.Context, frame *kookWSFrame) {
	switch frame.Signal {
	case signalMessage:
		if frame.SN != nil {
			c.lastSN = *frame.SN
		}
		if len(frame.Data) == 0 {
			return
		}
		var data kookMessageEventData
		if err := json.Unmarshal(frame.Data, &data); err != nil {
			logger.I18nError("[KOOK] 解析消息事件数据失败: %v", err)
			return
		}
		if c.eventCallback != nil {
			c.eventCallback(&data)
		}
	case signalHello:
		var hello kookHelloEventData
		if err := json.Unmarshal(frame.Data, &hello); err != nil {
			logger.I18nError("[KOOK] 解析HELLO数据失败: %v", err)
			return
		}
		c.handleHello(&hello)
	case signalPong:
		c.handlePong()
	case signalReconnect:
		c.handleReconnect()
	case signalResumeAck:
		var ack kookResumeAckEventData
		if err := json.Unmarshal(frame.Data, &ack); err != nil {
			logger.I18nError("[KOOK] 解析RESUME_ACK数据失败: %v", err)
			return
		}
		c.handleResumeAck(&ack)
	default:
		logger.Debug("[KOOK] 未处理的信令类型: %d", frame.Signal)
	}
}

// handleHello 处理 HELLO 握手 (对应 Python _handle_hello)。
func (c *KookClient) handleHello(data *kookHelloEventData) {
	if data.Code == 0 {
		c.sessionID = data.SessionID
		logger.I18nInfo("[KOOK] 握手成功，session_id: %s", c.sessionID)
	} else {
		logger.I18nError("[KOOK] 握手失败，错误码: %d", data.Code)
		if data.Code == 40103 { // token过期
			logger.I18nError("[KOOK] Token已过期，需要重新获取")
		}
		c.setRunning(false)
	}
}

// handlePong 处理 PONG 心跳响应 (对应 Python _handle_pong)。
func (c *KookClient) handlePong() {
	c.lastHeartbeatTime = time.Now()
	c.heartbeatFailedCount = 0
}

// handleReconnect 处理重连指令 (对应 Python _handle_reconnect)。
func (c *KookClient) handleReconnect() {
	logger.I18nWarn("[KOOK] 收到重连指令")
	// 清空本地状态
	c.lastSN = 0
	c.sessionID = ""
	c.setRunning(false)
}

// handleResumeAck 处理 RESUME 确认 (对应 Python _handle_resume_ack)。
func (c *KookClient) handleResumeAck(data *kookResumeAckEventData) {
	c.sessionID = data.SessionID
	logger.I18nInfo("[KOOK] Resume成功，session_id: %s", c.sessionID)
}

// heartbeatLoop 心跳循环 (对应 Python _heartbeat_loop)。
func (c *KookClient) heartbeatLoop(ctx context.Context) {
	for c.IsRunning() {
		// 随机化心跳间隔 (±5秒)
		interval := c.config.HeartbeatInterval + rand.Intn(11) - 5
		if interval < 1 {
			interval = 1
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(interval) * time.Second):
		}
		if !c.IsRunning() {
			break
		}
		// 发送心跳
		c.sendPing()

		// 等待 PONG 响应 (对应 Python await asyncio.sleep(heartbeat_timeout))
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(c.config.HeartbeatTimeout) * time.Second):
		}

		// 检查是否收到 PONG 响应
		if time.Since(c.lastHeartbeatTime) > time.Duration(c.config.HeartbeatTimeout)*time.Second {
			c.heartbeatFailedCount++
			logger.I18nWarn("[KOOK] 心跳超时，失败次数: %d", c.heartbeatFailedCount)
			if c.heartbeatFailedCount >= c.config.MaxHeartbeatFailures {
				logger.I18nError("[KOOK] 心跳失败次数过多，准备重连")
				c.setRunning(false)
				c.closeWS()
				return
			}
		}
	}
}

// sendPing 发送心跳 PING (对应 Python _send_ping)。
func (c *KookClient) sendPing() {
	conn := c.currentWS()
	if conn == nil {
		logger.I18nWarn("[KOOK] 尚未连接kook WebSocket服务器, 跳过发送心跳包流程")
		return
	}
	// 对应 Python KookWebsocketEvent(signal=PING, data=None, sn=...).to_json()
	// data 为 None 时被 exclude, 因此这里不发送 "d" 字段
	payload, _ := json.Marshal(map[string]interface{}{
		"s":  signalPing,
		"sn": c.lastSN,
	})
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		logger.I18nError("[KOOK] 发送心跳失败: %v", err)
	}
}

// SendText 发送文本消息 (对应 Python send_text)。
// 消息发送接口文档: https://developer.kookapp.cn/doc/http/message
// KMarkdown 格式: https://developer.kookapp.cn/doc/kmarkdown-desc
func (c *KookClient) SendText(ctx context.Context, targetID, content string, astrbotMsgType platform.MessageType, kookMsgType KookMessageType, replyID string) error {
	url := apiChannelMsgCreate
	if astrbotMsgType == platform.FriendMessage {
		url = apiDirectMsgCreate
	}
	payload := map[string]interface{}{
		"target_id": targetID,
		"content":   content,
		"type":      int(kookMsgType),
	}
	if replyID != "" {
		payload["quote"] = replyID
		payload["reply_msg_id"] = replyID
	}
	body, _ := json.Marshal(payload)
	req, err := c.newRequest(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		logger.I18nError("[KOOK] 发送kook消息类型 %q 异常: %v", kookMsgTypeName(kookMsgType), err)
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		var result struct {
			Code int `json:"code"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			return fmt.Errorf("发送kook消息类型 %q 失败: %v", kookMsgTypeName(kookMsgType), err)
		}
		if result.Code != 0 {
			return fmt.Errorf("发送kook消息类型 %q 失败: %s", kookMsgTypeName(kookMsgType), string(respBody))
		}
		return nil
	}
	return fmt.Errorf("发送kook消息类型 %q HTTP错误: %d, 响应内容: %s", kookMsgTypeName(kookMsgType), resp.StatusCode, string(respBody))
}

// kookMsgTypeName 返回消息类型名称, 用于日志。
func kookMsgTypeName(t KookMessageType) string {
	switch t {
	case KookMsgText:
		return "TEXT"
	case KookMsgImage:
		return "IMAGE"
	case KookMsgVideo:
		return "VIDEO"
	case KookMsgFile:
		return "FILE"
	case KookMsgAudio:
		return "AUDIO"
	case KookMsgKMarkdown:
		return "KMARKDOWN"
	case KookMsgCard:
		return "CARD"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", int(t))
	}
}

// UploadAsset 上传文件到 kook, 获得远端资源 url (对应 Python upload_asset)。
// 接口定义: https://developer.kookapp.cn/doc/http/asset
func (c *KookClient) UploadAsset(ctx context.Context, fileURL string) (string, error) {
	if fileURL == "" {
		return "", nil
	}
	// 已经是 http(s) 链接时直接返回 (对应 Python 的逻辑)
	if strings.HasPrefix(fileURL, "http://") || strings.HasPrefix(fileURL, "https://") {
		return fileURL, nil
	}
	// 处理 file:// 前缀
	localPath := strings.TrimPrefix(fileURL, "file://")
	data, err := os.ReadFile(localPath)
	if err != nil {
		return "", fmt.Errorf("上传文件到kook服务器失败: %v", err)
	}
	filename := filepath.Base(localPath)

	// 构造 multipart/form-data (对应 Python aiohttp FormData)
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}

	req, err := c.newRequest(ctx, http.MethodPost, apiAssetCreate, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("上传文件到kook服务器异常: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("上传文件到kook服务器 HTTP错误: %d, %s", resp.StatusCode, string(respBody))
	}
	var result struct {
		Code int `json:"code"`
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("上传文件到kook服务器失败: %v", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("上传文件到kook服务器失败: %s", string(respBody))
	}
	logger.I18nInfo("[KOOK] 上传文件到kook服务器成功")
	logger.Debug("[KOOK] 文件远端URL: %s", result.Data.URL)
	return result.Data.URL, nil
}

// React 给消息添加表情回应 (message/reaction/add)。
// 接口定义: https://developer.kookapp.cn/doc/http/message#给消息添加回应
func (c *KookClient) React(ctx context.Context, msgID, emoji string) error {
	payload := map[string]interface{}{
		"msg_id": msgID,
		"emoji":  emoji,
	}
	body, _ := json.Marshal(payload)
	req, err := c.newRequest(ctx, http.MethodPost, apiReactionAdd, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		logger.I18nError("[KOOK] 发送表情回应异常: %v", err)
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("发送表情回应 HTTP错误: %d, 响应内容: %s", resp.StatusCode, string(respBody))
	}
	var result struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("发送表情回应失败: %v", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("发送表情回应失败: %s", string(respBody))
	}
	return nil
}

// Close 关闭连接 (对应 Python close)。
func (c *KookClient) Close() {
	c.setRunning(false)
	c.stopOnce.Do(func() { close(c.stopEvent) })
	if c.heartbeatCancel != nil {
		c.heartbeatCancel()
	}
	c.closeWS()
	logger.I18nInfo("[KOOK] 连接已关闭")
}

// WaitUntilClosed 返回连接结束通知通道 (对应 Python wait_until_closed)。
func (c *KookClient) WaitUntilClosed() <-chan struct{} {
	return c.stopEvent
}
