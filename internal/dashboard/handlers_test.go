package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

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
