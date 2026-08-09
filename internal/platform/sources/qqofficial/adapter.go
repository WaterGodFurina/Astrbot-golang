// Package qqofficial implements the QQ Official Bot platform adapter.
// Ported from astrbot/core/platform/sources/qqofficial/qqofficial_platform_adapter.py
// and botpy (qq-botpy) WebSocket gateway protocol.
package qqofficial

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/core"
	"github.com/AstrBotDevs/AstrBot/internal/log"
	"github.com/AstrBotDevs/AstrBot/internal/platform"
	"github.com/AstrBotDevs/AstrBot/pkg/message"
	"github.com/gorilla/websocket"
)

var logger = log.GetDefault().WithComponent("QQOfficial")

// WebSocket opcodes
const (
	wsDispatch       = 0
	wsHeartbeat      = 1
	wsIdentify       = 2
	wsResume         = 6
	wsReconnect      = 7
	wsInvalidSession = 9
	wsHello          = 10
	wsHeartbeatAck   = 11
)

// Intents (bit fields)
const (
	intentPublicGuildMessages = 1 << 30
	intentDirectMessage       = 1 << 12
	intentPublicMessages      = 1 << 25
)

const (
	apiDomain     = "https://api.sgroup.qq.com"
	tokenEndpoint = "https://bots.qq.com/app/getAppAccessToken"
	fileTypeImage = 1
	fileTypeVideo = 2
	fileTypeVoice = 3
	fileTypeFile  = 4
)

// Adapter is the QQ Official Bot adapter.
type Adapter struct {
	platform.BaseAdapter
	AppID          string
	Secret         string
	EnableGroupC2C bool
	EnableGuildDM  bool
	Intents        int
	SelfID         string

	mu             sync.Mutex
	accessToken    string
	tokenExpiresAt time.Time
	ws             *websocket.Conn
	wsSessionID    string
	lastSeq        int
	sessionScene   map[string]string // convID -> "group"/"friend"/"channel"
	sessionLastMsg map[string]string // convID -> last message id
	stopCh         chan struct{}
	stopped        bool
	httpClient     *http.Client
}

// New creates a QQ official adapter from config.
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		BaseAdapter:    *platform.NewBaseAdapter(configID(config), "qq_official"),
		sessionScene:   make(map[string]string),
		sessionLastMsg: make(map[string]string),
		stopCh:         make(chan struct{}),
		httpClient:     &http.Client{Timeout: 30 * time.Second},
	}
	a.AppID, _ = config["appid"].(string)
	a.Secret, _ = config["secret"].(string)
	if id, ok := config["id"].(string); ok {
		a.SelfID = id
	}
	if v, ok := config["enable_group_c2c"].(bool); ok {
		a.EnableGroupC2C = v
	}
	if v, ok := config["enable_guild_direct_message"].(bool); ok {
		a.EnableGuildDM = v
	}
	a.Intents = intentPublicGuildMessages | intentPublicMessages
	if a.EnableGuildDM {
		a.Intents |= intentDirectMessage
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
	return "qq_official"
}

// Start begins the gateway connection loop.
func (a *Adapter) Start(ctx context.Context) error {
	a.mu.Lock()
	a.stopped = false
	a.mu.Unlock()
	go a.runLoop(ctx)
	return nil
}

// Stop shuts down the adapter.
func (a *Adapter) Stop() error {
	a.mu.Lock()
	a.stopped = true
	a.mu.Unlock()
	select {
	case <-a.stopCh:
	default:
		close(a.stopCh)
	}
	a.mu.Lock()
	if a.ws != nil {
		_ = a.ws.Close()
		a.ws = nil
	}
	a.mu.Unlock()
	return nil
}

