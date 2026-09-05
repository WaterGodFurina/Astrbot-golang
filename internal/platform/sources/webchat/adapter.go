// Package webchat implements a built-in web chat platform adapter.
// Ported from astrbot/core/platform/sources/webchat/
package webchat

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

var logger = log.GetDefault().WithComponent("WebChat")

// Adapter provides a web-based chat interface.
type Adapter struct {
	Host      string
	Port      int
	AuthToken string
	server    *http.Server
	EventBus  *core.EventBus
	mu        sync.Mutex
	clients   map[string]chan *message.MessageChain
	// pollClients 是 /poll 长轮询专用通道（与 /chat 的同步回复通道分离），
	// 避免同一 session 的回复被 /chat 与 /poll 两个消费者竞争抢走。
	pollClients map[string]chan *message.MessageChain
	// pollRefs 记录每个会话当前在等待的 /poll 长轮询数量，用于轮询结束后
	// 引用计数归零时清理 pollClients 通道，防止通道永久泄漏。
	pollRefs map[string]int
}

// New creates a WebChat adapter.
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		EventBus:    eventBus,
		clients:     make(map[string]chan *message.MessageChain),
		pollClients: make(map[string]chan *message.MessageChain),
		pollRefs:    make(map[string]int),
	}
	a.Host, _ = config["host"].(string)
	if a.Host == "" {
		a.Host = "0.0.0.0"
	}
	if port, ok := config["port"].(float64); ok {
		a.Port = int(port)
	}
	if a.Port == 0 {
		// 独立默认端口，避免与企业微信回调端口（6195）冲突
		a.Port = 6193
	}
	a.AuthToken, _ = config["auth_token"].(string)
	return a
}

// SetEventBus injects the event bus (implements platform.EventBusSetter so the
// lifecycle wires it after construction, since the factory passes nil).
func (a *Adapter) SetEventBus(bus platform.EventBus) {
	if be, ok := bus.(*core.EventBus); ok {
		a.EventBus = be
	}
}

// Start starts the web chat server.
func (a *Adapter) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/chat", a.handleChat)
	mux.HandleFunc("/poll", a.handlePoll)

	a.server = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", a.Host, a.Port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		logger.I18nInfo("WebChat 适配器正在监听 %s:%d", a.Host, a.Port)
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			// 端口冲突提示：与其他适配器（如企业微信默认 6195）或系统服务同端口时给出线索
			logger.Error("WebChat server error: %v (若为端口被占用, 请检查是否与其他适配器/服务的端口冲突)", err)
		}
	}()

	return nil
}

// Stop stops the adapter.
func (a *Adapter) Stop() error {
	if a.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return a.server.Shutdown(ctx)
	}
	return nil
}

// ID returns the adapter ID.
func (a *Adapter) ID() string { return "webchat" }

// Type returns the platform type.
func (a *Adapter) Type() string { return "webchat" }

// Send sends a message to a web chat client.
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	a.mu.Lock()
	ch := a.clients[sessionID]
	pch := a.pollClients[sessionID]
	a.mu.Unlock()
	if ch == nil && pch == nil {
		return fmt.Errorf("client %s not connected", sessionID)
	}
	// 优先投递给 /chat 的同步请求（该请求持有自己的专用回复通道，不会被
	// /poll 抢走）；若 /chat 无人在等待则退化为投递给 /poll 长轮询端。
	// 两个通道均为非阻塞写入：通道满/无人消费时丢弃，绝不阻塞回复链路。
	if ch != nil {
		select {
		case ch <- chain:
			return nil
		default:
		}
	}
	if pch != nil {
		select {
		case pch <- chain:
			return nil
		default:
		}
	}
	return fmt.Errorf("send timeout")
}

