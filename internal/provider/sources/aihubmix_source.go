// Package sources - AIHubMix OpenAI-compatible chat provider.
// Ported from astrbot/core/provider/sources/oai_aihubmix_source.py
package sources

import (
	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

// AIHubMixSource is an OpenAI-compatible chat provider for AIHubMix.
type AIHubMixSource struct {
	*OpenAISource
}

// NewAIHubMixSource creates an AIHubMix provider. Every request carries the
// APP-Code header (referral code for a 10% discount).
func NewAIHubMixSource(config, settings map[string]interface{}) *AIHubMixSource {
	s := &AIHubMixSource{OpenAISource: NewOpenAISource(config, settings)}
	s.extraHeaders = map[string]string{"APP-Code": "KRLC5702"}
	return s
}

var _ provider.ChatProvider = (*AIHubMixSource)(nil)
