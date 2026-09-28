package config

import (
	"reflect"
	"strconv"
)

// 对齐 Python astrbot/core/utils/migra_helper.py 的 Agent Runner 配置迁移。
// 启动加载配置时自动把旧结构（provider_settings.agent_runner_type +
// <runner>_agent_runner_provider_id + 分散在 provider_settings 的
// model/persona/compression/misc 键）迁移为 Python 4.28.2 的新结构
// `agent_runner: {runner_type, config}`，随后 checkConfigIntegrity 清理。

// agentRunnerTypes 与 Python AGENT_RUNNER_TYPES 一致。
var agentRunnerTypes = map[string]bool{
	"local": true, "dify": true, "coze": true, "dashscope": true, "deerflow": true,
}

// legacyAgentRunnerProviderIDKeys 旧 runner -> provider 引用键。
var legacyAgentRunnerProviderIDKeys = map[string]string{
	"dify":      "dify_agent_runner_provider_id",
	"coze":      "coze_agent_runner_provider_id",
	"dashscope": "dashscope_agent_runner_provider_id",
	"deerflow":  "deerflow_agent_runner_provider_id",
}

// legacyAgentRunnerSettingKeys 与 Python _LEGACY_AGENT_RUNNER_SETTING_KEYS 一致：
// 迁移后需从 provider_settings 中删除的旧键。
var legacyAgentRunnerSettingKeys = []string{
	"agent_runner_type",
	"dify_agent_runner_provider_id",
	"coze_agent_runner_provider_id",
	"dashscope_agent_runner_provider_id",
	"deerflow_agent_runner_provider_id",
	"default_provider_id",
	"fallback_chat_models",
	"request_max_retries",
	"default_personality",
	"llm_safety_mode",
	"safety_mode_strategy",
	"max_agent_step",
	"tool_schema_mode",
	"tool_call_timeout",
	"sanitize_context_by_modalities",
	"context_limit_reached_strategy",
	"llm_compress_instruction",
	"llm_compress_keep_recent_ratio",
	"llm_compress_provider_id",
	"max_context_length",
	"dequeue_context_length",
	"fallback_max_context_tokens",
}

// legacyProviderIdentityFields 与 Python _LEGACY_PROVIDER_IDENTITY_FIELDS 一致。
var legacyProviderIdentityFields = map[string]bool{
	"id": true, "type": true, "provider": true, "provider_type": true,
	"enable": true, "provider_source_id": true, "model_config": true,
}

// agentRunnerConfigDefault 与 Python AGENT_RUNNER_CONFIG_DEFAULTS 一致。
func agentRunnerConfigDefault(runnerType string) map[string]interface{} {
	switch runnerType {
	case "local":
		return map[string]interface{}{
			"model": map[string]interface{}{
				"provider_id": "", "fallback_provider_ids": []interface{}{}, "request_max_retries": 5,
			},
			"persona": map[string]interface{}{
				"persona_id": "default", "safety_mode": true, "safety_mode_strategy": "system_prompt",
			},
			"compression": map[string]interface{}{
				"max_turns": -1, "trim_turns": 1, "overflow_strategy": "llm_compress",
				"instruction": "", "keep_recent_ratio": 0.15, "provider_id": "", "fallback_max_tokens": 128000,
			},
			"misc": map[string]interface{}{
				"max_steps": 30, "tool_schema_mode": "full", "tool_call_timeout": 120,
				"sanitize_context_by_modalities": false,
			},
		}
	case "dify":
		return map[string]interface{}{
			"dify_api_type": "chat", "dify_api_key": "", "dify_api_base": "https://api.dify.ai/v1",
			"dify_workflow_output_key": "astrbot_wf_output", "dify_query_input_key": "astrbot_text_query",
			"variables": map[string]interface{}{}, "timeout": 60, "proxy": "",
		}
	case "coze":
		return map[string]interface{}{
			"coze_api_key": "", "bot_id": "", "coze_api_base": "https://api.coze.cn",
			"auto_save_history": true, "timeout": 60, "proxy": "",
		}
	case "dashscope":
		return map[string]interface{}{
			"dashscope_app_type": "agent", "dashscope_api_key": "", "dashscope_app_id": "",
			"rag_options": map[string]interface{}{
				"pipeline_ids": []interface{}{}, "file_ids": []interface{}{}, "output_reference": false,
			},
			"variables": map[string]interface{}{}, "timeout": 60, "proxy": "",
		}
	case "deerflow":
		return map[string]interface{}{
			"deerflow_api_base": "http://127.0.0.1:2026", "deerflow_api_key": "", "deerflow_auth_header": "",
			"deerflow_assistant_id": "lead_agent", "deerflow_model_name": "", "deerflow_thinking_enabled": false,
			"deerflow_plan_mode": false, "deerflow_subagent_enabled": false,
			"deerflow_max_concurrent_subagents": 3, "deerflow_recursion_limit": 1000,
			"timeout": 300, "proxy": "",
		}
	}
	return map[string]interface{}{}
}

