package coze

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// Deps 宿主注入依赖（会话持久化 / 媒体解析 / 日志）。
type Deps struct {
	SessionGet func(ctx context.Context, sessionID, key string) (interface{}, error)
	SessionPut func(ctx context.Context, sessionID, key string, value interface{}) error
	// ResolveImageBytes 把图片引用解析为字节（对齐 Python MediaResolver.to_bytes）。
	ResolveImageBytes func(ctx context.Context, url string) ([]byte, error)
	HTTPClient        *http.Client
	Logger            *log.Logger
}

// Options 来自 agent_runner.config（runner_type=coze）。
type Options struct {
	APIKey          string
	BotID           string
	APIBase         string
	TimeoutSec      float64
	AutoSaveHistory bool
	Streaming       bool
}

// Runner 对齐 Python CozeAgentRunner。
type Runner struct {
	botID           string
	autoSaveHistory bool
	streaming       bool
	timeout         time.Duration
	client          *APIClient
	deps            Deps
	fileIDCache     map[string]map[string]string
}

// NewRunner 对齐 Python reset()。
func NewRunner(opts Options, deps Deps) (*Runner, error) {
	if opts.APIKey == "" {
		return nil, fmt.Errorf("Coze API Key 不能为空。")
	}
	if opts.BotID == "" {
		return nil, fmt.Errorf("Coze Bot ID 不能为空。")
	}
	apiBase := opts.APIBase
	if apiBase == "" {
		apiBase = "https://api.coze.cn"
	}
	if !strings.HasPrefix(apiBase, "http://") && !strings.HasPrefix(apiBase, "https://") {
		return nil, fmt.Errorf("Coze API Base URL 格式不正确，必须以 http:// 或 https:// 开头。")
	}
	timeout := time.Duration(opts.TimeoutSec * float64(time.Second))
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Runner{
		botID:           opts.BotID,
		autoSaveHistory: opts.AutoSaveHistory,
		streaming:       opts.Streaming,
		timeout:         timeout,
		client:          NewAPIClient(opts.APIKey, apiBase, deps.HTTPClient),
		deps:            deps,
		fileIDCache:     map[string]map[string]string{},
	}, nil
}

func (r *Runner) logf(format string, args ...interface{}) {
	if r.deps.Logger != nil {
		r.deps.Logger.Printf(format, args...)
	}
}

