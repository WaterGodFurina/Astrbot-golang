// Package sources - auto-registration of all built-in providers.
package sources

import (
	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

func init() {
	// Register OpenAI-compatible provider
	provider.RegisterProvider("openai", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewOpenAISource(config, settings), nil
	})

	// Register OpenRouter (OpenAI-compatible)
	provider.RegisterProvider("openrouter", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		s := NewOpenAISource(config, settings)
		if s.apiBase == "https://api.openai.com/v1" {
			s.apiBase = "https://openrouter.ai/api/v1"
		}
		return s, nil
	})

	// Register Anthropic
	provider.RegisterProvider("anthropic", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewAnthropicSource(config, settings), nil
	})

	// Register Gemini
	provider.RegisterProvider("gemini", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewGeminiSource(config, settings), nil
	})

	// Register Ollama
	provider.RegisterProvider("ollama", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewOllamaSource(config, settings), nil
	})

	// Register DashScope (Aliyun Qwen)
	provider.RegisterProvider("dashscope", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewDashScopeSource(config, settings), nil
	})

	// Register non-chat capability providers (STT / TTS / Embedding / Rerank).
	provider.RegisterProvider("openai_whisper", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewOpenAIWhisperSource(config, settings), nil
	})
	provider.RegisterProvider("openai_tts", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewOpenAITTSSource(config, settings), nil
	})
	provider.RegisterProvider("openai_embedding", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewOpenAIEmbeddingSource(config, settings), nil
	})
	provider.RegisterProvider("tei_rerank", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewTEIRerankSource(config, settings), nil
	})
}
