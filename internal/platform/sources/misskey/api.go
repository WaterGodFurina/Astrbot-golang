// Package misskey - Misskey API 客户端。
// 移植自 astrbot/core/platform/sources/misskey/misskey_api.py。
// REST 部分使用 github.com/yitsushi/go-misskey（纯 Go 官方 API 封装），
// go-misskey 未覆盖的聊天消息端点（chat/messages/create-to-user 等）与文件下载走原生 HTTP。
package misskey

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/yitsushi/go-misskey"
	"github.com/yitsushi/go-misskey/core"
	"github.com/yitsushi/go-misskey/models"
	"github.com/yitsushi/go-misskey/services/drive/files"
	"github.com/yitsushi/go-misskey/services/notes"
)

var apiLogger = log.GetDefault().WithComponent("Misskey-API")

// MisskeyAPI 封装 Misskey REST API 与 WebSocket streaming。
// 对应 Python 的 MisskeyAPI 类。
type MisskeyAPI struct {
	instanceURL  string
	instanceHost string
	accessToken  string
	client       *misskey.Client
	httpClient   *http.Client

	// 下载/安全相关选项
	allowInsecureDownloads bool
	downloadTimeout        int
	chunkSize              int
	maxDownloadBytes       int64

	streaming *StreamingClient
}

// NewMisskeyAPI 创建 Misskey API 客户端。
func NewMisskeyAPI(instanceURL, accessToken string, allowInsecureDownloads bool, downloadTimeout, chunkSize int, maxDownloadBytes int64) *MisskeyAPI {
	baseURL := strings.TrimRight(instanceURL, "/")
	instanceHost := ""
	if u, err := url.Parse(baseURL); err == nil {
		instanceHost = strings.ToLower(u.Hostname())
	}
	return &MisskeyAPI{
		instanceURL:            baseURL,
		instanceHost:           instanceHost,
		accessToken:            accessToken,
		client:                 misskey.NewClient(baseURL, accessToken),
		allowInsecureDownloads: allowInsecureDownloads,
		downloadTimeout:        downloadTimeout,
		chunkSize:              chunkSize,
		maxDownloadBytes:       maxDownloadBytes,
		httpClient: &http.Client{
			Timeout: time.Duration(downloadTimeout) * time.Second,
		},
	}
}

// Close 关闭 streaming 连接与 HTTP 客户端（对应 close）。
func (a *MisskeyAPI) Close() {
	if a.streaming != nil {
		a.streaming.Disconnect()
		a.streaming = nil
	}
	a.httpClient.CloseIdleConnections()
	apiLogger.Debug("Misskey API 客户端已关闭")
}

// GetStreamingClient 获取（或创建）streaming 客户端（对应 get_streaming_client）。
func (a *MisskeyAPI) GetStreamingClient() *StreamingClient {
	if a.streaming == nil {
		a.streaming = NewStreamingClient(a.instanceURL, a.accessToken)
	}
	return a.streaming
}

// GetCurrentUser 获取当前用户信息（对应 get_current_user，POST /api/i）。
func (a *MisskeyAPI) GetCurrentUser(ctx context.Context) (models.User, error) {
	return a.client.Users().Me()
}

// UploadFile 上传本地文件到网盘（对应 upload_file，POST /api/drive/files/create）。
// 返回提取到的文件 ID。
func (a *MisskeyAPI) UploadFile(localPath, name, folderID string) (string, error) {
	if localPath == "" {
		return "", fmt.Errorf("未提供上传文件路径")
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		return "", fmt.Errorf("本地文件不存在: %s: %w", localPath, err)
	}
	filename := name
	if filename == "" {
		filename = localPath[strings.LastIndex(localPath, "/")+1:]
	}
	req := files.CreateRequest{
		FolderID: folderID,
		Name:     filename,
		Content:  data,
	}
	result, err := a.client.Drive().File().Create(req)
	if err != nil {
		return "", err
	}
	apiLogger.Debug("Misskey API 本地文件上传成功: %s -> %s", filename, result.ID)
	return result.ID, nil
}

