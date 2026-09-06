// Package aiocqhttp implements the OneBot v11 platform adapter.
// Ported from astrbot/core/platform/sources/aiocqhttp/
//
// Supported modes:
//   - Reverse WebSocket (OneBot 实现主动连入本服务的 /ws 端点；事件与 API 调用
//     都通过同一条连接传输) — 推荐，Send 通过连接下发 send_msg。
//   - HTTP POST (OneBot 实现向 / 推送事件)。无可用 WebSocket 连接时 Send 会报错。
package aiocqhttp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

var logger = log.GetDefault().WithComponent("aiocqhttp")

// qqHousekeeperID 是 QQ 官方助手（"QQ 管家"）的账号，其推送的消息非真实
// 用户消息，直接过滤（对齐 Python adapter.py:133-135）。
const qqHousekeeperID = "2854196310"

// randomHex 生成 n 字节随机数的十六进制串（用作 request 等无原生 message_id
// 事件的本地唯一 ID，对齐 Python 的 uuid4().hex）。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// Adapter implements the OneBot v11 reverse WebSocket protocol.
type Adapter struct {
	platform.BaseAdapter
	Host     string
	Port     int
	Token    string
	server   *http.Server
	EventBus *core.EventBus
	SelfID   string
	upgrader websocket.Upgrader

	// allowedOrigins is the WebSocket Origin 白名单（配置 ws_reverse_origins，
	// 逗号分隔）。非浏览器客户端（OneBot 实现）不发 Origin，放行；带 Origin 的
	// 浏览器连接必须命中白名单，否则拒绝（防 CSWSH 跨站 WebSocket 劫持）。
	allowedOrigins []string

	// maxReverseWSConns is the cap on concurrent reverse-WS connections
	// (config ws_reverse_max_conns, default defaultMaxReverseWSConns). 0/negative
	// means the default applies.
	maxReverseWSConns int

	mu          sync.Mutex
	conns       map[*websocket.Conn]struct{}    // active reverse-WS connections
	connWriteMu map[*websocket.Conn]*sync.Mutex // per-connection write serialization
	groupConvs  map[string]bool                 // convID -> is group (from received events)

	pendingMu sync.Mutex
	pending   map[string]chan map[string]interface{} // echo -> response channel

	// quotedParser carries quoted_message_parser settings for forward-message
	// fetching (get_forward_msg / get_msg).
	quotedParser quotedParserSettings
}

// New creates an aiocqhttp adapter from config.
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	id, _ := config["id"].(string)
	a := &Adapter{
		// BaseAdapter 必须初始化（id/platform）：PlatformManager 按
		// ID()/Type() 注册与解析（Send/React/resolveAdapter 回退按 Type），
		// 缺失会导致插件经 HostService.SendMessage 发送时
		// "platform %q not found"（box 等 OneBot 插件回发失败）。
		BaseAdapter:    *platform.NewBaseAdapter(id, "aiocqhttp"),
		EventBus:       eventBus,
		conns:          make(map[*websocket.Conn]struct{}),
		connWriteMu:    make(map[*websocket.Conn]*sync.Mutex),
		groupConvs:     make(map[string]bool),
		pending:        make(map[string]chan map[string]interface{}),
		allowedOrigins: parseOriginList(config["ws_reverse_origins"]),
	}
	a.upgrader = websocket.Upgrader{
		CheckOrigin: a.checkOrigin,
	}
	a.Host, _ = config["ws_reverse_host"].(string)
	if a.Host == "" {
		a.Host = "0.0.0.0"
	}
	if port, ok := config["ws_reverse_port"].(float64); ok {
		a.Port = int(port)
	}
	if a.Port == 0 {
		a.Port = 6199
	}
	a.Token, _ = config["ws_reverse_token"].(string)
	if n, ok := config["ws_reverse_max_conns"].(float64); ok && n > 0 {
		a.maxReverseWSConns = int(n)
	} else if n, ok := config["ws_reverse_max_conns"].(int); ok && n > 0 {
		a.maxReverseWSConns = n
	}
	if id != "" {
		a.SelfID = id
	}
	a.quotedParser = resolveQuotedParserSettings(settings)
	return a
}

// parseOriginList reads the ws_reverse_origins config value, which may be a
// comma-separated string or a list of strings.
func parseOriginList(raw interface{}) []string {
	var origins []string
	switch v := raw.(type) {
	case string:
		for _, o := range strings.Split(v, ",") {
			if o = strings.TrimSpace(o); o != "" {
				origins = append(origins, o)
			}
		}
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					origins = append(origins, s)
				}
			}
		}
	}
	return origins
}

// checkOrigin gates the reverse-WebSocket upgrade on the request's Origin
// header. Non-browser clients (OneBot 实现) do not send Origin and are
// allowed; a present Origin must match the configured whitelist, otherwise the
// connection is rejected to prevent cross-site WebSocket hijacking (CSWSH).
func (a *Adapter) checkOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	for _, allowed := range a.allowedOrigins {
		if strings.EqualFold(origin, allowed) {
			return true
		}
	}
	logger.Warn("aiocqhttp: 拒绝跨站 WebSocket 连接 (Origin=%q 不在 ws_reverse_origins 白名单)", origin)
	return false
}

// SetEventBus injects the event bus. This overrides BaseAdapter.SetEventBus so
// both the embedded bus (used by PublishEvent) and this adapter's own field
// (used by handleEvent) are wired, since lifecycle creates adapters via the
// factory with a nil bus and injects it afterwards.
func (a *Adapter) SetEventBus(bus platform.EventBus) {
	a.BaseAdapter.SetEventBus(bus)
	if be, ok := bus.(*core.EventBus); ok {
		a.EventBus = be
	}
}

// Start starts the HTTP server for reverse WebSocket connections.
func (a *Adapter) Start(ctx context.Context) error {
	// ⚠️ 不要把 Host 强制改成 127.0.0.1（曾作为"secure-by-default"引入后
	// 已撤销）：0.0.0.0 监听是 Docker 桥接与跨服务器 OneBot 客户端（反向
	// WS）的硬需求，缺 token 只告警——对齐 Python 原版 run() 的行为。
	if a.Token == "" {
		logger.I18nWarn("aiocqhttp: 未配置 ws_reverse_token，事件入口（GET /ws）未做访问鉴权。若本端口可被公网访问，请配置访问令牌")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleHTTP)
	mux.HandleFunc("/ws", a.handleWebSocket)

	addr := fmt.Sprintf("%s:%d", a.Host, a.Port)
	// 监听带重试（对齐 dashboard.listenWithRetry）：WebUI 重启（Restart）时
	// 新实例先启动、旧实例后 Stop，旧实例仍占着端口 → bind EADDRINUSE；
	// 失败不重试会让适配器从此无监听（OneBot 客户端 connect ECONNREFUSED，
	// 如 snowluma 连 192.168.3.9:6190 被拒）。旧实例退出释放端口后自动绑上。
	ln, err := listenWithRetry(ctx, addr)
	if err != nil {
		return err
	}
	a.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.I18nInfo("aiocqhttp(OneBot v11) 适配器正在监听 %s", addr)
		if err := a.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Error("aiocqhttp server error: %v", err)
		}
	}()

	return nil
}

