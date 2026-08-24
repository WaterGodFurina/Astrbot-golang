// Package mattermost implements a Mattermost platform adapter.
// 移植自 astrbot/core/platform/sources/mattermost/client.py
package mattermost

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// MattermostClient 封装 Mattermost REST API 与文件上传逻辑。
// 对应 Python 的 MattermostClient（无现成 Go SDK，全部手写）。
type MattermostClient struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewMattermostClient 创建一个新的 Mattermost 客户端。
func NewMattermostClient(baseURL, token string) *MattermostClient {
	return &MattermostClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client: &http.Client{
			Timeout: 30 * time.Second, // 对应 aiohttp.ClientTimeout(total=30)
		},
	}
}

// Close 释放底层 HTTP 连接池。
func (c *MattermostClient) Close() {
	c.client.CloseIdleConnections()
}

// 带 JSON Content-Type 的请求头（用于 GET/POST JSON）。
func (c *MattermostClient) jsonHeaders() http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+c.token)
	h.Set("Content-Type", "application/json")
	return h
}

// 仅带认证的请求头（用于文件下载/上传）。
func (c *MattermostClient) authHeaders() http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+c.token)
	return h
}

// getJSON 发起 GET 请求并解析 JSON 对象（对应 get_json）。
func (c *MattermostClient) getJSON(ctx context.Context, path string) (map[string]interface{}, error) {
	u := c.baseURL + "/api/v4/" + strings.TrimLeft(path, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.jsonHeaders()
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("mattermost GET %s 失败: %d %s", path, resp.StatusCode, body)
	}
	data, err := decodeJSONObject(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("mattermost GET %s 返回了非对象 JSON: %w", path, err)
	}
	return data, nil
}

// postJSON 发起 POST 请求并解析 JSON 对象（对应 post_json）。
func (c *MattermostClient) postJSON(ctx context.Context, path string, payload map[string]interface{}) (map[string]interface{}, error) {
	u := c.baseURL + "/api/v4/" + strings.TrimLeft(path, "/")
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header = c.jsonHeaders()
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("mattermost POST %s 失败: %d %s", path, resp.StatusCode, body)
	}
	data, err := decodeJSONObject(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("mattermost POST %s 返回了非对象 JSON: %w", path, err)
	}
	return data, nil
}

// decodeJSONObject 将响应体解码为 JSON 对象，非对象时返回错误。
func decodeJSONObject(r io.Reader) (map[string]interface{}, error) {
	var data map[string]interface{}
	if err := json.NewDecoder(r).Decode(&data); err != nil {
		return nil, err
	}
	return data, nil
}

// GetMe 获取当前 Bot 用户信息（GET /api/v4/users/me）。
func (c *MattermostClient) GetMe(ctx context.Context) (map[string]interface{}, error) {
	return c.getJSON(ctx, "users/me")
}

// GetChannel 获取频道信息（GET /api/v4/channels/{id}）。
func (c *MattermostClient) GetChannel(ctx context.Context, channelID string) (map[string]interface{}, error) {
	return c.getJSON(ctx, "channels/"+channelID)
}

// GetFileInfo 获取文件信息（GET /api/v4/files/{id}/info）。
func (c *MattermostClient) GetFileInfo(ctx context.Context, fileID string) (map[string]interface{}, error) {
	return c.getJSON(ctx, "files/"+fileID+"/info")
}

// DownloadFile 下载文件内容（GET /api/v4/files/{id}）。
func (c *MattermostClient) DownloadFile(ctx context.Context, fileID string) ([]byte, error) {
	u := c.baseURL + "/api/v4/files/" + fileID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.authHeaders()
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("mattermost 下载文件 %s 失败: %d %s", fileID, resp.StatusCode, body)
	}
	return io.ReadAll(resp.Body)
}

// UploadFile 以 multipart/form-data 上传文件（POST /api/v4/files），返回 file_id。
func (c *MattermostClient) UploadFile(ctx context.Context, channelID string, fileBytes []byte, filename, contentType string) (string, error) {
	u := c.baseURL + "/api/v4/files"
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	if err := writer.WriteField("channel_id", channelID); err != nil {
		return "", err
	}
	part, err := writer.CreateFormFile("files", filename)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(fileBytes); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return "", err
	}
	req.Header = c.authHeaders()
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("mattermost 上传文件失败: %d %s", resp.StatusCode, raw)
	}
	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}
	fileInfos, _ := data["file_infos"].([]interface{})
	if len(fileInfos) == 0 {
		return "", fmt.Errorf("mattermost 上传文件未返回 file_infos")
	}
	first, ok := fileInfos[0].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("mattermost 上传文件返回格式异常")
	}
	fileID, _ := first["id"].(string)
	if fileID == "" {
		return "", fmt.Errorf("mattermost 上传文件返回了空的 file id")
	}
	return fileID, nil
}

