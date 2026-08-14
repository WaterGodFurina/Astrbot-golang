//go:build baidu_aip

package contentsafety

import (
	"encoding/json"
	"fmt"

	"github.com/Baidu-AIP/golang-sdk/aip/censor"
)

// baiduAipSDKEnabled reports whether the official baidu-aip golang-sdk was
// compiled in (true only under `-tags baidu_aip`). The stub in baidu_stub.go
// provides the false counterpart for the default build.
const baiduAipSDKEnabled = true

// BaiduAipChecker checks text via the Baidu AI content moderation API using the
// official golang-sdk (github.com/Baidu-AIP/golang-sdk).
//
// This implementation is compiled only when the SDK is installed and the binary
// is built with `-tags baidu_aip`:
//
//	go get github.com/Baidu-AIP/golang-sdk
//	go build -tags baidu_aip ./cmd/astrbot
type BaiduAipChecker struct {
	client *censor.ContentCensorClient
}

// NewBaiduAipChecker creates a Baidu AI content moderation checker. The Go SDK
// only needs apiKey/secretKey; appID is accepted for config compatibility but
// unused.
func NewBaiduAipChecker(appID, apiKey, secretKey string) *BaiduAipChecker {
	return &BaiduAipChecker{client: censor.NewClient(apiKey, secretKey)}
}

// Check returns (ok, info). ok=false if the text is flagged.
func (c *BaiduAipChecker) Check(text string) (bool, string) {
	if c.client == nil {
		return true, "baidu aip: client not initialized"
	}
	resp := c.client.TextCensor(text, nil)

	var body struct {
		Conclusion     string `json:"conclusion"`
		ConclusionType int    `json:"conclusionType"`
		ErrorCode      int    `json:"error_code"`
		ErrorMsg       string `json:"error_msg"`
	}
	if err := json.Unmarshal([]byte(resp), &body); err != nil {
		// 传输/解析失败时 fail open：短暂 API 故障不应阻塞所有消息。
		return true, "baidu aip decode error: " + err.Error()
	}
	if body.ErrorCode != 0 {
		return true, fmt.Sprintf("baidu aip error(%d): %s", body.ErrorCode, body.ErrorMsg)
	}
	if body.Conclusion == "不合规" || body.ConclusionType == 2 {
		return false, "baidu aip flagged: " + body.Conclusion
	}
	return true, ""
}
