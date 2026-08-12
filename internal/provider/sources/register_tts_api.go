package sources

import (
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

func init() {
	// Register TTS API providers (ported from the Python astrbot sources).
	provider.RegisterProvider("azure_tts", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewAzureTTSSource(config, settings), nil
	})
	provider.RegisterProvider("elevenlabs_tts_api", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewElevenLabsTTSSource(config, settings), nil
	})
	provider.RegisterProvider("fishaudio_tts_api", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewFishAudioTTSSource(config, settings), nil
	})
	provider.RegisterProvider("minimax_tts_api", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewMiniMaxTTSSource(config, settings), nil
	})
	provider.RegisterProvider("edge_tts", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewEdgeTTSSource(config, settings), nil
	})
}
