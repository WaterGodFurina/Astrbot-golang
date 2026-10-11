//go:build linux || darwin || freebsd

package plugin

import (
	"fmt"
	"plugin"
	"runtime/debug"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/v2"
	pluginNative "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/v2/native"
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
//
// 崩溃隔离：entry 内部会执行插件的 init()（plugin.Open 时）与 OnLoad（任意
// 第三方代码），panic 时在此 recover 并转为错误返回，避免击落宿主进程。
// 注意：cgo 层的段错误（SIGSEGV 走 signal 机制）无法被 recover 捕获，那是
// Native 运行方式固有的隔离边界，只能靠重启宿主恢复。
func openNativePlugin(path, pluginID string) (client pluginsdk.PluginClient, cleanup func() error, err error) {
	defer func() {
		// panic → error：错误信息带插件路径与堆栈，便于定位是哪个插件的
		// OnLoad/init 出问题。
		if r := recover(); r != nil {
			client, cleanup, err = nil, nil, fmt.Errorf("native plugin %s panic: %v\n%s", path, r, debug.Stack())
		}
	}()
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
	c := pluginNative.NewClient(pluginID, np)
	return c, c.Close, nil
}
