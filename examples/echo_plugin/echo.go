// Example AstrBot Go plugin: "echo"
//
// Build with:
//
//	go build -buildmode=plugin -o data/plugins/echo.so examples/echo_plugin/echo.go
//
// This plugin registers an "echo" command that echoes the user's message back.
package main

import (
	"context"
	"strings"

	"github.com/AstrBotDevs/AstrBot/internal/plugin"
)

func PluginName() string        { return "echo" }
func PluginVersion() string     { return "1.0.0" }
func PluginDescription() string { return "Echoes your message back" }

// GetConfigSchema exports the plugin's config schema (optional symbol).
// The dashboard renders a config dialog from this and saves to
// data/plugins/<name>/config.json.
func GetConfigSchema() map[string]interface{} {
	return map[string]interface{}{
		"description": "Echo 插件配置",
		"type":        "object",
		"items": map[string]interface{}{
			"prefix": map[string]interface{}{
				"description": "回复前缀",
				"type":        "string",
				"default":     "[Echo] ",
			},
			"upper": map[string]interface{}{
				"description": "转大写",
				"type":        "bool",
				"default":     false,
			},
		},
	}
}

func Init(ctx *plugin.Context) error {
	ctx.Logger.Info("Echo plugin v1.0.0 loaded")
	return nil
}

func RegisterHandlers(reg *plugin.HandlerRegistry) {
	reg.RegisterCommand(plugin.CommandHandler{
		Name:        "echo",
		Aliases:     []string{"repeat"},
		Description: "Echoes your message",
		Usage:       "echo <text>",
		Permission:  "everyone",
		HandlerEx: func(pc *plugin.Context, ctx context.Context, args []string) (string, error) {
			if len(args) == 0 {
				return "Usage: echo <text>", nil
			}
			text := strings.Join(args, " ")
			cfg := pc.GetConfig("echo")
			if p, ok := cfg["prefix"].(string); ok && p != "" {
				text = p + text
			}
			if upper, ok := cfg["upper"].(bool); ok && upper {
				text = strings.ToUpper(text)
			}
			return text, nil
		},
	})

	reg.RegisterHook(plugin.HookHandler{
		Name:  "echo_on_start",
		Event: "startup",
		Handler: func(ctx context.Context) error {
			// This runs when AstrBot starts
			return nil
		},
	})
}

func Cleanup() error { return nil }
