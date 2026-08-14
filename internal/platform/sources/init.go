// Package sources - auto-registration of all built-in platform adapters.
package sources

import (
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform/sources/aiocqhttp"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform/sources/dingtalk"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform/sources/discord"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform/sources/kook"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform/sources/lark"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform/sources/line"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform/sources/mattermost"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform/sources/misskey"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform/sources/qqofficial"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform/sources/qqofficial_webhook"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform/sources/satori"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform/sources/slack"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform/sources/telegram"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform/sources/webchat"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform/sources/wecom"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform/sources/wecom_ai_bot"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform/sources/weixin_oc"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform/sources/weixin_official_account"
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
	platform.RegisterPlatform("qq_official_webhook", func(config, settings map[string]interface{}) (platform.PlatformAdapter, error) {
		return qqofficial_webhook.New(config, settings, nil), nil
	})
	platform.RegisterPlatform("satori", func(config, settings map[string]interface{}) (platform.PlatformAdapter, error) {
		return satori.New(config, settings, nil), nil
	})
	platform.RegisterPlatform("lark", func(config, settings map[string]interface{}) (platform.PlatformAdapter, error) {
		return lark.New(config, settings, nil), nil
	})
	platform.RegisterPlatform("discord", func(config, settings map[string]interface{}) (platform.PlatformAdapter, error) {
		return discord.New(config, settings, nil), nil
	})
	platform.RegisterPlatform("mattermost", func(config, settings map[string]interface{}) (platform.PlatformAdapter, error) {
		return mattermost.New(config, settings, nil), nil
	})
	platform.RegisterPlatform("misskey", func(config, settings map[string]interface{}) (platform.PlatformAdapter, error) {
		return misskey.New(config, settings, nil), nil
	})
	platform.RegisterPlatform("line", func(config, settings map[string]interface{}) (platform.PlatformAdapter, error) {
		return line.New(config, settings, nil), nil
	})
	platform.RegisterPlatform("slack", func(config, settings map[string]interface{}) (platform.PlatformAdapter, error) {
		return slack.New(config, settings, nil), nil
	})
	platform.RegisterPlatform("wecom", func(config, settings map[string]interface{}) (platform.PlatformAdapter, error) {
		return wecom.New(config, settings, nil), nil
	})
	platform.RegisterPlatform("wecom_ai_bot", func(config, settings map[string]interface{}) (platform.PlatformAdapter, error) {
		return wecom_ai_bot.New(config, settings, nil), nil
	})
	platform.RegisterPlatform("kook", func(config, settings map[string]interface{}) (platform.PlatformAdapter, error) {
		return kook.New(config, settings, nil), nil
	})
	platform.RegisterPlatform("dingtalk", func(config, settings map[string]interface{}) (platform.PlatformAdapter, error) {
		return dingtalk.New(config, settings, nil), nil
	})
	platform.RegisterPlatform("weixin_oc", func(config, settings map[string]interface{}) (platform.PlatformAdapter, error) {
		return weixin_oc.New(config, settings, nil), nil
	})
	platform.RegisterPlatform("weixin_official_account", func(config, settings map[string]interface{}) (platform.PlatformAdapter, error) {
		return weixin_official_account.New(config, settings, nil), nil
	})
}
