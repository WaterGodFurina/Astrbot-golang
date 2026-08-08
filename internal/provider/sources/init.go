// Package sources - auto-registration of all built-in providers.
package sources

import (
	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

func init() {
	// Register OpenAI-compatible provider
	provider.RegisterProvider("openai", func(config, settings map[string]interface{}) (provider.ChatProvider, error) {
		return NewOpenAISource(config, settings), nil
	})

	// Register OpenRouter (OpenAI-compatible)
	provider.RegisterProvider("openrouter", func(config, settings map[string]interface{}) (provider.ChatProvider, error) {
		s := NewOpenAISource(config, settings)
		if s.apiBase == "https://api.openai.com/v1" {
			s.apiBase = "https://openrouter.ai/api/v1"
		}
		return s, nil
	})

	// Register Anthropic
	provider.RegisterProvider("anthropic", func(config, settings map[string]interface{}) (provider.ChatProvider, error) {
		return NewAnthropicSource(config, settings), nil
	})

	// Register Gemini
	provider.RegisterProvider("gemini", func(config, settings map[string]interface{}) (provider.ChatProvider, error) {
		return NewGeminiSource(config, settings), nil
	})

	// Register Ollama
	provider.RegisterProvider("ollama", func(config, settings map[string]interface{}) (provider.ChatProvider, error) {
		return NewOllamaSource(config, settings), nil
	})

	// Register DashScope (Aliyun Qwen)
	provider.RegisterProvider("dashscope", func(config, settings map[string]interface{}) (provider.ChatProvider, error) {
		return NewDashScopeSource(config, settings), nil
	})
}
