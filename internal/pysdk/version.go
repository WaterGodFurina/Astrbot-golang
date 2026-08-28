package pysdk

// SDKVersion is the released Python SDK version (the git tag on the
// astrbot-python-sdk GitHub repo). The SDK is NOT embedded in the host binary:
// Ensure() downloads the repo tarball at tag v<SDKVersion> into
// <dataDir>/python-sdk when the on-disk copy is missing or its VERSION marker
// differs, so a released host always picks up the matching SDK and an SDK
// update only needs a tag bump (no host rebuild).
const SDKVersion = "0.8.1"

// ProtocolVersion is the gRPC protocol version spoken between the host and the
// Python bridge (_bridge/server.py). It is INDEPENDENT of SDKVersion: an SDK
// bump may carry only Python-side code/regenerated stubs (SDKVersion goes up
// while ProtocolVersion stays), and a wire-protocol change (e.g. the
// EventResult message introduced here) bumps ProtocolVersion alone. Old plugin
// binaries and old hosts keep working across both via proto3 field-appending
// compatibility (legacy bool fields are preserved on every response message).
const ProtocolVersion = "2"
