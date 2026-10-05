//go:build linux || darwin || freebsd

package plugin

import (
	"fmt"
	"plugin"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
	pluginNative "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/native"
)

// openNativePlugin opens a Go plugin shared library (.so, built with
// `-buildmode=plugin`), looks up its injected AstrBotNativePlugin entry, calls
// it with the manifest plugin id and returns a ready PluginClient.
//
// Unix is the zero-serialization path: the entry runs OnLoad, binds the
// in-process host service and returns the plugin's PluginService, which
// native.NativeClient invokes with Go object pointers directly — no gRPC, no
// protobuf marshalling, no subprocess.
//
// The returned cleanup releases nothing for Unix (the .so cannot be unloaded);
// it exists so both platforms share the loader contract.
func openNativePlugin(path, pluginID string) (pluginsdk.PluginClient, func() error, error) {
	p, err := plugin.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("plugin.Open(%s): %w", path, err)
	}
	sym, err := p.Lookup("AstrBotNativePlugin")
	if err != nil {
		return nil, nil, fmt.Errorf("Lookup(AstrBotNativePlugin) in %s: %w", path, err)
	}
	fn, ok := sym.(func(string) (pluginNative.Plugin, error))
	if !ok {
		return nil, nil, fmt.Errorf("AstrBotNativePlugin in %s has unexpected type %T", path, sym)
	}
	np, err := fn(pluginID)
	if err != nil {
		return nil, nil, fmt.Errorf("native entry %s: %w", path, err)
	}
	client := pluginNative.NewClient(pluginID, np)
	return client, client.Close, nil
}
