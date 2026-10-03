package plugin

import (
	"runtime"
	"strings"
	"testing"
)

func TestHostLibcOverride(t *testing.T) {
	t.Setenv("ASTRBOT_LIBC", "musl")
	if got := hostLibc(); got != "musl" {
		t.Fatalf("ASTRBOT_LIBC=musl -> %q, want musl", got)
	}
	t.Setenv("ASTRBOT_LIBC", "glibc")
	if got := hostLibc(); got != "gnu" {
		t.Fatalf("ASTRBOT_LIBC=glibc -> %q, want gnu", got)
	}
	t.Setenv("ASTRBOT_LIBC", "gnu")
	if got := hostLibc(); got != "gnu" {
		t.Fatalf("ASTRBOT_LIBC=gnu -> %q, want gnu", got)
	}
}

func TestZigTargetTripleLibc(t *testing.T) {
	if runtime.GOOS != "linux" {
		// libc 只参与 Linux 目标三元组；其他平台（darwin/windows）三元组
		// 不含 libc 段，ASTRBOT_LIBC 不应泄漏进去。
		t.Setenv("ASTRBOT_LIBC", "musl")
		if tr := zigTargetTriple(); strings.Contains(tr, "musl") {
			t.Fatalf("non-linux triple %q must not contain musl", tr)
		}
		return
	}
	t.Setenv("ASTRBOT_LIBC", "musl")
	tr := zigTargetTriple()
	if tr != "" && !strings.Contains(tr, "-musl") {
		t.Fatalf("musl host triple = %q, want -musl suffix", tr)
	}
	t.Setenv("ASTRBOT_LIBC", "gnu")
	tr = zigTargetTriple()
	if tr != "" && !strings.Contains(tr, "-gnu") {
		t.Fatalf("gnu host triple = %q, want -gnu suffix", tr)
	}
}