// getEffectiveProviderMap 对齐 Python _get_effective_provider_map：
// 把 provider_sources 的字段合并进 provider，按 id 索引。
func getEffectiveProviderMap(config interface{}) map[string]map[string]interface{} {
	out := map[string]map[string]interface{}{}
	cm, ok := config.(map[string]interface{})
	if !ok {
		return out
	}
	sourceMap := map[string]map[string]interface{}{}
	if sources, ok := cm["provider_sources"].([]interface{}); ok {
		for _, s := range sources {
			sm, ok := s.(map[string]interface{})
			if !ok {
				continue
			}
			if id, _ := sm["id"].(string); id != "" {
				sourceMap[id] = copyMap(sm)
			}
		}
	}
	if providers, ok := cm["provider"].([]interface{}); ok {
		for _, p := range providers {
			pm, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			id, _ := pm["id"].(string)
			if id == "" {
				continue
			}
			eff := map[string]interface{}{}
			if srcID, _ := pm["provider_source_id"].(string); srcID != "" {
				if src, ok := sourceMap[srcID]; ok {
					eff = copyMap(src)
				}
			}
			for k, v := range copyMap(pm) {
				eff[k] = v
			}
			out[id] = eff
		}
	}
	return out
}

// getProviderRunnerType 对齐 Python _get_provider_runner_type。
func getProviderRunnerType(provider interface{}) string {
	pm, ok := provider.(map[string]interface{})
	if !ok {
		return ""
	}
	providerType, _ := pm["provider_type"].(string)
	runnerType, _ := pm["type"].(string)
	if runnerType == "" {
		runnerType, _ = pm["provider"].(string)
	}
	if providerType == "agent_runner" && agentRunnerTypes[runnerType] && runnerType != "local" {
		return runnerType
	}
	expectedField := map[string]string{
		"dify": "dify_api_key", "coze": "coze_api_key",
		"dashscope": "dashscope_app_id", "deerflow": "deerflow_api_base",
	}
	if field, ok := expectedField[runnerType]; ok {
		if _, exists := pm[field]; exists {
			return runnerType
		}
	}
	return ""
}

// copyProviderConfig 对齐 Python _copy_provider_config：把 provider 复制成内联 runner config。
func copyProviderConfig(runnerType string, provider map[string]interface{}) map[string]interface{} {
	cfg := map[string]interface{}{}
	for k, v := range provider {
		if legacyProviderIdentityFields[k] {
			continue
		}
		cfg[k] = deepCopyValue(v)
	}
	return normalizeAgentRunner(runnerType, cfg)
}

// normalizeValue 对齐 Python _normalize_value：按默认值类型归一化。
func normalizeValue(value interface{}, def interface{}) interface{} {
	switch d := def.(type) {
	case map[string]interface{}:
		vm, ok := value.(map[string]interface{})
		if !ok {
			return copyMap(d)
		}
		if len(d) == 0 {
			return copyMap(vm)
		}
		out := map[string]interface{}{}
		for k, childDefault := range d {
			out[k] = normalizeValue(vm[k], childDefault)
		}
		return out
	case []interface{}:
		if vl, ok := value.([]interface{}); ok {
			return deepCopyValue(vl)
		}
		return deepCopyValue(d)
	case bool:
		if vb, ok := value.(bool); ok {
			return vb
		}
		return d
	case int:
		if _, ok := value.(bool); ok {
			return d
		}
		switch n := value.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case string:
			if i, err := parseIntStrict(n); err == nil {
				return i
			}
		}
		return d
	case float64:
		if _, ok := value.(bool); ok {
			return d
		}
		switch n := value.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		case string:
			if f, err := parseFloatStrict(n); err == nil {
				return f
			}
		}
		return d
	case string:
		if vs, ok := value.(string); ok {
			return vs
		}
		return d
	}
	if value != nil {
		return deepCopyValue(value)
	}
	return def
}

