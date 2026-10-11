// Package dashscope implements the DashScope (阿里云百炼) Agent Runner.
// 移植 Python astrbot/core/agent/runners/dashscope/dashscope_agent_runner.py；
// 流式 HTTP 层使用 Go SDK github.com/the-open-agent/dashscope-go-sdk/httpclient
// （PostSSE），非流式为普通 POST + JSON 解析（对齐 Python Application.call 的
// 同步路径），调用 Application API（apps/{app_id}/completion）。
package dashscope

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/the-open-agent/dashscope-go-sdk/httpclient"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

const defaultAPIBase = "https://dashscope.aliyuncs.com/api/v1"

var refPattern = regexp.MustCompile(`<ref>\[(\d+)\]</ref>`)

// Deps 宿主注入。
type Deps struct {
	SessionGet func(ctx context.Context, sessionID, key string) (interface{}, error)
	SessionPut func(ctx context.Context, sessionID, key string, value interface{}) error
	Logger     *log.Logger
}

// Options 来自 agent_runner.config（runner_type=dashscope）。
type Options struct {
	APIKey          string
	AppID           string
	AppType         string
	Variables       map[string]interface{}
	RagOptions      map[string]interface{}
	OutputReference bool
	TimeoutSec      float64
	Streaming       bool
	APIBase         string
}

// Runner 对齐 Python DashscopeAgentRunner。
type Runner struct {
	apiKey          string
	appID           string
	appType         string
	variables       map[string]interface{}
	ragOptions      map[string]interface{}
	outputReference bool
	timeout         time.Duration
	streaming       bool
	apiBase         string
	cli             *httpclient.HTTPCli
	deps            Deps
}

// NewRunner 对齐 Python reset()。
func NewRunner(opts Options, deps Deps) (*Runner, error) {
	if opts.APIKey == "" {
		return nil, fmt.Errorf("阿里云百炼 API Key 不能为空。")
	}
	if opts.AppID == "" {
		return nil, fmt.Errorf("阿里云百炼 APP ID 不能为空。")
	}
	if opts.AppType == "" {
		return nil, fmt.Errorf("阿里云百炼 APP 类型不能为空。")
	}
	timeout := time.Duration(opts.TimeoutSec * float64(time.Second))
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	base := opts.APIBase
	if base == "" {
		base = defaultAPIBase
	}
	rag := map[string]interface{}{}
	outRef := opts.OutputReference
	if opts.RagOptions != nil {
		for k, v := range opts.RagOptions {
			if k == "output_reference" {
				if b, ok := v.(bool); ok {
					outRef = b
				}
				continue
			}
			rag[k] = v
		}
	}
	vars := opts.Variables
	if vars == nil {
		vars = map[string]interface{}{}
	}
	return &Runner{
		apiKey: opts.APIKey, appID: opts.AppID, appType: opts.AppType,
		variables: vars, ragOptions: rag, outputReference: outRef,
		timeout: timeout, streaming: opts.Streaming, apiBase: strings.TrimRight(base, "/"),
		cli:  httpclient.NewHTTPClient(),
		deps: deps,
	}, nil
}

func (r *Runner) logf(format string, args ...interface{}) {
	if r.deps.Logger != nil {
		r.deps.Logger.Printf(format, args...)
	}
}

func (r *Runner) hasRagOptions() bool {
	if len(r.ragOptions) == 0 {
		return false
	}
	for _, k := range []string{"pipeline_ids", "file_ids"} {
		if l, ok := r.ragOptions[k].([]interface{}); ok && len(l) > 0 {
			return true
		}
	}
	return false
}

func (r *Runner) buildPayload(prompt, conversationID string, payloadVars map[string]interface{}) map[string]interface{} {
	input := map[string]interface{}{"prompt": prompt}
	var biz interface{}
	if len(payloadVars) > 0 {
		biz = payloadVars
	}
	if r.appType == "agent" || r.appType == "dialog-workflow" {
		if !r.hasRagOptions() {
			if biz != nil {
				input["biz_params"] = biz
			}
			if conversationID != "" {
				input["session_id"] = conversationID
			}
			return map[string]interface{}{
				"model":      "",
				"input":      input,
				"parameters": map[string]interface{}{"stream": r.streaming, "incremental_output": true},
			}
		}
	}
	if biz != nil {
		input["biz_params"] = biz
	}
	params := map[string]interface{}{"stream": r.streaming, "incremental_output": true}
	if len(r.ragOptions) > 0 {
		params["rag_options"] = r.ragOptions
	}
	return map[string]interface{}{"model": "", "input": input, "parameters": params}
}

