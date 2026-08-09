// Example AstrBot Go plugin: "echo" (subprocess runtime).
//
// Build with the bundled/system Go toolchain (this module is independent of
// the AstrBot host):
//
//	go build -o data/plugins/echo-<GOOS>-<GOARCH> .
//
// The host launches the resulting binary as a child process and talks to it
// over gRPC (go-plugin). This plugin registers an "echo" command that echoes
// the user's message back.
package main

import (
	"strings"

	sdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
)

func main() {
	sdk.Serve(&sdk.Plugin{
		Name:        "echo",
		Version:     "2.0.0",
		Description: "Echoes your message back",
		Author:      "AstrBot Devs",
		ConfigSchema: map[string]any{
			"description": "Echo 插件配置",
			"type":        "object",
			"items": map[string]any{
				"prefix": map[string]any{
					"description": "回复前缀",
					"type":        "string",
					"default":     "[Echo] ",
				},
				"upper": map[string]any{
					"description": "转大写",
					"type":        "bool",
					"default":     false,
				},
			},
		},
		Commands: []sdk.Command{
			{
				Name:        "echo",
				Aliases:     []string{"repeat"},
				Description: "Echoes your message",
				Usage:       "echo <text>",
				Permission:  "everyone",
				Handler: func(e *sdk.Event, args []string) (string, error) {
					if len(args) == 0 {
						return "Usage: echo <text>", nil
					}
					return strings.Join(args, " "), nil
				},
			},
		},
		Hooks: []sdk.Hook{
			{
				Name:  "echo_on_start",
				Event: "startup",
				Handler: func(e *sdk.Event) error {
					return nil
				},
			},
		},
	})
}