// listenWithRetry binds addr, retrying while the address is in use (the
// previous host instance may still be releasing the port during a restart).
func listenWithRetry(ctx context.Context, addr string) (net.Listener, error) {
	const (
		maxAttempts  = 40
		retryBackoff = 500 * time.Millisecond
	)
	for attempt := 1; ; attempt++ {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
		if !isAddrInUse(err) {
			return nil, fmt.Errorf("bind aiocqhttp %s: %w", addr, err)
		}
		if attempt >= maxAttempts {
			return nil, fmt.Errorf("bind aiocqhttp %s: port still in use after %d attempts: %w", addr, maxAttempts, err)
		}
		logger.I18nWarn("aiocqhttp 监听端口 %s 仍被占用，等待释放后重试（%d/%d）", addr, attempt, maxAttempts)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryBackoff):
		}
	}
}

func isAddrInUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return errors.Is(opErr.Err, syscall.EADDRINUSE)
	}
	return false
}

// Stop stops the adapter, shutting down the HTTP server and closing all
// reverse-WS connections so their read-loop goroutines exit.
func (a *Adapter) Stop() error {
	var shutdownErr error
	if a.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr = a.server.Shutdown(ctx)
	}
	a.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(a.conns))
	for c := range a.conns {
		conns = append(conns, c)
	}
	a.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
		a.removeConn(c)
	}
	return shutdownErr
}

// Send sends a message chain to a session.
//
// The target session (from core.Event.UnifiedMsgOrigin) is the convID. We
// track which convIDs are groups from received events so the correct OneBot
// action (send_group_msg vs send_private_msg) is chosen. Delivery happens over
// the reverse WebSocket connection; an error is returned when none is active.
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	a.mu.Lock()
	isGroup := a.groupConvs[sessionID]
	// unique_session 开启时会话 ID 被宿主拼接为 "{sender_id}_{group_id}"
	//（buildUniqueSessionID: aiocqhttp → sender+"_"+group），而 groupConvs
	// 的 key 是原始会话 ID。此时从拼接串解析出末段群号回退判定群/私聊，
	// 否则会话被误判为私聊导致 send_msg 携带非数字 user_id 被 NapCat 拒绝
	//（retcode 1400 "user_id: expected a positive integer"）。
	if !isGroup && strings.Contains(sessionID, "_") {
		if idx := strings.LastIndex(sessionID, "_"); idx > 0 && idx < len(sessionID)-1 {
			if g, ok := a.groupConvs[sessionID[idx+1:]]; ok && g {
				isGroup = true
				// 用解析出的纯群号发送（拼接串会导致 NapCat 拒绝）。
				sessionID = sessionID[idx+1:]
			}
		}
	}
	a.mu.Unlock()

	// 链中含合并转发节点（Node/Nodes）时不能整链一次 send_msg：转发段必须走
	// send_group_forward_msg / send_private_forward_msg，转发节点之外的普通段
	// 仍按普通消息继续逐个发送（对齐 Python aiocqhttp_message_event.send_message
	// 的逐段遍历发送逻辑）。
	if hasForwardNode(chain) {
		return a.sendChainMixed(sessionID, chain, isGroup)
	}
	return a.sendAction("send_msg", a.dispatchParams(isGroup, sessionID, a.convertToCQFormat(chain)))
}

// dispatchParams 构造一次普通消息发送（send_msg）的 OneBot 参数：
// 消息段数组 + 会话目标（群号 / QQ 号）。
func (a *Adapter) dispatchParams(isGroup bool, sessionID string, segments []map[string]interface{}) map[string]interface{} {
	params := map[string]interface{}{"message": segments}
	if isGroup {
		params["group_id"] = sessionID
	} else {
		params["user_id"] = sessionID
	}
	return params
}

// sendChainMixed 发送含合并转发节点（Node/Nodes）的混排消息链：遍历链，
// 遇 Node/Nodes 发送合并转发消息（单个 Node 包装成只有一个节点的转发），
// 遇其他段（含 File）继续按普通消息逐段发送（对齐 Python
// AiocqhttpMessageEvent.send_message：转发节点不再导致链中其余段被丢弃）。
func (a *Adapter) sendChainMixed(sessionID string, chain *message.MessageChain, isGroup bool) error {
	sentAny := false
	for _, comp := range chain.Chain {
		switch c := comp.(type) {
		case *message.Node:
			// 单个 Node 包装成只有一个节点的 Nodes 发送。
			if err := a.sendNodes(sessionID, &message.Nodes{Nodes: []*message.Node{c}}, isGroup); err != nil {
				return err
			}
		case *message.Nodes:
			if err := a.sendNodes(sessionID, c, isGroup); err != nil {
				return err
			}
		default:
			// 普通段/文件段：转成 OneBot 段后单段发送。
			segments := a.convertToCQFormat(&message.MessageChain{Chain: []message.Component{comp}})
			if len(segments) == 0 {
				continue
			}
			// 段间限速（对齐 Python 逐段发送时的 asyncio.sleep(0.5)）。
			if sentAny {
				time.Sleep(500 * time.Millisecond)
			}
			if err := a.sendAction("send_msg", a.dispatchParams(isGroup, sessionID, segments)); err != nil {
				return err
			}
			sentAny = true
		}
	}
	return nil
}

// sendNodes 通过 send_group_forward_msg / send_private_forward_msg 发送一个
// 合并转发消息。节点列表为空时跳过（不向 OneBot 侧发送空转发）。
func (a *Adapter) sendNodes(sessionID string, nodes *message.Nodes, isGroup bool) error {
	msgs := []map[string]interface{}{}
	for _, n := range nodes.Nodes {
		if n == nil {
			continue
		}
		msgs = append(msgs, a.nodeToCQ(n))
	}
	if len(msgs) == 0 {
		logger.Warn("aiocqhttp: 合并转发消息无可用节点，已跳过发送")
		return nil
	}
	action := "send_group_forward_msg"
	params := map[string]interface{}{"messages": msgs}
	if isGroup {
		params["group_id"] = sessionID
	} else {
		action = "send_private_forward_msg"
		params["user_id"] = sessionID
	}
	return a.sendAction(action, params)
}

// hasForwardNode reports whether the chain contains Node/Nodes components.
func hasForwardNode(chain *message.MessageChain) bool {
	if chain == nil {
		return false
	}
	for _, comp := range chain.Chain {
		switch comp.(type) {
		case *message.Node, *message.Nodes:
			return true
		}
	}
	return false
}

