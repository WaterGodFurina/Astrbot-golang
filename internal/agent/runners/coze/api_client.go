// Package coze implements the Coze Agent Runner (HTTP client + runner),
// ported from Python astrbot/core/agent/runners/coze/.
package coze

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// APIClient 对齐 Python CozeAPIClient。
type APIClient struct {
	apiKey  string
	apiBase string
	http    *http.Client
}

// NewAPIClient 构造。apiBase 为空用 Coze 官方地址。
func NewAPIClient(apiKey, apiBase string, hc *http.Client) *APIClient {
	if apiBase == "" {
		apiBase = "https://api.coze.cn"
	}
	apiBase = strings.TrimRight(apiBase, "/")
	if hc == nil {
		hc = &http.Client{}
	}
	return &APIClient{apiKey: apiKey, apiBase: apiBase, http: hc}
}

// Close 对齐接口（net/http 无显式会话）。
func (c *APIClient) Close() error { return nil }

// streamChatSSE 解析 Coze 的 event/data 成对 SSE：
// 空行结束一个事件；`event:` 设类型，`data:` 累积 JSON 数据。
func streamChatSSE(body io.Reader, onEvent func(eventType string, data map[string]interface{}) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var eventType string
	var dataStr strings.Builder
	flush := func() error {
		if eventType == "" && dataStr.Len() == 0 {
			return nil
		}
		var data map[string]interface{}
		if dataStr.Len() > 0 {
			if err := json.Unmarshal([]byte(dataStr.String()), &data); err != nil {
				data = map[string]interface{}{"content": dataStr.String()}
			}
		}
		if data == nil {
			data = map[string]interface{}{}
		}
		et := eventType
		eventType = ""
		dataStr.Reset()
		return onEvent(et, data)
	}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(line[len("event:"):])
		} else if strings.HasPrefix(line, "data:") {
			d := strings.TrimSpace(line[len("data:"):])
			if d != "" && d != "[DONE]" {
				if dataStr.Len() > 0 {
					dataStr.WriteString("\n")
				}
				dataStr.WriteString(d)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

// UploadFile 对齐 Python upload_file：POST /v1/files/upload → 返回 file_id。
func (c *APIClient) UploadFile(ctx context.Context, fileData []byte) (string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="file"; filename="file"`)
	header.Set("Content-Type", "application/octet-stream")
	part, err := writer.CreatePart(header)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(fileData); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.apiBase+"/v1/files/upload", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("文件上传失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return "", fmt.Errorf("Coze API 认证失败，请检查 API Key 是否正确")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("文件上传失败，状态码: %d, 响应: %s", resp.StatusCode, string(body))
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("文件上传响应解析失败: %s", string(body))
	}
	if code, ok := result["code"].(float64); ok && code != 0 {
		msg, _ := result["msg"].(string)
		if msg == "" {
			msg = "未知错误"
		}
		return "", fmt.Errorf("文件上传失败: %s", msg)
	}
	data, _ := result["data"].(map[string]interface{})
	fileID, _ := data["id"].(string)
	return fileID, nil
}

// DownloadImage 对齐 Python download_image。
func (c *APIClient) DownloadImage(ctx context.Context, imageURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载图片失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载图片失败，状态码: %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// ChatMessages 对齐 Python chat_messages：POST /v3/chat（SSE）。
func (c *APIClient) ChatMessages(ctx context.Context, botID, userID string, additionalMessages []map[string]interface{}, conversationID string, autoSaveHistory, stream bool, timeout time.Duration, onEvent func(eventType string, data map[string]interface{}) error) error {
	payload := map[string]interface{}{
		"bot_id":            botID,
		"user_id":           userID,
		"stream":            stream,
		"auto_save_history": autoSaveHistory,
	}
	if len(additionalMessages) > 0 {
		payload["additional_messages"] = additionalMessages
	}
	body, _ := json.Marshal(payload)
	u := c.apiBase + "/v3/chat"
	if conversationID != "" {
		u += "?conversation_id=" + url.QueryEscape(conversationID)
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Coze API 流式请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("Coze API 认证失败，请检查 API Key 是否正确")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Coze API 流式请求失败，状态码: %d", resp.StatusCode)
	}
	return streamChatSSE(resp.Body, onEvent)
}

// ClearContext 对齐 Python clear_context。
func (c *APIClient) ClearContext(ctx context.Context, conversationID string) (map[string]interface{}, error) {
	payload := map[string]interface{}{"conversation_id": conversationID}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase+"/v3/conversation/message/clear_context", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	return c.doJSON(req)
}

// GetMessageList 对齐 Python get_message_list。
func (c *APIClient) GetMessageList(ctx context.Context, conversationID, order string, limit, offset int) (map[string]interface{}, error) {
	if order == "" {
		order = "desc"
	}
	if limit == 0 {
		limit = 10
	}
	u := fmt.Sprintf("%s/v3/conversation/message/list?conversation_id=%s&order=%s&limit=%d&offset=%d",
		c.apiBase, url.QueryEscape(conversationID), order, limit, offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	return c.doJSON(req)
}

func (c *APIClient) doJSON(req *http.Request) (map[string]interface{}, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
