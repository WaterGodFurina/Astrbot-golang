package pipeline

import (
	"github.com/mitchellh/mapstructure"
)

// ProviderSettings is the structured binding of `provider_settings`, decoded
// once via mapstructure instead of repeated map[string]interface{} assertions.
//
// Enable and Proactive.AddCronTools are pointers so we can distinguish "key
// absent" (nil) from "explicitly false" — matching the original assertion
// logic which treated absent keys as true/allow.
type ProviderSettings struct {
	Enable                       *bool  `mapstructure:"enable"`
	DefaultProviderID            string `mapstructure:"default_provider_id"`
	DefaultPersonality           string `mapstructure:"default_personality"`
	Persona                      string `mapstructure:"persona"`
	ComputerUseRuntime           string `mapstructure:"computer_use_runtime"`
	ComputerUseRequireAdmin      *bool  `mapstructure:"computer_use_require_admin"`
	ComputerUseMaxSameToolCalls  *int   `mapstructure:"computer_use_max_same_tool_calls"`
	StreamingResponse            bool   `mapstructure:"streaming_response"`
	WakePrefix                   string `mapstructure:"wake_prefix"`
	Identifier                   bool   `mapstructure:"identifier"`
	GroupNameDisplay             bool   `mapstructure:"group_name_display"`
	DatetimeSystemPrompt         bool   `mapstructure:"datetime_system_prompt"`
	MaxAgentStep                 int    `mapstructure:"max_agent_step"`
	ToolCallTimeout              int    `mapstructure:"tool_call_timeout"`
	ToolSchemaMode               string `mapstructure:"tool_schema_mode"`
	LLMSafetyMode                bool   `mapstructure:"llm_safety_mode"`
	SafetyModeStrategy           string `mapstructure:"safety_mode_strategy"`
	UnsupportedStreamingStrategy string `mapstructure:"unsupported_streaming_strategy"`
	DisplayReasoningText         bool   `mapstructure:"display_reasoning_text"`
	ShowToolUseStatus            bool   `mapstructure:"show_tool_use_status"`
	ShowToolCallResult           bool   `mapstructure:"show_tool_call_result"`
	BufferIntermediateMessages   bool   `mapstructure:"buffer_intermediate_messages"`
	SanitizeContextByModalities  bool   `mapstructure:"sanitize_context_by_modalities"`
	MaxContextLength             int    `mapstructure:"max_context_length"`
	// max_context_length 是"轮数"上限（py enforce_max_turns）；token 预算
	// 走下面两个字段（py max_context_tokens / fallback_max_context_tokens），
	// 两者语义分立，不能混用。
	MaxContextTokens           int     `mapstructure:"max_context_tokens"`
	FallbackMaxContextTokens   int     `mapstructure:"fallback_max_context_tokens"`
	DequeueContextLength       int     `mapstructure:"dequeue_context_length"`
	ContextLimitStrategy       string  `mapstructure:"context_limit_reached_strategy"`
	LLMCompressInstruction     string  `mapstructure:"llm_compress_instruction"`
	LLMCompressKeepRecentRatio float64 `mapstructure:"llm_compress_keep_recent_ratio"`
	LLMCompressProviderID      string  `mapstructure:"llm_compress_provider_id"`
	Proactive                  struct {
		AddCronTools *bool `mapstructure:"add_cron_tools"`
	} `mapstructure:"proactive_capability"`
}

// bindProviderSettings decodes provider_settings from a config map into a
// typed struct. Missing/invalid values fall back to zero values, matching the
// previous assertion-based reads. 随后叠加 agent_runner.config.compression 的
// 共享压缩配置（compression 优先，缺省回退 provider_settings）。
func bindProviderSettings(cfg map[string]interface{}) *ProviderSettings {
	ps := &ProviderSettings{}
	raw, _ := cfg["provider_settings"].(map[string]interface{})
	if raw != nil {
		if err := mapstructure.Decode(raw, ps); err != nil {
			logger.Warn("provider_settings decode failed: %v", err)
		}
	}
	applyContextCompressionConfig(ps, cfg)
	return ps
}

// agentRunnerCompressionSection 读取 agent_runner.config.compression
// （键路径对齐 Python astrbot/core/config/agent_runner.py:99 的共享解析函数入参；
// Go 默认配置未内置该段，存在时优先于 provider_settings）。
func agentRunnerCompressionSection(cfg map[string]interface{}) map[string]interface{} {
	ar, _ := cfg["agent_runner"].(map[string]interface{})
	if ar == nil {
		return nil
	}
	arCfg, _ := ar["config"].(map[string]interface{})
	if arCfg == nil {
		return nil
	}
	comp, _ := arCfg["compression"].(map[string]interface{})
	return comp
}

