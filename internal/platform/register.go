// Package platform - platform adapter registration system.
// Ported from astrbot/core/platform/register.py
package platform

import (
	"fmt"
	"sync"
)

// PlatformFactory creates a platform adapter from config.
type PlatformFactory func(config, settings map[string]interface{}) (PlatformAdapter, error)

// platformRegistry holds platform type → factory mappings.
var (
	platformRegistryMu sync.RWMutex
	platformRegistry   = make(map[string]PlatformFactory)
)

// RegisterPlatform registers a platform adapter factory for a type.
func RegisterPlatform(typeName string, factory PlatformFactory) {
	platformRegistryMu.Lock()
	defer platformRegistryMu.Unlock()
	platformRegistry[typeName] = factory
}

// CreatePlatform instantiates a platform adapter by type.
func CreatePlatform(typeName string, config, settings map[string]interface{}) (PlatformAdapter, error) {
	platformRegistryMu.RLock()
	factory, ok := platformRegistry[typeName]
	platformRegistryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown platform type: %s", typeName)
	}
	return factory(config, settings)
}

// RegisteredPlatformTypes returns all registered platform type names.
func RegisteredPlatformTypes() []string {
	platformRegistryMu.RLock()
	defer platformRegistryMu.RUnlock()
	types := make([]string, 0, len(platformRegistry))
	for t := range platformRegistry {
		types = append(types, t)
	}
	return types
}
