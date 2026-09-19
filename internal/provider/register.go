// Package provider - provider registration system.
// Ported from astrbot/core/provider/register.py
package provider

import (
	"fmt"
	"sync"
)

// ProviderFactory creates a provider from config. Factories may return any
// AbstractProvider (chat, STT, TTS, embedding or rerank); the concrete
// capability is determined by the interfaces the instance implements.
type ProviderFactory func(config, settings map[string]interface{}) (AbstractProvider, error)

// providerRegistry holds provider type → factory mappings.
var (
	providerRegistryMu sync.RWMutex
	providerRegistry   = make(map[string]ProviderFactory)
)

// providerTypeAliases 将 Python 模板/用户配置中的 provider type 全名
// 归一为 Go sources 注册的短名。Python 在 register_provider_adapter 中
// 以 "<厂商>_chat_completion" 全名注册，WebUI 模板（dashboard 模板与
// py default.py 保持一致）及从 py 迁移的用户 config 持久化的 type 均
// 为全名；Go 侧 sources 以短名（openai/anthropic/gemini 等）注册。
// 仅在原名直查未命中时才走此映射，openai_responses、
// kimi_code_chat_completion 等原名注册不受影响。
var providerTypeAliases = map[string]string{
	"openai_chat_completion":      "openai",
	"anthropic_chat_completion":   "anthropic",
	"googlegenai_chat_completion": "gemini",
	"gemini_chat_completion":      "gemini",
	"ollama_chat_completion":      "ollama",
	"dashscope_chat_completion":   "dashscope",
	"openrouter_chat_completion":  "openrouter",
}

// RegisterProvider registers a provider factory for a type.
func RegisterProvider(typeName string, factory ProviderFactory) {
	providerRegistryMu.Lock()
	defer providerRegistryMu.Unlock()
	providerRegistry[typeName] = factory
}

// CreateProvider instantiates a provider by type.
func CreateProvider(typeName string, config, settings map[string]interface{}) (AbstractProvider, error) {
	providerRegistryMu.RLock()
	factory, ok := providerRegistry[typeName]
	if !ok {
		// 别名归一：对齐 Python 模板的 type 命名。py 在 register_provider_adapter
		// 中以 "<厂商>_chat_completion" 全名注册，WebUI 模板（与 py default.py
		// 保持一致）及从 py 迁移的用户 config 持久化的 type 均为全名，而 Go
		// sources 以短名注册。原名直查优先，未命中再走别名映射，
		// openai_responses / kimi_code_chat_completion 等原名注册不受影响。
		if aliased, exist := providerTypeAliases[typeName]; exist {
			factory, ok = providerRegistry[aliased]
		}
	}
	providerRegistryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown provider type: %s", typeName)
	}
	return factory(config, settings)
}

// RegisteredProviderTypes returns all registered provider type names.
func RegisteredProviderTypes() []string {
	providerRegistryMu.RLock()
	defer providerRegistryMu.RUnlock()
	types := make([]string, 0, len(providerRegistry))
	for t := range providerRegistry {
		types = append(types, t)
	}
	return types
}
