package toolchain

import (
	"runtime"
	"strings"
	"testing"
)

// TestHostGoVersion 验证 bundled 工具链默认版本取自宿主 Go 版本：Go 的 plugin
// 机制要求 Native 插件与宿主用完全相同的 Go 版本构建，故 New() 的默认版本
// 必须等于 runtime.Version()（去掉 "go" 前缀）。
func TestHostGoVersion(t *testing.T) {
	got := hostGoVersion()
	rv := runtime.Version()
	if strings.HasPrefix(rv, "go1.") {
		if got != strings.TrimPrefix(rv, "go") {
			t.Errorf("hostGoVersion() = %q, want %q (runtime.Version=%q)", got, strings.TrimPrefix(rv, "go"), rv)
		}
	} else if got != "" {
		t.Errorf("devel 版本应返回空串, got %q (runtime.Version=%q)", got, rv)
	}
}