// Run 对齐 Python step() + _execute_coze_request()。
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
	userID := sessionID
	systemPrompt := req.SystemPrompt

	conversationID := ""
	if r.deps.SessionGet != nil {
		if v, err := r.deps.SessionGet(ctx, userID, "coze_conversation_id"); err == nil {
			if s, ok := v.(string); ok {
				conversationID = s
			}
		}
	}

	additional := []map[string]interface{}{}

	if systemPrompt != "" && (!r.autoSaveHistory || conversationID == "") {
		additional = append(additional, map[string]interface{}{
			"role": "system", "content": systemPrompt, "content_type": "text",
		})
	}

	// 历史上下文（仅 auto_save_history=false）。
	if !r.autoSaveHistory {
		for _, c := range req.Contexts {
			if isCheckpointMessage(c) {
				continue
			}
			role, _ := c["role"].(string)
			content, ok := c["content"]
			if role == "" || !ok {
				continue
			}
			switch cv := content.(type) {
			case []interface{}:
				processed := []interface{}{}
				for _, item := range cv {
					im, ok := item.(map[string]interface{})
					if !ok {
						continue
					}
					if im["type"] == "text" {
						processed = append(processed, im)
					} else if im["type"] == "image_url" {
						iu, _ := im["image_url"].(map[string]interface{})
						u, _ := iu["url"].(string)
						if u == "" {
							continue
						}
						if fid, err := r.downloadAndUploadImage(ctx, u, sessionID); err == nil {
							processed = append(processed, map[string]interface{}{"type": "file", "file_id": fid, "file_url": u})
						}
					}
				}
				if len(processed) > 0 {
					additional = append(additional, map[string]interface{}{"role": role, "content": processed, "content_type": "object_string"})
				}
			default:
				additional = append(additional, map[string]interface{}{"role": role, "content": content, "content_type": "text"})
			}
		}
	}

	// 当前消息。
	if prompt != "" || len(req.ImageURLs) > 0 {
		if len(req.ImageURLs) > 0 {
			obj := []map[string]interface{}{}
			if prompt != "" {
				obj = append(obj, map[string]interface{}{"type": "text", "text": prompt})
			}
			for _, u := range req.ImageURLs {
				fid, err := r.downloadAndUploadImage(ctx, u, sessionID)
				if err != nil {
					r.logf("处理图片失败 %s: %v", u, err)
					continue
				}
				obj = append(obj, map[string]interface{}{"type": "image", "file_id": fid})
			}
			if len(obj) > 0 {
				content, _ := json.Marshal(obj)
				additional = append(additional, map[string]interface{}{"role": "user", "content": string(content), "content_type": "object_string"})
			}
		} else {
			additional = append(additional, map[string]interface{}{"role": "user", "content": prompt, "content_type": "text"})
		}
	}

	accumulated := ""
	messageStarted := false
	err := r.client.ChatMessages(ctx, r.botID, userID, additional, conversationID, r.autoSaveHistory, true, r.timeout, func(eventType string, data map[string]interface{}) error {
		switch eventType {
		case "conversation.chat.created":
			if cid, _ := data["conversation_id"].(string); cid != "" && r.deps.SessionPut != nil {
				_ = r.deps.SessionPut(ctx, userID, "coze_conversation_id", cid)
			}
		case "conversation.message.delta":
			content, _ := data["content"].(string)
			if content == "" {
				if d, ok := data["delta"].(map[string]interface{}); ok {
					content, _ = d["content"].(string)
				}
			}
			if content == "" {
				content, _ = data["text"].(string)
			}
			if content != "" {
				accumulated += content
				messageStarted = true
				if r.streaming && onDelta != nil {
					return onDelta(content)
				}
			}
		case "conversation.message.completed":
			messageStarted = true
		case "conversation.chat.completed":
			return errStop
		case "error":
			msg, _ := data["msg"].(string)
			code := fmt.Sprint(data["code"])
			return fmt.Errorf("Coze 出现错误: %s - %s", code, msg)
		}
		return nil
	})
	if err != nil && err != errStop {
		return nil, err
	}
	_ = messageStarted
	if accumulated == "" {
		r.logf("Coze 未返回任何内容")
	}
	chain := message.NewMessageChain(&message.Plain{Text: accumulated})
	return &provider.LLMResponse{Role: "assistant", ResultChain: chain}, nil
}

// errStop 内部哨兵：SSE 回调中提前结束。
var errStop = fmt.Errorf("coze: stop")

// downloadAndUploadImage 对齐 Python _download_and_upload_image（带 MD5 缓存）。
func (r *Runner) downloadAndUploadImage(ctx context.Context, imageURL, sessionID string) (string, error) {
	sum := md5.Sum([]byte(imageURL))
	cacheKey := hex.EncodeToString(sum[:])
	if sessionID != "" {
		if m, ok := r.fileIDCache[sessionID]; ok {
			if fid, ok := m[cacheKey]; ok {
				return fid, nil
			}
		}
	}
	if r.deps.ResolveImageBytes == nil {
		return "", fmt.Errorf("无图片解析器")
	}
	data, err := r.deps.ResolveImageBytes(ctx, imageURL)
	if err != nil {
		return "", err
	}
	fid, err := r.client.UploadFile(ctx, data)
	if err != nil {
		return "", fmt.Errorf("处理图片失败: %w", err)
	}
	if sessionID != "" {
		if r.fileIDCache[sessionID] == nil {
			r.fileIDCache[sessionID] = map[string]string{}
		}
		r.fileIDCache[sessionID][cacheKey] = fid
	}
	return fid, nil
}

// isCheckpointMessage 对齐 Python is_checkpoint_message（简化：含 _checkpoint 标记）。
func isCheckpointMessage(ctx map[string]interface{}) bool {
	if ctx == nil {
		return false
	}
	if v, ok := ctx["_checkpoint"].(bool); ok && v {
		return true
	}
	return false
}