// fetchAccessToken exchanges appid/secret for an access token.
func (a *Adapter) fetchAccessToken() (string, error) {
	body, _ := json.Marshal(map[string]string{
		"appId":        a.AppID,
		"clientSecret": a.Secret,
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

// getAccessToken returns a valid access token (refreshing when needed).
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

// fetchGateway returns the WebSocket gateway URL.
func (a *Adapter) fetchGateway(token string) (string, error) {
	req, _ := http.NewRequest(http.MethodGet, apiDomain+"/gateway/bot", nil)
	req.Header.Set("Authorization", "QQBot "+token)
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	dataBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var data map[string]interface{}
	if err := json.Unmarshal(dataBytes, &data); err != nil {
		return "", fmt.Errorf("获取网关地址失败: %v", err)
	}
	url, _ := data["url"].(string)
	if url == "" {
		return "", fmt.Errorf("获取网关地址失败: %s", string(dataBytes))
	}
	return url, nil
}

// runLoop connects to the gateway and reconnects on failure.
func (a *Adapter) runLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.stopCh:
			return
		default:
		}
		err := a.connectOnce(ctx)
		if err != nil {
			logger.Error("[QQOfficial] gateway error: %v, retrying in 5s", err)
		}
		a.mu.Lock()
		if a.ws != nil {
			_ = a.ws.Close()
			a.ws = nil
		}
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-a.stopCh:
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// connectOnce performs one gateway connection lifecycle.
func (a *Adapter) connectOnce(ctx context.Context) error {
	token, err := a.getAccessToken()
	if err != nil {
		return err
	}
	gatewayURL, err := a.fetchGateway(token)
	if err != nil {
		return err
	}

	ws, _, err := websocket.DefaultDialer.Dial(gatewayURL, nil)
	if err != nil {
		return fmt.Errorf("websocket 连接失败: %v", err)
	}
	a.mu.Lock()
	a.ws = ws
	prevSession := a.wsSessionID
	prevSeq := a.lastSeq
	a.mu.Unlock()

	// Read loop with heartbeat
	heartbeatCh := make(chan time.Duration, 1)
	done := make(chan struct{})
	var heartbeatCancel context.CancelFunc

	go func() {
		select {
		case interval := <-heartbeatCh:
			if heartbeatCancel != nil {
				heartbeatCancel()
			}
			var hctx context.Context
			hctx, heartbeatCancel = context.WithCancel(ctx)
			a.heartbeatLoop(hctx, interval)
		case <-done:
			return
		case <-a.stopCh:
			return
		}
	}()

	defer func() {
		close(done)
		if heartbeatCancel != nil {
			heartbeatCancel()
		}
	}()

	for {
		select {
		case <-a.stopCh:
			_ = ws.Close()
			return nil
		case <-ctx.Done():
			_ = ws.Close()
			return nil
		default:
		}
		_, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		var frame map[string]interface{}
		if err := json.Unmarshal(data, &frame); err != nil {
			continue
		}
		op, _ := frame["op"].(float64)
		switch int(op) {
		case wsHello:
			if d, ok := frame["d"].(map[string]interface{}); ok {
				if interval, ok := d["heartbeat_interval"].(float64); ok {
					heartbeatCh <- time.Duration(interval) * time.Millisecond
				}
			}
			if prevSession != "" {
				_ = a.sendFrame(ws, wsResume, map[string]interface{}{
					"token":      "QQBot " + token,
					"session_id": prevSession,
					"seq":        prevSeq,
				})
			} else {
				_ = a.sendFrame(ws, wsIdentify, map[string]interface{}{
					"token":   "QQBot " + token,
					"intents": a.Intents,
					"shard":   []int{0, 1},
				})
			}
		case wsHeartbeatAck:
			// ok
		case wsReconnect:
			logger.Info("[QQOfficial] server requested reconnect")
			return fmt.Errorf("reconnect requested")
		case wsInvalidSession:
			a.mu.Lock()
			a.wsSessionID = ""
			a.lastSeq = 0
			a.mu.Unlock()
			return fmt.Errorf("invalid session")
		case wsDispatch:
			a.handleDispatch(frame)
		}
	}
}

func (a *Adapter) sendFrame(ws *websocket.Conn, op int, d map[string]interface{}) error {
	payload := map[string]interface{}{"op": op, "d": d}
	data, _ := json.Marshal(payload)
	return ws.WriteMessage(websocket.TextMessage, data)
}

func (a *Adapter) heartbeatLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.mu.Lock()
			ws := a.ws
			seq := a.lastSeq
			a.mu.Unlock()
			if ws == nil {
				return
			}
			payload := map[string]interface{}{"op": wsHeartbeat, "d": seq}
			data, _ := json.Marshal(payload)
			if err := ws.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		}
	}
}

