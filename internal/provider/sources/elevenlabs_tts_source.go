package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

const (
	elevenLabsDefaultVoiceID = "JBFqnCBsd6RMkjVDRZzb"
	elevenLabsDefaultModel   = "eleven_multilingual_v2"
	elevenLabsDefaultFormat  = "mp3_44100_128"
)

// ElevenLabsTTSSource synthesizes speech via the ElevenLabs
// /v1/text-to-speech/{voice_id} endpoint.
// Ported from astrbot/core/provider/sources/elevenlabs_tts_source.py
type ElevenLabsTTSSource struct {
	*provider.BaseProvider
	client       *http.Client
	apiKey       string
	apiBase      string
	voiceID      string
	outputFormat string
	// Only explicitly configured voice settings are sent so the API defaults apply.
	voiceSettings map[string]interface{}
	initErr       error
}

// NewElevenLabsTTSSource creates an ElevenLabs TTS provider.
func NewElevenLabsTTSSource(config, settings map[string]interface{}) *ElevenLabsTTSSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &ElevenLabsTTSSource{
		BaseProvider:  bp,
		apiBase:       strings.TrimSuffix(configString(config, "api_base", "https://api.elevenlabs.io/v1"), "/"),
		apiKey:        configString(config, "api_key", ""),
		voiceID:       configString(config, "elevenlabs-tts-voice-id", elevenLabsDefaultVoiceID),
		outputFormat:  configString(config, "elevenlabs-tts-output-format", elevenLabsDefaultFormat),
		voiceSettings: map[string]interface{}{},
		client: &http.Client{
			Timeout: time.Duration(configInt(config, "timeout", 20)) * time.Second,
		},
	}
	s.SetModel(configString(config, "model", elevenLabsDefaultModel))

	if !elevenLabsValidFormat(s.outputFormat) {
		s.initErr = fmt.Errorf("不支持的ElevenLabs输出格式 %q, 应使用 mp3/wav/opus 格式", s.outputFormat)
		s.outputFormat = elevenLabsDefaultFormat
	}
	// voice id 会拼进 URL 路径: 仅允许 URL 安全字符, 否则回退默认值。
	if !elevenLabsSafeVoiceID(s.voiceID) {
		s.initErr = fmt.Errorf("无效的ElevenLabs voice id %q", s.voiceID)
		s.voiceID = elevenLabsDefaultVoiceID
	}
	for cfgName, key := range map[string]string{
		"elevenlabs-tts-stability":        "stability",
		"elevenlabs-tts-similarity-boost": "similarity_boost",
		"elevenlabs-tts-style":            "style",
	} {
		if v, ok := ttsOptFloat(config, cfgName); ok {
			if v < 0 || v > 1 {
				s.initErr = fmt.Errorf("%s 必须为 0 到 1 之间的数字", cfgName)
				continue
			}
			s.voiceSettings[key] = v
		}
	}
	if _, present := config["elevenlabs-tts-use-speaker-boost"]; present {
		s.voiceSettings["use_speaker_boost"] = ttsConfigBool(config, "elevenlabs-tts-use-speaker-boost", false)
	}
	s.SetCapability(provider.CapTextToSpeech)
	return s
}

func elevenLabsValidFormat(fmtStr string) bool {
	f := strings.ToLower(fmtStr)
	return strings.HasPrefix(f, "mp3") || strings.HasPrefix(f, "wav") || strings.HasPrefix(f, "opus")
}

var elevenLabsVoiceIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// elevenLabsSafeVoiceID reports whether the voice id is a plain URL-safe token.
func elevenLabsSafeVoiceID(id string) bool {
	return elevenLabsVoiceIDPattern.MatchString(id)
}

// outputExtension infers the audio file extension from the output format.
func (s *ElevenLabsTTSSource) outputExtension() string {
	f := strings.ToLower(s.outputFormat)
	switch {
	case strings.HasPrefix(f, "wav"):
		return "wav"
	case strings.HasPrefix(f, "opus"):
		return "opus"
	default:
		return "mp3"
	}
}

// GetAudio synthesizes speech and returns the path to the generated audio file.
func (s *ElevenLabsTTSSource) GetAudio(ctx context.Context, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("text is empty")
	}
	if s.initErr != nil {
		return "", s.initErr
	}
	payload := map[string]interface{}{
		"text":     text,
		"model_id": s.GetModel(),
	}
	if len(s.voiceSettings) > 0 {
		payload["voice_settings"] = s.voiceSettings
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("%s/text-to-speech/%s", s.apiBase, s.voiceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("xi-api-key", s.apiKey)
	q := req.URL.Query()
	q.Set("output_format", s.outputFormat)
	req.URL.RawQuery = q.Encode()

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("ElevenLabs TTS API error %d: %s", resp.StatusCode, string(data))
	}
	return ttsSaveAudio(resp.Body, "elevenlabs_tts_api", s.outputExtension())
}

// SupportStream reports whether the provider supports streaming audio output.
func (s *ElevenLabsTTSSource) SupportStream() bool { return false }

// Test verifies the provider configuration.
func (s *ElevenLabsTTSSource) Test(ctx context.Context) error {
	return s.initErr
}
