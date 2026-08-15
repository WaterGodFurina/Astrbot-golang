package lifecycle

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestIsOrphanPluginCmdline(t *testing.T) {
	prefix := filepath.Join("/opt/astrbot/data", "plugins-bin") + string(filepath.Separator)

	// A real plugin child: argv[0] points under the plugins-bin directory.
	cmdline := "/opt/astrbot/data/plugins-bin/abc/astrbot_plugin\x00"
	if !isOrphanPluginCmdline([]byte(cmdline), prefix) {
		t.Errorf("plugin child should be recognized as orphan: %q", cmdline)
	}

	// A process that merely mentions plugins-bin somewhere in its args, but
	// whose executable is elsewhere, must NOT be killed (the bug this guards).
	cmdline = "/usr/bin/manager\x00--dir=/opt/astrbot/data/plugins-bin\x00"
	if isOrphanPluginCmdline([]byte(cmdline), prefix) {
		t.Errorf("non-plugin process mentioning plugins-bin must not match: %q", cmdline)
	}

	// An unrelated executable must not match.
	cmdline = "/usr/bin/bash\x00-c\x00plugins-bin\x00"
	if isOrphanPluginCmdline([]byte(cmdline), prefix) {
		t.Errorf("unrelated process must not match: %q", cmdline)
	}

	// A sibling directory is not a match (prefix boundary respected).
	cmdline = "/opt/astrbot/data/plugins-binx/def/tool\x00"
	if isOrphanPluginCmdline([]byte(cmdline), prefix) {
		t.Errorf("plugins-binx must not match plugins-bin prefix: %q", cmdline)
	}

	// Empty prefix never matches.
	if isOrphanPluginCmdline([]byte(cmdline), "") {
		t.Errorf("empty prefix must never match")
	}
}

func TestPluginBinaryPrefix(t *testing.T) {
	prefix := pluginBinaryPrefix()
	if prefix == "" {
		t.Fatal("pluginBinaryPrefix must resolve to a non-empty prefix")
	}
	abs, err := filepath.Abs("data")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(abs, "plugins-bin") + string(filepath.Separator)
	if prefix != want {
		t.Errorf("pluginBinaryPrefix() = %q, want %q", prefix, want)
	}
}

// resetDockerCache clears the dockerAvailable cache so a test observes fresh
// state.
func resetDockerCache() {
	dockerCache.mu.Lock()
	defer dockerCache.mu.Unlock()
	dockerCache.checked = time.Time{}
	dockerCache.ok = false
}

// TestDockerAvailableCacheExpiry verifies dockerAvailable re-checks the binary
// after dockerCheckTTL expires, instead of caching the first result forever
// (bug 6.1: a Docker installed after the first call must be detected).
func TestDockerAvailableCacheExpiry(t *testing.T) {
	oldTTL := dockerCheckTTL
	dockerCheckTTL = 50 * time.Millisecond
	defer func() {
		dockerCheckTTL = oldTTL
		resetDockerCache()
	}()

	dir := t.TempDir()
	fakeDocker := filepath.Join(dir, "docker")
	write := func() {
		if err := os.WriteFile(fakeDocker, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write()
	// ASTRBOT_DOCKER_BIN 指向 fake 二进制；PATH 不含 docker，结果只由它决定。
	t.Setenv("ASTRBOT_DOCKER_BIN", fakeDocker)
	t.Setenv("PATH", filepath.Join(dir, "empty"))

	if !dockerAvailable() {
		t.Fatal("docker 二进制存在时 dockerAvailable() 应为 true")
	}
	// 缓存期内删除二进制：仍应返回缓存的 true（未过期，不重新检查）。
	if err := os.Remove(fakeDocker); err != nil {
		t.Fatal(err)
	}
	if !dockerAvailable() {
		t.Fatal("TTL 内应返回缓存的 true，而不是重新检查")
	}
	// 缓存过期后重新检查：二进制已删除 → false。
	time.Sleep(dockerCheckTTL * 2)
	if dockerAvailable() {
		t.Fatal("缓存过期后应重新检查；二进制已删除应返回 false")
	}
	// 恢复二进制并再次等待过期：应检测到 docker（6.1 的核心回归场景）。
	write()
	time.Sleep(dockerCheckTTL * 2)
	if !dockerAvailable() {
		t.Fatal("缓存过期后应重新检查；二进制恢复后应返回 true")
	}
}

// TestOrphanCleanupNotBlockedByLock verifies cleanupOrphanPlugins does not
// depend on l.mu: Start 在获取 l.mu 之前执行孤儿清理（见 Start 注释，bug
// 6.2），因此即使其他调用方（如 Stop / ReloadPipelineScheduler）持有锁，
// 清理（含最长 orphanCleanupGrace 的休眠）也不会被阻塞。
func TestOrphanCleanupNotBlockedByLock(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cleanupOrphanPlugins 仅运行在 Linux（依赖 /proc）")
	}
	l := New()
	l.mu.Lock()
	defer l.mu.Unlock()

	done := make(chan struct{})
	go func() {
		cleanupOrphanPlugins()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanupOrphanPlugins 不应被 l.mu 阻塞")
	}
}
