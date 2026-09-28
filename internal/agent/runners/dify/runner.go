package dify

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// Deps 由宿主注入的运行时依赖（会话持久化 / 媒体解析 / 日志）。
type Deps struct {
	// SessionGet 读取 umo 作用域的会话变量（对应 Python sp.get_async scope=umo）。
	SessionGet func(ctx context.Context, sessionID, key string) (interface{}, error)
	// SessionPut 写入 umo 作用域的会话变量。
	SessionPut func(ctx context.Context, sessionID, key string, value interface{}) error
	// ResolveImageBase64 把图片 URL 转为 (bytes, mimeType)，对齐 Python MediaResolver.to_base64_data。
	ResolveImageBase64 func(ctx context.Context, url string) ([]byte, string, error)
	// HTTPClient 可选，用于代理等；nil 时用默认。
	HTTPClient *http.Client
	Logger     *log.Logger
}

// Runner 对齐 Python DifyAgentRunner。
type Runner struct {
	apiType           string
	workflowOutputKey string
	queryInputKey     string
	variables         map[string]interface{}
	timeout           time.Duration
	streaming         bool
	client            *APIClient
	deps              Deps
}

// Options 构造 Runner 的配置（来自 agent_runner.config，runner_type=dify）。
type Options struct {
	APIKey            string
	APIBase           string
	APIType           string
	WorkflowOutputKey string
	QueryInputKey     string
	Variables         map[string]interface{}
	TimeoutSec        float64
	Streaming         bool
}

// NewRunner 对齐 Python reset() 的字段初始化。
func NewRunner(opts Options, deps Deps) *Runner {
	apiType := opts.APIType
	if apiType == "" {
		apiType = "chat"
	}
	workflowOutputKey := opts.WorkflowOutputKey
	if workflowOutputKey == "" {
		workflowOutputKey = "astrbot_wf_output"
	}
	queryInputKey := opts.QueryInputKey
	if queryInputKey == "" {
		queryInputKey = "astrbot_text_query"
	}
	timeout := time.Duration(opts.TimeoutSec * float64(time.Second))
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	vars := opts.Variables
	if vars == nil {
		vars = map[string]interface{}{}
	}
	return &Runner{
		apiType:           apiType,
		workflowOutputKey: workflowOutputKey,
		queryInputKey:     queryInputKey,
		variables:         vars,
		timeout:           timeout,
		streaming:         opts.Streaming,
		client:            NewAPIClient(opts.APIKey, opts.APIBase, deps.HTTPClient),
		deps:              deps,
	}
}

func (r *Runner) logf(format string, args ...interface{}) {
	if r.deps.Logger != nil {
		r.deps.Logger.Printf(format, args...)
	}
}

