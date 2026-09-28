// Package dashscope implements the DashScope (阿里云百炼) Agent Runner.
// 移植 Python astrbot/core/agent/runners/dashscope/dashscope_agent_runner.py；
// HTTP 层使用 Go SDK github.com/the-open-agent/dashscope-go-sdk/httpclient
// （非手搓），调用 Application API（apps/{app_id}/completion）。
package dashscope

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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

// Run 对齐 Python step() + _execute_dashscope_request() + _handle_streaming_response()。
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

	stream, err := r.cli.PostSSE(ctx, url, payload,
		httpclient.WithTokenHeaderOption(r.apiKey),
		httpclient.WithHeader(httpclient.HeaderMap{"X-DashScope-SSE": "enable"}),
		httpclient.WithTimeout(r.timeout),
	)
	if err != nil {
		return nil, fmt.Errorf("阿里云百炼请求失败：%w", err)
	}

	outputText := ""
	var docReferences []interface{}
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
		// 错误响应。
		if sc, ok := chunk["status_code"].(float64); ok && sc != 200 {
			msg, _ := chunk["message"].(string)
			errMsg := fmt.Sprintf("阿里云百炼请求失败: message=%s code=%d", msg, int(sc))
			return nil, fmt.Errorf("%s", errMsg)
		}
		out, _ := chunk["output"].(map[string]interface{})
		if out == nil {
			continue
		}
		text, _ := out["text"].(string)
		if text != "" {
			text = refPattern.ReplaceAllString(text, "[$1]")
			outputText += text
			if r.streaming && onDelta != nil {
				if err := onDelta(text); err != nil {
					return nil, err
				}
			}
		}
		if dr, ok := out["doc_references"].([]interface{}); ok && len(dr) > 0 {
			docReferences = dr
		}
		if sid, _ := out["session_id"].(string); sid != "" && r.deps.SessionPut != nil {
			_ = r.deps.SessionPut(ctx, sessionID, "dashscope_conversation_id", sid)
		}
	}

	if r.outputReference && len(docReferences) > 0 {
		refText := formatDocReferences(docReferences)
		outputText += refText
		if r.streaming && onDelta != nil {
			_ = onDelta(refText)
		}
	}

	chain := message.NewMessageChain(&message.Plain{Text: outputText})
	return &provider.LLMResponse{Role: "assistant", ResultChain: chain}, nil
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
