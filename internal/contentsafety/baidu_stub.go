//go:build !baidu_aip

package contentsafety

// baiduAipSDKEnabled reports whether the official baidu-aip golang-sdk was
// compiled in. It is false in the default build because the SDK is an optional,
// user-installed dependency (github.com/Baidu-AIP/golang-sdk) that must NOT be
// pulled into every build.
//
// To enable the SDK-backed implementation:
//
//	go get github.com/Baidu-AIP/golang-sdk
//	go build -tags baidu_aip ./cmd/astrbot
const baiduAipSDKEnabled = false

// BaiduAipChecker is a placeholder compiled when the baidu-aip golang-sdk is
// NOT built in. It fails open but reports that the SDK is missing whenever the
// baidu_aip content-safety option is enabled (mirrors the "only error when the
// option is on" requirement: the default build never fails and never downloads
// the SDK).
type BaiduAipChecker struct{}

// NewBaiduAipChecker returns the placeholder checker for a default build.
func NewBaiduAipChecker(appID, apiKey, secretKey string) *BaiduAipChecker {
	return &BaiduAipChecker{}
}

// Check fails open and explains how to enable the real SDK implementation.
func (c *BaiduAipChecker) Check(text string) (bool, string) {
	return true, "baidu aip 未启用：未编译官方 golang-sdk。请执行 `go get github.com/Baidu-AIP/golang-sdk` 后以 `-tags baidu_aip` 重新编译。"
}
