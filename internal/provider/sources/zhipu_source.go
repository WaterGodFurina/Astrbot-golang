// Package sources - Zhipu (智谱) OpenAI-compatible chat provider.
// Ported from astrbot/core/provider/sources/zhipu_source.py
package sources

import (
	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

// ZhipuSource is an OpenAI-compatible chat provider for Zhipu.
type ZhipuSource struct {
	*OpenAISource
}

// NewZhipuSource creates a Zhipu provider.
func NewZhipuSource(config, settings map[string]interface{}) *ZhipuSource {
	return &ZhipuSource{OpenAISource: NewOpenAISource(config, settings)}
}

var _ provider.ChatProvider = (*ZhipuSource)(nil)
