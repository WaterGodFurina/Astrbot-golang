// Package provider - provider settings types and merge logic.
// Ported from astrbot/core/config/ provider_settings handling.
//
// Bug fix for issue #9573: UMO max_context_length not taking effect.
// The Python code used `or` short-circuit:
//
//	cfg = config.provider_settings or plugin_context.get_config(umo=...).get("provider_settings", {})
//
// Since global provider_settings is always a non-empty dict, the UMO-specific
// config was never fetched. Fix: merge global + UMO configs, with UMO overriding.
package provider

// ProviderSettings holds provider-related configuration.
type ProviderSettings struct {
	Enable               bool   `json:"enable"`
	WakePrefix           string `json:"wake_prefix"`
	Identifier           string `json:"identifier"`
	PromptPrefix         string `json:"prompt_prefix"`
	MaxContextLength     int    `json:"max_context_length"`
	DequeueContextLength int    `json:"dequeue_context_length"`
}

// MergeProviderSettings merges global and UMO-specific settings.
// UMO (non-zero) values override global values.
func MergeProviderSettings(global, umo ProviderSettings) ProviderSettings {
	result := global // start with global
	if umo.MaxContextLength != 0 {
		result.MaxContextLength = umo.MaxContextLength
	}
	if umo.DequeueContextLength != 0 {
		result.DequeueContextLength = umo.DequeueContextLength
	}
	if umo.PromptPrefix != "" {
		result.PromptPrefix = umo.PromptPrefix
	}
	if umo.WakePrefix != "" {
		result.WakePrefix = umo.WakePrefix
	}
	if umo.Identifier != "" {
		result.Identifier = umo.Identifier
	}
	// Enable is always taken from global (or UMO if explicitly set)
	if umo.Enable {
		result.Enable = umo.Enable
	}
	return result
}

// DefaultProviderSettings returns default provider settings.
func DefaultProviderSettings() ProviderSettings {
	return ProviderSettings{
		Enable:               true,
		WakePrefix:           "",
		Identifier:           "",
		PromptPrefix:         "",
		MaxContextLength:     50,
		DequeueContextLength: 10,
	}
}