// CreatePost 创建一条消息（POST /api/v4/posts）。
func (c *MattermostClient) CreatePost(ctx context.Context, channelID, messageText string, fileIDs []string, rootID string) (map[string]interface{}, error) {
	payload := map[string]interface{}{
		"channel_id": channelID,
		"message":    messageText,
	}
	if len(fileIDs) > 0 {
		payload["file_ids"] = fileIDs
	}
	if rootID != "" {
		payload["root_id"] = rootID
	}
	return c.postJSON(ctx, "posts", payload)
}

// mediaBytes 描述一份待上传的媒体数据。
type mediaBytes struct {
	data        []byte // 文件内容
	filename    string // 上传使用的文件名
	contentType string // 上传使用的 MIME 类型
}

// resolveMedia 将组件解析为可上传的字节流（对应 convert_to_file_path / get_file + 读文件）。
// 支持本地路径（Path/File）、Base64 编码与远程 URL。
func (c *MattermostClient) resolveMedia(ctx context.Context, path, file, base64Data, urlStr, fallbackName string, defaultMime string) (*mediaBytes, error) {
	var data []byte
	var err error
	var source string

	switch {
	case path != "":
		data, err = os.ReadFile(path)
		source = path
	case file != "":
		data, err = os.ReadFile(file)
		source = file
	case base64Data != "":
		data, err = base64.StdEncoding.DecodeString(strings.TrimSpace(base64Data))
		source = "base64"
	case urlStr != "":
		data, err = c.downloadBytes(ctx, urlStr)
		source = urlStr
	default:
		return nil, fmt.Errorf("媒体组件没有可用的 path/file/base64/url")
	}
	if err != nil {
		return nil, fmt.Errorf("读取媒体 %s 失败: %w", source, err)
	}

	filename := filepath.Base(source)
	if fallbackName != "" {
		filename = fallbackName
	}
	if filename == "." || filename == "/" || filename == "" {
		filename = "file"
	}
	contentType := detectMimeType(data, filename, defaultMime)
	return &mediaBytes{data: data, filename: filename, contentType: contentType}, nil
}

// downloadBytes 下载远程内容（经 SSRF 校验，上限 64MiB）。
func (c *MattermostClient) downloadBytes(ctx context.Context, rawURL string) ([]byte, error) {
	return platform.SafeDownloadBytes(ctx, rawURL, 64<<20)
}

// detectMimeType 探测文件 MIME 类型（对应 mimetypes.guess_type + 内容探测）。
func detectMimeType(data []byte, filename, defaultMime string) string {
	detected := http.DetectContentType(data)
	// http.DetectContentType 对大部分文件给出 application/octet-stream 之外的结果，
	// 但对文本文件总是 text/plain；此时优先使用扩展名推断。
	if detected != "" && detected != "text/plain; charset=utf-8" {
		return detected
	}
	if ext := mime.TypeByExtension(strings.ToLower(filepath.Ext(filename))); ext != "" {
		return ext
	}
	if defaultMime != "" {
		return defaultMime
	}
	return "application/octet-stream"
}

// SendMessageChain 将消息链发送到指定频道（对应 send_message_chain）。
// 文本/At 合并为 message，图片与文件先上传获取 file_id 后附加在 post 的 file_ids 中，
// Reply 组件作为 root_id（回复线程）。
func (c *MattermostClient) SendMessageChain(ctx context.Context, channelID string, chain *message.MessageChain) (map[string]interface{}, error) {
	var textParts []string
	var fileIDs []string
	rootID := ""

	for _, comp := range chain.Chain {
		switch seg := comp.(type) {
		case *message.Plain:
			textParts = append(textParts, seg.Text)
		case *message.At:
			// Python 使用 segment.name or segment.qq 生成 @提及
			mentionName := strings.TrimSpace(seg.Name)
			if mentionName == "" {
				mentionName = strings.TrimSpace(seg.TargetID)
			}
			if mentionName != "" {
				textParts = append(textParts, "@"+mentionName)
			}
		case *message.Reply:
			if seg.MessageID != "" {
				rootID = seg.MessageID
			}
		case *message.Image:
			media, err := c.resolveMedia(ctx, seg.Path, seg.File, seg.Base64, seg.URL, "image", "image/jpeg")
			if err != nil {
				return nil, err
			}
			fileID, err := c.UploadFile(ctx, channelID, media.data, media.filename, media.contentType)
			if err != nil {
				return nil, err
			}
			fileIDs = append(fileIDs, fileID)
		case *message.File, *message.Record, *message.Video:
			var media *mediaBytes
			var err error
			switch fileSeg := comp.(type) {
			case *message.File:
				// File 组件使用 name 或路径基名作为文件名
				filename := fileSeg.Name
				if filename == "" {
					filename = fileSeg.Path
				}
				media, err = c.resolveMedia(ctx, fileSeg.Path, "", "", fileSeg.URL, filename, "application/octet-stream")
			case *message.Record:
				media, err = c.resolveMedia(ctx, fileSeg.Path, fileSeg.File, fileSeg.Base64, fileSeg.URL, "voice", "application/octet-stream")
			default:
				videoSeg := comp.(*message.Video)
				media, err = c.resolveMedia(ctx, videoSeg.Path, "", "", videoSeg.URL, "video", "application/octet-stream")
			}
			if err != nil {
				return nil, err
			}
			fileID, err := c.UploadFile(ctx, channelID, media.data, media.filename, media.contentType)
			if err != nil {
				return nil, err
			}
			fileIDs = append(fileIDs, fileID)
		default:
			logger.Debug("Mattermost send_message_chain 跳过了不支持的组件: %s", comp.Type())
		}
	}

	var finalIDs []string
	if len(fileIDs) > 0 {
		finalIDs = fileIDs
	}
	return c.CreatePost(ctx, channelID, strings.TrimSpace(strings.Join(textParts, "")), finalIDs, rootID)
}