// uploadAndFindFile 简化的文件上传（对应 upload_and_find_file）：
// 下载 URL 内容后走本地上传，立即获得文件 ID。
func (a *MisskeyAPI) uploadAndFindFile(ctx context.Context, urlStr, name, folderID string) (string, error) {
	if urlStr == "" {
		return "", fmt.Errorf("URL不能为空")
	}
	// SSRF 防护：拒绝下载到内网/环回/链路本地等非公网地址
	if err := a.validateDownloadURL(urlStr); err != nil {
		apiLogger.Warn("Misskey API 拒绝下载 URL %s: %v", urlStr, err)
		return "", err
	}

	// SSL 验证下载，失败则重试不验证 SSL（对应 Python 的下载回退逻辑）
	tmpBytes, err := a.downloadWithSSL(ctx, urlStr, true)
	if err != nil {
		apiLogger.Debug("Misskey API SSL 验证下载失败: %v，重试不验证 SSL", err)
		if !a.allowInsecureDownloads {
			return "", err
		}
		apiLogger.Warn("Misskey API SSL 验证失败，allow_insecure_downloads 已开启，将关闭 SSL 验证重试: %v", err)
		tmpBytes, err = a.downloadWithSSL(ctx, urlStr, false)
		if err != nil {
			return "", err
		}
	}

	// 写入临时文件后本地上传
	tmp, err := os.CreateTemp("", "astrbot-misskey-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(tmpBytes); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()

	fileID, err := a.UploadFile(tmpPath, name, folderID)
	if err != nil {
		apiLogger.Error("Misskey API 本地上传失败: %v", err)
		return "", err
	}
	return fileID, nil
}

// maxDownloadRedirects 限制下载重定向次数，防止 SSRF 通过跳转逃逸主机校验。
const maxDownloadRedirects = 10

// blockedDownloadPrefixes 是禁止下载的内网/环回/链路本地/保留地址段
// （含云元数据 169.254.169.254），防止服务端请求伪造（SSRF）。
var blockedDownloadPrefixes = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("100.64.0.0/10"),
}

// downloadClient 构造带下载超时与 SSRF 重定向校验的下载客户端。
func (a *MisskeyAPI) downloadClient(verify bool) *http.Client {
	client := &http.Client{
		Timeout: time.Duration(a.downloadTimeout) * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxDownloadRedirects {
				return fmt.Errorf("下载重定向次数过多")
			}
			if req.URL == nil {
				return fmt.Errorf("下载重定向目标缺少 URL")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("下载重定向目标必须为 http(s)")
			}
			return a.validateDownloadHost(req.URL.Hostname())
		},
	}
	if !verify {
		client.Transport = &http.Transport{
			TLSClientConfig: insecureTLSConfig(),
		}
	}
	return client
}

// validateDownloadURL 校验下载目标：仅允许 http(s)，且主机不得为内网/保留地址。
func (a *MisskeyAPI) validateDownloadURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("无法解析下载 URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("下载 URL 必须为 http(s): %s", rawURL)
	}
	return a.validateDownloadHost(u.Hostname())
}

