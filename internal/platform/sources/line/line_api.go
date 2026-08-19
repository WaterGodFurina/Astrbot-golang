// LINE Messaging API 客户端。
// 1:1 移植自 astrbot/core/platform/sources/line/line_api.py。
// 使用官方 SDK github.com/line/line-bot-sdk-go/v8（v8.22.0）：
//   - 签名校验：linebot/webhook 的 ValidateSignature
//   - 发送消息：linebot/messaging_api（ReplyMessage / PushMessage）
//   - 下载消息内容：linebot/messaging_api blob API（GetMessageContent）
package line

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/line/line-bot-sdk-go/v8/linebot/messaging_api"
	"github.com/line/line-bot-sdk-go/v8/linebot/webhook"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

var lineLogger = log.GetDefault().WithComponent("Line")

// LineAPIClient 封装 LINE Messaging API 的调用（对应 Python 的 LineAPIClient）。
type LineAPIClient struct {
	channelAccessToken string
	channelSecret      string

	// api 负责 reply/push 等消息发送 API（api.line.me）。
	api *messaging_api.MessagingApiAPI
	// blob 负责消息内容下载 API（api-data.line.me）。
	blob *messaging_api.MessagingApiBlobAPI

	httpClient *http.Client
}

// NewLineAPIClient 创建 LINE API 客户端。
func NewLineAPIClient(channelAccessToken, channelSecret string) (*LineAPIClient, error) {
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
	}
	api, err := messaging_api.NewMessagingApiAPI(channelAccessToken,
		messaging_api.WithHTTPClient(httpClient),
	)
	if err != nil {
		return nil, fmt.Errorf("初始化 LINE Messaging API 失败: %w", err)
	}
	blob, err := messaging_api.NewMessagingApiBlobAPI(channelAccessToken,
		messaging_api.WithBlobHTTPClient(httpClient),
	)
	if err != nil {
		return nil, fmt.Errorf("初始化 LINE Blob API 失败: %w", err)
	}
	return &LineAPIClient{
		channelAccessToken: strings.TrimSpace(channelAccessToken),
		channelSecret:      strings.TrimSpace(channelSecret),
		api:                api,
		blob:               blob,
		httpClient:         httpClient,
	}, nil
}

// WithEndpointOverride 允许测试时把 API 与 Blob 端点替换为 httptest 地址。
func (c *LineAPIClient) WithEndpointOverride(endpoint string) {
	c.api, _ = messaging_api.NewMessagingApiAPI(c.channelAccessToken,
		messaging_api.WithHTTPClient(c.httpClient),
		messaging_api.WithEndpoint(endpoint),
	)
	c.blob, _ = messaging_api.NewMessagingApiBlobAPI(c.channelAccessToken,
		messaging_api.WithBlobHTTPClient(c.httpClient),
		messaging_api.WithBlobEndpoint(endpoint),
	)
}

// VerifySignature 校验 Webhook 请求的 X-Line-Signature 签名。
// 对应 Python 的 verify_signature（HMAC-SHA256 + Base64）。
func (c *LineAPIClient) VerifySignature(rawBody []byte, signature string) bool {
	return webhook.ValidateSignature(c.channelSecret, strings.TrimSpace(signature), rawBody)
}

// ReplyMessage 发送回复消息（replyToken 仅 1 分钟有效）。
// 对应 Python 的 reply_message：最多 5 条消息，超限丢弃。
func (c *LineAPIClient) ReplyMessage(ctx context.Context, replyToken string, messages []messaging_api.MessageInterface) bool {
	if len(messages) > 5 {
		lineLogger.I18nWarn("[LINE] 消息数量超过 5 条，多余消息将被丢弃")
		messages = messages[:5]
	}
	req := &messaging_api.ReplyMessageRequest{
		ReplyToken: replyToken,
		Messages:   messages,
	}
	_, err := c.api.ReplyMessage(req)
	if err != nil {
		lineLogger.I18nError("[LINE] 回复消息失败: %v", err)
		return false
	}
	return true
}

// PushMessage 主动推送消息到会话。
// 对应 Python 的 push_message：最多 5 条消息，超限丢弃。
func (c *LineAPIClient) PushMessage(ctx context.Context, to string, messages []messaging_api.MessageInterface) bool {
	if len(messages) > 5 {
		lineLogger.I18nWarn("[LINE] 消息数量超过 5 条，多余消息将被丢弃")
		messages = messages[:5]
	}
	req := &messaging_api.PushMessageRequest{
		To:       to,
		Messages: messages,
	}
	_, err := c.api.PushMessage(req, "")
	if err != nil {
		lineLogger.I18nError("[LINE] 推送消息失败: %v", err)
		return false
	}
	return true
}