// ParsePostAttachments 根据 file_ids 下载附件并转换为消息组件（对应 parse_post_attachments）。
// 返回组件列表与写入的临时文件路径列表。
func (c *MattermostClient) ParsePostAttachments(ctx context.Context, fileIDs []string) ([]message.Component, []string) {
	var components []message.Component
	var tempPaths []string

	for _, fileID := range fileIDs {
		info, err := c.GetFileInfo(ctx, fileID)
		if err != nil {
			logger.I18nWarn("Mattermost 获取附件信息失败 %s: %v", fileID, err)
			continue
		}
		fileBytes, err := c.DownloadFile(ctx, fileID)
		if err != nil {
			logger.I18nWarn("Mattermost 下载附件失败 %s: %v", fileID, err)
			continue
		}

		filename, _ := info["name"].(string)
		if filename == "" {
			filename = "file_" + fileID
		}
		mimeType, _ := info["mime_type"].(string)
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		suffix := filepath.Ext(filename)
		tmp, err := os.CreateTemp("", "astrbot_mattermost_*"+suffix)
		if err != nil {
			logger.I18nWarn("Mattermost 创建附件临时文件失败 %s: %v", fileID, err)
			continue
		}
		filePath := tmp.Name()
		if _, err := tmp.Write(fileBytes); err != nil {
			_ = tmp.Close()
			_ = os.Remove(filePath)
			logger.I18nWarn("Mattermost 写入附件失败 %s -> %s: %v", fileID, filePath, err)
			continue
		}
		_ = tmp.Close()
		scheduleAttachmentCleanup(filePath)
		tempPaths = append(tempPaths, filePath)

		switch {
		case strings.HasPrefix(mimeType, "image/"):
			// 对应 Image.fromFileSystem
			components = append(components, &message.Image{Path: filePath, File: filePath})
		case strings.HasPrefix(mimeType, "audio/"):
			// Python 在此处会调用 MediaResolver 将音频转为 wav；Go 侧无转码能力，
			// 直接以 Record 组件引用下载后的文件（扩展名保持原样）。
			components = append(components, &message.Record{URL: filePath, File: filePath})
		case strings.HasPrefix(mimeType, "video/"):
			components = append(components, &message.Video{URL: filePath, Path: filePath})
		default:
			components = append(components, &message.File{Name: filename, Path: filePath})
		}
	}
	return components, tempPaths
}

// attachmentCleanupDelay 是附件临时文件的清理延迟：消息在事件总线中同步处理，
// 回复在数十秒内完成，延迟清理保证媒体在消费期间可用。
const attachmentCleanupDelay = time.Hour

// scheduleAttachmentCleanup 在延迟后删除附件临时文件。
func scheduleAttachmentCleanup(path string) {
	if path == "" {
		return
	}
	time.AfterFunc(attachmentCleanupDelay, func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			logger.Debug("Mattermost 清理附件临时文件失败 %s: %v", path, err)
		}
	})
}

// ParseWebsocketPost 解析 WebSocket 事件中的 post 字符串（对应 parse_websocket_post）。
// 解析失败或结果不是对象时返回 nil。
func ParseWebsocketPost(rawPost string) map[string]interface{} {
	// #nosec unsafe-deserialization-interface -- Mattermost 服务端下发的 post 事件负载
	//（受信任的已认证平台服务器），结构动态故用 interface{}。
	var parsed interface{} // nosemgrep: go.lang.security.deserialization.unsafe-deserialization-interface.go-unsafe-deserialization-interface
	if err := json.Unmarshal([]byte(rawPost), &parsed); err != nil {
		return nil
	}
	obj, ok := parsed.(map[string]interface{})
	if !ok {
		return nil
	}
	return obj
}

// buildWSURL 将 REST 基础地址转换为 WebSocket 地址（对应 ws_connect 中的 URL 转换）。
func buildWSURL(baseURL string) string {
	wsURL := strings.Replace(baseURL, "https://", "wss://", 1) // nosemgrep: javascript.lang.security.detect-insecure-websocket.detect-insecure-websocket -- 依配置的 http(s) 方案生成 wss/ws 地址（对齐 Python ws_connect），非注入
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)      // nosemgrep: javascript.lang.security.detect-insecure-websocket.detect-insecure-websocket
	return wsURL + "/api/v4/websocket"
}
