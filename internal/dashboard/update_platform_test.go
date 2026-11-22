package dashboard

import "testing"

func TestResourceForPlatformLibc(t *testing.T) {
	cases := []struct {
		goos, goarch, libc, want string
	}{
		{"linux", "amd64", "gnu", "linux-x86_64-gnu"},
		{"linux", "amd64", "musl", "linux-x86_64-musl"},
		{"linux", "arm64", "gnu", "linux-aarch64-gnu"},
		{"linux", "arm64", "musl", "linux-aarch64-musl"},
		{"linux", "loong64", "gnu", "linux-loongarch64-gnu"},
		{"linux", "loong64", "musl", "linux-loongarch64-musl"},
		{"windows", "amd64", "", "windows-x86_64"},
		{"windows", "arm64", "", "windows-aarch64"},
		{"darwin", "amd64", "", "macos-x86_64"},
		{"darwin", "arm64", "", "macos-aarch64"},
		{"android", "arm64", "", "android-aarch64"},
		{"freebsd", "amd64", "", ""},
	}
	for _, c := range cases {
		if got := resourceForPlatformLibc(c.goos, c.goarch, c.libc); got != c.want {
			t.Errorf("resourceForPlatformLibc(%q, %q, %q) = %q, want %q",
				c.goos, c.goarch, c.libc, got, c.want)
		}
	}
}

// TestResourceForPlatformHonorsHostLibc 验证 resourceForPlatform 会通过
// plugin.HostLibc() 读取宿主的 libc（ASTRBOT_LIBC 覆盖即运行时探测入口），
// 从而在 musl 宿主上选择 -musl 资产。
func TestResourceForPlatformHonorsHostLibc(t *testing.T) {
	t.Setenv("ASTRBOT_LIBC", "musl")
	if got := resourceForPlatform("linux", "amd64"); got != "linux-x86_64-musl" {
		t.Fatalf("musl host must pick linux-x86_64-musl, got %q", got)
	}
	t.Setenv("ASTRBOT_LIBC", "glibc")
	if got := resourceForPlatform("linux", "arm64"); got != "linux-aarch64-gnu" {
		t.Fatalf("glibc host must pick linux-aarch64-gnu, got %q", got)
	}
	// 非 Linux 平台不受 ASTRBOT_LIBC 影响。
	t.Setenv("ASTRBOT_LIBC", "musl")
	if got := resourceForPlatform("windows", "amd64"); got != "windows-x86_64" {
		t.Fatalf("non-linux must ignore libc, got %q", got)
	}
}