// handleDispatch processes an op-0 dispatch frame.
func (a *Adapter) handleDispatch(frame map[string]interface{}) {
	t, _ := frame["t"].(string)
	d, _ := frame["d"].(map[string]interface{})
	if s, ok := frame["s"].(float64); ok {
		a.mu.Lock()
		a.lastSeq = int(s)
		a.mu.Unlock()
	}
	switch t {
	case "READY":
		a.mu.Lock()
		if sid, ok := d["session_id"].(string); ok {
			a.wsSessionID = sid
		}
		if user, ok := d["user"].(map[string]interface{}); ok {
			if uid, ok := user["id"].(string); ok {
				a.SelfID = uid
			}
		}
		a.mu.Unlock()
		logger.Info("[QQOfficial] 机器人 %s 启动成功！", a.SelfID)
	case "RESUMED":
		logger.Info("[QQOfficial] 机器人重连成功")
	case "C2C_MESSAGE_CREATE":
		a.handleMessage(d, "friend")
	case "GROUP_AT_MESSAGE_CREATE":
		a.handleGroupMessage(d, true)
	case "GROUP_MESSAGE_CREATE":
		a.handleGroupMessage(d, false)
	case "AT_MESSAGE_CREATE":
		a.handleChannelMessage(d)
	case "DIRECT_MESSAGE_CREATE":
		a.handleDirectMessage(d)
	}
}

// parseFaceMessage converts <faceType=...> tags to readable text.
func parseFaceMessage(content string) string {
	re := regexp.MustCompile(`<faceType=\d+[^>]*>`)
	return re.ReplaceAllStringFunc(content, func(tag string) string {
		extMatch := regexp.MustCompile(`ext="([^"]*)"`).FindStringSubmatch(tag)
		if len(extMatch) > 1 {
			if decoded, err := base64.StdEncoding.DecodeString(extMatch[1]); err == nil {
				var ext map[string]interface{}
				if json.Unmarshal(decoded, &ext) == nil {
					if text, ok := ext["text"].(string); ok && text != "" {
						return "[表情:" + text + "]"
					}
				}
			}
		}
		return "[表情]"
	})
}

// attachmentToComponent converts an attachment dict to a message component.
func attachmentToComponent(att map[string]interface{}) message.Component {
	contentType := ""
	if v, ok := att["content_type"].(string); ok {
		contentType = strings.ToLower(v)
	}
	url, _ := att["url"].(string)
	if url != "" && !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "https://" + url
	}
	filename, _ := att["filename"].(string)
	if filename == "" {
		filename = "attachment"
	}
	if url == "" {
		return nil
	}
	ext := strings.ToLower(extOf(filename))
	if strings.HasPrefix(contentType, "image") || isIn(ext, ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp") {
		return message.ImageFromURL(url)
	}
	if strings.HasPrefix(contentType, "voice") || isIn(ext, ".mp3", ".wav", ".ogg", ".m4a", ".amr", ".silk") {
		return &message.Record{URL: url}
	}
	if strings.HasPrefix(contentType, "video") || isIn(ext, ".mp4", ".mov", ".avi", ".mkv", ".webm") {
		return &message.Video{URL: url}
	}
	return &message.File{URL: url, Name: filename}
}

func extOf(name string) string {
	idx := strings.LastIndex(name, ".")
	if idx < 0 {
		return ""
	}
	return name[idx:]
}

func isIn(v string, items ...string) bool {
	for _, it := range items {
		if v == it {
			return true
		}
	}
	return false
}

// handleMessage parses and publishes a C2C (friend) message.
func (a *Adapter) handleMessage(d map[string]interface{}, scene string) {
	content, _ := d["content"].(string)
	msgID, _ := d["id"].(string)
	msgType, _ := d["message_type"].(float64)

	var senderOpenID, senderName string
	if author, ok := d["author"].(map[string]interface{}); ok {
		senderOpenID, _ = author["user_openid"].(string)
		senderName, _ = author["username"].(string)
	}
	if senderOpenID == "" {
		senderOpenID, _ = d["author"].(map[string]interface{})["member_openid"].(string)
	}
	// C2C messages carry no username (the QQ API does not expose it), so the
	// nickname is left empty here; the pipeline resolves it via the user's
	// `/name` alias when building the system reminder.
	if senderOpenID == "" {
		senderOpenID, _ = d["author"].(map[string]interface{})["member_openid"].(string)
	}

	chain := []message.Component{}
	if int(msgType) == 103 {
		chain = append(chain, a.buildQuotedReply(d))
	}
	plain := parseFaceMessage(strings.TrimSpace(content))
	chain = append(chain, &message.At{TargetID: "qq_official"})
	chain = append(chain, &message.Plain{Text: plain})
	chain = append(chain, a.parseAttachments(d)...)

	a.remember(scene, senderOpenID, msgID)
	a.publish(senderOpenID, senderName, senderOpenID, false, plain, msgID, chain, d)
}

