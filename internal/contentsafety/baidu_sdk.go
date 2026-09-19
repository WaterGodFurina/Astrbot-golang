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
		// 对齐 Python baidu_aip.py：响应缺少 conclusionType 时 return False
		// （fail-closed）。传输/解析失败同样拦截，绝不静默放行。
		return false, "baidu aip decode error: " + err.Error()
	}
	if body.ErrorCode != 0 {
		// 对齐 Python：API 返回错误时不会出现合法的 conclusionType==1，
		// 因此按不合规处理（fail-closed）。
		return false, fmt.Sprintf("baidu aip error(%d): %s", body.ErrorCode, body.ErrorMsg)
	}
	// conclusionType: 1=合规；2=不合规；3=疑似；4=审核失败。
	// Python 仅当 `res["conclusionType"] == 1` 才放行，其余（含缺失/未知）一律
	// 拦截（baidu_aip.py:21-32）。
	if body.ConclusionType == 1 {
		return true, ""
	}
	return false, "baidu aip flagged: " + body.Conclusion
}
