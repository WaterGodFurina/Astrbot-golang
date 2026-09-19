// Slack Webhook 模式 HTTP 服务器。
// 1:1 移植自 astrbot/core/platform/sources/slack/client.py 的 SlackWebhookClient。
//
// 提供两个端点：
//   - {path}：POST 回调入口（签名校验 + url_verification + event_callback）
//   - /health：健康检查
//
// 统一 Webhook 模式下不启动独立服务器，由 dashboard 的
// /api/v1/webhooks/platforms/{webhook_uuid} 注入请求（WebhookCallback）。
package slack

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

// maxWebhookBodySize 限制 Slack 回调请求体大小上限（1MB）。
const maxWebhookBodySize = 1 << 20

// slackEventDedupTTL 事件去重窗口（Slack 回调超时会重推同一事件）。
const slackEventDedupTTL = 60 * time.Second

// randRead 读取随机字节（对应 Python 的 uuid4 随机源）。
func randRead(b []byte) (int, error) { return rand.Read(b) }

// webhookServerLogger 是 Webhook 服务器使用的日志组件。
var webhookServerLogger = log.GetDefault().WithComponent("Slack")

// SlackWebhookServer 处理 Slack 的 HTTP 回调。
type SlackWebhookServer struct {
	signingSecret string
	path          string
	// eventHandler 接收 event_callback 的完整 payload（对应 Python 的 event_handler）。
	eventHandler func(eventData map[string]interface{})

	srv    *http.Server
	stopCh chan struct{}

	// srvMu 保护 srv 字段以及 stopped 状态：Start 与 Stop 可能来自不同 goroutine
	// （外部 Stop 可能早于 Start 完成，ctx 取消也会触发 Stop）。
	srvMu   sync.Mutex
	stopped bool

	// seenEventIDs 事件去重缓存（event_id -> 时间戳，带 TTL 惰性淘汰）。
	seenMu       sync.Mutex
	seenEventIDs map[string]time.Time
}

// NewSlackWebhookServer 创建 Slack Webhook 服务器。
func NewSlackWebhookServer(signingSecret, path string, eventHandler func(map[string]interface{})) *SlackWebhookServer {
	s := &SlackWebhookServer{
		signingSecret: signingSecret,
		path:          path,
		eventHandler:  eventHandler,
		stopCh:        make(chan struct{}),
		seenEventIDs:  make(map[string]time.Time),
	}
	return s
}

// markSeen 记录并检查事件 id（带 TTL 惰性淘汰，参考 qqofficial_webhook 的实现）。
// 返回 false 表示该事件在去重窗口内已处理过。
func (s *SlackWebhookServer) markSeen(eventID string) bool {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	now := time.Now()
	for id, ts := range s.seenEventIDs {
		if now.Sub(ts) > slackEventDedupTTL {
			delete(s.seenEventIDs, id)
		}
	}
	if _, seen := s.seenEventIDs[eventID]; seen {
		return false
	}
	s.seenEventIDs[eventID] = now
	return true
}

// Start 启动 Webhook HTTP 服务器。
func (s *SlackWebhookServer) Start(ctx context.Context, host string, port int) error {
	mux := http.NewServeMux()
	mux.HandleFunc(s.path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.HandleCallback(w, r)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSONError(w, http.StatusOK, map[string]interface{}{
			"status":  "ok",
			"service": "slack-webhook",
		})
	})

	addr := fmt.Sprintf("%s:%d", host, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("slack webhook 服务器绑定 %s 失败: %w", addr, err)
	}
	srv := &http.Server{Handler: mux}
	s.srvMu.Lock()
	if s.stopped {
		// Stop 已在 Start 完成前被调用（例如 ctx 启动前已取消，或外部提前 Stop）：
		// 直接关闭监听，绝不注册一个永不停止的 HTTP 服务器。
		s.srvMu.Unlock()
		_ = ln.Close()
		return nil
	}
	s.srv = srv
	s.srvMu.Unlock()
	go func() {
		<-ctx.Done()
		s.Stop()
	}()
	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			webhookServerLogger.I18nWarn("Slack Webhook 服务器退出: %v", serveErr)
		}
	}()
	webhookServerLogger.I18nInfo("Slack Webhook 服务器启动中，监听 %s...", addr)
	return nil
}

