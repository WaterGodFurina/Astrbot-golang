// Package sources - registration of OpenAI-compatible chat providers.
// Ported from astrbot/core/provider/sources/{groq,xai,zhipu,longcat,oai_aihubmix,xiaomi}_source.py
package sources

import (
	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

func init() {
	provider.RegisterProvider("groq_chat_completion", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewGroqSource(config, settings), nil
	})

	provider.RegisterProvider("xai_chat_completion", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewXAISource(config, settings), nil
	})

	provider.RegisterProvider("zhipu_chat_completion", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewZhipuSource(config, settings), nil
	})

	provider.RegisterProvider("longcat_chat_completion", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewLongcatSource(config, settings), nil
	})

	provider.RegisterProvider("aihubmix_chat_completion", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewAIHubMixSource(config, settings), nil
	})

	provider.RegisterProvider("xiaomi_chat_completion", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewXiaomiSource(config, settings), nil
	})
}
