//go:build windows

package plugin

import (
	"fmt"
	"syscall"
)

// openNativePlugin loads a Native plugin DLL on Windows. Go's stdlib `plugin`
// package is unsupported on Windows, so the plugin is built with
// `-buildmode=c-shared` and exposes a C ABI entry `AstrBotNativeServe`
// (C.int, zero args); the host reaches it via syscall.LoadDLL +
// GetProcAddress. C ABI is unavoidable on Windows; it is confined to this
// loader layer — the upper runtime API is identical to the Unix .so path.
//
// 宿主与插件无需自定义 build tag（见 native_loader_unix.go 说明）。
func openNativePlugin(path string) (func() int, error) {
	dll, err := syscall.LoadDLL(path)
	if err != nil {
		return nil, fmt.Errorf("LoadDLL(%s): %w", path, err)
	}
	proc, err := dll.FindProc("AstrBotNativeServe")
	if err != nil {
		return nil, fmt.Errorf("FindProc(AstrBotNativeServe) in %s: %w", path, err)
	}
	return func() int {
		r1, _, _ := proc.Call()
		return int(r1)
	}, nil
}
