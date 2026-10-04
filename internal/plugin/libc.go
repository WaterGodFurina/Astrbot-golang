package plugin

import (
	"os"
	"strings"
)

// hostLibc returns the libc component ("musl" or "gnu") for the current host.
//
// A Native plugin shares the host process's SDK core, so it MUST be built for
// the same libc as the host binary. The correct default is therefore the host's
// OWN libc, not a build-time assumption: Alpine/musl hosts get "musl", glibc
// hosts get "gnu". ASTRBOT_LIBC overrides for cross/edge setups.
func hostLibc() string {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("ASTRBOT_LIBC"))); v == "musl" || v == "gnu" || v == "glibc" {
		if v == "glibc" {
			return "gnu"
		}
		return v
	}
	if detectMusl() {
		return "musl"
	}
	return "gnu"
}
