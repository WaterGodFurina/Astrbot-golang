// Native POC plugin: minimal echo in the "var plugin" form required by the
// Native runtime (the injected native_entry.go references the package-level
// `plugin` variable). Built in CI on Windows as a .dll (-buildmode=c-shared,
// C-handle bridge) and on Linux as a .so (-buildmode=plugin, direct calls).
package main

import (
	"strings"

	sdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/v2"
)

var plugin = &sdk.Plugin{
	Name:        "nativepoc",
	Version:     "1.0.0",
	Description: "Native runtime POC plugin",
	Author:      "AstrBot Devs",
	Commands: []sdk.Command{{
		Name:        "echo",
		Description: "Echoes your message",
		Usage:       "echo <text>",
		Permission:  "everyone",
		Handler: func(e *sdk.Event, args []string) (string, error) {
			return strings.Join(args, " "), nil
		},
	}},
}

func main() {
	sdk.Serve(plugin)
}
