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
		}, {
			// hosttest exercises the bidirectional HostService: plugins call
			// back into the host (ChatLLM / SetConfig / GetConfig).
			Name: "hosttest",
			Handler: func(e *sdk.Event, args []string) (string, error) {
				llm, _ := sdk.Host.ChatLLM("ping", "you are a test", nil)
				if err := sdk.Host.SetConfig("testplugin", map[string]any{"k": "v"}); err != nil {
					return "setcfg-error: " + err.Error(), nil
				}
				cfg, err := sdk.Host.GetConfig("testplugin")
				if err != nil {
					return "getcfg-error: " + err.Error(), nil
				}
				v, _ := cfg["k"].(string)
				return "llm=" + llm + " cfg=" + v, nil
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
		Tools: []sdk.Tool{{
			Name:        "echo_tool",
			Description: "returns the given text",
			ParamsSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"text": map[string]any{"type": "string"}},
			},
			Handler: func(e *sdk.Event, args map[string]any) (string, error) {
				text, _ := args["text"].(string)
				return "tool:" + text, nil
			},
		}},
		LLMRequestHooks: []sdk.LLMRequestHook{{
			Name: "inject",
			Handler: func(e *sdk.Event, req *sdk.ProviderRequest) (*sdk.ProviderRequest, error) {
				req.SystemPrompt += "\n[injected]"
				return req, nil
			},
		}},
		ResultHooks: []sdk.ResultHook{{
			Name:  "decorate",
			Event: "on_decorating_result",
			Handler: func(e *sdk.Event, chain []sdk.Component) ([]sdk.Component, error) {
				return append(chain, sdk.Text("[decorated]")), nil
			},
		}},
	})
}
