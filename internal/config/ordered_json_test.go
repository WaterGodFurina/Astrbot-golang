package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultConfigJSONOrder verifies the canonical default config preserves
// the Python DEFAULT_CONFIG ordering (the WebUI renders fields in this order).
func TestDefaultConfigJSONOrder(t *testing.T) {
	oj, err := ParseOrderedJSON([]byte(DefaultConfigJSONRaw))
	if err != nil {
		t.Fatalf("parse default config: %v", err)
	}
	keys := oj.Keys()
	if len(keys) == 0 {
		t.Fatal("default config has no keys")
	}
	// First key should be config_version, matching the Python DEFAULT_CONFIG.
	if keys[0] != "config_version" {
		t.Errorf("first default config key = %q, want config_version", keys[0])
	}
	if keys[1] != "platform_settings" {
		t.Errorf("second default config key = %q, want platform_settings", keys[1])
	}
}

// TestOrderedJSONRoundTrip verifies key order survives a
// marshal/unmarshal round-trip.
func TestOrderedJSONRoundTrip(t *testing.T) {
	src := `{"b":1,"a":{"y":2,"x":3},"c":[{"m":1,"n":2}]}`
	oj, err := ParseOrderedJSON([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := json.Marshal(oj)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"b":1,"a":{"y":2,"x":3}`) {
		t.Errorf("order not preserved: %s", out)
	}
}

// TestSaveWritesPythonOrder verifies that saving a config file writes keys in
// the Python DEFAULT_CONFIG order instead of alphabetical order.
func TestSaveWritesPythonOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cmd_config.json")
	cfg := NewConfig(path)
	// Simulate a config that would otherwise be alphabetized.
	cfg.data = map[string]interface{}{
		"provider":          []interface{}{},
		"config_version":    2,
		"log_level":         "INFO",
		"dashboard":         map[string]interface{}{"host": "0.0.0.0"},
		"platform_settings": map[string]interface{}{"reply_prefix": ""},
		"platform":          []interface{}{},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var oj OrderedJSON
	if err := oj.UnmarshalJSON(data); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	keys := oj.Keys()
	if keys[0] != "config_version" {
		t.Errorf("saved first key = %q, want config_version (got %v)", keys[0], keys)
	}
	if keys[1] != "platform_settings" {
		t.Errorf("saved second key = %q, want platform_settings", keys[1])
	}
	// "dashboard" must come before "platform" per DEFAULT_CONFIG order.
	pos := map[string]int{}
	for i, k := range keys {
		pos[k] = i
	}
	if pos["dashboard"] > pos["platform"] {
		t.Errorf("dashboard(%d) should sort before platform(%d): %v", pos["dashboard"], pos["platform"], keys)
	}
}
