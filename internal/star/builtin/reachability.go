package builtin

import (
	"context"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// testProviderReachability performs a minimal chat request against a provider
// config to verify it is reachable (mirrors builtin_commands/commands/provider.py
// _test_provider_capability). Returns "" when reachable, else an error code.
func testProviderReachability(all map[string]interface{}, pc map[string]interface{}) string {
	providerType, _ := pc["type"].(string)
	if providerType == "" {
		providerType, _ = pc["provider"].(string)
	}
	if providerType == "" {
		return "NO_TYPE"
	}
	merged := mergeSource(all, pc)
	providerSettings, _ := all["provider_settings"].(map[string]interface{})
	inst, err := provider.CreateProvider(providerType, merged, providerSettings)
	if err != nil {
		return "INIT_FAIL"
	}
	chatInst, ok := inst.(provider.ChatProvider)
	if !ok {
		return "NOT_CHAT"
	}
	req := &provider.ProviderRequest{
		Prompt:    "ping",
		SessionID: "reachability_check",
		Contexts:  []map[string]interface{}{},
		ImageURLs: []string{},
		AudioURLs: []string{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := chatInst.TextChat(ctx, req)
	if err != nil {
		return "TEST_FAILED"
	}
	if resp.Role == "err" {
		return "TEST_FAILED"
	}
	return ""
}

// mergeSource merges the provider_source config (api_base/key) into the
// provider config (same semantics as pipeline.mergeProviderSource).
func mergeSource(all map[string]interface{}, pc map[string]interface{}) map[string]interface{} {
	sourceID, _ := pc["provider_source_id"].(string)
	if sourceID == "" {
		return pc
	}
	sources, _ := all["provider_sources"].([]interface{})
	var source map[string]interface{}
	for _, s := range sources {
		if sm, ok := s.(map[string]interface{}); ok {
			if id, _ := sm["id"].(string); id == sourceID {
				source = sm
				break
			}
		}
	}
	if source == nil {
		return pc
	}
	merged := map[string]interface{}{}
	for k, v := range source {
		merged[k] = v
	}
	for k, v := range pc {
		merged[k] = v
	}
	return merged
}

// checkReachability returns a map of provider id -> error code (or "" when
// reachable). Runs checks in parallel with a per-provider 15s timeout.
func checkReachability(all map[string]interface{}) map[string]string {
	providers, _ := all["provider"].([]interface{})
	results := make(map[string]string)
	resultsMu := sync.Mutex{}
	var wg sync.WaitGroup
	for _, p := range providers {
		pc, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		id, _ := pc["id"].(string)
		if id == "" {
			continue
		}
		wg.Add(1)
		go func(pc map[string]interface{}, id string) {
			defer wg.Done()
			code := testProviderReachability(all, pc)
			resultsMu.Lock()
			results[id] = code
			resultsMu.Unlock()
		}(pc, id)
	}
	wg.Wait()
	return results
}
