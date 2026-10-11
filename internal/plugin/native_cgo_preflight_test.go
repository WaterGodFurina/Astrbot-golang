package plugin

import (
	"context"
	"strings"
	"testing"
)

// TestNativeCGOPreflightDisabled 验证 CGO_ENABLED=0 构建的宿主在
// loadNativeInstance 入口被前置检测拦下（不走到 plugin.Open 的
// "plugin: not implemented" 晦涩错误）。cgo 构建下该检测恒通过，此用例跳过。
func TestNativeCGOPreflightDisabled(t *testing.T) {
	if nativeHostCGOAvailable {
		t.Skip("host built with cgo: preflight rejection only applies to CGO_ENABLED=0 builds")
	}
	m := NewSubprocessManager(nil, t.TempDir())
	// libPath 指向不存在的文件也无妨：CGO 检测必须先于一切文件访问。
	_, err := m.loadNativeInstance(context.Background(), "native_test", t.TempDir()+"/nope.so")
	if err == nil {
		t.Fatal("want CGO-disabled error, got nil")
	}
	if !strings.Contains(err.Error(), "CGO_ENABLED=1") {
		t.Fatalf("error should mention CGO_ENABLED=1, got: %v", err)
	}
}

// TestNativeCGOPreflightEnabled 验证 cgo 构建下前置检测放行（不拦正常加载）。
// !cgo 构建下跳过（此时检测必然拒绝，由上面的用例覆盖）。
func TestNativeCGOPreflightEnabled(t *testing.T) {
	if !nativeHostCGOAvailable {
		t.Skip("host built without cgo: loadNativeInstance rejects all native loads")
	}
	m := NewSubprocessManager(nil, t.TempDir())
	// 检测通过后应走到文件校验（报"plugin library not found"而非 CGO 错误）。
	_, err := m.loadNativeInstance(context.Background(), "native_test", t.TempDir()+"/nope.so")
	if err == nil {
		t.Fatal("want missing-library error, got nil")
	}
	if strings.Contains(err.Error(), "CGO_ENABLED=1") {
		t.Fatalf("cgo host should pass the preflight and fail on the missing library, got: %v", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want missing-library error, got: %v", err)
	}
}
