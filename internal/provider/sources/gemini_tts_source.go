// Google Gemini TTS provider.
// Ported from astrbot/core/provider/sources/gemini_tts_source.py
package sources

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// GeminiTTSSource synthesizes speech via the Google Gemini generateContent
// API (response_modalities=["AUDIO"]) and writes the returned PCM bytes into
// a local wav file.
type GeminiTTSSource struct {
	*provider.BaseProvider
	apiKey    string
	apiBase   string
	model     string
	prefix    string
	voiceName string
	client    *http.Client
}

// NewGeminiTTSSource creates a Gemini TTS provider.
func NewGeminiTTSSource(config, settings map[string]interface{}) *GeminiTTSSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &GeminiTTSSource{
		BaseProvider: bp,
		apiKey:       configString(config, "gemini_tts_api_key", ""),
		apiBase:      configString(config, "gemini_tts_api_base", "https://generativelanguage.googleapis.com/v1beta"),
		model:        configString(config, "gemini_tts_model", "gemini-2.5-flash-preview-tts"),
		prefix:       configString(config, "gemini_tts_prefix", ""),
		voiceName:    configString(config, "gemini_tts_voice_name", "Leda"),
		client: &http.Client{
			Timeout: time.Duration(mimoNormalizeTimeout(config["gemini_tts_timeout"], 20)) * time.Second,
		},
	}
	s.apiBase = strings.TrimSuffix(s.apiBase, "/")
	s.SetModel(s.model)
	s.SetCapability(provider.CapTextToSpeech)
	return s
}

// GetAudio synthesizes speech and returns the path to the generated wav file.
func (s *GeminiTTSSource) GetAudio(ctx context.Context, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("text is empty")
	}
	prompt := text
	if s.prefix != "" {
		prompt = s.prefix + ": " + text
	}

	body := map[string]interface{}{
		"contents": []map[string]interface{}{
			{"parts": []map[string]interface{}{{"text": prompt}}},
		},
		"generationConfig": map[string]interface{}{
			"responseModalities": []string{"AUDIO"},
			"speechConfig": map[string]interface{}{
				"voiceConfig": map[string]interface{}{
					"prebuiltVoiceConfig": map[string]interface{}{
						"voiceName": s.voiceName,
					},
				},
			},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	if !geminiModelPathSafe(s.model) {
		return "", fmt.Errorf("invalid gemini model name: %q", s.model)
	}
	url := fmt.Sprintf("%s/models/%s:generateContent", s.apiBase, s.model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", s.apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("gemini TTS API error %d: %s", resp.StatusCode, string(data))
	}

	var result struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData *struct {
						Data string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	var pcmData []byte
	for _, cand := range result.Candidates {
		for _, part := range cand.Content.Parts {
			if part.InlineData != nil && part.InlineData.Data != "" {
				decoded, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
				if err != nil {
					return "", err
				}
				pcmData = append(pcmData, decoded...)
			}
		}
	}
	if len(pcmData) == 0 {
		return "", fmt.Errorf("no audio content returned from Gemini TTS API")
	}

	dir := filepath.Join("data", "temp")
	_ = os.MkdirAll(dir, 0755)
	path := filepath.Join(dir, fmt.Sprintf("gemini_tts_%d.wav", time.Now().UnixNano()))
	if err := writePCMWavFile(path, pcmData, 24000); err != nil {
		return "", err
	}
	return path, nil
}

// SupportStream reports whether the provider supports streaming audio output.
func (s *GeminiTTSSource) SupportStream() bool { return false }

// Test verifies the provider.
func (s *GeminiTTSSource) Test(ctx context.Context) error {
	return nil
}

// writePCMWavFile writes raw 16-bit mono PCM data into a RIFF/WAVE file,
// mirroring the wave module usage in gemini_tts_source.py.
func writePCMWavFile(path string, pcm []byte, sampleRate int) error {
	const (
		channels      = 1
		bitsPerSample = 16
	)
	if sampleRate <= 0 || sampleRate > math.MaxUint32 {
		return fmt.Errorf("invalid sample rate: %d", sampleRate)
	}
	byteRate := sampleRate * channels * bitsPerSample / 8
	if byteRate > math.MaxUint32 {
		return fmt.Errorf("invalid byte rate: %d", byteRate)
	}
	blockAlign := channels * bitsPerSample / 8
	if len(pcm) > math.MaxUint32-36 {
		return fmt.Errorf("PCM data too large for WAV format: %d bytes", len(pcm))
	}

	var buf bytes.Buffer
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(36+len(pcm))) // #nosec G115 -- len(pcm) 已在上方校验 ≤ math.MaxUint32-36
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(channels))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(sampleRate)) // #nosec G115 -- sampleRate 已校验 ∈ (0, math.MaxUint32]
	_ = binary.Write(&buf, binary.LittleEndian, uint32(byteRate))   // #nosec G115 -- byteRate 已校验 ≤ math.MaxUint32
	_ = binary.Write(&buf, binary.LittleEndian, uint16(blockAlign))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(bitsPerSample))
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(pcm))) // #nosec G115 -- len(pcm) 已在上方校验 ≤ math.MaxUint32-36
	buf.Write(pcm)
	return os.WriteFile(path, buf.Bytes(), 0644)
}
