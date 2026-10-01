// Native POC plugin: minimal echo in the "var plugin" form required by the
// Native runtime (the injected native_entry.go references the package-level
// `plugin` variable). Built in CI as a Windows .dll (-buildmode=c-shared) to
// verify the Native entry symbol and C ABI layer.
package main

import (
	"strings"

	sdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
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
