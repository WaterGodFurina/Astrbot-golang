// Xiaomi MiMo TTS provider.
// Ported from astrbot/core/provider/sources/mimo_tts_api_source.py
package sources

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// MiMoTTSApiSource synthesizes speech via the Xiaomi MiMo /chat/completions
// endpoint and saves the returned base64 audio to a local file.
type MiMoTTSApiSource struct {
	*provider.BaseProvider
	apiKey      string
	apiBase     string
	voice       string
	audioFormat string
	stylePrompt string
	dialect     string
	seedText    string
	client      *http.Client
}

// NewMiMoTTSApiSource creates a MiMo TTS provider.
func NewMiMoTTSApiSource(config, settings map[string]interface{}) *MiMoTTSApiSource {
	bp := provider.NewBaseProvider(config, settings)
	// 音频格式会拼进输出文件名: 白名单收敛, 防止注入路径分隔符。
	audioFormat := strings.ToLower(configString(config, "mimo-tts-format", "wav"))
	switch audioFormat {
	case "wav", "mp3", "opus", "pcm":
	default:
		logger.Warn("MiMo TTS: 不支持的音频格式 %q, 回退到 wav", audioFormat)
		audioFormat = "wav"
	}
	s := &MiMoTTSApiSource{
		BaseProvider: bp,
		apiKey:       configString(config, "api_key", ""),
		apiBase:      configString(config, "api_base", mimoDefaultAPIBase),
		voice:        configString(config, "mimo-tts-voice", mimoDefaultTTSVoice),
		audioFormat:  audioFormat,
		stylePrompt:  configString(config, "mimo-tts-style-prompt", ""),
		dialect:      configString(config, "mimo-tts-dialect", ""),
		seedText:     configString(config, "mimo-tts-seed-text", mimoDefaultTTSSeed),
		client: &http.Client{
			Timeout: time.Duration(mimoNormalizeTimeout(config["timeout"], 20)) * time.Second,
		},
	}
	if s.GetModel() == "" {
		s.SetModel(mimoDefaultTTSModel)
	}
	s.SetCapability(provider.CapTextToSpeech)
	return s
}

// buildStylePrefix builds the <style>...</style> prefix, using only the
// singing tag when the style content mentions 唱歌.
func (s *MiMoTTSApiSource) buildStylePrefix() string {
	parts := make([]string, 0, 2)
	if strings.TrimSpace(s.stylePrompt) != "" {
		parts = append(parts, strings.TrimSpace(s.stylePrompt))
	}
	if strings.TrimSpace(s.dialect) != "" {
		parts = append(parts, strings.TrimSpace(s.dialect))
	}
	styleContent := strings.TrimSpace(strings.Join(parts, " "))
	if styleContent == "" {
		return ""
	}
	if strings.Contains(styleContent, "唱歌") {
		return "<style>唱歌</style>"
	}
	return "<style>" + styleContent + "</style>"
}

// GetAudio synthesizes speech and returns the path to the generated audio file.
func (s *MiMoTTSApiSource) GetAudio(ctx context.Context, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("text is empty")
	}

	messages := make([]map[string]interface{}, 0, 2)
	if seed := strings.TrimSpace(s.seedText); seed != "" {
		messages = append(messages, map[string]interface{}{
			"role":    "user",
			"content": seed,
		})
	}
	messages = append(messages, map[string]interface{}{
		"role":    "assistant",
		"content": s.buildStylePrefix() + text,
	})

	audioParams := map[string]interface{}{"format": s.audioFormat}
	// voice design 模型不支持 audio.voice 参数
	if !strings.Contains(s.GetModel(), "voicedesign") {
		audioParams["voice"] = s.voice
	}

	body := map[string]interface{}{
		"model":    s.GetModel(),
		"messages": messages,
		"audio":    audioParams,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mimoBuildAPIURL(s.apiBase), bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	for k, vs := range mimoBuildHeaders(s.apiKey) {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("MiMo TTS API request failed: HTTP %d, response: %s", resp.StatusCode, string(data))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Audio *struct {
					Data string `json:"data"`
				} `json:"audio"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	var audioData string
	for _, c := range result.Choices {
		if c.Message.Audio != nil && c.Message.Audio.Data != "" {
			audioData = c.Message.Audio.Data
			break
		}
	}
	if audioData == "" {
		return "", fmt.Errorf("MiMo TTS API returned no audio payload")
	}
	decoded, err := base64.StdEncoding.DecodeString(audioData)
	if err != nil {
		return "", err
	}

	path := filepath.Join(mimoTempDir(), fmt.Sprintf("mimo_tts_api_%d.%s", time.Now().UnixNano(), s.audioFormat))
	if err := os.WriteFile(path, decoded, 0644); err != nil {
		return "", err
	}
	return path, nil
}

// SupportStream reports whether the provider supports streaming audio output.
func (s *MiMoTTSApiSource) SupportStream() bool { return false }

// Test verifies the provider.
func (s *MiMoTTSApiSource) Test(ctx context.Context) error {
	return nil
}