// nodeToCQ converts a Node component to the OneBot v11 "node" segment.
func (a *Adapter) nodeToCQ(n *message.Node) map[string]interface{} {
	content := []map[string]interface{}{}
	if n.Content != nil {
		content = a.convertToCQFormat(&message.MessageChain{Chain: n.Content})
	}
	return map[string]interface{}{
		"type": "node",
		"data": map[string]interface{}{
			"uin":     n.UIN,
			"name":    n.Name,
			"content": content,
		},
	}
}

// sendAction sends a OneBot v11 API call over an active reverse-WS connection,
// fire-and-forget. The echo is registered in pending and consumed
// asynchronously by observeSendAction so the API-level response is not silently
// dropped: failures (status != ok) and missing responses are logged.
func (a *Adapter) sendAction(action string, params map[string]interface{}) error {
	// 与 CallAction 共用原子计数器：仅用纳秒时间戳在高并发下可能在同一纳秒
	// 碰撞，导致响应被路由到错误的调用方。
	echo := fmt.Sprintf("astrbot-%d-%d", time.Now().UnixNano(), actionSeq.Add(1))
	payload, err := json.Marshal(map[string]interface{}{
		"action": action,
		"params": params,
		"echo":   echo,
	})
	if err != nil {
		return err
	}

	a.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(a.conns))
	for c := range a.conns {
		conns = append(conns, c)
	}
	a.mu.Unlock()

	if len(conns) == 0 {
		return fmt.Errorf("aiocqhttp: no active WebSocket connection to send %s", action)
	}

	// Register the echo before writing so a fast response cannot arrive before
	// the waiter exists.
	ch := make(chan map[string]interface{}, 1)
	a.pendingMu.Lock()
	a.pending[echo] = ch
	a.pendingMu.Unlock()

	// Try each connection; drop ones that fail so future sends pick a healthy peer.
	var lastErr error
	for _, c := range conns {
		mu := a.connWriteLock(c)
		if mu == nil {
			continue
		}
		mu.Lock()
		_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		err := c.WriteMessage(websocket.TextMessage, payload)
		mu.Unlock()
		if err != nil {
			a.removeConn(c)
			lastErr = err
			continue
		}
		go a.observeSendAction(action, echo, ch)
		return nil
	}

	// Write failed on every connection: drop the pending registration so the
	// map does not accumulate a waiter that will never be consumed.
	a.pendingMu.Lock()
	delete(a.pending, echo)
	a.pendingMu.Unlock()
	if lastErr == nil {
		lastErr = fmt.Errorf("no reachable connection")
	}
	return fmt.Errorf("aiocqhttp: failed to send %s: %w", action, lastErr)
}

// observeSendAction consumes the echo response of a fire-and-forget sendAction.
// The read loop delivers the response to ch and closes it; a missing response is
// cleaned up after actionTimeout.
func (a *Adapter) observeSendAction(action, echo string, ch chan map[string]interface{}) {
	select {
	case resp, ok := <-ch:
		if !ok {
			return
		}
		if _, err := parseActionResult(resp); err != nil {
			logger.Warn("aiocqhttp: %s 返回失败: %v (resp=%v)", action, err, resp)
		}
	case <-time.After(actionTimeout):
		a.pendingMu.Lock()
		delete(a.pending, echo)
		a.pendingMu.Unlock()
		logger.Warn("aiocqhttp: %s 超时（%s 内未收到响应）", action, actionTimeout)
	}
}

// handleHTTP handles HTTP POST requests from OneBot v11 implementations.
func (a *Adapter) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !a.authValid(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Bound the request body so a peer cannot exhaust memory with an oversized
	// payload (the token is optional per OneBot v11, so this endpoint may be
	// reachable without authentication).
	const maxEventBody = 1 << 20 // 1 MiB
	body, err := io.ReadAll(io.LimitReader(r.Body, maxEventBody+1))
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if len(body) > maxEventBody {
		http.Error(w, "Payload Too Large", http.StatusRequestEntityTooLarge)
		return
	}

	var event map[string]interface{}
	if err := json.Unmarshal(body, &event); err != nil {
		logger.Error("Failed to decode event: %v", err)
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	go a.handleEvent(event)
	w.WriteHeader(http.StatusOK)
}

// authValid verifies the OneBot access token. Per the OneBot v11 spec the
// token is optional: when ws_reverse_token is not configured, any peer may
// connect (logged as a warning so the risk is visible); when it is configured,
// it is enforced on both the HTTP event endpoint and the reverse-WS endpoint.
func (a *Adapter) authValid(r *http.Request) bool {
	if a.Token == "" {
		logger.Warn("aiocqhttp: 未配置 ws_reverse_token，事件入口（%s %s）未做访问鉴权。若本端口可被公网访问，请配置访问令牌", r.Method, r.URL.Path)
		return true
	}
	auth := r.Header.Get("Authorization")
	queryToken := r.URL.Query().Get("access_token")
	// Bound input length so oversized headers/queries can't be used to probe
	// or exhaust resources.
	const maxAuthLen = 4096
	if len(auth) > maxAuthLen || len(queryToken) > maxAuthLen {
		return false
	}
	// Exact (constant-time) comparison: HasPrefix on "Bearer "+token would
	// accept trailing garbage (token "tok" + "Bearer tokXYZ").
	token := []byte(a.Token)
	const bearerPrefix = "Bearer "
	if strings.HasPrefix(auth, bearerPrefix) {
		got := []byte(strings.TrimPrefix(auth, bearerPrefix))
		if subtle.ConstantTimeCompare(got, token) == 1 {
			return true
		}
	}
	return subtle.ConstantTimeCompare([]byte(auth), token) == 1 ||
		subtle.ConstantTimeCompare([]byte(queryToken), token) == 1
}

// defaultMaxReverseWSConns is the default cap on concurrent reverse WebSocket
// connections (config ws_reverse_max_conns overrides it).
const defaultMaxReverseWSConns = 8

// maxConns returns the configured reverse-WS connection limit.
func (a *Adapter) maxConns() int {
	if a.maxReverseWSConns > 0 {
		return a.maxReverseWSConns
	}
	return defaultMaxReverseWSConns
}

// connCount returns the number of active reverse-WS connections.
func (a *Adapter) connCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.conns)
}

