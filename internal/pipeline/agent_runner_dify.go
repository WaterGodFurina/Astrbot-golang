package pipeline

// Agent Runner 分发（对齐 Python process_stage/method/agent_sub_stages/third_party.py）：
// 按 agent_runner.runner_type 选择执行器；local 走内置 agent loop，其余走第三方
// runner（当前实现 dify，HTTP 自研客户端，不引第三方 SDK）。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/WaterGodFurina/Astrbot-golang/internal/agent/runners/dify"
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	"github.com/WaterGodFurina/Astrbot-golang/internal/utils"
)

// agentRunnerType 读取 agent_runner.runner_type（缺省 local）。
func agentRunnerType(cfg map[string]interface{}) string {
	ar, _ := cfg["agent_runner"].(map[string]interface{})
	if ar == nil {
		return "local"
	}
	rt, _ := ar["runner_type"].(string)
	if rt == "" {
		return "local"
	}
	return rt
}

// agentRunnerConfig 读取 agent_runner.config。
func agentRunnerConfig(cfg map[string]interface{}) map[string]interface{} {
	ar, _ := cfg["agent_runner"].(map[string]interface{})
	if ar == nil {
		return nil
	}
	out, _ := ar["config"].(map[string]interface{})
	return out
}

// runDifyAgent 对齐 Python DifyAgentRunner：构造 runner，流式增量经 streamer 输出，
// 返回最终 LLMResponse（由 finalizeAgentReply 统一落库/发结果）。
func (s *ProcessStage) runDifyAgent(ctx context.Context, ar *agentRequest, streamer *streamSender) (*provider.LLMResponse, error) {
	rc := agentRunnerConfig(s.config)
	get := func(k, def string) string {
		if rc == nil {
			return def
		}
		if v, ok := rc[k].(string); ok && v != "" {
			return v
		}
		return def
	}
	timeout := 60.0
	if rc != nil {
		switch v := rc["timeout"].(type) {
		case float64:
			timeout = v
		case int:
			timeout = float64(v)
		case string:
			var f float64
			if _, err := jsonToFloat(v, &f); err == nil {
				timeout = f
			}
		}
	}
	vars := map[string]interface{}{}
	if rc != nil {
		if m, ok := rc["variables"].(map[string]interface{}); ok {
			vars = m
		}
	}

	deps := dify.Deps{
		SessionGet:         s.difySessionGet,
		SessionPut:         s.difySessionPut,
		ResolveImageBase64: resolveImageBase64ForDify,
	}

	runner := dify.NewRunner(dify.Options{
		APIKey:            get("dify_api_key", ""),
		APIBase:           get("dify_api_base", "https://api.dify.ai/v1"),
		APIType:           get("dify_api_type", "chat"),
		WorkflowOutputKey: get("dify_workflow_output_key", "astrbot_wf_output"),
		QueryInputKey:     get("dify_query_input_key", "astrbot_text_query"),
		Variables:         vars,
		TimeoutSec:        timeout,
		Streaming:         ar.streaming,
	}, deps)

	return runner.Run(ctx, ar.req, func(text string) error {
		if streamer != nil {
			streamer.push(text)
		}
		return nil
	})
}

// difySessionGet 读取 umo 作用域偏好（对齐 Python sp.get_async scope=umo）。
// 值可为 JSON 字符串或 {"val": ...} 包装；容错解析。
func (s *ProcessStage) difySessionGet(ctx context.Context, sessionID, key string) (interface{}, error) {
	if s.database == nil {
		return nil, nil
	}
	raw, found, err := s.database.GetPreference("umo", sessionID, key)
	if err != nil || !found || raw == "" {
		return nil, err
	}
	var val interface{}
	if json.Unmarshal([]byte(raw), &val) == nil {
		if m, ok := val.(map[string]interface{}); ok {
			if inner, exists := m["val"]; exists {
				return inner, nil
			}
		}
		return val, nil
	}
	return raw, nil
}

// difySessionPut 写入 umo 作用域偏好。
func (s *ProcessStage) difySessionPut(ctx context.Context, sessionID, key string, value interface{}) error {
	if s.database == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.database.SetPreference("umo", sessionID, key, string(data))
}

// resolveImageBase64ForDify 把图片引用解析为 (bytes, mimeType)。
func resolveImageBase64ForDify(ctx context.Context, imageRef string) ([]byte, string, error) {
	dataURL, err := utils.ResolveImageRefToDataURL(imageRef)
	if err != nil || dataURL == "" {
		return nil, "", err
	}
	mime := "image/png"
	payload := dataURL
	if strings.HasPrefix(dataURL, "data:") {
		if idx := strings.Index(dataURL, ";base64,"); idx >= 0 {
			mime = dataURL[5:idx]
			payload = dataURL[idx+len(";base64,"):]
		}
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, "", err
	}
	return raw, mime, nil
}

// jsonToFloat 解析 JSON 数值字符串。
func jsonToFloat(s string, out *float64) (bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return false, nil
	}
	var f float64
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return false, err
	}
	*out = f
	return true, nil
}
