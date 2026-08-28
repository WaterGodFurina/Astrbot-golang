package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// ChatLLMFromConfig calls the default chat LLM provider selected in the given
// host config (provider_settings.default_provider_id, else first enabled
// provider) with the given prompt + system prompt, and returns the reply text.
// Used to back sdk.Host.ChatLLM (plugins that need to call the LLM directly).
// imageURLs/audioURLs (may be nil) are appended as multimodal content parts;
// tools/contexts (may be empty) are forwarded to the provider request;
// providerID (may be empty) pins a specific provider by id.
func ChatLLMFromConfig(cfg map[string]interface{}, prompt, systemPrompt string, imageURLs, audioURLs []string, tools, contexts []map[string]interface{}, providerID string) (string, error) {
	providerCfg, providerSettings, err := resolveProviderFromConfig(cfg, providerID)
	if err != nil {
		return "", err
	}
	providerType, _ := providerCfg["type"].(string)
	if providerType == "" {
		providerType, _ = providerCfg["provider"].(string)
	}
	if providerType == "" {
		return "", fmt.Errorf("模型提供商配置缺少 type 字段")
	}
	mergedCfg := mergeProviderSource(providerCfg, cfg["provider_sources"])
	inst, err := provider.CreateProvider(providerType, mergedCfg, providerSettings)
	if err != nil {
		return "", fmt.Errorf("初始化模型提供商失败: %w", err)
	}
	chatInst, ok := inst.(provider.ChatProvider)
	if !ok {
		return "", fmt.Errorf("提供商 %s 不支持聊天能力", providerType)
	}
	req := &provider.ProviderRequest{
		Prompt:       prompt,
		SystemPrompt: systemPrompt,
		ImageURLs:    imageURLs,
		AudioURLs:    audioURLs,
		Tools:        tools,
		Contexts:     contexts,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	resp, err := chatInst.TextChat(ctx, req)
	if err != nil {
		return "", err
	}
	if resp.Role == "err" {
		return "", fmt.Errorf("%s", resp.CompletionText)
	}
	return resp.CompletionText, nil
}

// resolveProviderFromConfig picks the chat provider config from the host config
// (explicit provider id first, then default provider id, then the first enabled
// provider). Extracted from ProcessStage.resolveProvider so plugins' ChatLLM
// calls share the same logic. explicitID, when non-empty, pins the provider by
// id and errors if not found.
func resolveProviderFromConfig(config map[string]interface{}, explicitID string) (map[string]interface{}, map[string]interface{}, error) {
	providers, _ := config["provider"].([]interface{})
	providerSettings, _ := config["provider_settings"].(map[string]interface{})
	if providerSettings == nil {
		providerSettings = map[string]interface{}{}
	}
	if explicitID != "" {
		for _, p := range providers {
			pc, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			if id, _ := pc["id"].(string); id == explicitID {
				return pc, providerSettings, nil
			}
		}
		return nil, nil, fmt.Errorf("未找到指定的模型提供商 %s，请检查配置", explicitID)
	}
	selected, _ := providerSettings["default_provider_id"].(string)
	if selected != "" {
		for _, p := range providers {
			pc, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			if id, _ := pc["id"].(string); id == selected {
				return pc, providerSettings, nil
			}
		}
	}
	for _, p := range providers {
		pc, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		if enable, _ := pc["enable"].(bool); enable {
			return pc, providerSettings, nil
		}
	}
	return nil, nil, errNoAvailableProvider
}