// applyContextCompressionConfig 把 agent_runner.config.compression 映射为
// 压缩参数并覆盖到 provider_settings 绑定的结果上（对齐 Python v4.28.2
// `resolve_context_compression_config` 的字段语义）：
//
//	max_turns            -> MaxContextLength
//	trim_turns           -> DequeueContextLength（按 py 公式钳制）
//	overflow_strategy    -> ContextLimitStrategy
//	instruction          -> LLMCompressInstruction
//	keep_recent_ratio    -> LLMCompressKeepRecentRatio
//	provider_id          -> LLMCompressProviderID
//	fallback_max_tokens  -> FallbackMaxContextTokens
//
// 优先级：compression 中显式出现的键覆盖 provider_settings；未出现的键保持
// provider_settings（Go 现状兼容），整个 compression 段缺失时完全不改动。
// cron/后台任务唤醒合成的 proactive 事件与普通 chat 都经同一 ProcessStage
// 处理，因此二者共用这一套压缩参数。
func applyContextCompressionConfig(ps *ProviderSettings, cfg map[string]interface{}) {
	comp := agentRunnerCompressionSection(cfg)
	if len(comp) == 0 {
		return
	}
	if v, ok := comp["overflow_strategy"].(string); ok {
		ps.ContextLimitStrategy = v
	}
	if v, ok := comp["instruction"].(string); ok {
		ps.LLMCompressInstruction = v
	}
	if v, ok := comp["keep_recent_ratio"]; ok {
		if f, ok := configFloatValue(v); ok {
			ps.LLMCompressKeepRecentRatio = f
		}
	}
	if v, ok := comp["provider_id"].(string); ok {
		ps.LLMCompressProviderID = v
	}
	if v, ok := comp["fallback_max_tokens"]; ok {
		if n, ok := configIntValue(v); ok {
			ps.FallbackMaxContextTokens = n
		}
	}
	maxTurns := ps.MaxContextLength
	maxTurnsSet := false
	if v, ok := comp["max_turns"]; ok {
		if n, ok := configIntValue(v); ok {
			maxTurns = n
			ps.MaxContextLength = n
			maxTurnsSet = true
		}
	}
	trimTurns := ps.DequeueContextLength
	_, trimSet := comp["trim_turns"]
	if v, ok := comp["trim_turns"]; ok {
		if n, ok := configIntValue(v); ok {
			trimTurns = n
		}
	}
	if maxTurnsSet || trimSet {
		// py 公式：dequeue = min(max(1, trim), max>0 ? max-1 : trim)，再 max(1, dequeue)。
		dequeue := trimTurns
		if dequeue < 1 {
			dequeue = 1
		}
		if maxTurns > 0 && dequeue > maxTurns-1 {
			dequeue = maxTurns - 1
		}
		if dequeue < 1 {
			dequeue = 1
		}
		ps.DequeueContextLength = dequeue
	}
}

// configIntValue 从 JSON/Go 数字中取整数（int/int64/float64/json.Number）。
func configIntValue(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	default:
		return 0, false
	}
}

// configFloatValue 从 JSON/Go 数字中取浮点数。
func configFloatValue(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// PlatformSettings is the structured binding of `platform_settings`, shared by
// WakingCheckStage, WhitelistCheckStage, RateLimitStage and
// ResultDecorateStage.
type PlatformSettings struct {
	Nickname                     []string `mapstructure:"nickname"`
	WakeByAt                     *bool    `mapstructure:"wake_by_at"`
	WakeByPrefix                 *bool    `mapstructure:"wake_by_prefix"`
	WakeByFriend                 *bool    `mapstructure:"wake_by_friend"`
	FriendMessageNeedsWakePrefix bool     `mapstructure:"friend_message_needs_wake_prefix"`
	IgnoreAtAll                  bool     `mapstructure:"ignore_at_all"`
	IgnoreBotSelfMessage         bool     `mapstructure:"ignore_bot_self_message"`
	NoPermissionReply            bool     `mapstructure:"no_permission_reply"`
	UniqueSession                bool     `mapstructure:"unique_session"`
	CmdPrefix                    string   `mapstructure:"cmd_prefix"`
	EnableIDWhiteList            bool     `mapstructure:"enable_id_white_list"`
	IDWhitelist                  []string `mapstructure:"id_whitelist"`
	WLIgnoreAdmin                bool     `mapstructure:"wl_ignore_admin"`
	WLIgnoreAdminOnGroup         *bool    `mapstructure:"wl_ignore_admin_on_group"`
	WLIgnoreAdminOnFriend        *bool    `mapstructure:"wl_ignore_admin_on_friend"`
	WLLog                        bool     `mapstructure:"wl_log"`
	ReplyPrefix                  string   `mapstructure:"reply_prefix"`
	ReplyWithMention             bool     `mapstructure:"reply_with_mention"`
	ReplyWithQuote               bool     `mapstructure:"reply_with_quote"`
	ForwardThreshold             int      `mapstructure:"forward_threshold"`
	EmptyMentionWaiting          bool     `mapstructure:"empty_mention_waiting"`
	EmptyMentionWaitingNeedReply bool     `mapstructure:"empty_mention_waiting_need_reply"`
	// Legacy flat keys used by RateLimitStage.
	RateLimitTime     float64 `mapstructure:"rate_limit_time"`
	RateLimitStrategy string  `mapstructure:"rate_limit_strategy"`
	// Nested rate_limit {count,time,strategy}. Count 用指针以区分“未配置”
	// （nil → 默认值）与显式配置的 0（对齐 Python：count<=0 表示不开启限流）。
	RateLimit struct {
		Count    *int   `mapstructure:"count"`
		Time     int    `mapstructure:"time"`
		Strategy string `mapstructure:"strategy"`
	} `mapstructure:"rate_limit"`
}

// bindPlatformSettings decodes platform_settings from a config map.
func bindPlatformSettings(cfg map[string]interface{}) *PlatformSettings {
	ps := &PlatformSettings{}
	raw, _ := cfg["platform_settings"].(map[string]interface{})
	if raw != nil {
		if err := mapstructure.Decode(raw, ps); err != nil {
			logger.Warn("platform_settings decode failed: %v", err)
		}
	}
	return ps
}

// toStringList flattens a wake_prefix-style value (string or list) into a
// slice of non-empty strings.
func toStringList(raw interface{}) []string {
	var out []string
	switch v := raw.(type) {
	case []interface{}:
		for _, item := range v {
			if str, ok := item.(string); ok && str != "" {
				out = append(out, str)
			}
		}
	case []string:
		for _, str := range v {
			if str != "" {
				out = append(out, str)
			}
		}
	case string:
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
