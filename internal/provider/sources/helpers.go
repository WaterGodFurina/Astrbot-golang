package sources

import (
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

// logger 供 sources 包内各 LLM 提供方的请求/响应摘要 Debug 日志共用，
// 级别默认 INFO，Debug 仅在 DEBUG 级别下输出，不污染 INFO 日志。
var logger = log.GetDefault().WithComponent("Provider")

// configString returns the string value of key, or the fallback when absent.
func configString(config map[string]interface{}, key, fallback string) string {
	if v, ok := config[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

// configInt returns the int value of key, or the fallback when absent.
func configInt(config map[string]interface{}, key string, fallback int) int {
	switch v := config[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return fallback
}
