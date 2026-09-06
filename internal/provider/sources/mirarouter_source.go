// Package sources - MiraRouter OpenAI-compatible chat provider.
// Ported from astrbot/core/provider/sources/mirarouter_source.py
package sources

import (
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// MiraRouterSource is an OpenAI-compatible chat provider for MiraRouter.
type MiraRouterSource struct {
	*OpenAISource
}

// NewMiraRouterSource creates a MiraRouter provider.
func NewMiraRouterSource(config, settings map[string]interface{}) *MiraRouterSource {
	s := &MiraRouterSource{OpenAISource: NewOpenAISource(config, settings)}
	if s.extraHeaders == nil {
		s.extraHeaders = make(map[string]string)
	}
	s.extraHeaders["X-APP-CODE"] = "astrbot"
	return s
}

var _ provider.ChatProvider = (*MiraRouterSource)(nil)
