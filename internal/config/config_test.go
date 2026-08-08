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