// handleGroupMessage parses and publishes a group message.
func (a *Adapter) handleGroupMessage(d map[string]interface{}, forceMention bool) {
	groupOpenID, _ := d["group_openid"].(string)
	content, _ := d["content"].(string)
	msgID, _ := d["id"].(string)
	msgType, _ := d["message_type"].(float64)

	var memberOpenID, senderName string
	if author, ok := d["author"].(map[string]interface{}); ok {
		memberOpenID, _ = author["member_openid"].(string)
		senderName, _ = author["username"].(string)
	}

	// extract bot mentions
	botMentionIDs := []string{}
	mentions, _ := d["mentions"].([]interface{})
	for _, m := range mentions {
		mm, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		isYou, _ := mm["is_you"].(bool)
		mid, _ := mm["id"].(string)
		if isYou && mid != "" {
			botMentionIDs = append(botMentionIDs, mid)
		}
	}
	mentioned := forceMention || len(botMentionIDs) > 0

	plainRaw := content
	for _, mid := range botMentionIDs {
		plainRaw = strings.ReplaceAll(plainRaw, "<@"+mid+">", "")
		plainRaw = strings.ReplaceAll(plainRaw, "<@!"+mid+">", "")
	}
	plain := parseFaceMessage(strings.TrimSpace(plainRaw))

	chain := []message.Component{}
	if int(msgType) == 103 {
		chain = append(chain, a.buildQuotedReply(d))
	}
	selfID := "qq_official"
	if len(botMentionIDs) > 0 {
		selfID = botMentionIDs[0]
		a.mu.Lock()
		a.SelfID = selfID
		a.mu.Unlock()
	}
	if mentioned {
		name := ""
		if len(botMentionIDs) > 0 {
			if mm, ok := mentions[0].(map[string]interface{}); ok {
				name, _ = mm["username"].(string)
			}
		}
		chain = append(chain, &message.At{TargetID: selfID, Name: name})
	}
	chain = append(chain, &message.Plain{Text: plain})
	chain = append(chain, a.parseAttachments(d)...)

	a.remember("group", groupOpenID, msgID)
	a.publish(memberOpenID, senderName, groupOpenID, true, plain, msgID, chain, d)
}

// handleChannelMessage parses and publishes a guild (@) message.
func (a *Adapter) handleChannelMessage(d map[string]interface{}) {
	channelID, _ := d["channel_id"].(string)
	content, _ := d["content"].(string)
	msgID, _ := d["id"].(string)

	var authorID, authorName string
	if author, ok := d["author"].(map[string]interface{}); ok {
		authorID = fmt.Sprintf("%v", author["id"])
		authorName, _ = author["username"].(string)
	}
	selfID := ""
	if mentions, ok := d["mentions"].([]interface{}); ok && len(mentions) > 0 {
		if m, ok := mentions[0].(map[string]interface{}); ok {
			selfID = fmt.Sprintf("%v", m["id"])
		}
	}
	plain := parseFaceMessage(strings.TrimSpace(strings.ReplaceAll(content, "<@!"+selfID+">", "")))

	chain := []message.Component{}
	chain = append(chain, a.parseAttachments(d)...)
	chain = append(chain, &message.At{TargetID: "qq_official"})
	chain = append(chain, &message.Plain{Text: plain})

	a.remember("channel", channelID, msgID)
	a.publish(authorID, authorName, channelID, true, plain, msgID, chain, d)
}

// handleDirectMessage parses and publishes a direct (DM) message.
func (a *Adapter) handleDirectMessage(d map[string]interface{}) {
	content, _ := d["content"].(string)
	msgID, _ := d["id"].(string)
	var authorID, authorName string
	if author, ok := d["author"].(map[string]interface{}); ok {
		authorID = fmt.Sprintf("%v", author["id"])
		authorName, _ = author["username"].(string)
	}
	plain := parseFaceMessage(strings.TrimSpace(content))
	chain := []message.Component{}
	chain = append(chain, a.parseAttachments(d)...)
	chain = append(chain, &message.At{TargetID: "qq_official"})
	chain = append(chain, &message.Plain{Text: plain})

	a.remember("friend", authorID, msgID)
	a.publish(authorID, authorName, authorID, false, plain, msgID, chain, d)
}