// normalizeAgentRunner 对齐 Python normalize_agent_runner：按 runner 默认值归一化。
func normalizeAgentRunner(runnerType string, config map[string]interface{}) map[string]interface{} {
	normalized, _ := normalizeValue(config, agentRunnerConfigDefault(runnerType)).(map[string]interface{})
	if normalized == nil {
		normalized = agentRunnerConfigDefault(runnerType)
	}
	if runnerType == "local" {
		comp, _ := normalized["compression"].(map[string]interface{})
		if comp != nil {
			ratio := toFloat(comp["keep_recent_ratio"])
			ratio = clamp(ratio, 0.0, 0.3)
			comp["keep_recent_ratio"] = ratio
			if toInt(comp["trim_turns"]) < 1 {
				comp["trim_turns"] = 1
			}
		}
		model, _ := normalized["model"].(map[string]interface{})
		if model != nil && toInt(model["request_max_retries"]) < 1 {
			model["request_max_retries"] = 1
		}
		misc, _ := normalized["misc"].(map[string]interface{})
		if misc != nil && toInt(misc["max_steps"]) < 1 {
			misc["max_steps"] = 1
		}
	}
	return normalized
}

// migrateAgentRunnerConfig 对齐 Python _migrate_agent_runner_config。
// 返回是否发生变更。
func migrateAgentRunnerConfig(config map[string]interface{}, fallback map[string]interface{}) bool {
	changed := false

	providerSettings, ok := config["provider_settings"].(map[string]interface{})
	if !ok {
		providerSettings = map[string]interface{}{}
		config["provider_settings"] = providerSettings
		changed = true
	}

	existing, _ := config["agent_runner"].(map[string]interface{})
	configVersion, _ := config["config_version"].(float64)
	legacyVersion := configVersion < 3

	defaultLocal := map[string]interface{}{
		"runner_type": "local",
		"config":      agentRunnerConfigDefault("local"),
	}
	insertedBefore := legacyVersion && mapsEqual(existing, defaultLocal) && anyLegacyKeyIn(providerSettings)

	if existing != nil && !insertedBefore {
		// 已迁移过：仅清理残留旧键。
		for _, key := range legacyAgentRunnerSettingKeys {
			if _, ok := providerSettings[key]; ok {
				delete(providerSettings, key)
				changed = true
			}
		}
	} else {
		providerMap := getEffectiveProviderMap(fallback)
		for id, p := range getEffectiveProviderMap(config) {
			providerMap[id] = p
		}

		runnerType, _ := providerSettings["agent_runner_type"].(string)
		if !agentRunnerTypes[runnerType] {
			runnerType = "local"
		}
		defaultProviderID, _ := providerSettings["default_provider_id"].(string)
		defaultProvider := providerMap[defaultProviderID]
		defaultRunnerType := getProviderRunnerType(defaultProvider)
		if runnerType == "local" && defaultRunnerType != "" {
			runnerType = defaultRunnerType
		}

		var runnerConfig map[string]interface{}
		if runnerType == "local" {
			personaID, _ := providerSettings["default_personality"].(string)
			if personaID == "" {
				personaID = "default"
			}
			runnerConfig = agentRunnerConfigDefault("local")
			runnerConfig["model"] = map[string]interface{}{
				"provider_id":          defaultProviderID,
				"fallback_provider_ids": valueOr(providerSettings["fallback_chat_models"], []interface{}{}),
				"request_max_retries":  valueOr(providerSettings["request_max_retries"], float64(5)),
			}
			runnerConfig["persona"] = map[string]interface{}{
				"persona_id":            personaID,
				"safety_mode":           valueOr(providerSettings["llm_safety_mode"], true),
				"safety_mode_strategy":  valueOr(providerSettings["safety_mode_strategy"], "system_prompt"),
			}
			runnerConfig["compression"] = map[string]interface{}{
				"max_turns":          valueOr(providerSettings["max_context_length"], float64(-1)),
				"trim_turns":         valueOr(providerSettings["dequeue_context_length"], float64(1)),
				"overflow_strategy":  valueOr(providerSettings["context_limit_reached_strategy"], "llm_compress"),
				"instruction":        valueOr(providerSettings["llm_compress_instruction"], ""),
				"keep_recent_ratio":  valueOr(providerSettings["llm_compress_keep_recent_ratio"], 0.15),
				"provider_id":        valueOr(providerSettings["llm_compress_provider_id"], ""),
				"fallback_max_tokens": valueOr(providerSettings["fallback_max_context_tokens"], float64(128000)),
			}
			runnerConfig["misc"] = map[string]interface{}{
				"max_steps":                      valueOr(providerSettings["max_agent_step"], float64(30)),
				"tool_schema_mode":               valueOr(providerSettings["tool_schema_mode"], "full"),
				"tool_call_timeout":              valueOr(providerSettings["tool_call_timeout"], float64(120)),
				"sanitize_context_by_modalities": valueOr(providerSettings["sanitize_context_by_modalities"], false),
			}
			runnerConfig = normalizeAgentRunner("local", runnerConfig)

			availableModelProviderIDs := map[string]bool{}
			for id, p := range providerMap {
				if p["provider_type"] == "agent_runner" {
					continue
				}
				if getProviderRunnerType(p) != "" {
					continue
				}
				availableModelProviderIDs[id] = true
			}
			model, _ := runnerConfig["model"].(map[string]interface{})
			if model != nil {
				if pid, _ := model["provider_id"].(string); !availableModelProviderIDs[pid] {
					model["provider_id"] = ""
				}
				if fps, ok := model["fallback_provider_ids"].([]interface{}); ok {
					filtered := []interface{}{}
					for _, fp := range fps {
						if id, _ := fp.(string); availableModelProviderIDs[id] {
							filtered = append(filtered, fp)
						}
					}
					model["fallback_provider_ids"] = filtered
				}
			}
			comp, _ := runnerConfig["compression"].(map[string]interface{})
			if comp != nil {
				if pid, _ := comp["provider_id"].(string); !availableModelProviderIDs[pid] {
					comp["provider_id"] = ""
				}
			}
		} else {
			providerID, _ := providerSettings[legacyAgentRunnerProviderIDKeys[runnerType]].(string)
			if providerID == "" && defaultRunnerType == runnerType {
				providerID = defaultProviderID
			}
			provider := providerMap[providerID]
			if provider != nil && getProviderRunnerType(provider) == runnerType {
				runnerConfig = copyProviderConfig(runnerType, provider)
			} else {
				runnerConfig = agentRunnerConfigDefault(runnerType)
			}
		}

		config["agent_runner"] = map[string]interface{}{
			"runner_type": runnerType,
			"config":      runnerConfig,
		}
		for _, key := range legacyAgentRunnerSettingKeys {
			delete(providerSettings, key)
		}
		changed = true
	}

	if cv, _ := config["config_version"].(float64); cv != 3 {
		config["config_version"] = float64(3)
		changed = true
	}
	return changed
}

// --- 小工具 ---

func anyLegacyKeyIn(m map[string]interface{}) bool {
	for _, key := range legacyAgentRunnerSettingKeys {
		if _, ok := m[key]; ok {
			return true
		}
	}
	return false
}

func mapsEqual(a, b map[string]interface{}) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return reflect.DeepEqual(a, b)
}

func parseIntStrict(s string) (int, error) {
	return strconv.Atoi(s)
}

func parseFloatStrict(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

func valueOr(v interface{}, def interface{}) interface{} {
	if v == nil {
		return def
	}
	return v
}

func toInt(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

func toFloat(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
