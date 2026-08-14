package lifecycle

import (
	"path/filepath"
	"testing"
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
