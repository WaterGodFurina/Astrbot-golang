package sources

import (
	"bytes"
	"context"
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

// OpenAITTSSource synthesizes speech via the OpenAI-compatible
// /audio/speech endpoint and saves the result to a local wav file.
// Ported from astrbot/core/provider/sources/openai_tts_api_source.py
type OpenAITTSSource struct {
	*provider.BaseProvider
	apiBase string
	apiKey  string
	voice   string
	client  *http.Client
}

// NewOpenAITTSSource creates an OpenAI TTS provider.
func NewOpenAITTSSource(config, settings map[string]interface{}) *OpenAITTSSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &OpenAITTSSource{
		BaseProvider: bp,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
	s.apiBase, _ = config["api_base"].(string)
	if s.apiBase == "" {
		s.apiBase = "https://api.openai.com/v1"
	}
	s.apiBase = strings.TrimSuffix(s.apiBase, "/")
	s.apiKey = configString(config, "key", configString(config, "api_key", ""))
	s.voice = configString(config, "voice", configString(config, "openai-tts-voice", "alloy"))
	if m, _ := config["model"].(string); m != "" {
		s.SetModel(m)
	}
	if s.GetModel() == "" {
		s.SetModel("tts-1")
	}
	s.SetCapability(provider.CapTextToSpeech)
	return s
}

// GetAudio synthesizes speech and returns the path to the generated audio file.
func (s *OpenAITTSSource) GetAudio(ctx context.Context, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("text is empty")
	}
	body := map[string]interface{}{
		"model":           s.GetModel(),
		"voice":           s.voice,
		"input":           text,
		"response_format": "wav",
	}
	payloadBytes, _ := json.Marshal(body)

	url := s.apiBase + "/audio/speech"
	cfg := RetryConfigFromSettings(s.Settings())
	resp, err := DoWithRetry(ctx, s.client, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payloadBytes))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
		return req, nil
	}, cfg, "TTS-OpenAI")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("TTS API error %d: %s", resp.StatusCode, string(data))
	}

	dir := filepath.Join("data", "temp")
	_ = os.MkdirAll(dir, 0755)
	path := filepath.Join(dir, fmt.Sprintf("openai_tts_%d.wav", time.Now().UnixNano()))
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// SupportStream reports whether the provider supports streaming audio output.
func (s *OpenAITTSSource) SupportStream() bool { return false }

// Test verifies the provider by listing models.
func (s *OpenAITTSSource) Test(ctx context.Context) error {
	url := s.apiBase + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	cfg := RetryConfigFromSettings(s.Settings())
	resp, err := DoWithRetry(ctx, s.client, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
		return req, nil
	}, cfg, "TTS-OpenAI")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("TTS API error %d", resp.StatusCode)
	}
	return nil
}