// dsOutput 累积 DashScope 响应 output 节点的文本/引用/会话 ID（流式与非流式通用）。
type dsOutput struct {
	text          string
	docReferences []interface{}
	sessionID     string
}

// apply 处理单个 output 节点，返回需流式下发的增量文本（无文本时为空）。
// 对齐 Python _process_stream_chunk 的 text/doc_references/session_id 提取。
func (a *dsOutput) apply(out map[string]interface{}) string {
	delta := ""
	if text, _ := out["text"].(string); text != "" {
		text = refPattern.ReplaceAllString(text, "[$1]")
		a.text += text
		delta = text
	}
	if dr, ok := out["doc_references"].([]interface{}); ok && len(dr) > 0 {
		a.docReferences = dr
	}
	if sid, _ := out["session_id"].(string); sid != "" {
		a.sessionID = sid
	}
	return delta
}

// finalize 对齐 Python _handle_streaming_response 收尾：按配置拼接 RAG 引用、
// 组装最终 chain。onDelta 非 nil 时补发引用文本（仅流式路径传入）。
func (r *Runner) finalize(out *dsOutput, onDelta func(string) error) (*provider.LLMResponse, error) {
	if r.outputReference && len(out.docReferences) > 0 {
		refText := formatDocReferences(out.docReferences)
		out.text += refText
		if onDelta != nil {
			_ = onDelta(refText)
		}
	}
	chain := message.NewMessageChain(&message.Plain{Text: out.text})
	return &provider.LLMResponse{Role: "assistant", ResultChain: chain}, nil
}

// Run 对齐 Python step() + _execute_dashscope_request()：
// 非流式走普通 POST + JSON（Application.call），流式走 SSE 消费。
func (r *Runner) Run(ctx context.Context, req *provider.ProviderRequest, onDelta func(text string) error) (*provider.LLMResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("request is not set")
	}
	prompt := req.Prompt
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = "unknown"
	}
	if len(req.ImageURLs) > 0 {
		r.logf("阿里云百炼暂不支持图片输入，将自动忽略图片内容。")
	}

	conversationID := ""
	if r.deps.SessionGet != nil {
		if v, err := r.deps.SessionGet(ctx, sessionID, "dashscope_conversation_id"); err == nil {
			if s, ok := v.(string); ok {
				conversationID = s
			}
		}
	}
	payloadVars := map[string]interface{}{}
	for k, v := range r.variables {
		payloadVars[k] = v
	}
	if r.deps.SessionGet != nil {
		if sv, err := r.deps.SessionGet(ctx, sessionID, "session_variables"); err == nil {
			if m, ok := sv.(map[string]interface{}); ok {
				for k, v := range m {
					payloadVars[k] = v
				}
			}
		}
	}

	payload := r.buildPayload(prompt, conversationID, payloadVars)
	if !r.streaming {
		if params, ok := payload["parameters"].(map[string]interface{}); ok {
			params["incremental_output"] = false
		}
	}
	url := fmt.Sprintf("%s/apps/%s/completion", r.apiBase, r.appID)

	if !r.streaming {
		return r.runNonStreaming(ctx, url, payload, sessionID)
	}
	return r.runStreaming(ctx, url, payload, sessionID, onDelta)
}

// runNonStreaming 对齐 Python 非流式路径（问题 B）：Application.call 同步返回
// 完整 JSON，请求不带 X-DashScope-SSE 头。此前误用 PostSSE（恒带 SSE 头）且
// 仅解析 "data:" 前缀行，导致非流式普通 JSON 响应整体被丢弃。
// 注：不用 SDK 的 Post——它不检查 HTTP 状态码，这里手动检查（对齐 py 的
// Application.call 非 2xx 即抛错语义）。
func (r *Runner) runNonStreaming(ctx context.Context, url string, payload map[string]interface{}, sessionID string) (*provider.LLMResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+r.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: r.timeout}).Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("阿里云百炼请求失败：%w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("阿里云百炼请求失败：%w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("阿里云百炼请求失败: HTTP %d: %s", resp.StatusCode, bodySnippet(respBody))
	}
	var chunk map[string]interface{}
	if err := json.Unmarshal(respBody, &chunk); err != nil {
		return nil, fmt.Errorf("阿里云百炼响应解析失败：%w", err)
	}
	// 对齐 Python _process_stream_chunk：status_code != 200 视为错误。
	if sc, ok := chunk["status_code"].(float64); ok && sc != 200 {
		msg, _ := chunk["message"].(string)
		return nil, fmt.Errorf("阿里云百炼请求失败: message=%s code=%d", msg, int(sc))
	}
	acc := &dsOutput{}
	if out, _ := chunk["output"].(map[string]interface{}); out != nil {
		acc.apply(out)
	}
	if acc.sessionID != "" && r.deps.SessionPut != nil {
		_ = r.deps.SessionPut(ctx, sessionID, "dashscope_conversation_id", acc.sessionID)
	}
	return r.finalize(acc, nil)
}