// validateDownloadHost 解析主机并拒绝环回/私网/链路本地（含云元数据）等非公网地址。
// 本机 misskey 实例域名始终放行（自建内网实例仍需下载本站文件）。
func (a *MisskeyAPI) validateDownloadHost(host string) error {
	if host == "" {
		return fmt.Errorf("下载 URL 缺少主机名")
	}
	if a.instanceHost != "" && strings.EqualFold(host, a.instanceHost) {
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("下载主机解析失败 %q: %v", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("下载主机 %q 无解析结果", host)
	}
	for _, ip := range ips {
		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		addr = addr.Unmap()
		for _, p := range blockedDownloadPrefixes {
			if p.Contains(addr) {
				return fmt.Errorf("拒绝下载内网/保留地址 %s", addr)
			}
		}
	}
	return nil
}

// downloadWithSSL 下载文件内容，可按需关闭 SSL 验证。
func (a *MisskeyAPI) downloadWithSSL(ctx context.Context, urlStr string, verify bool) ([]byte, error) {
	client := a.downloadClient(verify)
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 %s 失败: %d", urlStr, resp.StatusCode)
	}
	var reader io.Reader = resp.Body
	if a.maxDownloadBytes > 0 {
		reader = io.LimitReader(resp.Body, a.maxDownloadBytes+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if a.maxDownloadBytes > 0 && int64(len(data)) > a.maxDownloadBytes {
		return nil, fmt.Errorf("下载内容超过最大允许字节数 %d", a.maxDownloadBytes)
	}
	return data, nil
}

// CreateNote 创建帖子（对应 create_note，POST /api/notes/create）。
// 通过 go-misskey 的 notes.Create 构造请求。
func (a *MisskeyAPI) CreateNote(text, visibility string, replyID string, visibleUserIDs, fileIDs []string, localOnly bool, cw string, poll map[string]interface{}, renoteID, channelID string) (map[string]interface{}, error) {
	req := notes.CreateRequest{
		Visibility: models.Visibility(visibility),
		LocalOnly:  localOnly,
	}
	if text != "" {
		req.Text = core.NewString(text)
	}
	if replyID != "" {
		req.ReplyID = core.NewString(replyID)
	}
	if cw != "" {
		req.CW = core.NewString(cw)
	}
	if renoteID != "" {
		req.RenoteID = core.NewString(renoteID)
	}
	if channelID != "" {
		req.ChannelID = core.NewString(channelID)
	}
	if len(fileIDs) > 0 {
		req.FileIDs = fileIDs
	}
	if len(visibleUserIDs) > 0 && visibility == "specified" {
		req.VisibleUserIDs = visibleUserIDs
	}
	// poll 字典（来自 extra_data）转换为 SDK 结构
	if poll != nil {
		req.Poll = buildPoll(poll)
	}

	result, err := a.client.Notes().Create(req)
	if err != nil {
		return nil, err
	}
	noteID := result.CreatedNote.ID
	apiLogger.Debug("Misskey API 发帖成功: %s", noteID)
	return map[string]interface{}{
		"createdNote": map[string]interface{}{"id": noteID},
	}, nil
}

// buildPoll 将 poll 字典转换为 notes.Poll（choices 为字符串列表）。
func buildPoll(poll map[string]interface{}) *notes.Poll {
	p := &notes.Poll{}
	if multiple, ok := poll["multiple"].(bool); ok {
		p.Multiple = multiple
	}
	if choices, ok := poll["choices"].([]interface{}); ok {
		for _, c := range choices {
			switch v := c.(type) {
			case string:
				p.Choices = append(p.Choices, v)
			case map[string]interface{}:
				if text, ok := v["text"].(string); ok {
					p.Choices = append(p.Choices, text)
				}
			}
		}
	}
	return p
}

// SendMessage 发送聊天消息（对应 send_message，POST /api/chat/messages/create-to-user）。
// payload 由调用方构造（toUserId/text/fileId）。
func (a *MisskeyAPI) SendMessage(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, error) {
	result, err := a.apiRequest(ctx, "chat/messages/create-to-user", payload, false)
	if err != nil {
		return nil, err
	}
	messageID, _ := result["id"].(string)
	apiLogger.Debug("Misskey API 聊天消息发送成功: %s", messageID)
	return result, nil
}

// SendRoomMessage 发送房间消息（对应 send_room_message，POST /api/chat/messages/create-to-room）。
func (a *MisskeyAPI) SendRoomMessage(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, error) {
	result, err := a.apiRequest(ctx, "chat/messages/create-to-room", payload, false)
	if err != nil {
		return nil, err
	}
	messageID, _ := result["id"].(string)
	apiLogger.Debug("Misskey API 房间消息发送成功: %s", messageID)
	return result, nil
}

// apiRequest 通用 API 请求（对应 _make_request + _process_response + _handle_response_status）。
// payload 会自动带上 i（token）字段。retry=true 时对网络错误/429/5xx 最多重试 3 次；
// 发消息类端点（create 类）非幂等，重复请求会导致重复发送，必须传 false 禁止重试。
func (a *MisskeyAPI) apiRequest(ctx context.Context, endpoint string, data map[string]interface{}, retry bool) (map[string]interface{}, error) {
	urlStr := a.instanceURL + "/api/" + endpoint
	payload := map[string]interface{}{"i": a.accessToken}
	for k, v := range data {
		payload[k] = v
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	attempts := 1
	if retry {
		attempts = 3
	}
	// 网络错误/429/5xx 在 retry=true 时最多重试 3 次（对应 retry_async 的指数退避）
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("HTTP 请求错误: %w", err)
			apiLogger.Error("Misskey API HTTP 请求错误: %v", err)
		} else {
			result, procErr := a.processResponse(resp, endpoint)
			resp.Body.Close()
			if procErr != nil {
				lastErr = procErr
			} else {
				return result, nil
			}
		}
		// 仅在可重试错误（连接错误/429/5xx）时重试
		if !retry || !isRetryableError(lastErr) || attempt == attempts {
			break
		}
		apiLogger.Warn("Misskey API %s 第 %d 次重试失败: %v", endpoint, attempt, lastErr)
		time.Sleep(backoffDelay(attempt))
	}
	return nil, lastErr
}

// processResponse 处理 API 响应（对应 _process_response），返回解码后的 JSON 对象。
func (a *MisskeyAPI) processResponse(resp *http.Response, endpoint string) (map[string]interface{}, error) {
	if resp.StatusCode == http.StatusOK {
		var result map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			apiLogger.Error("Misskey API 响应格式错误: %v", err)
			return nil, fmt.Errorf("Invalid JSON response")
		}
		apiLogger.Debug("Misskey API 请求成功: %s", endpoint)
		return result, nil
	}
	body, _ := io.ReadAll(resp.Body)
	apiLogger.Error("Misskey API 请求失败: %s - HTTP %d, 响应: %s", endpoint, resp.StatusCode, body)
	return nil, handleResponseStatus(resp.StatusCode, endpoint)
}

