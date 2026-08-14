// Package sources - LongCat OpenAI-compatible chat provider.
// Ported from astrbot/core/provider/sources/longcat_source.py
package sources

import (
	"strings"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// LongcatSource is an OpenAI-compatible chat provider for LongCat.
type LongcatSource struct {
	*OpenAISource
}

// NewLongcatSource creates a LongCat provider, defaulting api_base to
// https://api.longcat.chat/openai/v1 and normalizing ".../openai" to ".../openai/v1".
func NewLongcatSource(config, settings map[string]interface{}) *LongcatSource {
	cfg := cloneMap(config)
	apiBase := strings.TrimSpace(configString(cfg, "api_base", ""))
	if apiBase == "" {
		apiBase = "https://api.longcat.chat/openai/v1"
	} else {
		apiBase = strings.TrimRight(apiBase, "/")
		if strings.HasSuffix(apiBase, "/openai") {
			apiBase += "/v1"
		}
	}
	cfg["api_base"] = apiBase
	return &LongcatSource{OpenAISource: NewOpenAISource(cfg, settings)}
}

var _ provider.ChatProvider = (*LongcatSource)(nil)
