// Package sources - auto-registration of all built-in platform adapters.
package sources

import (
	"github.com/AstrBotDevs/AstrBot/internal/platform"
	"github.com/AstrBotDevs/AstrBot/internal/platform/sources/aiocqhttp"
	"github.com/AstrBotDevs/AstrBot/internal/platform/sources/qqofficial"
	"github.com/AstrBotDevs/AstrBot/internal/platform/sources/telegram"
	"github.com/AstrBotDevs/AstrBot/internal/platform/sources/webchat"
)

// Note: This file uses init() to register all built-in platform adapters.
// However, because platform adapters require an *core.EventBus at construction time,
// the factory functions wrap the adapter creation to accept eventBus via closure.
//
// In practice, the lifecycle manager creates adapters directly using the source packages.

func init() {
	// Register platform adapter types for discovery (actual creation happens via lifecycle)
	platform.RegisterPlatform("aiocqhttp", func(config, settings map[string]interface{}) (platform.PlatformAdapter, error) {
		// EventBus is injected later by lifecycle via SetEventBus
		return aiocqhttp.New(config, settings, nil), nil
	})
	platform.RegisterPlatform("webchat", func(config, settings map[string]interface{}) (platform.PlatformAdapter, error) {
		return webchat.New(config, settings, nil), nil
	})
	platform.RegisterPlatform("telegram", func(config, settings map[string]interface{}) (platform.PlatformAdapter, error) {
		return telegram.New(config, settings, nil), nil
	})
	platform.RegisterPlatform("qq_official", func(config, settings map[string]interface{}) (platform.PlatformAdapter, error) {
		return qqofficial.New(config, settings, nil), nil
	})
}