// isAuthorized 校验 /chat 与 /poll 的访问身份。配置了 auth_token 时要求
// `Authorization: Bearer <token>` 或 `?token=`；未配置时仅允许本机回环来源
// 访问，避免 /chat 被任意来源盗刷 LLM 额度、/poll 窃取他人会话回复。
func (a *Adapter) isAuthorized(r *http.Request) bool {
	if token := strings.TrimSpace(a.AuthToken); token != "" {
		provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if provided == "" {
			provided = strings.TrimSpace(r.URL.Query().Get("token"))
		}
		return subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// handleChat handles incoming chat messages.
func (a *Adapter) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAuthorized(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
		// Message 对齐本体 payload["message"]：可以是字符串，也可以是
		// message parts 数组（[{type:"plain",text:...},...]，对齐 dashboard
		// 链路模式），解码后按形态分流。
		Message json.RawMessage `json:"message"`
		// SenderID / Flags 同本体队列 payload 字段。
		SenderID string                 `json:"sender_id"`
		Flags    map[string]interface{} `json:"flags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	if req.SessionID == "" {
		req.SessionID = fmt.Sprintf("webchat_%d", time.Now().UnixNano())
	}
	if req.SenderID == "" {
		req.SenderID = req.SessionID
	}

	// Create message chain: message 为 parts 数组时按组件解析（对齐本体
	// convert_message → parse_webchat_message_parts），否则按字符串处理。
	chain := &message.MessageChain{}
	var messageText string
	textOnly := true
	if len(req.Message) > 0 && !isJSONString(req.Message) {
		var parts []map[string]interface{}
		if err := json.Unmarshal(req.Message, &parts); err == nil && len(parts) > 0 {
			textOnly = false
			chain.Chain = parseMessageParts(parts)
			messageText = chainPlainTextConcat(chain)
		}
	}
	if textOnly {
		_ = json.Unmarshal(req.Message, &messageText)
		chain.Chain = []message.Component{&message.Plain{Text: messageText}}
	}

	// 每个 /chat 请求使用独立的回复通道（先注册再发布事件，避免回复在
	// 注册前到达导致丢失），请求结束即移除，防止旧回复残留串扰后续请求。
	replyCh := make(chan *message.MessageChain, 1)
	a.mu.Lock()
	a.clients[req.SessionID] = replyCh
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if a.clients[req.SessionID] == replyCh {
			delete(a.clients, req.SessionID)
		}
		a.mu.Unlock()
	}()

	// Publish event
	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   "webchat",
			PlatformID: a.ID(),
			SelfID:     "webchat",
			SenderID:   req.SenderID,
			SenderName: req.SenderID,
			ConvID:     req.SessionID,
			IsGroup:    false,
		},
		Message:    chain,
		MessageStr: messageText,
		Timestamp:  time.Now(),
		Metadata:   make(map[string]interface{}),
	}
	// flags 未传默认 true（对齐本体 request_flags.resolve_webchat_request_flags）。
	event.Metadata["flags"] = resolveRequestFlags(req.Flags)
	// 对齐本体 webchat_adapter.create_event 注入 extra 的字段。
	event.Metadata["action_type"] = "chat"

	// typing 信号（对齐本体 send_typing）：发布事件前向等待中的 /poll
	// 投递哨兵，客户端感知"开始处理"。
	a.sendTyping(req.SessionID)

	if a.EventBus == nil {
		logger.Error("webchat event bus not configured; cannot publish")
		http.Error(w, "Event bus not configured", http.StatusInternalServerError)
		return
	}
	if err := a.EventBus.Publish(event); err != nil {
		logger.Error("Failed to publish event: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	// Wait for the reply on the request-owned channel.
	select {
	case resp := <-replyCh:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"session_id": req.SessionID,
			"reply":      extractPlainText(resp),
			// 对齐本体 send_typing 信号：携带 run 起始标记（响应即运行开始）。
			"run_started": true,
			"parts":       chainToReplyParts(resp),
		})
	case <-time.After(60 * time.Second):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"session_id":  req.SessionID,
			"reply":       "Timeout waiting for response",
			"run_started": false,
		})
	}
}

// isJSONString 判断原始 JSON 值是否为字符串字面量（以引号开头）。
func isJSONString(raw json.RawMessage) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '"':
			return true
		default:
			return false
		}
	}
	return false
}

// resolveRequestFlags 对齐本体 request_flags.resolve_webchat_request_flags：
// flags[key] 显式 bool 优先，未传默认 true。
func resolveRequestFlags(flags map[string]interface{}) map[string]interface{} {
	resolved := make(map[string]interface{}, 3)
	for _, key := range []string{"enable_inline_genui", "enable_default_system_prompt", "enable_streaming"} {
		value := true
		if b, ok := flags[key].(bool); ok {
			value = b
		}
		resolved[key] = value
	}
	return resolved
}

// parseMessageParts 把 message parts 数组解析为组件链（对齐本体
// parse_webchat_message_parts 的非严格模式：未知类型跳过）。
func parseMessageParts(parts []map[string]interface{}) []message.Component {
	var out []message.Component
	for _, p := range parts {
		if p == nil {
			continue
		}
		partType, _ := p["type"].(string)
		switch partType {
		case "plain", "":
			text, _ := p["text"].(string)
			if text != "" {
				out = append(out, &message.Plain{Text: text})
			}
		case "image":
			out = append(out, mediaComponentFromPart(p, "image"))
		case "record":
			out = append(out, mediaComponentFromPart(p, "record"))
		case "video":
			out = append(out, mediaComponentFromPart(p, "video"))
		case "file":
			out = append(out, mediaComponentFromPart(p, "file"))
		}
	}
	if len(out) == 0 {
		out = append(out, &message.Plain{})
	}
	return out
}

// mediaComponentFromPart 把媒体 part 转为对应组件（本地 path/file、url、
// base64 均接受；对齐本体 media part 的 path/url 字段语义）。
func mediaComponentFromPart(p map[string]interface{}, kind string) message.Component {
	path, _ := p["path"].(string)
	if path == "" {
		if f, ok := p["file"].(string); ok {
			path = f
		}
	}
	url, _ := p["url"].(string)
	b64, _ := p["base64"].(string)
	name, _ := p["filename"].(string)
	switch kind {
	case "image":
		return &message.Image{Path: path, File: path, URL: url, Base64: b64}
	case "record":
		return &message.Record{Path: path, File: path, URL: url, Base64: b64}
	case "video":
		return &message.Video{Path: path, URL: url}
	default:
		return &message.File{Path: path, URL: url, Name: name}
	}
}

// chainPlainTextConcat 提取链内全部 plain 文本（无分隔拼接）。
func chainPlainTextConcat(mc *message.MessageChain) string {
	if mc == nil {
		return ""
	}
	var b strings.Builder
	for _, comp := range mc.Chain {
		if plain, ok := comp.(*message.Plain); ok {
			b.WriteString(plain.Text)
		}
	}
	return b.String()
}

// chainToReplyParts 把回复链转为 message parts 数组（plain 文本 + 媒体
// 占位），让 /chat 调用方能拿到全组件回复（对齐本体分帧语义的同步版本）。
func chainToReplyParts(mc *message.MessageChain) []map[string]interface{} {
	parts := make([]map[string]interface{}, 0)
	if mc == nil {
		return parts
	}
	for _, comp := range mc.Chain {
		switch c := comp.(type) {
		case *message.Plain:
			if c.Text != "" {
				parts = append(parts, map[string]interface{}{"type": "plain", "text": c.Text})
			}
		case *message.Json:
			if b, err := json.Marshal(c.Data); err == nil {
				parts = append(parts, map[string]interface{}{"type": "plain", "text": string(b)})
			}
		case *message.Image:
			parts = append(parts, map[string]interface{}{"type": "image", "url": c.URL, "path": c.Path, "base64": c.Base64})
		case *message.Record:
			parts = append(parts, map[string]interface{}{"type": "record", "url": c.URL, "path": c.Path})
		case *message.Video:
			parts = append(parts, map[string]interface{}{"type": "video", "url": c.URL, "path": c.Path})
		case *message.File:
			parts = append(parts, map[string]interface{}{"type": "file", "url": c.URL, "path": c.Path, "filename": c.Name})
		}
	}
	return parts
}

// webchatTypingSentinel 是 typing 信号哨兵链（对齐本体 webchat_event.send_typing
// 在 LLM 请求前向 back_queue 发 run_started 帧的语义）。用指针相等判断，
// 不会与真实回复混淆。
var webchatTypingSentinel = &message.MessageChain{Type: "typing"}

// sendTyping 向会话的 /poll 长轮询通道投递 typing 信号。非阻塞投递，
// 无等待中的 /poll 时信号直接丢弃（等价于无订阅者的 run_started）。
func (a *Adapter) sendTyping(sessionID string) {
	a.mu.Lock()
	pch := a.pollClients[sessionID]
	a.mu.Unlock()
	if pch == nil {
		return
	}
	select {
	case pch <- webchatTypingSentinel:
	default:
	}
}

// handlePoll handles long-polling for responses.
func (a *Adapter) handlePoll(w http.ResponseWriter, r *http.Request) {
	if !a.isAuthorized(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		http.Error(w, "Missing session_id", http.StatusBadRequest)
		return
	}

	// /poll 使用独立的轮询通道（pollClients），与 /chat 的回复通道分离，
	// 二者互不抢数据。
	a.mu.Lock()
	if _, ok := a.pollClients[sessionID]; !ok {
		a.pollClients[sessionID] = make(chan *message.MessageChain, 10)
	}
	a.pollRefs[sessionID]++
	ch := a.pollClients[sessionID]
	a.mu.Unlock()
	// 轮询结束（收到回复、超时或客户端断开）后引用计数归零时清理通道，
	// 防止 pollClients 随任意 session_id 无限增长。
	defer func() {
		a.mu.Lock()
		a.pollRefs[sessionID]--
		if a.pollRefs[sessionID] <= 0 {
			delete(a.pollRefs, sessionID)
			delete(a.pollClients, sessionID)
		}
		a.mu.Unlock()
	}()

	select {
	case resp := <-ch:
		if resp == webchatTypingSentinel {
			// typing 信号（对齐本体 run_started 帧）：立即返回，客户端
			// 感知"开始处理"并继续轮询，真实回复由下一次 /poll 取走。
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"reply":  nil,
				"typing": true,
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"reply": extractPlainText(resp),
			"parts": chainToReplyParts(resp),
		})
	case <-r.Context().Done():
		// 客户端断开连接：直接返回，由 defer 清理轮询通道。
	case <-time.After(30 * time.Second):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"reply": nil,
		})
	}
}

func extractPlainText(mc *message.MessageChain) string {
	if mc == nil {
		return ""
	}
	var result string
	for _, comp := range mc.Chain {
		if plain, ok := comp.(*message.Plain); ok {
			result += plain.Text
		}
	}
	return result
}
