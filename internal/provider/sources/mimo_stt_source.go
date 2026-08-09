// Xiaomi MiMo STT provider.
// Ported from astrbot/core/provider/sources/mimo_stt_api_source.py
package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

// MiMoSTTApiSource transcribes audio into text via the Xiaomi MiMo
// /chat/completions endpoint with input_audio content.
type MiMoSTTApiSource struct {
	provider.BaseProvider
	apiKey  string
	apiBase string
	client  *http.Client
}

// NewMiMoSTTApiSource creates a MiMo STT provider.
func NewMiMoSTTApiSource(config, settings map[string]interface{}) *MiMoSTTApiSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &MiMoSTTApiSource{
		BaseProvider: bp,
		apiKey:       configString(config, "api_key", ""),
		apiBase:      configString(config, "api_base", mimoDefaultAPIBase),
		client: &http.Client{
			Timeout: time.Duration(mimoNormalizeTimeout(config["timeout"], 20)) * time.Second,
		},
	}
	if s.GetModel() == "" {
		s.SetModel(mimoDefaultSTTModel)
	}
	s.SetCapability(provider.CapSpeechToText)
	return s
}

func (s *MiMoSTTApiSource) isASRModel() bool {
	return strings.Contains(strings.ToLower(s.GetModel()), "asr")
}

// buildMessages builds the message list, sending bare audio for dedicated ASR
// models and a system/user instruction pair for multimodal models.
func (s *MiMoSTTApiSource) buildMessages(audioDataURL string) []map[string]interface{} {
	audioContent := map[string]interface{}{
		"type": "input_audio",
		"input_audio": map[string]interface{}{
			"data": audioDataURL,
		},
	}
	if s.isASRModel() {
		return []map[string]interface{}{
			{"role": "user", "content": []interface{}{audioContent}},
		}
	}
	return []map[string]interface{}{
		{"role": "system", "content": mimoSTTSystemPrompt},
		{
			"role": "user",
			"content": []interface{}{
				audioContent,
				map[string]interface{}{"type": "text", "text": mimoSTTUserPrompt},
			},
		},
	}
}

// GetText transcribes an audio file (URL or local path) into text.
func (s *MiMoSTTApiSource) GetText(ctx context.Context, audioURL string) (string, error) {
	if strings.TrimSpace(audioURL) == "" {
		return "", fmt.Errorf("audio url is empty")
	}
	path, cleanup, err := mimoFetchAudio(ctx, s.client, audioURL)
	if err != nil {
		return "", err
	}
	defer cleanup()

	audioData, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if err := mimoValidateWAV(audioData); err != nil {
		return "", err
	}

	body := map[string]interface{}{
		"model":                 s.GetModel(),
		"messages":              s.buildMessages(mimoDataURL(audioData, "audio/wav")),
		"max_completion_tokens": 1024,
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
		return "", fmt.Errorf("MiMo STT API request failed: HTTP %d, response: %s", resp.StatusCode, string(data))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	for _, c := range result.Choices {
		text := c.Message.Content
		if text == "" {
			text = c.Message.ReasoningContent
		}
		if strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text), nil
		}
	}
	return "", fmt.Errorf("MiMo STT API returned empty transcription")
}

// Test verifies the provider.
func (s *MiMoSTTApiSource) Test(ctx context.Context) error {
	return nil
}
