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
	Enable                       *bool   `mapstructure:"enable"`
	DefaultProviderID            string  `mapstructure:"default_provider_id"`
	DefaultPersonality           string  `mapstructure:"default_personality"`
	Persona                      string  `mapstructure:"persona"`
	ComputerUseRuntime           string  `mapstructure:"computer_use_runtime"`
	StreamingResponse            bool    `mapstructure:"streaming_response"`
	WakePrefix                   string  `mapstructure:"wake_prefix"`
	Identifier                   bool    `mapstructure:"identifier"`
	GroupNameDisplay             bool    `mapstructure:"group_name_display"`
	DatetimeSystemPrompt         bool    `mapstructure:"datetime_system_prompt"`
	MaxAgentStep                 int     `mapstructure:"max_agent_step"`
	ToolCallTimeout              int     `mapstructure:"tool_call_timeout"`
	ToolSchemaMode               string  `mapstructure:"tool_schema_mode"`
	LLMSafetyMode                bool    `mapstructure:"llm_safety_mode"`
	SafetyModeStrategy           string  `mapstructure:"safety_mode_strategy"`
	UnsupportedStreamingStrategy string  `mapstructure:"unsupported_streaming_strategy"`
	DisplayReasoningText         bool    `mapstructure:"display_reasoning_text"`
	ShowToolUseStatus            bool    `mapstructure:"show_tool_use_status"`
	ShowToolCallResult           bool    `mapstructure:"show_tool_call_result"`
	BufferIntermediateMessages   bool    `mapstructure:"buffer_intermediate_messages"`
	SanitizeContextByModalities  bool    `mapstructure:"sanitize_context_by_modalities"`
	MaxContextLength             int     `mapstructure:"max_context_length"`
	DequeueContextLength         int     `mapstructure:"dequeue_context_length"`
	ContextLimitStrategy         string  `mapstructure:"context_limit_reached_strategy"`
	LLMCompressInstruction       string  `mapstructure:"llm_compress_instruction"`
	LLMCompressKeepRecentRatio   float64 `mapstructure:"llm_compress_keep_recent_ratio"`
	LLMCompressProviderID        string  `mapstructure:"llm_compress_provider_id"`
	Proactive                    struct {
		AddCronTools *bool `mapstructure:"add_cron_tools"`
	} `mapstructure:"proactive_capability"`
}

// bindProviderSettings decodes provider_settings from a config map into a
// typed struct. Missing/invalid values fall back to zero values, matching the
// previous assertion-based reads.
func bindProviderSettings(cfg map[string]interface{}) *ProviderSettings {
	ps := &ProviderSettings{}
	raw, _ := cfg["provider_settings"].(map[string]interface{})
	if raw != nil {
		if err := mapstructure.Decode(raw, ps); err != nil {
			logger.Warn("provider_settings decode failed: %v", err)
		}
	}
	return ps
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
	// Nested rate_limit {count,time,strategy}.
	RateLimit struct {
		Count    int    `mapstructure:"count"`
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
