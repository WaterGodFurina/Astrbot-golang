// Package dify implements the Dify Agent Runner (HTTP client + runner),
// ported from Python astrbot/core/agent/runners/dify/.
package dify

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
	"strings"
	"time"
)

// APIClient 对齐 Python DifyAPIClient（dify_api_client.py）。
type APIClient struct {
	apiKey  string
	apiBase string
	http    *http.Client
}

// NewAPIClient 构造客户端。apiBase 为空时使用 Dify 官方地址。
func NewAPIClient(apiKey, apiBase string, hc *http.Client) *APIClient {
	if apiBase == "" {
		apiBase = "https://api.dify.ai/v1"
	}
	apiBase = strings.TrimRight(apiBase, "/")
	if hc == nil {
		hc = &http.Client{}
	}
	return &APIClient{apiKey: apiKey, apiBase: apiBase, http: hc}
}

// Close 对齐 Python close()。net/http 客户端无需显式关闭连接池；保留以对齐接口。
func (c *APIClient) Close() error { return nil }

// StreamSSE 对齐 Python _stream_sse：按 "\n\n" 分块，取 "data:" 前缀的 JSON。
func StreamSSE(body io.Reader, onEvent func(map[string]interface{}) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var block strings.Builder
	flush := func() error {
		raw := block.String()
		block.Reset()
		trimmed := strings.TrimSpace(raw)
		if !strings.HasPrefix(trimmed, "data:") {
			return nil
		}
		payload := strings.TrimSpace(trimmed[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			return nil
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			// 对齐 Python：丢弃非法 JSON 仅告警。
			return nil
		}
		return onEvent(event)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		block.WriteString(line)
		block.WriteString("\n")
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

// streamPost 发起 POST 并逐事件回调（对齐 Python chat_messages / workflow_run 的 SSE 消费）。
func (c *APIClient) streamPost(ctx context.Context, path string, payload map[string]interface{}, timeout time.Duration, errPrefix string, onEvent func(map[string]interface{}) error) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	reqCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		reqCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.apiBase+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		text, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s：%d. %s", errPrefix, resp.StatusCode, string(text))
	}
	return StreamSSE(resp.Body, onEvent)
}

// ChatMessages 对齐 Python chat_messages（POST /chat-messages，SSE）。
func (c *APIClient) ChatMessages(ctx context.Context, payload map[string]interface{}, timeout time.Duration, onEvent func(map[string]interface{}) error) error {
	if _, ok := payload["response_mode"]; !ok {
		payload["response_mode"] = "streaming"
	}
	if _, ok := payload["files"]; !ok {
		payload["files"] = []interface{}{}
	}
	return c.streamPost(ctx, "/chat-messages", payload, timeout, "Dify /chat-messages 接口请求失败", onEvent)
}

// WorkflowRun 对齐 Python workflow_run（POST /workflows/run，SSE）。
func (c *APIClient) WorkflowRun(ctx context.Context, payload map[string]interface{}, timeout time.Duration, onEvent func(map[string]interface{}) error) error {
	if _, ok := payload["response_mode"]; !ok {
		payload["response_mode"] = "streaming"
	}
	if _, ok := payload["files"]; !ok {
		payload["files"] = []interface{}{}
	}
	return c.streamPost(ctx, "/workflows/run", payload, timeout, "Dify /workflows/run 接口请求失败", onEvent)
}

// FileUpload 对齐 Python file_upload（POST /files/upload，multipart）。
// 返回 {"id": "xxx", ...}。
func (c *APIClient) FileUpload(ctx context.Context, user string, fileData []byte, fileName, mimeType string) (map[string]interface{}, error) {
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	if fileName == "" {
		fileName = "uploaded_file"
	}
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if err := writer.WriteField("user", user); err != nil {
		return nil, err
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, fileName))
	header.Set("Content-Type", mimeType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(fileData); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase+"/files/upload", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		text, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Dify 文件上传失败：%d. %s", resp.StatusCode, string(text))
	}
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetChatConversations 对齐 Python get_chat_convs。
func (c *APIClient) GetChatConversations(ctx context.Context, user string, limit int) (map[string]interface{}, error) {
	if limit <= 0 {
		limit = 20
	}
	url := fmt.Sprintf("%s/conversations?user=%s&limit=%d", c.apiBase, user, limit)
	return c.getJSON(ctx, url)
}

// DeleteChatConversation 对齐 Python delete_chat_conv。
func (c *APIClient) DeleteChatConversation(ctx context.Context, user, conversationID string) (map[string]interface{}, error) {
	payload := map[string]interface{}{"user": user}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.apiBase+"/conversations/"+conversationID, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	return c.doJSON(req)
}

// Rename 对齐 Python rename。
func (c *APIClient) Rename(ctx context.Context, conversationID, name, user string, autoGenerate bool) (map[string]interface{}, error) {
	payload := map[string]interface{}{"user": user, "name": name, "auto_generate": autoGenerate}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase+"/conversations/"+conversationID+"/name", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	return c.doJSON(req)
}

func (c *APIClient) getJSON(ctx context.Context, url string) (map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
