//go:build race

package plugin

// nativeTestRaceEnabled lets buildTestNativeLib build the Native test plugin
// with -race, so it matches the -race host test binary (Go plugin requires the
// host and plugin to share identical package versions, including internal/race).
const nativeTestRaceEnabled = true