// Stop 停止 Webhook 服务器。幂等：可被 ctx 取消 goroutine 与外部调用重复触发；
// 若在 Start 之前调用则记录 stopped 标记，Start 会发现并立即关闭监听。
func (s *SlackWebhookServer) Stop() {
	s.srvMu.Lock()
	if s.stopped {
		s.srvMu.Unlock()
		return
	}
	s.stopped = true
	srv := s.srv
	s.srvMu.Unlock()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	webhookServerLogger.I18nInfo("Slack Webhook 服务器已停止")
}

// HandleCallback 处理 Slack 回调请求，可被统一 Webhook 入口复用。
// 对应 Python 的 handle_callback。
func (s *SlackWebhookServer) HandleCallback(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			webhookServerLogger.I18nError("处理 Slack 事件时出错: %v", rec)
			writeJSONError(w, http.StatusInternalServerError, map[string]interface{}{
				"error": "Internal Server Error",
			})
		}
	}()

	// 读取请求体
	bodyBytes, readErr := readRequestBody(r)
	if readErr != nil {
		webhookServerLogger.I18nWarn("读取 Slack 事件请求体失败: %v", readErr)
		writeJSONError(w, http.StatusBadRequest, map[string]interface{}{
			"error": "request body too large",
		})
		return
	}
	eventData := map[string]interface{}{}
	if err := json.Unmarshal(bodyBytes, &eventData); err != nil {
		webhookServerLogger.I18nWarn("解析 Slack 事件 JSON 失败: %v", err)
		writeJSONError(w, http.StatusBadRequest, map[string]interface{}{
			"error": "invalid JSON",
		})
		return
	}

	// 校验 Slack 请求签名
	timestamp := r.Header.Get("X-Slack-Request-Timestamp")
	signature := r.Header.Get("X-Slack-Signature")
	if timestamp == "" || signature == "" {
		writeJSONError(w, http.StatusBadRequest, map[string]interface{}{
			"error": "Missing headers",
		})
		return
	}
	// 时间戳新鲜度校验：拒绝时间戳与当前时间偏差超过 5 分钟的请求（防重放）。
	if !isFreshTimestamp(timestamp, slackTimestampSkew) {
		webhookServerLogger.I18nWarn("Slack request timestamp is invalid or stale")
		writeJSONError(w, http.StatusBadRequest, map[string]interface{}{
			"error": "Invalid timestamp",
		})
		return
	}
	if !verifySlackSignature(s.signingSecret, bodyBytes, timestamp, signature) {
		webhookServerLogger.I18nWarn("Slack request signature verification failed")
		writeJSONError(w, http.StatusBadRequest, map[string]interface{}{
			"error": "Invalid signature",
		})
		return
	}
	webhookServerLogger.I18nInfo("收到 Slack 事件: %v", eventData)

	// 处理 URL 验证事件
	if eventType, _ := eventData["type"].(string); eventType == "url_verification" {
		writeJSONError(w, http.StatusOK, map[string]interface{}{
			"challenge": eventData["challenge"],
		})
		return
	}
	// 处理事件
	if s.eventHandler != nil {
		if eventType, _ := eventData["type"].(string); eventType == "event_callback" {
			if eventID, _ := eventData["event_id"].(string); eventID != "" && !s.markSeen(eventID) {
				return
			}
			// 先返回 200 再异步处理：Slack 要求 3 秒内响应，超时会重推同一事件
			w.WriteHeader(http.StatusOK)
			go func() {
				defer func() {
					if rec := recover(); rec != nil {
						webhookServerLogger.I18nError("处理 Slack 事件时出错: %v", rec)
					}
				}()
				s.eventHandler(eventData)
			}()
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(""))
}

// readRequestBody 读取请求体（上限 maxWebhookBodySize，超限或读取失败时返回错误）。
func readRequestBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodySize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxWebhookBodySize {
		return nil, fmt.Errorf("请求体超过大小上限 %d 字节", maxWebhookBodySize)
	}
	return data, nil
}
