package pipeline

// DashScope (阿里云百炼) Agent Runner 接线。HTTP 层用 Go SDK
// github.com/the-open-agent/dashscope-go-sdk（非手搓）。

import (
	"context"

	"github.com/WaterGodFurina/Astrbot-golang/internal/agent/runners/dashscope"
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// runDashscopeAgent 构造 DashScope runner 并执行。
func (s *ProcessStage) runDashscopeAgent(ctx context.Context, ar *agentRequest, streamer *streamSender) (*provider.LLMResponse, error) {
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
	timeout := 120.0
	vars := map[string]interface{}{}
	rag := map[string]interface{}{}
	if rc != nil {
		switch v := rc["timeout"].(type) {
		case float64:
			timeout = v
		case int:
			timeout = float64(v)
		}
		if m, ok := rc["variables"].(map[string]interface{}); ok {
			vars = m
		}
		if m, ok := rc["rag_options"].(map[string]interface{}); ok {
			rag = m
		}
	}

	runner, err := dashscope.NewRunner(dashscope.Options{
		APIKey:     get("dashscope_api_key", ""),
		AppID:      get("dashscope_app_id", ""),
		AppType:    get("dashscope_app_type", ""),
		Variables:  vars,
		RagOptions: rag,
		TimeoutSec: timeout,
		Streaming:  ar.streaming,
	}, dashscope.Deps{
		SessionGet: s.difySessionGet,
		SessionPut: s.difySessionPut,
	})
	if err != nil {
		return nil, err
	}
	return runner.Run(ctx, ar.req, func(text string) error {
		if streamer != nil {
			streamer.push(text)
		}
		return nil
	})
}
