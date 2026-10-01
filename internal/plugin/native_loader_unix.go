//go:build linux || darwin || freebsd

package plugin

import (
	"fmt"
	"plugin"
)

// openNativePlugin opens a Go plugin shared library (.so, built with
// `-buildmode=plugin`; the injected native_entry.go provides the
// AstrBotNativeServe symbol) and returns its AstrBotNativeServe entry.
// The host calls the returned function in a goroutine after setting up the
// Native rendezvous environment; the plugin then serves PluginService over a
// loopback gRPC listener (see sdk.NativeServe). No HashiCorp go-plugin is
// involved.
//
// 宿主与插件无需自定义 build tag：SDK 的 Native 代码总是编译（gRPC 模式
// 下为死代码），因此宿主与插件对 SDK 包的 build 配置一致，plugin.Open 可
// 直接加载。
func openNativePlugin(path string) (func() int, error) {
	p, err := plugin.Open(path)
	if err != nil {
		return nil, fmt.Errorf("plugin.Open(%s): %w", path, err)
	}
	sym, err := p.Lookup("AstrBotNativeServe")
	if err != nil {
		return nil, fmt.Errorf("Lookup(AstrBotNativeServe) in %s: %w", path, err)
	}
	fn, ok := sym.(func() int)
	if !ok {
		return nil, fmt.Errorf("AstrBotNativeServe in %s has unexpected type %T", path, sym)
	}
	return fn, nil
}
