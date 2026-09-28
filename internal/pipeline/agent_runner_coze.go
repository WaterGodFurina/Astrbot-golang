package pipeline

// Coze Agent Runner 接线（对齐 Python third_party.py 的 coze 分支）。

import (
	"context"

	"github.com/WaterGodFurina/Astrbot-golang/internal/agent/runners/coze"
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// runCozeAgent 构造 Coze runner 并执行（流式增量经 streamer）。
func (s *ProcessStage) runCozeAgent(ctx context.Context, ar *agentRequest, streamer *streamSender) (*provider.LLMResponse, error) {
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
	autoSave := true
	if rc != nil {
		switch v := rc["timeout"].(type) {
		case float64:
			timeout = v
		case int:
			timeout = float64(v)
		}
		if b, ok := rc["auto_save_history"].(bool); ok {
			autoSave = b
		}
	}

	deps := coze.Deps{
		SessionGet: s.difySessionGet,
		SessionPut: s.difySessionPut,
		ResolveImageBytes: func(ctx context.Context, url string) ([]byte, error) {
			raw, _, err := resolveImageBase64ForDify(ctx, url)
			return raw, err
		},
	}

	runner, err := coze.NewRunner(coze.Options{
		APIKey:          get("coze_api_key", ""),
		BotID:           get("bot_id", ""),
		APIBase:         get("coze_api_base", "https://api.coze.cn"),
		TimeoutSec:      timeout,
		AutoSaveHistory: autoSave,
		Streaming:       ar.streaming,
	}, deps)
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
