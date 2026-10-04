package plugin

import (
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
