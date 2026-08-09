// Package sources - xAI OpenAI-compatible chat provider.
// Ported from astrbot/core/provider/sources/xai_source.py
package sources

import (
	"strings"

	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

// XAISource is an OpenAI-compatible chat provider for xAI.
type XAISource struct {
	*OpenAISource
}

// NewXAISource creates an xAI provider. When xai_native_search is enabled the
// request payload gains {"search_parameters": {"mode": "auto"}}.
func NewXAISource(config, settings map[string]interface{}) *XAISource {
	s := &XAISource{OpenAISource: NewOpenAISource(config, settings)}
	if xaiNativeSearchEnabled(config) {
		s.postProcessBody = func(body map[string]interface{}) {
			body["search_parameters"] = map[string]interface{}{"mode": "auto"}
		}
	}
	return s
}

func xaiNativeSearchEnabled(config map[string]interface{}) bool {
	switch v := config["xai_native_search"].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true") || v == "1"
	case float64:
		return v != 0
	}
	return false
}

var _ provider.ChatProvider = (*XAISource)(nil)
