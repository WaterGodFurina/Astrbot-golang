// 企业微信智能机器人 webhook 消息推送客户端。
// 1:1 移植自 wecomai_webhook.py：
//   - 支持 markdown_v2 / image(base64) / file / voice 消息类型；
//   - 长 markdown 按 4096 字节分块；
//   - 文件/语音先经 upload_media 上传获取 media_id；
//   - 消息链转换：Plain/At 累积为 markdown 缓冲，Image/File/Video/Record 单独发送。
package wecom_ai_bot

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- md5 用于图片消息内容指纹（协议要求），非密码学用途
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// WecomAIBotWebhookError 企业微信 webhook 推送异常。
type WecomAIBotWebhookError struct{ msg string }

func (e *WecomAIBotWebhookError) Error() string { return e.msg }

// newWebhookError 构造推送异常。
func newWebhookError(format string, args ...interface{}) error {
	return &WecomAIBotWebhookError{msg: fmt.Sprintf(format, args...)}
}

// WecomAIBotWebhookClient 企业微信智能机器人 webhook 消息推送客户端。
type WecomAIBotWebhookClient struct {
	webhookURL string
	timeout    time.Duration
	webhookKey string
	httpClient *http.Client
}

// NewWecomAIBotWebhookClient 构造 webhook 推送客户端（校验 URL 与 key 参数）。
func NewWecomAIBotWebhookClient(webhookURL string) (*WecomAIBotWebhookClient, error) {
	webhookURL = strings.TrimSpace(webhookURL)
	if webhookURL == "" {
		return nil, newWebhookError("消息推送 webhook URL 不能为空")
	}
	parsed, err := url.Parse(webhookURL)
	if err != nil {
		return nil, newWebhookError("消息推送 webhook URL 无效: %v", err)
	}
	key := strings.TrimSpace(parsed.Query().Get("key"))
	if key == "" {
		return nil, newWebhookError("消息推送 webhook URL 缺少 key 参数")
	}
	return &WecomAIBotWebhookClient{
		webhookURL: webhookURL,
		timeout:    15 * time.Second,
		webhookKey: key,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// buildUploadURL 构造素材上传地址（对应 _build_upload_url）。
func (c *WecomAIBotWebhookClient) buildUploadURL(mediaType string) string {
	return "https://qyapi.weixin.qq.com/cgi-bin/webhook/upload_media?key=" + url.QueryEscape(c.webhookKey) + "&type=" + mediaType
}

// SplitMarkdownV2Content 将 markdown 内容按字节数分块（对应 _split_markdown_v2_content，上限 4096 字节）。
func (c *WecomAIBotWebhookClient) SplitMarkdownV2Content(content string, maxBytes int) []string {
	if maxBytes <= 0 {
		maxBytes = 4096
	}
	if content == "" {
		return []string{}
	}
	var chunks []string
	buffer := strings.Builder{}
	currentSize := 0
	for _, r := range content {
		charSize := len(string(r))
		if currentSize+charSize > maxBytes && buffer.Len() > 0 {
			chunks = append(chunks, buffer.String())
			buffer.Reset()
			buffer.WriteRune(r)
			currentSize = charSize
		} else {
			buffer.WriteRune(r)
			currentSize += charSize
		}
	}
	if buffer.Len() > 0 {
		chunks = append(chunks, buffer.String())
	}
	return chunks
}

// SendPayload 发送 webhook 消息（对应 send_payload）。
func (c *WecomAIBotWebhookClient) SendPayload(ctx context.Context, payload map[string]interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return newWebhookError("Webhook 请求失败: HTTP %d, %s", resp.StatusCode, string(text))
	}
	var result map[string]interface{}
	if err := json.Unmarshal(text, &result); err != nil {
		return newWebhookError("Webhook 响应解析失败: %v", err)
	}
	if errCode, ok := result["errcode"].(float64); ok && int(errCode) != 0 {
		return newWebhookError("Webhook 返回错误: %v %v", result["errcode"], result["errmsg"])
	}
	logger.Debug("企业微信消息推送成功: %v", payload["msgtype"])
	return nil
}

// SendMarkdownV2 发送 markdown_v2 消息（对应 send_markdown_v2）。
func (c *WecomAIBotWebhookClient) SendMarkdownV2(ctx context.Context, content string) error {
	for _, chunk := range c.SplitMarkdownV2Content(content, 4096) {
		if err := c.SendPayload(ctx, map[string]interface{}{
			"msgtype":     "markdown_v2",
			"markdown_v2": map[string]interface{}{"content": chunk},
		}); err != nil {
			return err
		}
	}
	return nil
}

// SendImageBase64 发送 base64 图片消息（对应 send_image_base64）。
func (c *WecomAIBotWebhookClient) SendImageBase64(ctx context.Context, imageBase64 string) error {
	imageBytes, err := base64.StdEncoding.DecodeString(imageBase64)
	if err != nil {
		return err
	}
	// #nosec G401 -- md5 是企业微信智能机器人图片消息协议的必填字段（用于
	// 服务端校验图片内容，非安全用途），仅作内容指纹，不用于密码学场景。
	sum := md5.Sum(imageBytes) // nosemgrep: go.lang.security.audit.crypto.use_of_weak_crypto.use-of-md5
	return c.SendPayload(ctx, map[string]interface{}{
		"msgtype": "image",
		"image": map[string]interface{}{
			"base64": imageBase64,
			"md5":    hex.EncodeToString(sum[:]),
		},
	})
}

// UploadMedia 上传媒体素材（对应 upload_media，media_type: file / voice）。
func (c *WecomAIBotWebhookClient) UploadMedia(ctx context.Context, filePath, mediaType string) (string, error) {
	if _, err := os.Stat(filePath); err != nil {
		return "", newWebhookError("文件不存在: %s", filePath)
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("media", filepath.Base(filePath))
	if err != nil {
		return "", err
	}
	// #nosec G304 -- filePath 为调用方指定的待上传素材路径（bot 管理员/插件控制），
	// 用于上传企业微信素材，属预期功能而非任意文件包含。
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(part, f); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.buildUploadURL(mediaType), &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", newWebhookError("上传媒体失败: HTTP %d, %s", resp.StatusCode, string(text))
	}
	var result map[string]interface{}
	if err := json.Unmarshal(text, &result); err != nil {
		return "", newWebhookError("上传媒体响应解析失败: %v", err)
	}
	if errCode, ok := result["errcode"].(float64); ok && int(errCode) != 0 {
		return "", newWebhookError("上传媒体失败: %v %v", result["errcode"], result["errmsg"])
	}
	mediaID, _ := result["media_id"].(string)
	if mediaID == "" {
		return "", newWebhookError("上传媒体失败: 返回缺少 media_id")
	}
	return mediaID, nil
}

// SendFile 发送文件消息（对应 send_file）。
func (c *WecomAIBotWebhookClient) SendFile(ctx context.Context, filePath string) error {
	mediaID, err := c.UploadMedia(ctx, filePath, "file")
	if err != nil {
		return err
	}
	return c.SendPayload(ctx, map[string]interface{}{
		"msgtype": "file",
		"file":    map[string]interface{}{"media_id": mediaID},
	})
}

// SendVoice 发送语音消息（对应 send_voice）。
func (c *WecomAIBotWebhookClient) SendVoice(ctx context.Context, filePath string) error {
	mediaID, err := c.UploadMedia(ctx, filePath, "voice")
	if err != nil {
		return err
	}
	return c.SendPayload(ctx, map[string]interface{}{
		"msgtype": "voice",
		"voice":   map[string]interface{}{"media_id": mediaID},
	})
}

// isStreamSupportedComponent 判断是否为流式消息支持的组件（Plain/Image/At）。
func isStreamSupportedComponent(comp message.Component) bool {
	switch comp.(type) {
	case *message.Plain, *message.Image, *message.At:
		return true
	}
	return false
}

// SendMessageChain 发送消息链（对应 send_message_chain）。
// unsupportedOnly 为 true 时只发送流式不支持的组件（图片/文件等），文本走流式通道。
func (c *WecomAIBotWebhookClient) SendMessageChain(ctx context.Context, chain *message.MessageChain, unsupportedOnly bool) error {
	flushMarkdown := func(parts *[]string) error {
		content := strings.TrimSpace(strings.Join(*parts, ""))
		*parts = nil
		if content == "" {
			return nil
		}
		return c.SendMarkdownV2(ctx, content)
	}

	var markdownBuffer []string
	for _, comp := range chain.Chain {
		if unsupportedOnly && isStreamSupportedComponent(comp) {
			continue
		}
		switch cc := comp.(type) {
		case *message.Plain:
			markdownBuffer = append(markdownBuffer, cc.Text)
		case *message.At:
			mentionName := cc.Name
			if mentionName == "" {
				mentionName = cc.TargetID
			}
			markdownBuffer = append(markdownBuffer, " @"+mentionName+" ")
		case *message.Image:
			if err := flushMarkdown(&markdownBuffer); err != nil {
				return err
			}
			b64, err := componentImageBase64(cc)
			if err != nil {
				logger.I18nWarn("图片消息转换失败，已跳过: %v", err)
				continue
			}
			if err := c.SendImageBase64(ctx, b64); err != nil {
				return err
			}
		case *message.File:
			if err := flushMarkdown(&markdownBuffer); err != nil {
				return err
			}
			filePath, err := componentFilePath(cc.Path, cc.URL)
			if err != nil {
				logger.I18nWarn("文件消息缺少有效文件路径，已跳过: %v", err)
				continue
			}
			defer removeWecomAITemp(filePath)
			if err := c.SendFile(ctx, filePath); err != nil {
				return err
			}
		case *message.Video:
			if err := flushMarkdown(&markdownBuffer); err != nil {
				return err
			}
			videoPath, err := componentFilePath(cc.Path, cc.URL)
			if err != nil {
				logger.I18nWarn("视频消息缺少有效文件路径，已跳过: %v", err)
				continue
			}
			defer removeWecomAITemp(videoPath)
			if err := c.SendFile(ctx, videoPath); err != nil {
				return err
			}
		case *message.Record:
			if err := flushMarkdown(&markdownBuffer); err != nil {
				return err
			}
			sourcePath, err := componentFilePath(cc.Path, cc.URL)
			if err != nil {
				logger.I18nWarn("语音消息缺少有效文件路径，已跳过: %v", err)
				continue
			}
			defer removeWecomAITemp(sourcePath)
			// Python 此处会将非 amr 音频转码为 amr；Go 端直接上传原文件
			if err := c.SendVoice(ctx, sourcePath); err != nil {
				return err
			}
		default:
			logger.I18nWarn("企业微信消息推送暂不支持组件类型 %s，已跳过", comp.Type())
		}
	}
	return flushMarkdown(&markdownBuffer)
}

// componentImageBase64 将图片组件转换为 base64（优先本地文件，其次 URL 下载）。
func componentImageBase64(img *message.Image) (string, error) {
	if img.Base64 != "" {
		return img.Base64, nil
	}
	path := strings.TrimSpace(img.Path)
	if path == "" {
		path = strings.TrimSpace(img.File)
	}
	if path != "" {
		// #nosec G304 -- path 为消息图片组件指定的本地文件路径（由 bot 管理员/插件提供），
		// 用于读取后以 base64 发送，属预期功能而非任意文件包含。
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return base64.StdEncoding.EncodeToString(data), nil
	}
	if img.URL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		data, err := platform.SafeDownloadBytes(ctx, img.URL, 32<<20)
		if err != nil {
			return "", err
		}
		return base64.StdEncoding.EncodeToString(data), nil
	}
	return "", fmt.Errorf("图片组件没有可用的路径或 URL")
}

// removeWecomAITemp 删除发送链路经 componentFilePath 下载到临时目录的媒体文件
// （仅限本模块创建的 astrbot_wecomai_* 文件，避免误删调用方自备文件）。
func removeWecomAITemp(p string) {
	if p != "" && strings.HasPrefix(p, os.TempDir()+string(os.PathSeparator)) &&
		strings.Contains(filepath.Base(p), "astrbot_wecomai_") {
		_ = os.Remove(p)
	}
}

// componentFilePath 将文件/语音/视频组件解析为本地路径（下载 URL 到临时文件）。
func componentFilePath(path, url string) (string, error) {
	path = strings.TrimSpace(path)
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	if url != "" {
		tmp := filepath.Join(os.TempDir(), fmt.Sprintf("astrbot_wecomai_%d", time.Now().UnixNano()))
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		data, err := platform.SafeDownloadBytes(ctx, url, 64<<20)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(tmp, data, 0600); err != nil {
			return "", err
		}
		return tmp, nil
	}
	return "", fmt.Errorf("组件没有可用的路径或 URL")
}
