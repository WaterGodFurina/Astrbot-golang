// Example AstrBot Go plugin: "echo"
//
// Build with:
//   go build -buildmode=plugin -o examples/echo_plugin/echo.so examples/echo_plugin/echo.go
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
		Handler: func(ctx context.Context, args []string) (string, error) {
			if len(args) == 0 {
				return "Usage: echo <text>", nil
			}
			return strings.Join(args, " "), nil
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
