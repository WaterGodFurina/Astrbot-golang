//go:build !linux

package plugin

// detectMusl: musl detection only matters on Linux hosts.
func detectMusl() bool { return false }
