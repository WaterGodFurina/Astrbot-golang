package pipeline

import (
	"testing"
)

func testProcessStageWithConfig(t *testing.T, cfg map[string]interface{}) *ProcessStage {
	t.Helper()
	s := NewProcessStage()
	ctx := &PipelineContext{AstrbotConfig: cfg}
	if err := s.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return s
}

func TestToolSchemaModeRead(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{
		"provider_settings": map[string]interface{}{"tool_schema_mode": "skills_like"},
	})
	if s.toolSchemaMode != "skills_like" {
		t.Fatalf("tool_schema_mode = %q, want skills_like", s.toolSchemaMode)
	}
	s2 := testProcessStageWithConfig(t, map[string]interface{}{})
	if s2.toolSchemaMode != "full" {
		t.Fatalf("default tool_schema_mode = %q, want full", s2.toolSchemaMode)
	}
}

func TestLightAndParamToolSchemas(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{
		"subagent_orchestrator": map[string]interface{}{
			"main_enable": true,
			"agents": []interface{}{
				map[string]interface{}{"name": "translator", "enabled": true},
			},
		},
	})
	// Full tools include get_current_time + transfer_to_translator.
	full := s.collectTools("none")
	if len(full) == 0 {
		t.Fatal("no full tools")
	}
	names := func(tools []map[string]interface{}) map[string]bool {
		m := map[string]bool{}
		for _, tool := range tools {
			fn, _ := tool["function"].(map[string]interface{})
			if n, _ := fn["name"].(string); n != "" {
				m[n] = true
			}
		}
		return m
	}
	fullNames := names(full)

	// Light tools: same names, but empty parameters.
	light := s.collectLightTools("none")
	lightNames := names(light)
	if len(lightNames) != len(fullNames) {
		t.Fatalf("light tools %v != full tools %v", lightNames, fullNames)
	}
	for _, tool := range light {
		fn, _ := tool["function"].(map[string]interface{})
		params, ok := fn["parameters"].(map[string]interface{})
		if !ok {
			t.Fatal("light tool missing parameters")
		}
		props, _ := params["properties"].(map[string]interface{})
		if len(props) != 0 {
			t.Fatalf("light tool parameters should be empty, got %v", props)
		}
	}

	// Param-only subset: only requested tools, full parameters preserved.
	param := s.collectParamToolsFor("none", []string{"get_current_time"})
	if len(param) != 1 {
		t.Fatalf("expected 1 param tool, got %d", len(param))
	}
	fn, _ := param[0]["function"].(map[string]interface{})
	if fn["name"] != "get_current_time" {
		t.Fatalf("param tool name = %v", fn["name"])
	}
	if _, ok := fn["parameters"].(map[string]interface{}); !ok {
		t.Fatal("param tool missing full parameters")
	}
}

func TestCollectLightToolsDoesNotPolluteMCPCache(t *testing.T) {
	s := NewProcessStage()
	s.mcpMu.Lock()
	s.mcpLoaded = true
	s.mcpSchemas = map[string]map[string]interface{}{
		"files.read": {
			"type": "function",
			"function": map[string]interface{}{
				"name":        "files.read",
				"description": "MCP 服务器工具（files）: 读取文件",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type": "string",
							"items": []interface{}{
								map[string]interface{}{"nested": true},
							},
						},
					},
					"required": []interface{}{"path"},
				},
			},
		},
	}
	s.mcpMu.Unlock()

	light := s.collectLightTools("none")
	var found bool
	for _, tool := range light {
		fn, _ := tool["function"].(map[string]interface{})
		if fn["name"] != "files.read" {
			continue
		}
		found = true
		params, _ := fn["parameters"].(map[string]interface{})
		if props, _ := params["properties"].(map[string]interface{}); len(props) != 0 {
			t.Fatalf("light MCP tool parameters should be empty, got %v", props)
		}
	}
	if !found {
		t.Fatal("files.read tool missing from light tools")
	}

	// The cached schema must keep its full parameters after the light pass.
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()
	cached := s.mcpSchemas["files.read"]
	fn, _ := cached["function"].(map[string]interface{})
	params, ok := fn["parameters"].(map[string]interface{})
	if !ok {
		t.Fatal("cached MCP tool lost parameters map")
	}
	props, _ := params["properties"].(map[string]interface{})
	if _, ok := props["path"]; !ok {
		t.Fatalf("cached MCP tool parameters were polluted, got %v", props)
	}
	if _, ok := params["required"]; !ok {
		t.Fatal("cached MCP tool lost required field")
	}
}
