package sources

import (
	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

func init() {
	// Register Gemini Embedding
	provider.RegisterProvider("gemini_embedding", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewGeminiEmbeddingSource(config, settings), nil
	})

	// Register NVIDIA Embedding
	provider.RegisterProvider("nvidia_embedding", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewNvidiaEmbeddingSource(config, settings), nil
	})

	// Register Ollama Embedding
	provider.RegisterProvider("ollama_embedding", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewOllamaEmbeddingSource(config, settings), nil
	})

	// Register DashScope (Aliyun Bailian) Embedding
	provider.RegisterProvider("dashscope_embedding", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewDashScopeEmbeddingSource(config, settings), nil
	})
}