// handleResponseStatus 处理 HTTP 响应状态码（对应 _handle_response_status）。
func handleResponseStatus(status int, endpoint string) error {
	switch status {
	case http.StatusBadRequest:
		return fmt.Errorf("Bad request for %s", endpoint)
	case http.StatusUnauthorized:
		return fmt.Errorf("Unauthorized access for %s", endpoint)
	case http.StatusForbidden:
		return fmt.Errorf("Forbidden access for %s", endpoint)
	case http.StatusNotFound:
		return fmt.Errorf("Resource not found for %s", endpoint)
	case http.StatusRequestEntityTooLarge:
		return fmt.Errorf("Request entity too large for %s", endpoint)
	case http.StatusTooManyRequests:
		return fmt.Errorf("Rate limit exceeded for %s", endpoint)
	case http.StatusInternalServerError:
		return fmt.Errorf("Internal server error for %s", endpoint)
	case http.StatusBadGateway:
		return fmt.Errorf("Bad gateway for %s", endpoint)
	case http.StatusServiceUnavailable:
		return fmt.Errorf("Service unavailable for %s", endpoint)
	case http.StatusGatewayTimeout:
		return fmt.Errorf("Gateway timeout for %s", endpoint)
	}
	return fmt.Errorf("HTTP %d for %s", status, endpoint)
}

// isRetryableError 判断错误是否可重试（对应 retry_async 的 retryable_exceptions）。
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// 网络连接错误（HTTP 请求错误）
	if strings.HasPrefix(msg, "HTTP 请求错误") {
		return true
	}
	// 429 频率限制
	if strings.Contains(msg, "Rate limit") {
		return true
	}
	// 5xx 服务器错误
	return strings.Contains(msg, "Internal server error") ||
		strings.Contains(msg, "Bad gateway") ||
		strings.Contains(msg, "Service unavailable") ||
		strings.Contains(msg, "Gateway timeout")
}

// backoffDelay 计算第 attempt 次重试前的退避时间（指数退避 + 上限）。
func backoffDelay(attempt int) time.Duration {
	delay := time.Duration(1) * time.Second
	for i := 1; i < attempt; i++ {
		delay *= 2
	}
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return delay
}

// insecureTLSConfig 返回关闭 SSL 验证的 TLS 配置。
func insecureTLSConfig() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- 用户显式开启的不安全下载选项
}
