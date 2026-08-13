package pipeline

import (
	"testing"
)

func TestLoadSubAgents(t *testing.T) {
	cfg := map[string]interface{}{
		"subagent_orchestrator": map[string]interface{}{
			"main_enable": true,
			"agents": []interface{}{
				map[string]interface{}{
					"name":               "translator",
					"persona_id":         "default",
					"public_description": "翻译助手",
					"enabled":            true,
					"provider_id":        "stepfun/step-router-v1",
				},
				map[string]interface{}{
					"name":    "coder",
					"enabled": false,
				},
				map[string]interface{}{"name": ""}, // skipped (empty name)
			},
		},
	}
	agents, mainEnable := loadSubAgents(cfg)
	if !mainEnable {
		t.Fatal("main_enable should be true")
	}
	if len(agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(agents))
	}
	if agents[0].Name != "translator" || agents[0].PersonaID != "default" ||
		agents[0].ProviderID != "stepfun/step-router-v1" {
		t.Fatalf("agent[0] = %+v", agents[0])
	}
	if agents[1].Enabled {
		t.Fatal("coder should be disabled")
	}
}

func TestSubAgentToolSchemas(t *testing.T) {
	agents := []*SubAgent{
		{Name: "translator", PublicDescription: "翻译助手", Enabled: true},
		{Name: "coder", Enabled: false},
	}
	schemas := subAgentToolSchemas(agents)
	if len(schemas) != 1 {
		t.Fatalf("expected 1 enabled schema, got %d", len(schemas))
	}
	fn, ok := schemas[0]["function"].(map[string]interface{})
	if !ok {
		t.Fatal("missing function wrapper")
	}
	if fn["name"] != "transfer_to_translator" {
		t.Fatalf("tool name = %v", fn["name"])
	}
	if fn["description"] != "翻译助手" {
		t.Fatalf("description = %v", fn["description"])
	}
	params, ok := fn["parameters"].(map[string]interface{})
	if !ok {
		t.Fatal("missing parameters")
	}
	props, _ := params["properties"].(map[string]interface{})
	if _, ok := props["input"]; !ok {
		t.Fatal("missing input property")
	}
	// Default description when public_description empty.
	agents2 := []*SubAgent{{Name: "coder", Enabled: true}}
	schemas2 := subAgentToolSchemas(agents2)
	fn2 := schemas2[0]["function"].(map[string]interface{})
	if fn2["name"] != "transfer_to_coder" {
		t.Fatalf("tool name = %v", fn2["name"])
	}
}

func TestCollectToolsIncludesSubagents(t *testing.T) {
	s := NewProcessStage()
	ctx := &PipelineContext{
		AstrbotConfig: map[string]interface{}{
			"subagent_orchestrator": map[string]interface{}{
				"main_enable": true,
				"agents": []interface{}{
					map[string]interface{}{"name": "translator", "enabled": true},
					map[string]interface{}{"name": "coder", "enabled": true},
				},
			},
		},
	}
	if err := s.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	tools := s.collectTools("none")
	var names []string
	for _, tool := range tools {
		if fn, ok := tool["function"].(map[string]interface{}); ok {
			if n, ok := fn["name"].(string); ok {
				names = append(names, n)
			}
		}
	}
	has := func(n string) bool {
		for _, x := range names {
			if x == n {
				return true
			}
		}
		return false
	}
	if !has("transfer_to_translator") || !has("transfer_to_coder") {
		t.Fatalf("subagent tools missing from collectTools: %v", names)
	}

	// Disabled main_enable -> no handoff tools.
	s2 := NewProcessStage()
	ctx2 := &PipelineContext{AstrbotConfig: map[string]interface{}{
		"subagent_orchestrator": map[string]interface{}{
			"main_enable": false,
			"agents":      []interface{}{map[string]interface{}{"name": "x", "enabled": true}},
		},
	}}
	_ = s2.Initialize(ctx2)
	for _, tool := range s2.collectTools("none") {
		if fn, ok := tool["function"].(map[string]interface{}); ok {
			if n, _ := fn["name"].(string); n == "transfer_to_x" {
				t.Fatal("handoff tool injected while main_enable=false")
			}
		}
	}
}
