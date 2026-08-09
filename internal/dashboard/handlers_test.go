package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AstrBotDevs/AstrBot/internal/config"
	"github.com/AstrBotDevs/AstrBot/internal/plugin"
)

func TestHandlersReturnNamedObjects(t *testing.T) {
	s := NewServer(0, "/tmp/test_pw.json")
	defer s.Stop()

	tests := []struct {
		method string
		path   string
		check  func(t *testing.T, body string)
	}{
		{"GET", "/api/system-config/schema", func(t *testing.T, body string) {
			var v map[string]interface{}
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				t.Fatalf("invalid json: %v", err)
			}
			data, ok := v["data"].(map[string]interface{})
			if !ok {
				t.Fatalf("expected data object, got: %T", v["data"])
			}
			if _, ok := data["config"]; !ok {
				t.Errorf("missing 'config' key in data")
			}
			if _, ok := data["metadata"]; !ok {
				t.Errorf("missing 'metadata' key in data")
			}
		}},
		{"GET", "/api/config-profiles", func(t *testing.T, body string) {
			if !strings.Contains(body, `"info_list"`) {
				t.Errorf("expected 'info_list' key in: %s", body)
			}
		}},
		{"GET", "/api/config-routes", func(t *testing.T, body string) {
			if !strings.Contains(body, `"routing"`) {
				t.Errorf("expected 'routing' key in: %s", body)
			}
		}},
		{"GET", "/api/plugins", func(t *testing.T, body string) {
			var v map[string]interface{}
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				t.Fatalf("invalid json: %v", err)
			}
			if _, ok := v["data"].([]interface{}); !ok {
				t.Errorf("expected data to be an array in: %s", body)
			}
		}},
		{"GET", "/api/providers", func(t *testing.T, body string) {
			if !strings.Contains(body, `"providers"`) {
				t.Errorf("expected 'providers' key in: %s", body)
			}
		}},
		{"GET", "/api/conversations", func(t *testing.T, body string) {
			if !strings.Contains(body, `"conversations"`) {
				t.Errorf("expected 'conversations' key in: %s", body)
			}
		}},
		{"GET", "/api/logs", func(t *testing.T, body string) {
			if !strings.Contains(body, `"logs"`) {
				t.Errorf("expected 'logs' key in: %s", body)
			}
		}},
		{"GET", "/api/tools/list", func(t *testing.T, body string) {
			var v map[string]interface{}
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				t.Fatalf("invalid json: %v", err)
			}
			if _, ok := v["data"].([]interface{}); !ok {
				t.Errorf("expected data to be an array in: %s", body)
			}
		}},
		{"GET", "/api/children", func(t *testing.T, body string) {
			var v map[string]interface{}
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				t.Fatalf("invalid json: %v", err)
			}
			data, ok := v["data"].(map[string]interface{})
			if !ok {
				t.Fatalf("expected data object, got: %T", v["data"])
			}
			if len(data) == 0 {
				t.Errorf("expected non-empty data object, got: %s", body)
			}
		}},
		{"GET", "/api/personas", func(t *testing.T, body string) {
			var v map[string]interface{}
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				t.Fatalf("invalid json: %v", err)
			}
			if _, ok := v["data"].([]interface{}); !ok {
				t.Errorf("expected data to be an array in: %s", body)
			}
		}},
		{"GET", "/api/mcp/servers", func(t *testing.T, body string) {
			var v map[string]interface{}
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				t.Fatalf("invalid json: %v", err)
			}
			if _, ok := v["data"].([]interface{}); !ok {
				t.Errorf("expected data to be an array in: %s", body)
			}
		}},
		{"GET", "/api/commands", func(t *testing.T, body string) {
			if !strings.Contains(body, `"items"`) {
				t.Errorf("expected 'items' key in: %s", body)
			}
		}},
		{"GET", "/api/files", func(t *testing.T, body string) {
			if !strings.Contains(body, `"files"`) && !strings.Contains(body, `"entries"`) {
				t.Errorf("expected 'files' or 'entries' key in: %s", body)
			}
		}},
		{"GET", "/api/backups", func(t *testing.T, body string) {
			if !strings.Contains(body, `"items"`) {
				t.Errorf("expected 'items' key in: %s", body)
			}
		}},
		{"GET", "/api/provider-sources", func(t *testing.T, body string) {
			if !strings.Contains(body, `"provider_sources"`) {
				t.Errorf("expected 'provider_sources' key in: %s", body)
			}
		}},
		{"GET", "/api/plugin-sources", func(t *testing.T, body string) {
			if !strings.Contains(body, `"sources"`) {
				t.Errorf("expected 'sources' key in: %s", body)
			}
		}},
		{"GET", "/api/skills", func(t *testing.T, body string) {
			if !strings.Contains(body, `"skills"`) {
				t.Errorf("expected 'skills' key in: %s", body)
			}
		}},
		{"GET", "/api/mcp", func(t *testing.T, body string) {
			var v map[string]interface{}
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				t.Fatalf("invalid json: %v", err)
			}
			if _, ok := v["data"]; !ok {
				t.Errorf("missing 'data' key in: %s", body)
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, req)
			tt.check(t, w.Body.String())
		})
	}
}

