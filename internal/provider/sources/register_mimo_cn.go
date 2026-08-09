// Auto-registration of the Chinese-language & misc TTS/STT providers.
package sources

import (
	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

func init() {
	provider.RegisterProvider("volcengine_tts", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewVolcengineTTSSource(config, settings), nil
	})

	provider.RegisterProvider("gemini_tts", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewGeminiTTSSource(config, settings), nil
	})

	provider.RegisterProvider("mimo_tts_api", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewMiMoTTSApiSource(config, settings), nil
	})

	provider.RegisterProvider("mimo_stt_api", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewMiMoSTTApiSource(config, settings), nil
	})
}