// runStreaming 对齐 Python _handle_streaming_response。
//
// stream 生命周期说明（问题 A）：SDK PostSSE 无关闭接口——返回裸 chan string
// （缓冲 500），其生产 goroutine 为 `scanner -> chan` 死循环，仅在 Body 读取
// 结束时 close channel。若消费端提前 return，缓冲填满后生产 goroutine 将永久
// 阻塞在 chan 发送上（泄漏）。因此：
//  1. 传入可取消 ctx（SDK 内部用 http.NewRequestWithContext，取消后 Body 读取
//     立即失败，生产循环退出）；
//  2. 提前返回前先 cancel 再排空 channel，保证生产 goroutine 执行到 close。
//     正常消费完毕时 channel 已被生产者关闭，排空为空操作，defer cancel 兜底。
//
// 已知 SDK 限制（超出本文件可控范围）：PostSSE 的生产 goroutine 在请求级别
// 出错（DNS/连接失败等，resp 为 nil）时会解引用 nil resp 而 panic，SDK 侧
// 未做防护；本 runner 只能保证自身不额外触发该路径。
func (r *Runner) runStreaming(ctx context.Context, url string, payload map[string]interface{}, sessionID string, onDelta func(string) error) (*provider.LLMResponse, error) {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := r.cli.PostSSE(reqCtx, url, payload,
		httpclient.WithTokenHeaderOption(r.apiKey),
		httpclient.WithHeader(httpclient.HeaderMap{"X-DashScope-SSE": "enable"}),
		httpclient.WithTimeout(r.timeout),
	)
	if err != nil {
		return nil, fmt.Errorf("阿里云百炼请求失败：%w", err)
	}
	// abort 在提前返回路径上调用（见上方生命周期说明）。
	abort := func(abortErr error) (*provider.LLMResponse, error) {
		cancel()
		for range stream { // 排空，等待生产 goroutine close 后退出
		}
		return nil, abortErr
	}

	acc := &dsOutput{}
	// lastPutSID 记录已落库的会话 ID，避免对不含 session_id 的后续 chunk 重复写。
	lastPutSID := ""
	for line := range stream {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		// 错误响应（对齐 Python _process_stream_chunk 的 status_code 检查）。
		if sc, ok := chunk["status_code"].(float64); ok && sc != 200 {
			msg, _ := chunk["message"].(string)
			return abort(fmt.Errorf("阿里云百炼请求失败: message=%s code=%d", msg, int(sc)))
		}
		out, _ := chunk["output"].(map[string]interface{})
		if out == nil {
			continue
		}
		if delta := acc.apply(out); delta != "" && onDelta != nil {
			if err := onDelta(delta); err != nil {
				return abort(err)
			}
		}
		if acc.sessionID != "" && acc.sessionID != lastPutSID && r.deps.SessionPut != nil {
			lastPutSID = acc.sessionID
			_ = r.deps.SessionPut(ctx, sessionID, "dashscope_conversation_id", acc.sessionID)
		}
	}

	return r.finalize(acc, onDelta)
}

// bodySnippet 截取响应 body 前 4KB 用于错误信息。
func bodySnippet(b []byte) string {
	const limit = 4096
	s := string(b)
	if len(s) > limit {
		s = s[:limit] + "…(截断)"
	}
	return s
}

// formatDocReferences 对齐 Python _format_doc_references。
func formatDocReferences(refs []interface{}) string {
	var sb strings.Builder
	for _, ref := range refs {
		m, ok := ref.(map[string]interface{})
		if !ok {
			continue
		}
		title, _ := m["title"].(string)
		if title == "" {
			title, _ = m["doc_name"].(string)
		}
		idx := m["index_id"]
		sb.WriteString(fmt.Sprintf("%v. %s\n", idx, title))
	}
	return "\n\n回答来源:\n" + sb.String()
}
