// 企业微信智能机器人 HTTP 服务器。
// 1:1 移植自 wecomai_server.py：
//   - GET /webhook/wecom-ai-bot：URL 验证（verify_url）；
//   - POST /webhook/wecom-ai-bot：消息回调（handle_callback），返回加密后的响应。
package wecom_ai_bot

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"time"
)

// WecomAIBotServer 企业微信智能机器人 HTTP 回调服务器。
type WecomAIBotServer struct {
	host string
	port int

	apiClient *WecomAIBotAPIClient
	// messageHandler 消息处理回调：返回加密后的响应消息（无需响应时返回空串）
	messageHandler func(messageData map[string]interface{}, callbackParams map[string]string) (string, error)

	httpSrv *http.Server
}

// NewWecomAIBotServer 构造回调服务器。
func NewWecomAIBotServer(host string, port int, apiClient *WecomAIBotAPIClient,
	messageHandler func(map[string]interface{}, map[string]string) (string, error)) *WecomAIBotServer {
	return &WecomAIBotServer{
		host:           host,
		port:           port,
		apiClient:      apiClient,
		messageHandler: messageHandler,
	}
}

// Handler 返回服务器根处理器（供 net/http 使用）。
func (s *WecomAIBotServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/wecom-ai-bot", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			s.handleVerify(w, r)
			return
		}
		s.handleCallback(w, r)
	})
	return mux
}

// handleVerify 处理 URL 验证请求（GET）。
func (s *WecomAIBotServer) handleVerify(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	msgSignature := query.Get("msg_signature")
	timestamp := query.Get("timestamp")
	nonce := query.Get("nonce")
	echostr := query.Get("echostr")

	if msgSignature == "" || timestamp == "" || nonce == "" || echostr == "" {
		logger.I18nError("URL 验证参数缺失")
		http.Error(w, "verify fail", http.StatusBadRequest)
		return
	}

	logger.I18nInfo("收到企业微信智能机器人 WebHook URL 验证请求。")
	result := s.apiClient.VerifyURL(msgSignature, timestamp, nonce, echostr)
	w.Header().Set("Content-Type", "text/plain")
	// #nosec G705 -- 响应类型为 text/plain，写回的是解密后的 echostr 明文，
	// 非 HTML 上下文，不构成反射型 XSS。
	_, _ = io.WriteString(w, result) // nosemgrep: go.lang.security.audit.xss.no-io-writestring-to-responsewriter.no-io-writestring-to-responsewriter
}

// handleCallback 处理消息回调（POST）。
func (s *WecomAIBotServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	msgSignature := query.Get("msg_signature")
	timestamp := query.Get("timestamp")
	nonce := query.Get("nonce")

	if msgSignature == "" || timestamp == "" || nonce == "" {
		logger.I18nError("消息回调参数缺失")
		http.Error(w, "缺少必要参数", http.StatusBadRequest)
		return
	}
	// 时间戳新鲜度校验：拒绝与当前时间偏差超过 5 分钟的请求（防重放）。
	if ts, err := strconv.ParseInt(timestamp, 10, 64); err != nil || math.Abs(float64(time.Now().Unix()-ts)) > 300 {
		http.Error(w, "timestamp 过期", http.StatusBadRequest)
		return
	}
	logger.Debug("收到消息回调，msg_signature=%s, timestamp=%s, nonce=%s", msgSignature, timestamp, nonce)

	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		logger.I18nError("读取请求体失败: %v", err)
		http.Error(w, "内部服务器错误", http.StatusInternalServerError)
		return
	}

	retCode, messageData := s.apiClient.DecryptMessage(body, msgSignature, timestamp, nonce)
	if retCode != MsgCryptOK || messageData == nil {
		logger.I18nError("消息解密失败，错误码: %d", retCode)
		http.Error(w, "消息解密失败", http.StatusBadRequest)
		return
	}

	response := ""
	if s.messageHandler != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.I18nError("消息处理器执行异常: %v", r)
				}
			}()
			response, err = s.messageHandler(messageData, map[string]string{
				"nonce":     nonce,
				"timestamp": timestamp,
			})
		}()
		if err != nil {
			logger.I18nError("消息处理器执行异常: %v", err)
			http.Error(w, "消息处理异常", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "text/plain")
	if response != "" {
		// #nosec no-io-writestring-to-responsewriter -- 回调响应为企业微信智能机器人协议的
		// 文本应答（非 HTML，响应对象是企微服务器），Content-Type 已固定为 text/plain。
		_, _ = io.WriteString(w, response) // nosemgrep: go.lang.security.audit.xss.no-io-writestring-to-responsewriter.no-io-writestring-to-responsewriter
		return
	}
	_, _ = io.WriteString(w, "success")
}

// Start 启动回调服务器（非阻塞）。
func (s *WecomAIBotServer) Start() error {
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.host, s.port))
	if err != nil {
		return fmt.Errorf("企业微信智能机器人服务器监听 %s:%d 失败: %w", s.host, s.port, err)
	}
	s.httpSrv = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", s.host, s.port),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	logger.I18nInfo("启动企业微信智能机器人服务器，监听 %s:%d", s.host, s.port)
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.I18nError("服务器运行异常: %v", err)
		}
	}()
	return nil
}

// Shutdown 关闭服务器。
func (s *WecomAIBotServer) Shutdown() {
	if s.httpSrv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	logger.I18nInfo("企业微信智能机器人服务器正在关闭...")
	_ = s.httpSrv.Shutdown(ctx)
}
