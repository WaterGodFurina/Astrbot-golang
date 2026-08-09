// Package sdk documents the AstrBot Go plugin SDK.
//
// The plugin SDK is a standalone module —
// github.com/WaterGodFurina/Astrbot-go-plugin-sdk — kept as a sibling
// repository at ../astrbot-go-plugin-sdk (its own go.mod). Plugin authors
// build against that module and call sdk.Serve(...) from their main function;
// the host talks to the plugin over gRPC (go-plugin). This in-repo package
// exists purely as documentation; the actual SDK types live in the standalone
// module.
package sdk
