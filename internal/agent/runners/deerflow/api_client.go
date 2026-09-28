package deerflow

// 移植 Python deerflow_api_client.py：DeerFlow LangGraph HTTP 客户端。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const sseMaxBufferChars = 1_048_576

// APIError 对齐 Python DeerFlowAPIError。
type APIError struct {
	Operation string
	Status    int
	Body      string
	URL       string
	ThreadID  string
}

func (e *APIError) Error() string {
	if e.ThreadID != "" {
		return fmt.Sprintf("DeerFlow %s failed: status=%d, url=%s, thread_id=%s, body=%s",
			e.Operation, e.Status, e.URL, e.ThreadID, e.Body)
	}
	return fmt.Sprintf("DeerFlow %s failed: status=%d, url=%s, body=%s",
		e.Operation, e.Status, e.URL, e.Body)
}

// APIClient 对齐 DeerFlowAPIClient。
type APIClient struct {
	apiBase string
	headers map[string]string
	http    *http.Client
}

// NewAPIClient 构造。api_base 默认 http://127.0.0.1:2026。
func NewAPIClient(apiBase, apiKey, authHeader, proxy string, hc *http.Client) *APIClient {
	if apiBase == "" {
		apiBase = "http://127.0.0.1:2026"
	}
	headers := map[string]string{}
	if authHeader != "" {
		headers["Authorization"] = authHeader
	} else if apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}
	if hc == nil {
		hc = &http.Client{}
	}
	return &APIClient{apiBase: strings.TrimRight(apiBase, "/"), headers: headers, http: hc}
}

// Close 对齐接口。
func (c *APIClient) Close() error { return nil }

// parseSSEDataLines 对齐 Python _parse_sse_data_lines。
func parseSSEDataLines(dataLines []string) interface{} {
	raw := strings.Join(dataLines, "\n")
	var out interface{}
	if err := json.Unmarshal([]byte(raw), &out); err == nil {
		return out
	}
	parsed := []interface{}{}
	allOK := true
	for _, line := range dataLines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var v interface{}
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			allOK = false
			break
		}
		parsed = append(parsed, v)
	}
	if allOK && len(parsed) > 0 {
		if len(parsed) == 1 {
			return parsed[0]
		}
		return parsed
	}
	return raw
}

// parseSSEBlock 对齐 Python _parse_sse_block。
func parseSSEBlock(block string) (event string, data interface{}, ok bool) {
	if strings.TrimSpace(block) == "" {
		return "", nil, false
	}
	eventName := "message"
	dataLines := []string{}
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(line[len("event:"):])
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimLeft(line[len("data:"):], " \t"))
		}
	}
	if len(dataLines) == 0 {
		return "", nil, false
	}
	return eventName, parseSSEDataLines(dataLines), true
}

// streamSSE 对齐 Python _stream_sse（按 \n\n 分块；CRLF 归一）。
func streamSSE(body io.Reader, onEvent func(event string, data interface{}) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var block strings.Builder
	emit := func() error {
		ev, data, ok := parseSSEBlock(block.String())
		block.Reset()
		if !ok {
			return nil
		}
		return onEvent(ev, data)
	}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			if err := emit(); err != nil {
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
	return emit()
}

// CreateThread 对齐 Python create_thread。
func (c *APIClient) CreateThread(ctx context.Context, timeout time.Duration) (map[string]interface{}, error) {
	url := c.apiBase + "/api/langgraph/threads"
	body, _ := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{}})
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		text, _ := io.ReadAll(resp.Body)
		return nil, &APIError{Operation: "create thread", Status: resp.StatusCode, Body: string(text), URL: url}
	}
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteThread 对齐 Python delete_thread。
func (c *APIClient) DeleteThread(ctx context.Context, threadID string, timeout time.Duration) error {
	url := c.apiBase + "/api/threads/" + threadID
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200, 202, 204, 404:
		return nil
	}
	text, _ := io.ReadAll(resp.Body)
	return &APIError{Operation: "delete thread", Status: resp.StatusCode, Body: string(text), URL: url, ThreadID: threadID}
}

// StreamRun 对齐 Python stream_run（SSE）。
func (c *APIClient) StreamRun(ctx context.Context, threadID string, payload map[string]interface{}, timeout time.Duration, onEvent func(event string, data interface{}) error) error {
	url := fmt.Sprintf("%s/api/langgraph/threads/%s/runs/stream", c.apiBase, threadID)
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
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		text, _ := io.ReadAll(resp.Body)
		return &APIError{Operation: "runs/stream request", Status: resp.StatusCode, Body: string(text), URL: url, ThreadID: threadID}
	}
	return streamSSE(resp.Body, onEvent)
}