// handleWebSocket serves the reverse WebSocket endpoint. OneBot
// implementations connect here and both push events and receive API calls.
func (a *Adapter) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if !a.authValid(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// 限制并发反向 WS 连接数（6.4）：在 Upgrade 之前检查，避免为将被拒绝
	// 的连接浪费一个已升级的 socket。预检是尽力而为的，addConn 在锁内会做
	// 权威校验以堵住并发连接涌入的竞态窗口。
	if a.connCount() >= a.maxConns() {
		logger.I18nWarn("反向 WebSocket 连接数已达上限（%d），拒绝新连接", a.maxConns())
		http.Error(w, fmt.Sprintf("aiocqhttp: too many reverse WebSocket connections (max %d)", a.maxConns()), http.StatusServiceUnavailable)
		return
	}

	// #nosec websocket-missing-origin-check -- 已双重防护：a.upgrader.CheckOrigin
	// 校验 Origin 白名单（ws_reverse_origins，非浏览器客户端无 Origin 则放行），
	// 且上方 authValid 要求 ws_reverse_token 令牌（Bearer/access_token 常量时间比较）。
	conn, err := a.upgrader.Upgrade(w, r, nil) // nosemgrep: go.gorilla.security.audit.websocket-missing-origin-check.websocket-missing-origin-check
	if err != nil {
		logger.Error("WebSocket upgrade failed: %v", err)
		return
	}
	if err := a.addConn(conn); err != nil {
		// 预检之后、addConn 之前有并发连接涌入，addConn 在锁内再次校验上限
		// 并拒绝。连接此时已升级，直接关闭。
		logger.I18nWarn("反向 WebSocket 连接数已达上限，拒绝: %v", err)
		_ = conn.Close()
		return
	}
	logger.I18nInfo("反向 WebSocket 客户端已连接 (%s)", conn.RemoteAddr())

	// 查询 bot 自身信息（get_login_info）以得到真实 QQ 号，供 @ 唤醒匹配
	// （事件 self 字段缺失/占位为 config.id 时自愈）。
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if resp, err := a.CallActionCtx(ctx, "get_login_info", map[string]interface{}{}); err == nil {
			if data, ok := resp["data"].(map[string]interface{}); ok {
				if uid := toString(data["user_id"]); uid != "" {
					a.setSelfID(uid)
				}
			}
		}
	}()

	defer func() {
		a.removeConn(conn)
		_ = conn.Close()
		logger.I18nInfo("反向 WebSocket 客户端已断开")
	}()

	// 限制单连接消息体大小，防止异常对端放大内存占用。
	conn.SetReadLimit(1 << 20)

	// Heartbeat：反向 WS 每收到一帧（事件/echo/心跳 JSON）都刷新读超时。
	// 对齐 Python 本体：aiocqhttp 库靠 Quart 长连接 + 对端应用层心跳保活，
	// 本体没有 5min 读超时——OneBot 实现（NapCat 等）的心跳是应用层 JSON
	// （meta_event.heartbeat）而非 WS ping，PongHandler 收不到；固定
	// ReadDeadline 到期即断（表现为连接精确每 5 分钟断开重连）。
	// 另每 60s 主动发 WS ping，双保险（对端实现 Pong 处理则更稳）。
	refreshReadDeadline := func() {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
	}
	refreshReadDeadline()
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
	})
	stopPing := make(chan struct{})
	defer close(stopPing)
	go func(c *websocket.Conn) {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-ticker.C:
				mu := a.connWriteLock(c)
				if mu == nil {
					return
				}
				mu.Lock()
				_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
				err := c.WriteMessage(websocket.PingMessage, nil)
				mu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}(conn)

	// Events are handled on a single per-connection goroutine so a slow
	// handleEvent (quoted-message fetching via CallAction) never blocks this
	// read loop from consuming the echo frames it waits for.
	events := make(chan map[string]interface{}, 64)
	defer close(events)
	go func() {
		for ev := range events {
			a.handleEvent(ev)
		}
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				logger.I18nWarn("WebSocket 读取错误: %v", err)
			}
			return
		}
		// 任何到达的帧（含应用层心跳 JSON）都证明连接存活，刷新读超时。
		refreshReadDeadline()
		if len(data) == 0 {
			continue
		}
		// Reverse-WS frames are either events (post_type) or API responses
		// (echo) to our send_msg calls.
		var msg map[string]interface{}
		if err := json.Unmarshal(data, &msg); err != nil {
			logger.I18nWarn("WebSocket 消息不是 JSON: %v", err)
			continue
		}
		if _, hasPost := msg["post_type"]; hasPost {
			select {
			case events <- msg:
			default:
				logger.I18nWarn("aiocqhttp: 事件处理队列已满，丢弃一条事件（echo 通道不受影响）")
			}
			continue
		}
		if echo, hasEcho := msg["echo"].(string); hasEcho {
			// 用 JSON 序列化打印而非 %v：%v 对 float64 用科学计数法
			// （user_id 2408045264 显示成 2.408045264e+09），易被误认为
			// 精度丢失；JSON 序列化输出整数原形。
			if b, jerr := json.Marshal(msg); jerr == nil {
				logger.Debug("OneBot API response: %s", b)
			} else {
				logger.Debug("OneBot API response: %v", msg)
			}
			a.pendingMu.Lock()
			if ch, ok := a.pending[echo]; ok {
				delete(a.pending, echo)
				select {
				case ch <- msg:
				default:
				}
				close(ch)
			}
			a.pendingMu.Unlock()
		}
	}
}

