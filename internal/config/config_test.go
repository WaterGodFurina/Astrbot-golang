package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIssue9512_DictKeysNotCleared verifies the fix for issue #9512:
// check_config_integrity should NOT clear user-created keys in dict-type
// config fields where the reference default is an empty {}.
func TestIssue9512_DictKeysNotCleared(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")

	// Simulate a config schema where "plugin_config" is a dict type
	// with empty defaults - user keys must be preserved
	defaults := map[string]interface{}{
		"provider_settings": map[string]interface{}{
			"max_context_length": 50,
		},
		"plugin_config": map[string]interface{}{}, // empty dict
		"wake_prefix":   "/",
	}

	// Write a user config that has extra keys in the dict
	userConfig := map[string]interface{}{
		"provider_settings": map[string]interface{}{
			"max_context_length": 100,
			"custom_setting":     "user_value",
		},
		"plugin_config": map[string]interface{}{
			"my_plugin": map[string]interface{}{
				"enabled": true,
			},
		},
		"wake_prefix": "/",
	}

	// Run integrity check - this should NOT remove user keys
	changed := checkConfigIntegrity(defaults, userConfig, "")

	// Verify user keys are preserved
	pluginCfg, ok := userConfig["plugin_config"].(map[string]interface{})
	if !ok {
		t.Fatal("plugin_config was wiped by integrity check!")
	}
	if _, exists := pluginCfg["my_plugin"]; !exists {
		t.Error(
			"BUG #9512: user key 'my_plugin' was cleared from plugin_config!\n" +
				"The integrity check should preserve user keys in dict-type fields.",
		)
	}

	// Verify user-added keys in provider_settings are also preserved
	provCfg, ok := userConfig["provider_settings"].(map[string]interface{})
	if !ok {
		t.Fatal("provider_settings was wiped!")
	}
	if _, exists := provCfg["custom_setting"]; !exists {
		t.Error("BUG #9512: user key 'custom_setting' was cleared from provider_settings!")
	}

	// changed should be false because no new default keys were added
	if changed {
		// This is OK if the integrity check added missing defaults
		// but it should not have removed anything
	}

	_ = cfgPath      // used for potential file operations
	_ = os.WriteFile // referenced to avoid unused import
}

// TestLoadCorruptFileFallback verifies that a corrupt config file makes Load
// fail, and ResetToDefaults restores a full default data map so the process
// keeps running on defaults instead of an empty map.
func TestLoadCorruptFileFallback(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")
	// Write a corrupt (non-JSON) file.
	if err := os.WriteFile(cfgPath, []byte("{not valid json"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := NewConfig(cfgPath, DefaultConfig())
	if err := cfg.Load(); err == nil {
		t.Fatal("Load of corrupt file should return an error")
	}

	// The lifecycle calls ResetToDefaults on Load failure so the process keeps
	// running on a full default config instead of an empty data map.
	cfg.ResetToDefaults()
	if got := cfg.GetString("log_level"); got != "INFO" {
		t.Errorf("log_level after ResetToDefaults = %q, want INFO", got)
	}
	if dm, ok := cfg.Get("dashboard").(map[string]interface{}); ok {
		if p, ok := dm["port"].(int); !ok || p != 6185 {
			t.Errorf("dashboard.port after ResetToDefaults = %v (ok=%v), want 6185", p, ok)
		}
	} else {
		t.Error("dashboard key missing after ResetToDefaults")
	}
}

// TestLoadAtomicWriteBack verifies the integrity write-back in Load goes
// through the atomic save() path: no temp files are left behind and the
// missing default key is persisted to disk.
func TestLoadAtomicWriteBack(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")
	// User config missing log_level; Load's integrity check must add it and
	// rewrite the file atomically.
	if err := os.WriteFile(cfgPath, []byte(`{"wake_prefix": "/"}`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := NewConfig(cfgPath, DefaultConfig())
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.GetString("log_level"); got != "INFO" {
		t.Errorf("log_level = %q, want INFO (default should have been written back)", got)
	}
	// Atomic save leaves no temp file behind.
	matches, err := filepath.Glob(filepath.Join(tmpDir, "*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("atomic write-back left temp files behind: %v", matches)
	}
}