// buildQuotedReply builds a Reply component from a quoted message (message_type 103).
func (a *Adapter) buildQuotedReply(d map[string]interface{}) message.Component {
	reply := &message.Reply{}
	if elems, ok := d["msg_elements"].([]interface{}); ok && len(elems) > 0 {
		if e, ok := elems[0].(map[string]interface{}); ok {
			if id, ok := e["id"].(string); ok {
				reply.MessageID = id
			}
			if content, ok := e["content"].(string); ok {
				reply.MessageStr = parseFaceMessage(strings.TrimSpace(content))
				reply.Chain = append(reply.Chain, &message.Plain{Text: reply.MessageStr})
			}
			if atts, ok := e["attachments"].([]interface{}); ok {
				for _, att := range atts {
					if am, ok := att.(map[string]interface{}); ok {
						if c := attachmentToComponent(am); c != nil {
							reply.Chain = append(reply.Chain, c)
						}
					}
				}
			}
		}
	}
	return reply
}

// parseAttachments converts the attachments array to components.
func (a *Adapter) parseAttachments(d map[string]interface{}) []message.Component {
	result := []message.Component{}
	if atts, ok := d["attachments"].([]interface{}); ok {
		for _, att := range atts {
			if am, ok := att.(map[string]interface{}); ok {
				if c := attachmentToComponent(am); c != nil {
					result = append(result, c)
				}
			}
		}
	}
	return result
}

func (a *Adapter) remember(scene, convID, msgID string) {
	if convID == "" {
		return
	}
	a.mu.Lock()
	a.sessionScene[convID] = scene
	if msgID != "" {
		a.sessionLastMsg[convID] = msgID
	}
	a.mu.Unlock()
}

func (a *Adapter) publish(senderID, senderName, convID string, isGroup bool, msgStr, msgID string, chain []message.Component, raw interface{}) {
	logger.Info("[QQOfficial] received message from %s (group=%v): %q", convID, isGroup, msgStr)
	msgObj := platform.NewAstrBotMessage()
	if isGroup {
		msgObj.Type = platform.GroupMessage
		msgObj.Group = &platform.Group{GroupID: convID}
	} else {
		msgObj.Type = platform.FriendMessage
	}
	msgObj.SelfID = a.SelfID
	msgObj.SessionID = convID
	msgObj.MessageID = msgID
	msgObj.Sender = platform.MessageMember{UserID: senderID, Nickname: senderName}
	msgObj.Message = chain
	msgObj.MessageStr = msgStr
	msgObj.RawMessage = raw
	_ = a.PublishEvent(msgStr, msgObj)
}

// ---------------------------------------------------------------------------
// Sending
// ---------------------------------------------------------------------------

// Send sends a message chain to a session (convID).
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	if chain == nil || len(chain.Chain) == 0 {
		return nil
	}
	a.mu.Lock()
	scene := a.sessionScene[sessionID]
	lastMsgID := a.sessionLastMsg[sessionID]
	a.mu.Unlock()

	plainText, imageRef, fileRef, fileName := extractSendParts(chain)

	switch scene {
	case "friend":
		return a.sendC2C(sessionID, plainText, imageRef, fileRef, fileName, lastMsgID)
	case "group":
		return a.sendGroup(sessionID, plainText, imageRef, fileRef, fileName, lastMsgID)
	case "channel":
		return a.sendChannel(sessionID, plainText, imageRef)
	default:
		// fallback: treat as friend (C2C)
		return a.sendC2C(sessionID, plainText, imageRef, fileRef, fileName, lastMsgID)
	}
}

// extractSendParts pulls plain text and media refs from a message chain.
func extractSendParts(chain *message.MessageChain) (plain string, imageRef string, fileRef string, fileName string) {
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
			}
		case *message.Video:
			if fileRef == "" {
				if comp.Path != "" {
					fileRef = comp.Path
				} else {
					fileRef = comp.URL
				}
			}
		case *message.Record:
			if fileRef == "" {
				if comp.Path != "" {
					fileRef = comp.Path
				} else {
					fileRef = comp.URL
				}
			}
		}
	}
	return plain, imageRef, fileRef, fileName
}