// CallActionCtx sends a OneBot v11 API call with a context timeout.
func (a *Adapter) CallActionCtx(ctx context.Context, api string, params map[string]any) (map[string]any, error) {
	done := make(chan struct{})
	var (
		ret map[string]any
		err error
	)
	go func() {
		ret, err = a.CallAction(api, params)
		close(done)
	}()
	select {
	case <-done:
		return ret, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// CallAction sends a OneBot v11 API call over an active reverse-WS connection
// and waits for the echo'd response (up to actionTimeout). Returns the action's
// "data" object.
func (a *Adapter) CallAction(api string, params map[string]any) (map[string]any, error) {
	echo := fmt.Sprintf("astrbot-%d-%d", time.Now().UnixNano(), actionSeq.Add(1))
	payload, err := json.Marshal(map[string]interface{}{
		"action": api,
		"params": params,
		"echo":   echo,
	})
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(a.conns))
	for c := range a.conns {
		conns = append(conns, c)
	}
	a.mu.Unlock()

	if len(conns) == 0 {
		return nil, fmt.Errorf("aiocqhttp: no active WebSocket connection to call %s", api)
	}

	ch := make(chan map[string]interface{}, 1)
	a.pendingMu.Lock()
	a.pending[echo] = ch
	a.pendingMu.Unlock()
	defer func() {
		a.pendingMu.Lock()
		delete(a.pending, echo)
		a.pendingMu.Unlock()
	}()

	// Try each connection; drop ones that fail so future calls pick a healthy peer.
	var lastErr error
	for _, c := range conns {
		mu := a.connWriteLock(c)
		if mu == nil {
			continue
		}
		// The write lock is released before waiting for the echo so a slow
		// action never blocks other writers on the same connection.
		mu.Lock()
		_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		err := c.WriteMessage(websocket.TextMessage, payload)
		mu.Unlock()
		if err != nil {
			a.removeConn(c)
			lastErr = err
			continue
		}
		select {
		case resp := <-ch:
			return parseActionResult(resp)
		case <-time.After(actionTimeout):
			return nil, fmt.Errorf("aiocqhttp: call %s timed out (no echo response)", api)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no reachable connection")
	}
	return nil, fmt.Errorf("aiocqhttp: failed to call %s: %w", api, lastErr)
}

// actionTimeout bounds how long CallAction waits for an echo response, and how
// long sendAction's async observer waits for the fire-and-forget response.
// Var (not const) so tests can shorten it.
var actionTimeout = 10 * time.Second

// actionSeq is a process-wide monotonic counter producing unique echo strings
// for OneBot actions. Concurrent plugin goroutines call CallAction, so the
// counter must be updated atomically (a plain int read-modify-write races).
var actionSeq = &actionCounter{}

type actionCounter struct{ n int64 }

func (c *actionCounter) Add(delta int64) int64 {
	return atomic.AddInt64(&c.n, delta)
}

// parseActionResult unwraps the OneBot v11 response envelope, returning its
// "data" object. Array data is wrapped under the key "list" so callers always
// receive a JSON object across the RPC boundary.
func parseActionResult(resp map[string]interface{}) (map[string]interface{}, error) {
	if status, _ := resp["status"].(string); status != "" && status != "ok" {
		if msg, _ := resp["msg"].(string); msg != "" {
			return nil, fmt.Errorf("aiocqhttp: action failed: %s", msg)
		}
		return nil, fmt.Errorf("aiocqhttp: action failed: status=%s", status)
	}
	switch data := resp["data"].(type) {
	case map[string]interface{}:
		return data, nil
	case []interface{}:
		return map[string]interface{}{"list": data}, nil
	default:
		return map[string]interface{}{}, nil
	}
}

// getSelfID returns the bot's own id (mutex-protected snapshot).
func (a *Adapter) getSelfID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.SelfID
}

// setSelfID records the bot's own id (mutex-protected).
func (a *Adapter) setSelfID(id string) {
	a.mu.Lock()
	a.SelfID = id
	a.mu.Unlock()
}

// addConn registers a connection and enforces the max-connection limit under
// lock (the pre-Upgrade check in handleWebSocket is advisory; this is the
// authoritative one that closes the check-then-insert race window). Returns an
// error — without inserting — once the limit is reached, so the caller can
// reject the excess connection.
func (a *Adapter) addConn(c *websocket.Conn) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.conns) >= a.maxConns() {
		return fmt.Errorf("aiocqhttp: reverse WebSocket connection limit reached (%d)", a.maxConns())
	}
	a.conns[c] = struct{}{}
	a.connWriteMu[c] = &sync.Mutex{}
	return nil
}

func (a *Adapter) removeConn(c *websocket.Conn) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.conns, c)
	delete(a.connWriteMu, c)
}

// connWriteLock returns the per-connection write mutex, or nil when the
// connection is no longer registered.
func (a *Adapter) connWriteLock(c *websocket.Conn) *sync.Mutex {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.connWriteMu[c]
}

// handleEvent processes a OneBot v11 event, dispatching to message or notice
// handling by post_type.
func (a *Adapter) handleEvent(raw map[string]interface{}) {
	postType, _ := raw["post_type"].(string)
	if postType == "" {
		postType = "message"
	}
	if messageType, ok := raw["message_type"].(string); ok {
		logger.Debug("aiocqhttp event: post_type=%s message_type=%s", postType, messageType)
	} else {
		logger.Debug("aiocqhttp event: post_type=%s", postType)
	}

	// Track the bot's own ID from the event's self field so @-mentions of the
	// bot can be detected by WakingCheckStage. Always override the config
	// instance id placeholder with the real bot id when the event carries it
	// (self.user_id), so @-wake compares like-for-like (QQ number vs number).
	if self, ok := raw["self"].(map[string]interface{}); ok {
		if id, ok := self["user_id"]; ok {
			if sid := toString(id); sid != "" {
				a.setSelfID(sid)
			}
		}
	}

	switch postType {
	case "message":
		a.handleMessage(raw)
	case "notice":
		a.handleNotice(raw)
	case "request":
		a.handleRequest(raw)
	default:
		// meta 事件（heartbeat/lifecycle）不进入事件管线。
		logger.Debug("aiocqhttp: ignoring event post_type=%q", postType)
	}
}

// handleRequest processes a OneBot v11 request event (好友申请 / 加群申请 /
// 群邀请，notice_type 为 friend / group)。对齐 Python
// _convert_handle_request_event：构造一个含描述文本的事件发布进管线，
// 原始事件 JSON 放入 RawMessage 供插件自行处理（同意/拒绝等）。
func (a *Adapter) handleRequest(raw map[string]interface{}) {
	requestType, _ := raw["request_type"].(string)
	// 请求类型描述（对齐 OneBot v11：friend=好友申请、group=加群申请/邀请）。
	segText := "请求"
	switch requestType {
	case "friend":
		segText = "好友申请"
	case "group":
		segText = "加群申请"
	}
	// 有 comment 时附带申请留言。
	if comment, _ := raw["comment"].(string); strings.TrimSpace(comment) != "" {
		segText += "：" + comment
	}

	userID := toString(raw["user_id"])
	senderName := userID
	senderID := userID

	// 对齐 Python _convert_handle_request_event：有 group_id 视为群请求
	// （GROUP_MESSAGE），否则视为好友请求（FRIEND_MESSAGE）。
	convID := senderID
	isGroup := false
	messageType := "FriendMessage"
	if gid, ok := raw["group_id"]; ok {
		if g := toString(gid); g != "" {
			isGroup = true
			convID = g
			messageType = "GroupMessage"
			segText += fmt.Sprintf("（群 %s）", g)
		}
	}

	selfID := a.getSelfID()
	// 同消息路径：优先用事件自带 self_id 并回填快照（见 handleMessage 注释）。
	if evSelf, ok := raw["self_id"]; ok {
		if s := toString(evSelf); s != "" {
			selfID = s
			a.setSelfID(s)
		}
	}

	a.mu.Lock()
	a.groupConvs[convID] = isGroup
	a.mu.Unlock()

	// message_str 组装为 "[好友申请] xxx" 类描述文本，原始事件数据入 raw_message
	// （对齐 Python：abm.message_str 由事件类型组装、raw_message 存原始事件）。
	messageStr := "[" + segText + "] " + userID
	flag, _ := raw["flag"].(string)

	event := &core.Event{
		Type: core.EventRequest,
		Source: core.EventSource{
			Platform:   "aiocqhttp",
			PlatformID: a.ID(),
			SelfID:     selfID,
			SenderID:   senderID,
			SenderName: senderName,
			ConvID:     convID,
			IsGroup:    isGroup,
		},
		MessageStr: messageStr,
		RawMessage: rawJSON(raw),
		Timestamp:  time.Now(),
		Metadata: map[string]interface{}{
			// 插件处理请求所需的关键字段（同意/拒绝需 flag）。
			"request_type": requestType,
			"flag":         flag,
			"comment":      raw["comment"],
		},
		MessageObj: &core.MessageObj{
			MessageID:   randomHex(16),
			SelfID:      selfID,
			SessionID:   convID,
			MessageType: messageType,
			Platform:    "aiocqhttp",
			MessageStr:  messageStr,
			RawMessage:  raw,
			Timestamp:   time.Now(),
		},
	}

	a.publishEvent(event)
}

