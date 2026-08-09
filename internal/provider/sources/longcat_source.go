// Package sources - LongCat OpenAI-compatible chat provider.
// Ported from astrbot/core/provider/sources/longcat_source.py
package sources

import (
	"strings"

	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

// LongcatSource is an OpenAI-compatible chat provider for LongCat.
type LongcatSource struct {
	*OpenAISource
}

// NewLongcatSource creates a LongCat provider, defaulting api_base to
// https://api.longcat.chat/openai/v1 and normalizing ".../openai" to ".../openai/v1".
func NewLongcatSource(config, settings map[string]interface{}) *LongcatSource {
	apiBase := strings.TrimSpace(configString(config, "api_base", ""))
	if apiBase == "" {
		apiBase = "https://api.longcat.chat/openai/v1"
	} else {
		apiBase = strings.TrimRight(apiBase, "/")
		if strings.HasSuffix(apiBase, "/openai") {
			apiBase += "/v1"
		}
	}
	config["api_base"] = apiBase
	return &LongcatSource{OpenAISource: NewOpenAISource(config, settings)}
}

var _ provider.ChatProvider = (*LongcatSource)(nil)
