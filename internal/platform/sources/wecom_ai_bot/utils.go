// 企业微信智能机器人工具模块。
// 1:1 移植自 wecomai_utils.py。
package wecom_ai_bot

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// GenerateRandomString 生成随机字符串（字母+数字，对应 generate_random_string）。
func GenerateRandomString(length int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	if length <= 0 {
		return ""
	}
	randBytes := make([]byte, length)
	if _, err := rand.Read(randBytes); err != nil {
		// 回退到时间戳
		return fmt.Sprintf("%d", time.Now().UnixNano())[:length]
	}
	b := make([]byte, length)
	for i := 0; i < length; i++ {
		b[i] = letters[int(randBytes[i])%len(letters)]
	}
	return string(b)
}

// CalculateImageMD5 计算图片数据的 MD5 值（对应 calculate_image_md5）。
func CalculateImageMD5(imageData []byte) string {
	sum := md5.Sum(imageData)
	return hex.EncodeToString(sum[:])
}

// EncodeImageBase64 将图片数据编码为 Base64（对应 encode_image_base64）。
func EncodeImageBase64(imageData []byte) string {
	return base64.StdEncoding.EncodeToString(imageData)
}

// FormatSessionID 格式化会话 ID（对应 format_session_id）。
// 返回格式：wecom_ai_bot_<session_type>_<session_id>。
func FormatSessionID(sessionType, sessionID string) string {
	return "wecom_ai_bot_" + sessionType + "_" + sessionID
}

// ParseSessionID 解析格式化的会话 ID（对应 parse_session_id）。
// 返回 (会话类型, 原始会话 ID)。
func ParseSessionID(formattedSessionID string) (string, string) {
	parts := strings.SplitN(formattedSessionID, "_", 4)
	if len(parts) >= 4 && parts[0] == "wecom" && parts[1] == "ai" && parts[2] == "bot" {
		return parts[3], ""
	}
	return "user", formattedSessionID
}

// SafeJSONLoads 安全解析 JSON 字符串，失败返回默认值（对应 safe_json_loads）。
func SafeJSONLoads(jsonStr string, def interface{}) interface{} {
	var out interface{}
	if err := json.Unmarshal([]byte(jsonStr), &out); err != nil {
		logger.I18nWarn("JSON 解析失败: %v, 原始字符串: %s", err, jsonStr)
		return def
	}
	return out
}

// FormatErrorResponse 格式化错误响应（对应 format_error_response）。
func FormatErrorResponse(errorCode int, errorMsg string) string {
	return fmt.Sprintf("Error %d: %s", errorCode, errorMsg)
}