// publishEvent sends a core.Event to the bus (nil-safe).
func (a *Adapter) publishEvent(event *core.Event) {
	if a.EventBus == nil {
		logger.Error("aiocqhttp event bus not configured; cannot publish")
		return
	}
	if err := a.EventBus.Publish(event); err != nil {
		logger.Error("Failed to publish event: %v", err)
	}
}

// rawJSON marshals the raw OneBot event to a JSON string (best effort).
func rawJSON(raw map[string]interface{}) string {
	if raw == nil {
		return ""
	}
	b, _ := json.Marshal(raw)
	return string(b)
}

// handleMessage processes a OneBot v11 message event.
func (a *Adapter) handleMessage(raw map[string]interface{}) {
	messageType, _ := raw["message_type"].(string)
	isGroup := messageType == "group"

	// 屏蔽 QQ 管家（官方助手账号 2854196310）的消息（对齐 Python
	// adapter.py:133-135：该账号的推送非真实用户消息，进入管线会造成干扰）。
	if sender, ok := raw["sender"].(map[string]interface{}); ok {
		if uid := toString(sender["user_id"]); uid == qqHousekeeperID {
			logger.Debug("aiocqhttp: 忽略 QQ 管家消息 (user_id=%s)", uid)
			return
		}
	}
	if uid := toString(raw["user_id"]); uid == qqHousekeeperID {
		logger.Debug("aiocqhttp: 忽略 QQ 管家消息 (user_id=%s)", uid)
		return
	}

	var senderID, senderName, convID string
	if sender, ok := raw["sender"].(map[string]interface{}); ok {
		senderID = toString(sender["user_id"])
		if name, ok := sender["card"].(string); ok && name != "" {
			senderName = name
		} else if nick, ok := sender["nickname"].(string); ok {
			senderName = nick
		}
	}
	if senderID == "" {
		senderID = toString(raw["user_id"])
	}

	if isGroup {
		convID = toString(raw["group_id"])
	} else {
		convID = senderID
	}
	a.mu.Lock()
	a.groupConvs[convID] = isGroup
	a.mu.Unlock()

	// Convert message segments
	msgChain := a.convertFromCQFormat(raw)
	// Fetch quoted-reply content and combined-forward messages
	// (quoted_message_parser; mirrors QuotedMessageExtractor).
	a.enrichForwardAndQuoted(msgChain, toString(raw["group_id"]), toString(raw["user_id"]))

	msgID, _ := raw["message_id"].(string)
	if msgID == "" {
		if id, ok := raw["message_id"].(float64); ok {
			msgID = fmt.Sprintf("%v", int64(id))
		}
	}
	selfID := a.getSelfID()
	// 优先用事件自带的 self_id（OneBot 群/私聊事件均携带机器人自身 QQ 号）：
	// 启动早期 get_login_info 异步查询可能未完成，快照仍是 config.id 占位
	// （如 "default"），导致插件 event.get_self_id() 拿到平台 ID 而非真实
	// QQ 号（qqadmin 全禁/宵禁等 int(get_self_id()) 抛 ValueError）。
	if evSelf, ok := raw["self_id"]; ok {
		if s := toString(evSelf); s != "" {
			selfID = s
			a.setSelfID(s) // 顺带回填快照，后续事件直接命中
		}
	}

	groupName, _ := raw["group_name"].(string)

	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   "aiocqhttp",
			PlatformID: a.ID(),
			SelfID:     selfID,
			SenderID:   senderID,
			SenderName: senderName,
			ConvID:     convID,
			IsGroup:    isGroup,
		},
		Message:    msgChain,
		MessageStr: extractPlainText(msgChain),
		RawMessage: rawJSON(raw),
		Timestamp:  time.Now(),
		Metadata:   make(map[string]interface{}),
		MessageObj: &core.MessageObj{
			MessageID:   msgID,
			SelfID:      selfID,
			SessionID:   convID,
			MessageType: messageType,
			Platform:    "aiocqhttp",
			MessageStr:  extractPlainText(msgChain),
			RawMessage:  raw,
			Timestamp:   time.Now(),
		},
	}
	if isGroup && groupName != "" {
		event.MessageObj.Group = &core.Group{
			GroupID:   convID,
			GroupName: groupName,
		}
	}

	a.publishEvent(event)
}

// handleNotice processes a OneBot v11 notice event (recall, ban, poke, etc.)
// and publishes it as a notice event so plugins (e.g. recall-cancel) can react.
func (a *Adapter) handleNotice(raw map[string]interface{}) {
	noticeType, _ := raw["notice_type"].(string)
	if noticeType == "" {
		return
	}
	isGroup := false
	convID := ""
	if gid, ok := raw["group_id"]; ok {
		isGroup = true
		convID = toString(gid)
	}
	senderID := toString(raw["operator_id"])
	if senderID == "<nil>" || senderID == "" {
		senderID = toString(raw["user_id"])
	}
	if !isGroup {
		convID = senderID
	}

	msgID, _ := raw["message_id"].(string)
	if msgID == "" {
		if id, ok := raw["message_id"].(float64); ok {
			msgID = fmt.Sprintf("%v", int64(id))
		}
	}
	selfID := a.getSelfID()
	// 同消息路径：优先用事件自带 self_id 并回填快照（见上方注释）。
	if evSelf, ok := raw["self_id"]; ok {
		if s := toString(evSelf); s != "" {
			selfID = s
			a.setSelfID(s)
		}
	}

	a.mu.Lock()
	if convID != "" {
		a.groupConvs[convID] = isGroup
	}
	a.mu.Unlock()

	event := &core.Event{
		Type: core.EventNotice,
		Source: core.EventSource{
			Platform:   "aiocqhttp",
			PlatformID: a.ID(),
			SelfID:     selfID,
			SenderID:   senderID,
			ConvID:     convID,
			IsGroup:    isGroup,
		},
		MessageStr: "",
		RawMessage: rawJSON(raw),
		Timestamp:  time.Now(),
		Metadata:   make(map[string]interface{}),
		MessageObj: &core.MessageObj{
			MessageID:   msgID,
			SelfID:      selfID,
			SessionID:   convID,
			MessageType: "notice_" + noticeType,
			Platform:    "aiocqhttp",
			RawMessage:  raw,
			Timestamp:   time.Now(),
		},
	}
	if noticeType == "group_recall" || noticeType == "friend_recall" {
		event.MessageStr = "[撤回通知]"
	}
	// 戳一戳通知（对齐 Python adapter.py:192-194：notice 事件 sub_type=poke
	// 且携带 target_id 时，构造含 Poke(target_id) 组件的消息事件，插件可据此
	// 响应戳一戳）。
	if subType, _ := raw["sub_type"].(string); subType == "poke" {
		if targetID := toString(raw["target_id"]); targetID != "" {
			event.Message = &message.MessageChain{
				Chain: []message.Component{&message.Poke{Target: targetID}},
			}
		}
	}
	a.publishEvent(event)
}

