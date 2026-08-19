// Package version holds build-time version information for AstrBot Go.
package version

// Version is the AstrBot Go release version. It defaults to "dev" and is
// overridden at build time via
// -ldflags "-X github.com/WaterGodFurina/Astrbot-golang/internal/version.Version=...".
// In CI it follows the GitHub release tag (see release.yml).
var Version = "dev"

// PythonVersion is the AstrBot (Python) version this Go port tracks. It
// defaults to 4.27.3 and can be customized at build time via -ldflags or
// through the workflow's python_version input.
var PythonVersion = "4.27.3"
