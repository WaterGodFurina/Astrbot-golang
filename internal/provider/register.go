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
