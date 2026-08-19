// Shared helpers for the MiMo (Xiaomi) TTS/STT providers.
// Ported from astrbot/core/provider/sources/mimo_api_common.py
package sources

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	mimoDefaultAPIBase  = "https://api.xiaomimimo.com/v1"
	mimoDefaultTTSModel = "mimo-v2.5-tts"
	mimoDefaultTTSVoice = "mimo_default"
	mimoDefaultTTSSeed  = "Hello, MiMo, have you had lunch?"
	mimoDefaultSTTModel = "mimo-v2.5-asr"

	mimoSTTSystemPrompt = "You are a speech transcription assistant. " +
		"Transcribe the spoken content from the audio exactly and return only the transcription text."
	mimoSTTUserPrompt = "Please transcribe the content of the audio and return only the transcription text."
)

// mimoNormalizeTimeout coerces a config timeout (int, float or string) to a
// positive int, falling back to fallback when absent or invalid.
func mimoNormalizeTimeout(v interface{}, fallback int) int {
	switch t := v.(type) {
	case int:
		if t > 0 {
			return t
		}
	case float64:
		if int(t) > 0 {
			return int(t)
		}
	case string:
		if n, err := strconv.Atoi(t); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

// mimoBuildHeaders builds the standard JSON headers for MiMo requests.
func mimoBuildHeaders(apiKey string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if apiKey != "" {
		h.Set("Authorization", "Bearer "+apiKey)
	}
	return h
}

// mimoBuildAPIURL returns the chat/completions endpoint for an api_base,
// passing through a base that already ends with /chat/completions.
func mimoBuildAPIURL(apiBase string) string {
	base := strings.TrimRight(apiBase, "/")
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	return base + "/chat/completions"
}

// mimoTempDir creates and returns the shared temp directory.
func mimoTempDir() string {
	dir := filepath.Join("data", "temp")
	_ = os.MkdirAll(dir, 0755)
	return dir
}

// mimoValidateWAV rejects audio payloads whose bytes are not RIFF/WAVE,
// mirroring mimo_api_common._validate_wav_payload.
func mimoValidateWAV(data []byte) error {
	if len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WAVE")) {
		return nil
	}
	return fmt.Errorf("audio for MiMo STT could not be converted to WAV (unrecognized audio bytes)")
}

// mimoDataURL wraps raw audio bytes into a base64 data URL.
func mimoDataURL(data []byte, mediaType string) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// mimoFetchAudio resolves an audio URL or local path to a local file,
// returning a cleanup func for any temporary file it created.
func mimoFetchAudio(ctx context.Context, client *http.Client, audioURL string) (string, func(), error) {
	noop := func() {}
	if !strings.HasPrefix(audioURL, "http://") && !strings.HasPrefix(audioURL, "https://") {
		if info, err := os.Stat(audioURL); err != nil {
			return "", noop, fmt.Errorf("audio file not found: %s", audioURL)
		} else if info.IsDir() {
			return "", noop, fmt.Errorf("audio path is a directory: %s", audioURL)
		}
		return audioURL, noop, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, audioURL, nil)
	if err != nil {
		return "", noop, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", noop, fmt.Errorf("download audio: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", noop, fmt.Errorf("download audio: HTTP %d", resp.StatusCode)
	}

	ext := filepath.Ext(strings.Split(audioURL, "?")[0])
	if ext == "" {
		ext = ".wav"
	}
	path := filepath.Join(mimoTempDir(), fmt.Sprintf("mimo_stt_%d%s", time.Now().UnixNano(), ext))
	f, err := os.Create(path)
	if err != nil {
		return "", noop, err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxAudioBytes+1))
	_ = f.Close()
	if err != nil {
		_ = os.Remove(path)
		return "", noop, err
	}
	if n > maxAudioBytes {
		_ = os.Remove(path)
		return "", noop, fmt.Errorf("audio exceeds %d bytes", maxAudioBytes)
	}
	return path, func() { _ = os.Remove(path) }, nil
}

// randomUUID returns a random RFC-4122 v4 UUID string.
func randomUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d-%d-%d-%d-%d", time.Now().UnixNano(), 0, 0, 0, 0)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