// convertFromCQFormat converts OneBot v11 message segments to MessageChain.
// Forward (node/nodes) segments are parsed inline; their remote ids are
// fetched by enrichForwardAndQuoted.
func (a *Adapter) convertFromCQFormat(raw map[string]interface{}) *message.MessageChain {
	chain := &message.MessageChain{Chain: []message.Component{}}

	segments, ok := raw["message"].([]interface{})
	if !ok {
		if msg, isStr := raw["message"].(string); isStr {
			logger.Warn("aiocqhttp: OneBot 实现返回了 CQ 字符串格式 message，请配置为 array 格式: %q", msg)
		}
		return chain
	}

	// groupID 供 @ 段解析群昵称（get_group_member_info，见 parseOneBotSegments）。
	parsed, _ := a.parseOneBotSegments(segments, 0, toString(raw["group_id"]))
	chain.Chain = parsed
	a.enrichFileURLs(chain, raw)
	return chain
}

// enrichFileURLs resolves NapCat file segments that carry no URL by calling
// get_group_file_url / get_private_file_url. Requires the group/user context
// from the event; segments lacking one are left as-is.
func (a *Adapter) enrichFileURLs(chain *message.MessageChain, raw map[string]interface{}) {
	if chain == nil || len(chain.Chain) == 0 {
		return
	}
	groupID := toString(raw["group_id"])
	userID := toString(raw["user_id"])
	a.enrichFileURLsIn(chain.Chain, groupID, userID)
}

// enrichFileURLsIn walks a component slice and completes any File component
// that carries a file id but no URL, using the group/user context (get_
// group_file_url / get_private_file_url). Nested chains (quoted replies and
// forward nodes) are walked recursively so a quoted file message also gets a
// usable download URL for the agent context.
func (a *Adapter) enrichFileURLsIn(comps []message.Component, groupID, userID string) {
	if len(comps) == 0 || (groupID == "" && userID == "") {
		return
	}
	var walk func(cs []message.Component)
	walk = func(cs []message.Component) {
		for _, comp := range cs {
			switch c := comp.(type) {
			case *message.File:
				if c.URL != "" || c.FileID == "" {
					continue
				}
				if url := a.fetchFileURL(c.FileID, groupID, userID); url != "" {
					c.URL = url
				}
			case *message.Reply:
				walk(c.Chain)
			case *message.Nodes:
				for _, n := range c.Nodes {
					if n != nil {
						walk(n.Content)
					}
				}
			}
		}
	}
	walk(comps)
}

