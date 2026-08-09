// Test-only plugin used by the SubprocessManager integration tests
// (runtime_test.go). Behavior is controlled by environment variables so the
// host process can simulate crashes without rebuilding.
package main

import (
	"os"
	"strconv"
	"time"

	sdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
)

func init() {
	// TEST_PLUGIN_CRASH_AFTER=<ms>: exit(1) after the given delay, simulating
	// an unexpected crash while the plugin is serving.
	if v := os.Getenv("TEST_PLUGIN_CRASH_AFTER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			go func() {
				time.Sleep(time.Duration(n) * time.Millisecond)
				os.Exit(1)
			}()
		}
	}
}

func main() {
	sdk.Serve(&sdk.Plugin{
		Name:        "testplugin",
		Version:     "1.0.0",
		Description: "test plugin for SubprocessManager integration tests",
		Commands: []sdk.Command{{
			Name: "test",
			Handler: func(e *sdk.Event, args []string) (string, error) {
				if v := os.Getenv("TEST_PLUGIN_REPLY"); v != "" {
					return v, nil
				}
				return "pong", nil
			},
		}},
		Filters: []sdk.Filter{{
			Name:    "block_admins",
			Handler: func(e *sdk.Event) bool { return !e.IsAdmin },
		}},
		Hooks: []sdk.Hook{{
			Name:  "startup",
			Event: "startup",
			Handler: func(e *sdk.Event) error {
				return nil
			},
		}},
	})
}
