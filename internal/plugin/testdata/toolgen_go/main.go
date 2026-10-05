// AstrBot Go plugin with an LLM function tool, for verifying tool calling on
// the Native (.so) runtime. The model calls `echo_tool`; the handler runs
// inside the plugin and returns the echoed text.
package main

import (
	"strings"

	sdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
)

var plugin = &sdk.Plugin{
	Name:        "toolgen",
	Version:     "1.0.0",
	Description: "LLM tool test plugin",
	Author:      "AstrBot Devs",
	Commands: []sdk.Command{{
		Name:        "toolcmd",
		Description: "prints ok",
		Permission:  "everyone",
		Handler: func(e *sdk.Event, args []string) (string, error) {
			return "toolcmd-ok", nil
		},
	}},
	Tools: []sdk.Tool{{
		Name:        "echo_tool",
		Description: "Echo the given text back. Call this with the user's text.",
		ParamsSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{"type": "string", "description": "text to echo"},
			},
			"required": []string{"text"},
		},
		Handler: func(e *sdk.Event, args map[string]any) (string, error) {
			text, _ := args["text"].(string)
			if text == "" {
				return "TOOL_EMPTY", nil
			}
			return "TOOL_ECHO:" + strings.TrimSpace(text), nil
		},
	}},
}

func main() {
	sdk.Serve(plugin)
}
