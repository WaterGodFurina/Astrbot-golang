package provider

import (
        "testing"
)

// TestIssue9573_UMOConfigOverridesGlobal verifies the fix for issue #9573:
// UMO-specific provider_settings should override global settings via merge,
// not be skipped due to `or` short-circuit.
func TestIssue9573_UMOConfigOverridesGlobal(t *testing.T) {
        // Simulate global provider_settings (always non-empty)
        globalSettings := ProviderSettings{
                MaxContextLength:     50,
                DequeueContextLength: 10,
                PromptPrefix:         "global",
        }

        // Simulate UMO-specific settings (user wants max_context_length=5 for this chat)
        umoSettings := ProviderSettings{
                MaxContextLength: 5,
        }

        // The BUGGY Python code did:
        //   cfg = config.provider_settings or plugin_context.get_config(umo=...).get("provider_settings", {})
        // Since global is non-empty, `or` short-circuits and UMO is never used.

        // The FIXED code merges:
        merged := MergeProviderSettings(globalSettings, umoSettings)

        // UMO should override global
        if merged.MaxContextLength != 5 {
                t.Errorf(
                        "BUG #9573: UMO max_context_length not applied!\n"+
                                "  expected: 5 (from UMO config)\n"+
                                "  got:      %d (global was used due to `or` short-circuit)",
                        merged.MaxContextLength,
                )
        }

        // Non-overridden keys should retain global values
        if merged.DequeueContextLength != 10 {
                t.Errorf("global dequeue_context_length not preserved: %d", merged.DequeueContextLength)
        }
        if merged.PromptPrefix != "global" {
                t.Errorf("global prompt_prefix not preserved: %s", merged.PromptPrefix)
        }
}

// TestMergeProviderSettings_EmptyUMO verifies that empty UMO settings
// don't wipe global settings.
func TestMergeProviderSettings_EmptyUMO(t *testing.T) {
        global := ProviderSettings{MaxContextLength: 50}
        umo := ProviderSettings{}

        merged := MergeProviderSettings(global, umo)

        if merged.MaxContextLength != 50 {
                t.Error("empty UMO should not wipe global settings")
        }
}

// TestMergeProviderSettings_NilGlobal verifies that zero global is handled.
func TestMergeProviderSettings_NilGlobal(t *testing.T) {
        umo := ProviderSettings{MaxContextLength: 5}

        merged := MergeProviderSettings(ProviderSettings{}, umo)

        if merged.MaxContextLength != 5 {
                t.Error("UMO should be used when global is zero")
        }
}

// TestMergeProviderSettings_BothNil verifies no panic.
func TestMergeProviderSettings_BothNil(t *testing.T) {
        merged := MergeProviderSettings(ProviderSettings{}, ProviderSettings{})
        _ = merged // should not panic
}
