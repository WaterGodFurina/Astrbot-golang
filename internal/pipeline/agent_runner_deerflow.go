package pipeline

// DeerFlow Agent Runner 接线（手搓，LangGraph HTTP API）。

import (
	"context"

	"github.com/WaterGodFurina/Astrbot-golang/internal/agent/runners/deerflow"
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	"github.com/WaterGodFurina/Astrbot-golang/internal/utils"
)

// runDeerflowAgent 构造 DeerFlow runner 并执行。
func (s *ProcessStage) runDeerflowAgent(ctx context.Context, ar *agentRequest, streamer *streamSender) (*provider.LLMResponse, error) {
	rc := agentRunnerConfig(s.config)
	getStr := func(k, def string) string {
		if rc == nil {
			return def
		}
		if v, ok := rc[k].(string); ok && v != "" {
			return v
		}
		return def
	}
	getBool := func(k string) bool {
		if rc == nil {
			return false
		}
		b, _ := rc[k].(bool)
		return b
	}
	getInt := func(k string, def int) int {
		if rc == nil {
			return def
		}
		switch v := rc[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
		return def
	}
	timeout := 300.0
	if rc != nil {
		switch v := rc["timeout"].(type) {
		case float64:
			timeout = v
		case int:
			timeout = float64(v)
		}
	}

	runner, err := deerflow.NewRunner(deerflow.Options{
		APIBase:                getStr("deerflow_api_base", "http://127.0.0.1:2026"),
		APIKey:                 getStr("deerflow_api_key", ""),
		AuthHeader:             getStr("deerflow_auth_header", ""),
		Proxy:                  getStr("proxy", ""),
		AssistantID:            getStr("deerflow_assistant_id", "lead_agent"),
		ModelName:              getStr("deerflow_model_name", ""),
		ThinkingEnabled:        getBool("deerflow_thinking_enabled"),
		PlanMode:               getBool("deerflow_plan_mode"),
		SubagentEnabled:        getBool("deerflow_subagent_enabled"),
		MaxConcurrentSubagents: getInt("deerflow_max_concurrent_subagents", 3),
		RecursionLimit:         getInt("deerflow_recursion_limit", 1000),
		TimeoutSec:             timeout,
		Streaming:              ar.streaming,
	}, deerflow.Deps{
		SessionGet: s.difySessionGet,
		SessionPut: s.difySessionPut,
		ResolveImageDataURL: func(ctx context.Context, ref string) (string, error) {
			return utils.ResolveImageRefToDataURL(ref)
		},
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