// TestGetPluginListNilLegacyManager guards against a typed-nil *plugin.Manager
// (legacy_plugin_mode=false) causing getPluginList to panic on a nil receiver.
func TestGetPluginListNilLegacyManager(t *testing.T) {
	s := NewServer(0, "/tmp/test_pw.json")
	defer s.Stop()
	var nilMgr *plugin.Manager
	s.pluginMgr = nilMgr // typed-nil inside interface{}

	list := s.getPluginList()
	if list == nil {
		t.Fatal("getPluginList should return a non-nil list even with a nil legacy manager")
	}
	byID := s.pluginByID("anything")
	if byID == nil {
		t.Fatal("pluginByID should return a non-nil map with a nil legacy manager")
	}
	if failed := s.pluginFailed(); failed == nil {
		t.Fatal("pluginFailed should return a non-nil map with a nil legacy manager")
	}
}

// TestPluginSourceRoutes guards that the per-plugin source/update/reload routes
// are dispatched to their handlers instead of falling into the uninstall branch.
func TestPluginSourceRoutes(t *testing.T) {
	s := NewServer(0, "/tmp/test_pw.json")
	defer s.Stop()

	// Bind-source on a non-existent subprocess plugin: must NOT uninstall, and
	// must return an error rather than silently succeeding.
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/plugins/nonexistent/source",
		strings.NewReader(`{"install_method":"market","market_plugin_id":"a/b"}`))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var v map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if st, _ := v["status"].(string); st != "error" {
		t.Errorf("bind-source on unknown plugin should error, got: %s", w.Body.String())
	}

	// Single-plugin update route must not uninstall.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/plugins/nonexistent/update", nil)
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	v = map[string]interface{}{}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if st, _ := v["status"].(string); st != "error" {
		t.Errorf("update on unknown plugin should error, got: %s", w.Body.String())
	}

	// Batch update route with empty body.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/plugins/update", nil)
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	v = map[string]interface{}{}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if st, _ := v["status"].(string); st != "error" {
		t.Errorf("batch update with no ids should error, got: %s", w.Body.String())
	}
}

// TestMetadataOrderPreserved verifies the served config metadata keeps the
// Python CONFIG_METADATA_3 group/section order instead of alphabetical order.
func TestMetadataOrderPreserved(t *testing.T) {
	s := NewServer(0, "/tmp/test_pw.json")
	defer s.Stop()

	for _, path := range []string{"/api/system-config/schema", "/api/system-config/runtime"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		var v map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatalf("%s invalid json: %v", path, err)
		}
		data, ok := v["data"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s no data: %s", path, w.Body.String())
		}
		md, ok := data["metadata"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s no metadata: %s", path, w.Body.String())
		}
		// Re-derive order from raw JSON key sequence.
		order := rawKeys(t, w.Body.Bytes(), data, "metadata")
		if len(order) == 0 {
			t.Fatalf("%s metadata empty", path)
		}
		// ai_group must come before platform_group, which must come before
		// plugin_group / ext_group (Python order), and provider_group last.
		pos := map[string]int{}
		for i, k := range order {
			pos[k] = i
		}
		if pos["ai_group"] > pos["platform_group"] {
			t.Errorf("%s: ai_group(%d) should be before platform_group(%d): %v", path, pos["ai_group"], pos["platform_group"], order)
		}
		if pos["platform_group"] > pos["plugin_group"] {
			t.Errorf("%s: platform_group(%d) should be before plugin_group(%d): %v", path, pos["platform_group"], pos["plugin_group"], order)
		}
		if _, ok := pos["provider_group"]; ok {
			if pos["provider_group"] < pos["ext_group"] {
				t.Errorf("%s: provider_group(%d) should be last: %v", path, pos["provider_group"], order)
			}
		}
		_ = md
	}

	// Section order within ai_group must follow Python (agent_runner first).
	md := s.getConfigMetadata()
	ai, _ := md.Get("ai_group")
	aiMap, _ := config.GetOrderedJSON(ai)
	if aiMap == nil {
		t.Fatal("ai_group missing")
	}
	aiMeta, _ := aiMap.Get("metadata")
	aiMetaMap, _ := config.GetOrderedJSON(aiMeta)
	if aiMetaMap == nil {
		t.Fatal("ai_group.metadata missing")
	}
	secs := aiMetaMap.Keys()
	if secs[0] != "agent_runner" {
		t.Errorf("ai_group first section = %q, want agent_runner: %v", secs[0], secs)
	}
	if secs[len(secs)-1] != "others" {
		t.Errorf("ai_group last section = %q, want others: %v", secs[len(secs)-1], secs)
	}
}

// rawKeys reconstructs the ordered top-level key list of the JSON object at
// objPath within the full response body (writeJSON emits OrderedJSON in order).
func rawKeys(t *testing.T, body []byte, obj map[string]interface{}, key string) []string {
	t.Helper()
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ordered, err := config.ParseOrderedJSON([]byte(body))
	if err != nil {
		t.Fatalf("ordered parse: %v", err)
	}
	dataO, _ := ordered.Get("data")
	dataOM, ok := config.GetOrderedJSON(dataO)
	if !ok {
		return nil
	}
	mdO, ok := dataOM.Get(key)
	if !ok {
		return nil
	}
	mdOM, ok := config.GetOrderedJSON(mdO)
	if !ok {
		return nil
	}
	return mdOM.Keys()
}
