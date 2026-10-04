//go:build linux || darwin || freebsd

package plugin

import (
	"fmt"
	"plugin"

	pluginNative "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/native"
)

// openNativePlugin opens a Go plugin shared library (.so, built with
// `-buildmode=plugin`) and returns its injected AstrBotNativePlugin entry.
//
// The entry runs OnLoad, binds the in-process host service and returns the
// plugin's PluginService for DIRECT in-process calls — no gRPC, no subprocess.
// The host calls it once with the manifest plugin id and wraps the result in
// native.NativeClient.
func openNativePlugin(path string) (func(pluginID string) (pluginNative.Plugin, error), error) {
	p, err := plugin.Open(path)
	if err != nil {
		return nil, fmt.Errorf("plugin.Open(%s): %w", path, err)
	}
	sym, err := p.Lookup("AstrBotNativePlugin")
	if err != nil {
		return nil, fmt.Errorf("Lookup(AstrBotNativePlugin) in %s: %w", path, err)
	}
	fn, ok := sym.(func(string) (pluginNative.Plugin, error))
	if !ok {
		return nil, fmt.Errorf("AstrBotNativePlugin in %s has unexpected type %T", path, sym)
	}
	return fn, nil
}
