package config

import (
	"encoding/json"
	"testing"
)

// TestMigrateAgentRunnerConfigLegacy 验证旧 provider_settings.* 结构被迁移为
// agent_runner.config.*，且旧键被删除、config_version 升到 3。
func TestMigrateAgentRunnerConfigLegacy(t *testing.T) {
	conf := map[string]interface{}{
		"config_version": float64(2),
		"provider_settings": map[string]interface{}{
			"enable":                        true,
			"agent_runner_type":             "local",
			"default_provider_id":           "p1",
			"fallback_chat_models":          []interface{}{"p2"},
			"request_max_retries":           float64(7),
			"default_personality":           "cool",
			"llm_safety_mode":               false,
			"max_agent_step":                float64(42),
			"max_context_length":            float64(10),
			"dequeue_context_length":        float64(2),
			"tool_call_timeout":             float64(99),
			"reachability_check":            true,
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

// TestMigrateInsertedBeforeNumericEquality 验证 insertedBefore 守卫在数值类型
// 跨 int/float64 时依然触发：JSON 解析出的 existing 数值是 float64，而
// agentRunnerConfigDefault("local") 里是 Go int，语义相等必须视为相等
// （对齐 Python == 的数值语义），否则 legacy 键会被删除而值未迁移。
func TestMigrateInsertedBeforeNumericEquality(t *testing.T) {
	// 构造"先前插入过默认值"的 existing：结构同默认值，但数值全部是
	// JSON 反序列化产生的 float64 类型。
	existingConfig := map[string]interface{}{
		"model": map[string]interface{}{
			"provider_id":           "",
			"fallback_provider_ids": []interface{}{},
			"request_max_retries":   float64(5),
		},
		"persona": map[string]interface{}{
			"persona_id":           "default",
			"safety_mode":          true,
			"safety_mode_strategy": "system_prompt",
		},
		"compression": map[string]interface{}{
			"max_turns":           float64(-1),
			"trim_turns":          float64(1),
			"overflow_strategy":   "llm_compress",
			"instruction":         "",
			"keep_recent_ratio":   float64(0.15),
			"provider_id":         "",
			"fallback_max_tokens": float64(128000),
		},
		"misc": map[string]interface{}{
			"max_steps":                      float64(30),
			"tool_schema_mode":               "full",
			"tool_call_timeout":              float64(120),
			"sanitize_context_by_modalities": false,
		},
	}
	if !mapsEqualNormalized(existingConfig, agentRunnerConfigDefault("local")) {
		t.Fatal("float64-based JSON tree should equal int-based default tree")
	}

	conf := map[string]interface{}{
		"config_version": float64(2),
		"agent_runner": map[string]interface{}{
			"runner_type": "local",
			"config":      existingConfig,
		},
		"provider_settings": map[string]interface{}{
			"agent_runner_type":   "local",
			"default_provider_id": "p1",
			"max_agent_step":      float64(42), // 用户自定义值，必须被迁移而不是丢弃
		},
		"provider": []interface{}{
			map[string]interface{}{"id": "p1", "provider_type": "chat_completion"},
		},
	}
	if !migrateAgentRunnerConfig(conf, nil) {
		t.Fatal("expected re-migration (insertedBefore guard should trigger)")
	}
	ar, ok := conf["agent_runner"].(map[string]interface{})
	if !ok {
		t.Fatal("agent_runner missing after re-migration")
	}
	cfg := ar["config"].(map[string]interface{})
	misc := cfg["misc"].(map[string]interface{})
	// normalizeValue 会按默认值类型把数值归一化为 int，这里只校验值未丢失。
	if v, ok := normalizedNumber(misc["max_steps"]); !ok || v != 42 {
		t.Fatalf("user value max_steps lost in re-migration: %v", misc["max_steps"])
	}
	ps := conf["provider_settings"].(map[string]interface{})
	if _, exists := ps["max_agent_step"]; exists {
		t.Fatal("legacy key max_agent_step not removed after re-migration")
	}
}

// TestMigrateConfigVersionIntType 验证程序内写入的 int(3) config_version
// 不再被误判为旧版本（此前类型断言失败导致每次加载都重迁移）。
func TestMigrateConfigVersionIntType(t *testing.T) {
	conf := map[string]interface{}{
		"config_version": int(3), // 程序内写入的 int 而非 JSON 的 float64
		"agent_runner": map[string]interface{}{
			"runner_type": "local",
			"config":      agentRunnerConfigDefault("local"),
		},
		"provider_settings": map[string]interface{}{
			"enable": true, // 无残留 legacy 键
		},
	}
	if migrateAgentRunnerConfig(conf, nil) {
		t.Fatal("int(3) config_version should not trigger migration")
	}
	if _, ok := normalizedNumber(nil); ok {
		t.Fatal("normalizedNumber(nil) must not report a number")
	}
	if v, ok := normalizedNumber(json.Number("3")); !ok || v != 3 {
		t.Fatalf("json.Number support broken: %v %v", v, ok)
	}
}

// TestMigrateProviderMissingWarnsAndFallsBack 验证非 local runner 找不到
// provider 时走默认配置回退路径（与 Python 行为一致），并正确设置 runner。
func TestMigrateProviderMissingWarnsAndFallsBack(t *testing.T) {
	conf := map[string]interface{}{
		"config_version": float64(2),
		"provider_settings": map[string]interface{}{
			"agent_runner_type":             "coze",
			"coze_agent_runner_provider_id": "ghost", // provider 列表中不存在
		},
		"provider": []interface{}{},
	}
	if !migrateAgentRunnerConfig(conf, nil) {
		t.Fatal("expected migration to report change")
	}
	ar := conf["agent_runner"].(map[string]interface{})
	if ar["runner_type"] != "coze" {
		t.Fatalf("runner_type=%v", ar["runner_type"])
	}
	cfg := ar["config"].(map[string]interface{})
	def := agentRunnerConfigDefault("coze")
	if !mapsEqualNormalized(cfg, def) {
		t.Fatalf("expected coze default config fallback, got %v", cfg)
	}
	if cv := configVersionOf(conf); cv != 3 {
		t.Fatalf("config_version=%v", cv)
	}
}