// fetchFileURL resolves a file segment's download URL via the OneBot
// get_group_file_url / get_private_file_url actions.
func (a *Adapter) fetchFileURL(fileID, groupID, userID string) string {
	var action string
	var params map[string]interface{}
	switch {
	case groupID != "":
		action = "get_group_file_url"
		params = map[string]interface{}{"group_id": groupID, "file_id": fileID}
	case userID != "":
		action = "get_private_file_url"
		params = map[string]interface{}{"user_id": userID, "file_id": fileID}
	default:
		logger.Warn("aiocqhttp: 文件消息缺少群/用户上下文，无法获取下载 URL (file_id=%s)", fileID)
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ret, err := a.CallActionCtx(ctx, action, params)
	if err != nil {
		logger.Warn("aiocqhttp: %s 失败 file_id=%s: %v", action, fileID, err)
		return ""
	}
	url, _ := ret["url"].(string)
	if url == "" || !validHTTPURL(url) {
		logger.Warn("aiocqhttp: %s 未返回可用 URL file_id=%s (url=%q)", action, fileID, url)
		return ""
	}
	return url
}

// validHTTPURL reports whether raw is a parseable http(s) URL with a non-empty
// host. OneBot 的 get_group_file_url 可能返回形如 "https:///ftn_handler//?fname="
// 的空 host 伪 URL，这类链接不可下载，必须在进入 agent 上下文前过滤掉。
func validHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// enrichForwardAndQuoted resolves remote content referenced by the chain:
// reply ids are fetched with get_msg (building the quoted chain) and forward
// ids with get_forward_msg (BFS, max_forward_fetch hops). Mirrors the
// QuotedMessageExtractor flow for the aiocqhttp platform. groupID/userID
// provide the file-URL resolution context for File components inside the
// quoted content.
func (a *Adapter) enrichForwardAndQuoted(chain *message.MessageChain, groupID, userID string) {
	if chain == nil || len(chain.Chain) == 0 {
		return
	}
	var replyIDs []string
	for _, comp := range chain.Chain {
		if reply, ok := comp.(*message.Reply); ok && reply.MessageID != "" {
			replyIDs = append(replyIDs, reply.MessageID)
		}
	}

	// Fetch quoted reply content (get_msg) — build Reply.Chain.
	for _, rid := range replyIDs {
		quotedChain, _ := a.fetchQuotedContent(rid, groupID, userID)
		for _, comp := range chain.Chain {
			if reply, ok := comp.(*message.Reply); ok && reply.MessageID == rid {
				reply.Chain = quotedChain
				reply.SenderNick = ""
			}
		}
	}

	// Fetch combined-forward messages (get_forward_msg BFS), replacing every
	// placeholder Nodes component (top-level and inside quoted replies).
	a.resolveForwardPlaceholders(chain)
}

// resolveForwardPlaceholders walks a chain (including quoted reply content and
// inline node content) and replaces every placeholder Nodes component (forward
// ids present, nodes not yet fetched) with the nodes resolved via
// get_forward_msg BFS.
func (a *Adapter) resolveForwardPlaceholders(chain *message.MessageChain) {
	if chain == nil {
		return
	}
	var walk func(comps []message.Component)
	walk = func(comps []message.Component) {
		for i, comp := range comps {
			switch c := comp.(type) {
			case *message.Nodes:
				if len(c.Nodes) == 0 {
					if nodes := a.resolveNestedForwards(c.ForwardIDs); len(nodes) > 0 {
						comps[i] = &message.Nodes{Nodes: nodes}
					}
					continue
				}
				for _, n := range c.Nodes {
					if n != nil {
						walk(n.Content)
					}
				}
			case *message.Reply:
				walk(c.Chain)
			}
		}
	}
	walk(chain.Chain)
}

// convertToCQFormat converts a MessageChain to OneBot v11 message segments.
func (a *Adapter) convertToCQFormat(mc *message.MessageChain) []map[string]interface{} {
	if mc == nil {
		return nil
	}
	segments := []map[string]interface{}{}
	for _, comp := range mc.Chain {
		switch c := comp.(type) {
		case *message.Plain:
			// 发送走 array 段格式，OneBot 实现（NapCat/Lagrange/go-cqhttp）不会对
			// array 格式的 text 段做 CQ 反转义，转义反而会让 [、]、, 以实体原样显示，
			// 因此这里直接下发原始文本。
			segments = append(segments, map[string]interface{}{
				"type": "text",
				"data": map[string]interface{}{"text": c.Text},
			})
		case *message.At:
			segments = append(segments, map[string]interface{}{
				"type": "at",
				"data": map[string]interface{}{"qq": c.TargetID, "name": c.Name},
			})
			// At 组件后补一个空格段，避免 @ 与后续文本粘连
			//（对齐 Python _parse_onebot_json：At 后 append {"type":"text","data":{"text":" "}}）。
			segments = append(segments, map[string]interface{}{
				"type": "text",
				"data": map[string]interface{}{"text": " "},
			})
		case *message.AtAll:
			segments = append(segments, map[string]interface{}{
				"type": "at",
				"data": map[string]interface{}{"qq": "all"},
			})
		case *message.Reply:
			segments = append(segments, map[string]interface{}{
				"type": "reply",
				"data": map[string]interface{}{"id": c.MessageID},
			})
		case *message.Image:
			imgData := map[string]interface{}{}
			switch {
			case c.Base64 != "":
				imgData["file"] = "base64://" + c.Base64
			case c.URL != "":
				// 远程 URL 交由 OneBot 侧下载；materialize 会把同一 URL 同时
				// 填充为本地临时 Path/File，这里必须优先 URL，避免把会在
				// PlatformManager.Send 返回后被清理的临时路径发给 OneBot。
				imgData["url"] = c.URL
				imgData["file"] = c.URL
			case c.Path != "":
				imgData["file"] = c.Path
			case c.File != "":
				imgData["file"] = c.File
			}
			segments = append(segments, map[string]interface{}{
				"type": "image",
				"data": imgData,
			})
		case *message.Record:
			ref := c.URL
			switch {
			case c.Base64 != "":
				ref = "base64://" + c.Base64
			case c.URL != "":
				ref = c.URL
			case c.Path != "":
				ref = "file://" + c.Path
			case c.File != "":
				ref = c.File
			}
			segments = append(segments, map[string]interface{}{
				"type": "record",
				"data": map[string]interface{}{"file": ref},
			})
		case *message.Face:
			segments = append(segments, map[string]interface{}{
				"type": "face",
				"data": map[string]interface{}{"id": c.ID},
			})
		case *message.File:
			ref := c.URL
			switch {
			case c.URL != "":
				ref = c.URL
			case c.Path != "":
				ref = "file://" + c.Path
			case c.FileID != "":
				ref = c.FileID
			}
			segments = append(segments, map[string]interface{}{
				"type": "file",
				"data": map[string]interface{}{"file": ref, "name": c.Name},
			})
		case *message.Video:
			ref := c.URL
			switch {
			case c.URL != "":
				ref = c.URL
			case c.Path != "":
				ref = "file://" + c.Path
			case c.FileID != "":
				ref = c.FileID
			}
			segments = append(segments, map[string]interface{}{
				"type": "video",
				"data": map[string]interface{}{"file": ref},
			})
		case *message.Json:
			segments = append(segments, map[string]interface{}{
				"type": "json",
				"data": map[string]interface{}{"data": c.Data},
			})
		}
	}
	return segments
}

// GetGroupInfo enriches group metadata via OneBot get_group_info +
// get_group_member_list, preserving inbound group data on failure (Python
// aiocqhttp_message_event.py get_group).
func (a *Adapter) GetGroupInfo(ctx context.Context, groupID string) (*platform.Group, error) {
	if groupID == "" {
		return nil, nil
	}
	group := &platform.Group{GroupID: groupID}

	gid := groupID
	if _, err := strconv.Atoi(groupID); err == nil {
		gid = groupID
	}

	info, err := a.CallActionCtx(ctx, "get_group_info", map[string]interface{}{"group_id": gid})
	if err != nil {
		logger.Debug("aiocqhttp: get_group_info failed for %s: %v", groupID, err)
		return group, nil
	}
	if name, ok := info["group_name"].(string); ok {
		group.GroupName = name
	}
	if mc, ok := info["member_count"].(float64); ok {
		c := int(mc)
		group.MemberCount = &c
	}

	members, err := a.CallActionCtx(ctx, "get_group_member_list", map[string]interface{}{"group_id": gid})
	if err != nil {
		logger.Debug("aiocqhttp: get_group_member_list failed for %s: %v", groupID, err)
		return group, nil
	}
	memberList, _ := members["list"].([]interface{})
	if memberList == nil {
		if ml, ok := members["list"].([]map[string]interface{}); ok {
			for _, m := range ml {
				if uid, ok := m["user_id"].(float64); ok {
					nick, _ := m["nickname"].(string)
					card, _ := m["card"].(string)
					if card != "" {
						nick = card
					}
					group.Members = append(group.Members, platform.MessageMember{UserID: strconv.FormatFloat(uid, 'f', 0, 64), Nickname: nick})
					if role, _ := m["role"].(string); role == "owner" {
						group.GroupOwner = strconv.FormatFloat(uid, 'f', 0, 64)
					} else if role == "admin" {
						group.GroupAdmins = append(group.GroupAdmins, strconv.FormatFloat(uid, 'f', 0, 64))
					}
				}
			}
			return group, nil
		}
	}
	for _, item := range memberList {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		uid, ok := m["user_id"].(float64)
		if !ok {
			continue
		}
		nick, _ := m["nickname"].(string)
		card, _ := m["card"].(string)
		if card != "" {
			nick = card
		}
		memberID := strconv.FormatFloat(uid, 'f', 0, 64)
		group.Members = append(group.Members, platform.MessageMember{UserID: memberID, Nickname: nick})
		if role, _ := m["role"].(string); role == "owner" {
			group.GroupOwner = memberID
		} else if role == "admin" {
			group.GroupAdmins = append(group.GroupAdmins, memberID)
		}
	}
	if group.MemberCount == nil && len(group.Members) > 0 {
		c := len(group.Members)
		group.MemberCount = &c
	}
	return group, nil
}

// extractPlainText extracts plain text from a message chain.
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
