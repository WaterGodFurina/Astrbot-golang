package platform

import (
	"encoding/json"
	"strconv"
	"strings"
)

// WeChatErrCode 从企业微信/微信返回的 JSON map 中解析 errcode，兼容
// JSON number（float64）、json.Number、int 以及字符串（微信部分接口会把
// errcode 以字符串返回）几种类型。
//
// 返回 (code, present)：
//   - present=false：响应里没有 errcode 字段。企业微信正常响应一定带
//     errcode（成功为 0），缺失说明响应格式异常，调用方应按失败处理，
//     而不应默认成功。
//   - present=true 且 code!=0：接口返回错误。
//   - 字段存在但类型无法解析时返回 (-1, true)，同样按错误处理。
func WeChatErrCode(data map[string]interface{}) (int, bool) {
	v, ok := data["errcode"]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		if c, err := n.Int64(); err == nil {
			return int(c), true
		}
		return -1, true
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return -1, true
		}
		if c, err := strconv.Atoi(s); err == nil {
			return c, true
		}
		return -1, true
	default:
		return -1, true
	}
}
