// Package provider - provider 统一请求头构建。
// 移植自 astrbot/core/provider/headers.py（Python v4.28.2, commit d524b870）：
// 所有 provider 请求默认携带 astrbot/<版本> 的 User-Agent，供应商自定义
// custom_headers 可覆盖。
package provider

import (
	"fmt"
	"strings"

	"github.com/WaterGodFurina/Astrbot-golang/internal/version"
)

// DefaultUserAgent 是 provider 请求的默认 User-Agent（对齐 py
// DEFAULT_USER_AGENT = f"astrbot/{__version__}"；py 取应用 __version__，
// Go 侧取 internal/version.PythonVersion）。
var DefaultUserAgent = "astrbot/" + version.PythonVersion

// BuildProviderHeaders 构建统一 provider 请求头：默认携带 AstrBot User-Agent，
// 再合并 custom_headers。User-Agent 名称大小写不敏感，值为空白时保留默认值；
// 其余 header 原样保留（对齐 py build_provider_headers）。
func BuildProviderHeaders(custom map[string]string) map[string]string {
	headers := map[string]string{"User-Agent": DefaultUserAgent}
	for name, value := range custom {
		if strings.EqualFold(name, "user-agent") {
			if strings.TrimSpace(value) != "" {
				headers["User-Agent"] = value
			}
			continue
		}
		headers[name] = value
	}
	return headers
}

// customHeadersFromConfig 从 provider 配置中解析 custom_headers（对齐 py
// provider_config.get("custom_headers")：仅 dict 生效，键值统一转字符串；
// 缺失/类型不符时按未配置处理）。
func customHeadersFromConfig(config map[string]interface{}) map[string]string {
	raw, ok := config["custom_headers"]
	if !ok || raw == nil {
		return nil
	}
	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = fmt.Sprint(v)
	}
	return out
}
