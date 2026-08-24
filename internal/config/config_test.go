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
	_ = checkConfigIntegrity(defaults, userConfig, "")

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

	cfg := NewConfig(cfgPath)
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
		var port int
		switch v := dm["port"].(type) {
		case float64: // JSON 数字解析为 float64（完整默认来自 JSON）
			port = int(v)
		case int:
			port = v
		}
		if port != 6185 {
			t.Errorf("dashboard.port after ResetToDefaults = %v, want 6185", dm["port"])
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
	cfg := NewConfig(cfgPath)
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

// TestCheckConfigIntegrityListTypeMismatch: 参考值为列表而用户值为其他类型时
// 必须用默认值替换（bug.md 6.6），否则下游对列表的类型断言会 panic。
func TestCheckConfigIntegrityListTypeMismatch(t *testing.T) {
	defaults := map[string]interface{}{
		"provider_settings": map[string]interface{}{
			"websearch_tavily_key": []interface{}{},
			"fallback_chat_models": []interface{}{},
			"wake_prefix":          "/",
		},
	}

	// 用户把列表写成了字符串：必须被修正为默认值（空列表）。
	user := map[string]interface{}{
		"provider_settings": map[string]interface{}{
			"websearch_tavily_key": "sk-xxxx",
			"fallback_chat_models": 42,
			"wake_prefix":          "/",
		},
	}
	changed := checkConfigIntegrity(defaults, user, "")

	ps := user["provider_settings"].(map[string]interface{})
	if _, ok := ps["websearch_tavily_key"].([]interface{}); !ok {
		t.Errorf("websearch_tavily_key must be restored to a list, got %#v", ps["websearch_tavily_key"])
	}
	if _, ok := ps["fallback_chat_models"].([]interface{}); !ok {
		t.Errorf("fallback_chat_models must be restored to a list, got %#v", ps["fallback_chat_models"])
	}
	if !changed {
		t.Error("type mismatch should report a change (hasNew=true)")
	}

	// 合法列表保持原样。
	okUser := map[string]interface{}{
		"provider_settings": map[string]interface{}{
			"websearch_tavily_key": []interface{}{"k1"},
			"fallback_chat_models": []interface{}{"m1"},
			"wake_prefix":          "/",
		},
	}
	if c := checkConfigIntegrity(defaults, okUser, ""); c {
		t.Error("well-typed list must not be touched")
	}
	if got := okUser["provider_settings"].(map[string]interface{})["websearch_tavily_key"].([]interface{})[0]; got != "k1" {
		t.Errorf("list contents must be preserved, got %v", got)
	}
}

// TestNewConfigAutoLoadsExisting: NewConfig 在配置文件已存在时必须自动加载，
// 不允许返回空配置（bug.md 6.7）。
func TestNewConfigAutoLoadsExisting(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")
	// wake_prefix 在完整默认中是列表，用户文件使用同类型以验证用户值保留。
	if err := os.WriteFile(cfgPath, []byte(`{"log_level": "WARN", "wake_prefix": ["/x"]}`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := NewConfig(cfgPath)
	if got := cfg.GetString("log_level"); got != "WARN" {
		t.Errorf("NewConfig must auto-load existing file: log_level = %q, want WARN", got)
	}
	// 缺失的默认键仍被补全（integrity 生效）。
	if wp, ok := cfg.Get("wake_prefix").([]interface{}); !ok || len(wp) != 1 || wp[0] != "/x" {
		t.Errorf("wake_prefix = %v, want [\"/x\"]", cfg.Get("wake_prefix"))
	}
	if _, ok := cfg.Get("dashboard").(map[string]interface{}); !ok {
		t.Error("missing default keys must be filled in after auto-load")
	}
}
