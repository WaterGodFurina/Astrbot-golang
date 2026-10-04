//go:build windows

package plugin

import (
	"fmt"

	pluginNative "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/native"
)

// openNativePlugin is unsupported on Windows: the stdlib `plugin` package is
// unavailable, and `-buildmode=c-shared` exposes only a C ABI, which cannot
// carry the Go interface values the direct-call Native runtime requires. A
// Windows Native build must therefore use the gRPC runtime instead.
func openNativePlugin(path string) (func(pluginID string) (pluginNative.Plugin, error), error) {
	return nil, fmt.Errorf("Native runtime (direct in-process calls) is unsupported on Windows; use the gRPC runtime")
}
