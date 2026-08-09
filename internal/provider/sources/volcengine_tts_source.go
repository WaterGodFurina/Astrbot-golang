// Volcengine TTS provider.
// Ported from astrbot/core/provider/sources/volcengine_tts.py
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

	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

// VolcengineTTSSource synthesizes speech via the Volcengine (火山引擎)
// openspeech TTS API and saves the result to a local mp3 file.
type VolcengineTTSSource struct {
	provider.BaseProvider
	apiKey     string
	appid      string
	cluster    string
	voiceType  string
	speedRatio float64
	apiBase    string
	client     *http.Client
}

// NewVolcengineTTSSource creates a Volcengine TTS provider.
func NewVolcengineTTSSource(config, settings map[string]interface{}) *VolcengineTTSSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &VolcengineTTSSource{
		BaseProvider: bp,
		apiKey:       configString(config, "api_key", ""),
		appid:        configString(config, "appid", ""),
		cluster:      configString(config, "volcengine_cluster", ""),
		voiceType:    configString(config, "volcengine_voice_type", ""),
		speedRatio:   1.0,
		apiBase:      configString(config, "api_base", "https://openspeech.bytedance.com/api/v1/tts"),
		client: &http.Client{
			Timeout: time.Duration(mimoNormalizeTimeout(config["timeout"], 20)) * time.Second,
		},
	}
	if v, ok := config["volcengine_speed_ratio"].(float64); ok && v > 0 {
		s.speedRatio = v
	}
	s.SetCapability(provider.CapTextToSpeech)
	return s
}

// GetAudio synthesizes speech and returns the path to the generated mp3 file.
func (s *VolcengineTTSSource) GetAudio(ctx context.Context, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("text is empty")
	}
	payload := map[string]interface{}{
		"app": map[string]interface{}{
			"appid":   s.appid,
			"token":   s.apiKey,
			"cluster": s.cluster,
		},
		"user": map[string]interface{}{
			"uid": randomUUID(),
		},
		"audio": map[string]interface{}{
			"voice_type":   s.voiceType,
			"encoding":     "mp3",
			"speed_ratio":  s.speedRatio,
			"volume_ratio": 1.0,
			"pitch_ratio":  1.0,
		},
		"request": map[string]interface{}{
			"reqid":         randomUUID(),
			"text":          text,
			"text_type":     "plain",
			"operation":     "query",
			"with_frontend": 1,
			"frontend_type": "unitTson",
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiBase, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer; "+s.apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("volcengine TTS API request failed: %d, %s", resp.StatusCode, string(data))
	}

	var result struct {
		Data    string `json:"data"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.Data == "" {
		return "", fmt.Errorf("volcengine TTS API error: %s", result.Message)
	}
	audioData, err := base64.StdEncoding.DecodeString(result.Data)
	if err != nil {
		return "", err
	}

	dir := filepath.Join("data", "temp")
	_ = os.MkdirAll(dir, 0755)
	path := filepath.Join(dir, fmt.Sprintf("volcengine_tts_%d.mp3", time.Now().UnixNano()))
	if err := os.WriteFile(path, audioData, 0644); err != nil {
		return "", err
	}
	return path, nil
}

// SupportStream reports whether the provider supports streaming audio output.
func (s *VolcengineTTSSource) SupportStream() bool { return false }

// Test verifies the provider.
func (s *VolcengineTTSSource) Test(ctx context.Context) error {
	return nil
}
