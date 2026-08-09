package sources

import (
	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

func init() {
	// Register the four Rerank providers (type names match the Python
	// register_provider_adapter decorators).
	provider.RegisterProvider("bailian_rerank", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewBailianRerankSource(config, settings), nil
	})
	provider.RegisterProvider("nvidia_rerank", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewNvidiaRerankSource(config, settings), nil
	})
	provider.RegisterProvider("vllm_rerank", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewVLLMRerankSource(config, settings), nil
	})
	provider.RegisterProvider("xinference_rerank", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewXinferenceRerankSource(config, settings), nil
	})
}
