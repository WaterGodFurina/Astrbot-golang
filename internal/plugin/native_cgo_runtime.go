//go:build cgo

package plugin

// nativeHostCGOAvailable reports whether the host binary itself was built with
// cgo. Go's stdlib `plugin` package requires the host and the .so to share the
// cgo linkage; a CGO_ENABLED=0 host gets a plugin.Open stub that returns
// "plugin: not implemented".
const nativeHostCGOAvailable = true