// ContentResult 表示下载到的消息内容。
type ContentResult struct {
	Content     []byte // 文件内容
	ContentType string // Content-Type（可能为空）
	Filename    string // 从 Content-Disposition 提取的文件名（可能为空）
}

// GetMessageContent 下载图片/视频/音频/文件消息内容。
// 对应 Python 的 get_message_content：遇到 202（转码中）轮询转码状态后重试。
func (c *LineAPIClient) GetMessageContent(ctx context.Context, messageID string) (*ContentResult, error) {
	resp, err := c.blob.GetMessageContent(messageID)
	if err != nil {
		// 非 2xx 也会返回 error（含响应体描述）
		lineLogger.I18nWarn("[LINE] 获取消息内容失败: message_id=%s err=%v", messageID, err)
		return nil, err
	}
	defer resp.Body.Close()

	// 202 表示内容仍在转码，等待转码完成后再重试（最多 10 次，每次 1s）
	if resp.StatusCode == http.StatusAccepted {
		if !c.waitForTranscoding(ctx, messageID) {
			return nil, fmt.Errorf("消息内容转码等待超时: %s", messageID)
		}
		retry, err := c.blob.GetMessageContent(messageID)
		if err != nil {
			lineLogger.I18nWarn("[LINE] 获取内容重试失败: message_id=%s err=%v", messageID, err)
			return nil, err
		}
		defer retry.Body.Close()
		if retry.StatusCode != http.StatusOK {
			body := readBody(retry)
			lineLogger.I18nWarn("[LINE] 获取内容重试失败: message_id=%s status=%d body=%s", messageID, retry.StatusCode, body)
			return nil, fmt.Errorf("获取内容重试失败: status=%d", retry.StatusCode)
		}
		return readContentResponse(retry), nil
	}

	if resp.StatusCode != http.StatusOK {
		body := readBody(resp)
		lineLogger.I18nWarn("[LINE] 获取内容失败: message_id=%s status=%d body=%s", messageID, resp.StatusCode, body)
		return nil, fmt.Errorf("获取内容失败: status=%d", resp.StatusCode)
	}
	return readContentResponse(resp), nil
}

// readContentResponse 读取响应体与响应头，对应 Python 的 _read_content_response。
func readContentResponse(resp *http.Response) *ContentResult {
	content := readBody(resp)
	contentType := resp.Header.Get("Content-Type")
	filename := extractFilenameFromDisposition(resp.Header.Get("Content-Disposition"))
	return &ContentResult{
		Content:     content,
		ContentType: contentType,
		Filename:    filename,
	}
}

// readBody 读取响应体（仅 io.EOF 视为正常结束，其他读取错误记录并保留已读部分）。
func readBody(resp *http.Response) []byte {
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			lineLogger.I18nWarn("[LINE] 读取响应体失败: %v", err)
			break
		}
	}
	return buf
}

// waitForTranscoding 轮询转码状态接口（GET /v2/bot/message/{id}/content/transcoding）。
// 对应 Python 的 _wait_for_transcoding：最多 10 次，间隔 1 秒。
func (c *LineAPIClient) waitForTranscoding(ctx context.Context, messageID string) bool {
	for i := 0; i < 10; i++ {
		resp, err := c.blob.GetMessageContentTranscodingByMessageId(messageID)
		if err == nil && resp != nil {
			status := strings.ToLower(string(resp.Status))
			if status == "succeeded" {
				return true
			}
			if status == "failed" {
				return false
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(1 * time.Second):
		}
	}
	return false
}

// extractFilenameFromDisposition 从 Content-Disposition 响应头中提取文件名。
// 对应 Python 的 _extract_filename_from_disposition（支持 filename 与 filename* 两种格式）。
func extractFilenameFromDisposition(disposition string) string {
	if disposition == "" {
		return ""
	}
	for _, part := range strings.Split(disposition, ";") {
		token := strings.TrimSpace(part)
		if strings.HasPrefix(token, "filename*=") {
			val := strings.Trim(strings.TrimPrefix(token, "filename*="), `"`)
			if strings.HasPrefix(strings.ToLower(val), "utf-8''") {
				val = val[7:]
			}
			if decoded, err := url.QueryUnescape(val); err == nil {
				return decoded
			}
			return val
		}
		if strings.HasPrefix(token, "filename=") {
			return strings.Trim(strings.TrimPrefix(token, "filename="), `"`)
		}
	}
	return ""
}

// guessSuffix 根据 Content-Type 猜测文件后缀，失败时使用 fallback。
// 对应 Python 的 _guess_suffix（mimetypes.guess_extension）。
func guessSuffix(contentType string, fallback string) string {
	if contentType == "" {
		return fallback
	}
	baseType := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	if exts, err := mime.ExtensionsByType(baseType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return fallback
}

// hmacBase64 计算 HMAC-SHA256 的 Base64（测试辅助：构造合法签名）。
func hmacBase64(secret string, body []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(body)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
