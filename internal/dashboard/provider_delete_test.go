package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/config"
)

// testServerWithConfig builds a Server backed by an isolated config manager
// seeded with the given config map.
func testServerWithConfig(t *testing.T, cfgMap map[string]interface{}) *Server {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cmd_config.json")
	data, _ := json.Marshal(cfgMap)
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	cm := config.NewConfigManager()
	acfg := config.NewConfig(cfgPath)
	if err := acfg.Load(); err != nil {
		t.Fatal(err)
	}
	cm.Register("default", acfg)
	return NewServerWithManagers(0, filepath.Join(dir, "pw.json"), map[string]interface{}{
		"config": cm,
	})
}

// TestDeleteProviderSourceCascadesProviders guards against orphaned providers
// left behind when a provider source is deleted.
func TestDeleteProviderSourceCascadesProviders(t *testing.T) {
	s := testServerWithConfig(t, map[string]interface{}{
		"provider_sources": []interface{}{
			map[string]interface{}{"id": "srcA"},
			map[string]interface{}{"id": "srcB"},
		},
		"provider": []interface{}{
			map[string]interface{}{"id": "p1", "provider_source_id": "srcA"},
			map[string]interface{}{"id": "p2", "provider_source_id": "srcA"},
			map[string]interface{}{"id": "p3", "provider_source_id": "srcB"},
		},
	})

	if err := s.deleteProviderSource("srcA"); err != nil {
		t.Fatalf("deleteProviderSource: %v", err)
	}
	cfg := s.getConfigSnapshot()

	sources, _ := cfg["provider_sources"].([]interface{})
	if len(sources) != 1 {
		t.Fatalf("expected 1 source left, got %d", len(sources))
	}

	providers, _ := cfg["provider"].([]interface{})
	if len(providers) != 1 {
		t.Fatalf("expected 1 provider left, got %d", len(providers))
	}
	if pid, _ := providers[0].(map[string]interface{})["id"].(string); pid != "p3" {
		t.Errorf("expected p3 to survive, got %q", pid)
	}
}

// TestDeleteProviderCleansSettingsRefs guards against provider_settings still
// referencing a deleted provider (default_provider_id / provider_pool).
func TestDeleteProviderCleansSettingsRefs(t *testing.T) {
	s := testServerWithConfig(t, map[string]interface{}{
		"provider": []interface{}{
			map[string]interface{}{"id": "p1"},
			map[string]interface{}{"id": "p2"},
		},
		"provider_settings": map[string]interface{}{
			"default_provider_id": "p1",
			"provider_pool":       []interface{}{"p1", "p2"},
		},
	})

	if err := s.deleteProviderByID("p1"); err != nil {
		t.Fatalf("deleteProviderByID: %v", err)
	}
	cfg := s.getConfigSnapshot()

	providers, _ := cfg["provider"].([]interface{})
	if len(providers) != 1 {
		t.Fatalf("expected 1 provider left, got %d", len(providers))
	}

	ps, _ := cfg["provider_settings"].(map[string]interface{})
	if v, _ := ps["default_provider_id"].(string); v != "" {
		t.Errorf("default_provider_id should be cleared, got %q", v)
	}
	pool, _ := ps["provider_pool"].([]interface{})
	if len(pool) != 1 {
		t.Fatalf("provider_pool should have 1 entry, got %v", pool)
	}
	if p, _ := pool[0].(string); p != "p2" {
		t.Errorf("provider_pool should keep p2, got %q", p)
	}
}
