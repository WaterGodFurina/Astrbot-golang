// Package sources - registration of special chat completion providers
// (OpenAI Responses API and Kimi Code), ported from
// astrbot/core/provider/sources/{openai_responses_source,kimi_code_source}.py
package sources

import (
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

func init() {
	provider.RegisterProvider("openai_responses", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewOpenAIResponsesSource(config, settings), nil
	})

	provider.RegisterProvider("kimi_code_chat_completion", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewKimiCodeSource(config, settings), nil
	})
}
