package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// OpenAIWhisperSource transcribes audio into text via the OpenAI-compatible
// /audio/transcriptions endpoint.
// Ported from astrbot/core/provider/sources/whisper_api_source.py
type OpenAIWhisperSource struct {
	provider.BaseProvider
	apiBase string
	apiKey  string
	client  *http.Client
}

// NewOpenAIWhisperSource creates a Whisper STT provider.
func NewOpenAIWhisperSource(config, settings map[string]interface{}) *OpenAIWhisperSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &OpenAIWhisperSource{
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
	if m, _ := config["model"].(string); m != "" {
		s.SetModel(m)
	}
	if s.GetModel() == "" {
		s.SetModel("whisper-1")
	}
	s.SetCapability(provider.CapSpeechToText)
	return s
}

// GetText transcribes an audio file (URL or local path) into text.
func (s *OpenAIWhisperSource) GetText(ctx context.Context, audioURL string) (string, error) {
	if strings.TrimSpace(audioURL) == "" {
		return "", fmt.Errorf("audio url is empty")
	}
	path, cleanup, err := s.fetchAudio(ctx, audioURL)
	if err != nil {
		return "", err
	}
	defer cleanup()

	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	fileData, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("read audio file: %w", err)
	}

	url := s.apiBase + "/audio/transcriptions"
	cfg := RetryConfigFromSettings(s.Settings())
	resp, err := DoWithRetry(ctx, s.client, func() (*http.Request, error) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("model", s.GetModel())
		fw, err := mw.CreateFormFile("file", filepath.Base(path))
		if err != nil {
			return nil, err
		}
		if _, err := fw.Write(fileData); err != nil {
			return nil, err
		}
		if err := mw.Close(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
		return req, nil
	}, cfg, "STT-Whisper")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("STT API error %d: %s", resp.StatusCode, string(body))
	}
	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Text), nil
}

// fetchAudio resolves an audio URL or local path to a local file, returning a
// cleanup func for any temporary file it created.
func (s *OpenAIWhisperSource) fetchAudio(ctx context.Context, audioURL string) (string, func(), error) {
	noop := func() {}
	if !strings.HasPrefix(audioURL, "http://") && !strings.HasPrefix(audioURL, "https://") {
		if info, err := os.Stat(audioURL); err != nil {
			return "", noop, fmt.Errorf("audio file not found: %s", audioURL)
		} else if info.IsDir() {
			return "", noop, fmt.Errorf("audio path is a directory: %s", audioURL)
		}
		return audioURL, noop, nil
	}

	cfg := RetryConfigFromSettings(s.Settings())
	resp, err := DoWithRetry(ctx, s.client, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, audioURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
		return req, nil
	}, cfg, "STT-Whisper")
	if err != nil {
		return "", noop, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", noop, fmt.Errorf("download audio: HTTP %d", resp.StatusCode)
	}

	dir := filepath.Join("data", "temp")
	_ = os.MkdirAll(dir, 0755)
	ext := filepath.Ext(strings.Split(audioURL, "?")[0])
	if ext == "" {
		ext = ".wav"
	}
	path := filepath.Join(dir, fmt.Sprintf("stt_%d%s", time.Now().UnixNano(), ext))
	f, err := os.Create(path)
	if err != nil {
		return "", noop, err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(path)
		return "", noop, err
	}
	f.Close()
	return path, func() { os.Remove(path) }, nil
}

// Test verifies the provider by listing models.
func (s *OpenAIWhisperSource) Test(ctx context.Context) error {
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
	}, cfg, "STT-Whisper")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("STT API error %d", resp.StatusCode)
	}
	return nil
}
