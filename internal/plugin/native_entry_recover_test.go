package plugin

import (
	"context"
	"strings"
	"testing"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/v2"
)

// OnLoad panic 的 Native 测试插件源码：宿主 openNativePlugin 的 recover 包装
// 必须把 entry 内（native.Serve → OnLoad）的 panic 转为错误返回，而不是击落
// 宿主进程。
const nativeTestSrcPanicOnLoad = `package main

import (
	"errors"

	sdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/v2"
	native "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/v2/native"
)

var plugin = &sdk.Plugin{
	Name:    "native_panic_test",
	Version: "1.0.0",
	OnLoad: func() error {
		panic("boom in OnLoad")
	},
}

func AstrBotNativePlugin(pluginID string) (native.Plugin, error) {
	return native.Serve(plugin, pluginID)
}

func main() {}
`

// 包级 init panic 的变体：panic 发生在 plugin.Open 的初始化阶段，同样应被
// recover 包装拦截。
const nativeTestSrcPanicInit = `package main

import (
	sdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/v2"
	native "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/v2/native"
)

func init() {
	panic("boom in init")
}

var plugin = &sdk.Plugin{Name: "native_panic_init_test", Version: "1.0.0"}

func AstrBotNativePlugin(pluginID string) (native.Plugin, error) {
	return native.Serve(plugin, pluginID)
}

func main() {}
`

// TestNativeEntryOnLoadPanicRecovered 注入 OnLoad 会 panic 的假插件：宿主
// openNativePlugin 的 recover 必须转为错误返回且宿主存活。
//
// -race 与 cgo 说明：沿 buildTestNativeLib 既有约束（.so 必须与宿主测试
// 二进制同 -race 构建、同 cgo 开关），不满足时由其内部 t.Skip 处理；cgo
// 段错误（SIGSEGV）不可 recover，不在本用例覆盖范围（如实标注的固有边界）。
func TestNativeEntryOnLoadPanicRecovered(t *testing.T) {
	so := buildTestNativeLib(t, nativeTestSrcPanicOnLoad)
	client, cleanup, err := openNativePlugin(so, "native_panic_test")
	if err == nil {
		_ = cleanup()
		if client != nil {
			_ = client.Close()
		}
		t.Fatal("panicking OnLoad must surface as an error, got nil")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Fatalf("error should mention panic, got: %v", err)
	}
	if client != nil || cleanup != nil {
		t.Fatal("client/cleanup must be nil after a recovered entry panic")
	}
	// 宿主进程存活（能继续发起 gRPC 路径的实例操作）即为本用例的核心断言：
	// 走一遍普通管理器操作确认运行时未被击落。
	m := NewSubprocessManager(nil, t.TempDir())
	if got := m.Get("native_panic_test"); got != nil {
		t.Fatal("failed load must not register an instance")
	}
}

// TestNativeEntryInitPanicRecovered 覆盖 plugin.Open 初始化阶段（包 init）
// 的 panic：同样必须 recover 转错误。
func TestNativeEntryInitPanicRecovered(t *testing.T) {
	so := buildTestNativeLib(t, nativeTestSrcPanicInit)
	_, _, err := openNativePlugin(so, "native_panic_init_test")
	if err == nil {
		t.Fatal("panicking init must surface as an error, got nil")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Fatalf("error should mention panic, got: %v", err)
	}
}

// TestNativeLoadViaManagerRejectsPanickingPlugin 端到端验证：OnLoad panic 的
// 插件经 loadLocked → loadNativeInstance → openNativePlugin 被拒绝加载，
// 错误上抛且宿主运行时存活。
func TestNativeLoadViaManagerRejectsPanickingPlugin(t *testing.T) {
	so := buildTestNativeLib(t, nativeTestSrcPanicOnLoad)
	m := NewSubprocessManager(nil, t.TempDir())
	_, err := m.loadLocked(context.Background(), "native_panic_test", so, "go", "native", false)
	if err == nil {
		t.Fatal("panicking plugin must fail to load")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Fatalf("error should mention panic, got: %v", err)
	}
	if got := m.Get("native_panic_test"); got != nil {
		t.Fatal("failed load must not register an instance")
	}
}

// 编译期接口守卫：确认 recover 包装没有改变 openNativePlugin 的签名契约。
var _ func(path, pluginID string) (pluginsdk.PluginClient, func() error, error) = openNativePlugin
