package config

import "testing"

// TestMigrateAgentRunnerConfigLegacy 验证旧 provider_settings.* 结构被迁移为
// agent_runner.config.*，且旧键被删除、config_version 升到 3。
func TestMigrateAgentRunnerConfigLegacy(t *testing.T) {
	conf := map[string]interface{}{
		"config_version": float64(2),
		"provider_settings": map[string]interface{}{
			"enable":                    true,
			"agent_runner_type":         "local",
			"default_provider_id":       "p1",
			"fallback_chat_models":      []interface{}{"p2"},
			"request_max_retries":       float64(7),
			"default_personality":       "cool",
			"llm_safety_mode":           false,
			"max_agent_step":            float64(42),
			"max_context_length":        float64(10),
			"dequeue_context_length":    float64(2),
			"tool_call_timeout":         float64(99),
			"reachability_check":        true,
			"dify_agent_runner_provider_id": "",
		},
		"provider": []interface{}{
			map[string]interface{}{"id": "p1", "provider_type": "chat_completion"},
			map[string]interface{}{"id": "p2", "provider_type": "chat_completion"},
		},
	}
	if !migrateAgentRunnerConfig(conf, nil) {
		t.Fatal("expected migration to report change")
	}
	ar, ok := conf["agent_runner"].(map[string]interface{})
	if !ok {
		t.Fatal("agent_runner not created")
	}
	if ar["runner_type"] != "local" {
		t.Fatalf("runner_type=%v", ar["runner_type"])
	}
	cfg := ar["config"].(map[string]interface{})
	model := cfg["model"].(map[string]interface{})
	if model["provider_id"] != "p1" {
		t.Fatalf("model.provider_id=%v", model["provider_id"])
	}
	if model["request_max_retries"] != 7 {
		t.Fatalf("request_max_retries=%v", model["request_max_retries"])
	}
	misc := cfg["misc"].(map[string]interface{})
	if misc["max_steps"] != 42 {
		t.Fatalf("misc.max_steps=%v", misc["max_steps"])
	}
	comp := cfg["compression"].(map[string]interface{})
	if comp["max_turns"] != 10 {
		t.Fatalf("compression.max_turns=%v", comp["max_turns"])
	}
	ps := conf["provider_settings"].(map[string]interface{})
	for _, k := range []string{"agent_runner_type", "default_provider_id", "max_agent_step", "max_context_length"} {
		if _, exists := ps[k]; exists {
			t.Fatalf("legacy key %q not removed", k)
		}
	}
	if ps["reachability_check"] != true {
		t.Fatal("non-legacy key reachability_check should be preserved")
	}
	if conf["config_version"] != float64(3) {
		t.Fatalf("config_version=%v", conf["config_version"])
	}
}

// TestMigrateAgentRunnerConfigIdempotent 验证已迁移的配置再次运行只清理残键。
func TestMigrateAgentRunnerConfigIdempotent(t *testing.T) {
	conf := map[string]interface{}{
		"config_version": float64(3),
		"agent_runner": map[string]interface{}{
			"runner_type": "coze",
			"config":      agentRunnerConfigDefault("coze"),
		},
		"provider_settings": map[string]interface{}{
			"enable":            true,
			"agent_runner_type": "coze", // 残留旧键
		},
	}
	if !migrateAgentRunnerConfig(conf, nil) {
		t.Fatal("expected change (residual legacy key removal)")
	}
	ar := conf["agent_runner"].(map[string]interface{})
	if ar["runner_type"] != "coze" {
		t.Fatalf("runner_type overwritten: %v", ar["runner_type"])
	}
	ps := conf["provider_settings"].(map[string]interface{})
	if _, exists := ps["agent_runner_type"]; exists {
		t.Fatal("residual legacy key not removed")
	}
}