func readFileBase64(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(data)
}

// apiRequest performs an authenticated QQ Open Platform API request.
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
	req.Header.Set("X-Union-Appid", a.AppID)
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

// uploadFile uploads a media file for a C2C or group target.
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
		fileData = strings.SplitN(fileData, ",", 2)[1]
	}
	if kind == "friend" {
		payload["openid"] = targetID
		path = "/v2/users/" + targetID + "/files"
	} else {
		payload["group_openid"] = targetID
		path = "/v2/groups/" + targetID + "/files"
	}
	// If the file ref looks like a URL, pass it directly
	if strings.HasPrefix(fileData, "http://") || strings.HasPrefix(fileData, "https://") {
		payload["url"] = fileData
	} else {
		payload["file_data"] = fileData
	}
	return a.apiRequest(http.MethodPost, path, payload)
}

// sendC2C sends a message to a C2C user.
func (a *Adapter) sendC2C(openID, plainText, imageRef, fileRef, fileName, msgID string) error {
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
		fileType := fileTypeFile
		if fileName == "" {
			fileType = fileTypeVideo
		}
		media, err := a.uploadFile("friend", openID, fileRef, fileType, fileName)
		if err != nil {
			return err
		}
		payload["media"] = media
		payload["msg_type"] = 7
	}
	return a.postMessage("/v2/users/"+openID+"/messages", payload)
}

// sendGroup sends a message to a group.
func (a *Adapter) sendGroup(groupOpenID, plainText, imageRef, fileRef, fileName, msgID string) error {
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
		fileType := fileTypeFile
		if fileName == "" {
			fileType = fileTypeVideo
		}
		media, err := a.uploadFile("group", groupOpenID, fileRef, fileType, fileName)
		if err != nil {
			return err
		}
		payload["media"] = media
		payload["msg_type"] = 7
	}
	return a.postMessage("/v2/groups/"+groupOpenID+"/messages", payload)
}

// sendChannel sends a message to a guild channel.
func (a *Adapter) sendChannel(channelID, plainText, imageRef string) error {
	payload := map[string]interface{}{"content": plainText}
	if imageRef != "" {
		payload["file_image"] = imageRef
	}
	return a.postMessage("/channels/"+channelID+"/messages", payload)
}

func (a *Adapter) postMessage(path string, payload map[string]interface{}) error {
	data, err := a.apiRequest(http.MethodPost, path, payload)
	if err != nil {
		return err
	}
	if data != nil {
		if id, ok := data["id"].(string); ok {
			// refresh cached msg id happens on next remember
			_ = id
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// QQ C2C native streaming protocol (state=1 start/update, state=10 end).
// Mirrors Python's qqofficial_message_event.send_streaming: each fragment
// carries the FULL accumulated text and replaces the same message, so the
// client sees a single progressively-updated message.
// ---------------------------------------------------------------------------

// streamFragment sends one C2C streaming fragment and returns the message id.
func (a *Adapter) streamFragment(openID string, state int, id string, index int, text string) (string, error) {
	payload := map[string]interface{}{
		"content":  text,
		"msg_type": 1,
		"msg_seq":  rand.Intn(10000) + 1,
		"state":    state,
		"index":    index,
		"reset":    false,
	}
	if id != "" {
		payload["id"] = id
	}
	data, err := a.apiRequest(http.MethodPost, "/v2/users/"+openID+"/messages", payload)
	if err != nil {
		return "", err
	}
	msgID := ""
	if data != nil {
		if v, ok := data["id"].(string); ok {
			msgID = v
		}
	}
	return msgID, nil
}

// StreamStart opens a C2C streaming message.
func (a *Adapter) StreamStart(sessionID, text string) (string, error) {
	return a.streamFragment(sessionID, 1, "", 0, text)
}

// StreamUpdate updates an in-progress C2C streaming message.
func (a *Adapter) StreamUpdate(sessionID, msgID, text string) error {
	_, err := a.streamFragment(sessionID, 1, msgID, 1, text)
	return err
}

// StreamEnd finalizes the C2C streaming message.
func (a *Adapter) StreamEnd(sessionID, msgID, text string) error {
	_, err := a.streamFragment(sessionID, 10, msgID, 2, text)
	return err
}