// Run 对齐 Python step() + _execute_dify_request()：
// 执行请求，通过 onDelta 回调流式增量，返回最终 LLMResponse。
func (r *Runner) Run(ctx context.Context, req *provider.ProviderRequest, onDelta func(text string) error) (*provider.LLMResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("request is not set")
	}
	defer r.client.Close()

	prompt := req.Prompt
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = "unknown"
	}
	systemPrompt := req.SystemPrompt

	conversationID := ""
	if r.deps.SessionGet != nil {
		if v, err := r.deps.SessionGet(ctx, sessionID, "dify_conversation_id"); err == nil {
			if s, ok := v.(string); ok {
				conversationID = s
			}
		}
	}

	// 处理图片上传（对齐 _upload_image_for_dify）。
	filesPayload := []interface{}{}
	for _, imageURL := range req.ImageURLs {
		if r.deps.ResolveImageBase64 == nil {
			break
		}
		data, mime, err := r.deps.ResolveImageBase64(ctx, imageURL)
		if err != nil || len(data) == 0 {
			r.logf("上传图片失败：%v", err)
			continue
		}
		ext := "png"
		if idx := lastIndexByte(mime, '/'); idx >= 0 && idx < len(mime)-1 {
			ext = mime[idx+1:]
		}
		if ext == "jpeg" {
			ext = "jpg"
		}
		fileResp, err := r.client.FileUpload(ctx, sessionID, data, "image."+ext, mime)
		if err != nil {
			r.logf("上传图片失败：%v", err)
			continue
		}
		fileID, _ := fileResp["id"].(string)
		if fileID == "" {
			r.logf("上传图片后得到未知的 Dify 响应：%v，图片将忽略。", fileResp)
			continue
		}
		filesPayload = append(filesPayload, map[string]interface{}{
			"type":            "image",
			"transfer_method": "local_file",
			"upload_file_id":  fileID,
		})
	}

	// 会话变量（对齐 payload_vars）。
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
	payloadVars["system_prompt"] = systemPrompt

	var result interface{} // string（chat）或 map（workflow）
	switch r.apiType {
	case "chat", "agent", "chatflow":
		if prompt == "" {
			prompt = "请描述这张图片。"
		}
		payload := map[string]interface{}{
			"inputs":          payloadVars,
			"query":           prompt,
			"user":            sessionID,
			"conversation_id": conversationID,
			"files":           filesPayload,
			"response_mode":   "streaming",
		}
		var sb []byte
		err := r.client.ChatMessages(ctx, payload, r.timeout, func(chunk map[string]interface{}) error {
			event, _ := chunk["event"].(string)
			switch event {
			case "message", "agent_message":
				answer, _ := chunk["answer"].(string)
				sb = append(sb, []byte(answer)...)
				if conversationID == "" {
					if cid, _ := chunk["conversation_id"].(string); cid != "" && r.deps.SessionPut != nil {
						_ = r.deps.SessionPut(ctx, sessionID, "dify_conversation_id", cid)
						conversationID = cid
					}
				}
				if r.streaming && answer != "" && onDelta != nil {
					return onDelta(answer)
				}
			case "message_end":
				return errStop
			case "error":
				status := chunk["status"]
				messageText, _ := chunk["message"].(string)
				return fmt.Errorf("Dify 出现错误 status: %v message: %s", status, messageText)
			}
			return nil
		})
		if err != nil && err != errStop {
			return nil, err
		}
		result = string(sb)

	case "workflow":
		inputs := map[string]interface{}{
			r.queryInputKey:      prompt,
			"astrbot_session_id": sessionID,
		}
		for k, v := range payloadVars {
			inputs[k] = v
		}
		payload := map[string]interface{}{
			"inputs":        inputs,
			"user":          sessionID,
			"files":         filesPayload,
			"response_mode": "streaming",
		}
		var finished map[string]interface{}
		err := r.client.WorkflowRun(ctx, payload, r.timeout, func(chunk map[string]interface{}) error {
			event, _ := chunk["event"].(string)
			switch event {
			case "text_chunk":
				if data, ok := chunk["data"].(map[string]interface{}); ok {
					if text, _ := data["text"].(string); r.streaming && text != "" && onDelta != nil {
						return onDelta(text)
					}
				}
			case "workflow_finished":
				data, _ := chunk["data"].(map[string]interface{})
				if data != nil {
					if errVal, ok := data["error"]; ok && errVal != nil && errVal != "" {
						return fmt.Errorf("Dify 工作流出现错误：%v", errVal)
					}
					outputs, _ := data["outputs"].(map[string]interface{})
					if _, ok := outputs[r.workflowOutputKey]; !ok {
						return fmt.Errorf("Dify 工作流的输出不包含指定的键名：%s", r.workflowOutputKey)
					}
				}
				finished = chunk
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		result = finished

	default:
		return nil, fmt.Errorf("未知的 Dify API 类型：%s", r.apiType)
	}

	if result == nil || result == "" {
		r.logf("Dify 请求结果为空，请查看 Debug 日志。")
	}

	chain, err := r.parseResult(ctx, result)
	if err != nil {
		return nil, err
	}
	return &provider.LLMResponse{Role: "assistant", ResultChain: chain}, nil
}

// parseResult 对齐 Python parse_dify_result。
func (r *Runner) parseResult(ctx context.Context, result interface{}) (*message.MessageChain, error) {
	chain := message.NewMessageChain()
	switch v := result.(type) {
	case string:
		// Chat 纯文本。
		chain.Append(&message.Plain{Text: v})
		return chain, nil
	case map[string]interface{}:
		data, _ := v["data"].(map[string]interface{})
		if data == nil {
			return chain, nil
		}
		outputs, _ := data["outputs"].(map[string]interface{})
		output := outputs[r.workflowOutputKey]
		switch out := output.(type) {
		case string:
			chain.Append(&message.Plain{Text: out})
		case []interface{}:
			// 主要适配 Dify HTTP 请求结点的多模态输出。
			appended := false
			for _, item := range out {
				im, ok := item.(map[string]interface{})
				if !ok || im["dify_model_identity"] != "__dify__file__" {
					chain.Append(&message.Plain{Text: fmt.Sprint(out)})
					appended = true
					break
				}
			}
			_ = appended
		default:
			if output != nil {
				chain.Append(&message.Plain{Text: fmt.Sprint(output)})
			}
		}
		// scan files。
		if files, ok := data["files"].([]interface{}); ok {
			for _, f := range files {
				fm, ok := f.(map[string]interface{})
				if !ok {
					continue
				}
				chain.Append(fileComponent(fm))
			}
		}
		return chain, nil
	default:
		chain.Append(&message.Plain{Text: fmt.Sprint(result)})
		return chain, nil
	}
}

// fileComponent 对齐 Python parse_file：按 type 生成组件。
func fileComponent(item map[string]interface{}) message.Component {
	t, _ := item["type"].(string)
	url, _ := item["url"].(string)
	switch t {
	case "image":
		return &message.Image{URL: url, File: url}
	case "video":
		return &message.Video{URL: url}
	case "audio":
		return &message.Record{URL: url, File: url}
	default:
		name, _ := item["filename"].(string)
		return &message.File{Name: name, URL: url}
	}
}

// errStop 内部用于在回调中提前结束 SSE 消费。
var errStop = fmt.Errorf("dify: stop")

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// MarshalJSON 便于调试。
func (r *Runner) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]interface{}{
		"api_type": r.apiType, "workflow_output_key": r.workflowOutputKey,
	})
}
