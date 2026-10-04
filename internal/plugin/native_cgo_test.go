//go:build cgo

package plugin

// nativeTestCgoEnabled reports whether the host binary was built with cgo, which
// Go's stdlib `plugin` package requires. CI's plain `go test` job sets
// CGO_ENABLED=0, where plugin.Open is a stub returning "plugin: not implemented".
const nativeTestCgoEnabled = true
