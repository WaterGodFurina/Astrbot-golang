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

var fishReferenceIDPattern = regexp.MustCompile(`^[a-fA-F0-9]{32}$`)

// FishAudioTTSSource synthesizes speech via the FishAudio /v1/tts endpoint.
// Ported from astrbot/core/provider/sources/fishaudio_tts_api_source.py
// The request body is MessagePack encoded (application/msgpack) and the model
// is passed as an HTTP header instead of a body field.
type FishAudioTTSSource struct {
	provider.BaseProvider
	client      *http.Client
	apiKey      string
	apiBase     string
	referenceID string
	character   string
}

// NewFishAudioTTSSource creates a FishAudio TTS provider.
func NewFishAudioTTSSource(config, settings map[string]interface{}) *FishAudioTTSSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &FishAudioTTSSource{
		BaseProvider: bp,
		apiBase:      strings.TrimSuffix(configString(config, "api_base", "https://api.fish-audio.cn/v1"), "/"),
		apiKey:       configString(config, "api_key", ""),
		referenceID:  configString(config, "fishaudio-tts-reference-id", ""),
		character:    configString(config, "fishaudio-tts-character", "可莉"),
		client: &http.Client{
			Timeout: time.Duration(configInt(config, "timeout", 20)) * time.Second,
		},
	}
	if s.GetModel() == "" {
		s.SetModel("s2-pro")
	}
	s.SetCapability(provider.CapTextToSpeech)
	return s
}

// lookupReferenceID queries the /model endpoint to find the reference_id for
// a character title. Returns an empty string when no match is found.
func (s *FishAudioTTSSource) lookupReferenceID(ctx context.Context) (string, error) {
	base := strings.TrimSuffix(s.apiBase, "/v1")
	for _, sortBy := range []string{"score", "task_count", "created_at"} {
		url := base + "/model"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
		req.Header.Set("model", s.GetModel())
		q := req.URL.Query()
		q.Set("title", s.character)
		q.Set("sort_by", sortBy)
		req.URL.RawQuery = q.Encode()

		resp, err := s.client.Do(req)
		if err != nil {
			return "", err
		}
		var data struct {
			Total int `json:"total"`
			Items []struct {
				ID    string `json:"_id"`
				Title string `json:"title"`
			} `json:"items"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&data)
		_ = resp.Body.Close()
		if decodeErr != nil {
			return "", decodeErr
		}
		if data.Total == 0 {
			continue
		}
		for _, item := range data.Items {
			if strings.Contains(item.Title, s.character) {
				return item.ID, nil
			}
		}
	}
	return "", nil
}

// resolveReferenceID returns the configured reference_id or resolves it from
// the character name.
func (s *FishAudioTTSSource) resolveReferenceID(ctx context.Context) (string, error) {
	if rid := strings.TrimSpace(s.referenceID); rid != "" {
		if !fishReferenceIDPattern.MatchString(rid) {
			return "", fmt.Errorf("无效的FishAudio参考模型ID: %q, 应为32位十六进制字符串", rid)
		}
		return rid, nil
	}
	return s.lookupReferenceID(ctx)
}

// buildMsgpackRequest builds the MessagePack encoded /v1/tts request body.
func (s *FishAudioTTSSource) buildMsgpackRequest(text string, referenceID string) []byte {
	var refID []byte
	if referenceID != "" {
		refID = fishMsgpackString(referenceID)
	} else {
		refID = fishMsgpackNil()
	}
	return fishMsgpackMap([][2][]byte{
		{[]byte("text"), fishMsgpackString(text)},
		{[]byte("chunk_length"), fishMsgpackInt(200)},
		{[]byte("format"), fishMsgpackString("wav")},
		{[]byte("mp3_bitrate"), fishMsgpackInt(128)},
		{[]byte("references"), fishMsgpackArray(nil)},
		{[]byte("reference_id"), refID},
		{[]byte("normalize"), fishMsgpackBool(true)},
		{[]byte("latency"), fishMsgpackString("normal")},
	})
}

// GetAudio synthesizes speech and returns the path to the generated wav file.
func (s *FishAudioTTSSource) GetAudio(ctx context.Context, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("text is empty")
	}
	referenceID, err := s.resolveReferenceID(ctx)
	if err != nil {
		return "", err
	}
	payload := s.buildMsgpackRequest(text, referenceID)

	url := s.apiBase + "/tts"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("model", s.GetModel())
	req.Header.Set("Content-Type", "application/msgpack")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "audio/") {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("fish audio API error %d: %s", resp.StatusCode, string(data))
	}
	return ttsSaveAudio(resp.Body, "fishaudio_tts_api", "wav")
}

// SupportStream reports whether the provider supports streaming audio output.
func (s *FishAudioTTSSource) SupportStream() bool { return false }

// Test verifies the provider configuration.
func (s *FishAudioTTSSource) Test(ctx context.Context) error {
	return nil
}

// --- minimal MessagePack encoder (subset used by the /v1/tts request) ---

func fishMsgpackNil() []byte { return []byte{0xc0} }
func fishMsgpackBool(b bool) []byte {
	if b {
		return []byte{0xc3}
	}
	return []byte{0xc2}
}

func fishMsgpackString(s string) []byte {
	l := len(s)
	switch {
	case l < 32:
		return append([]byte{0xa0 | byte(l)}, s...)
	case l <= 0xff:
		return append([]byte{0xd9, byte(l)}, s...)
	case l <= 0xffff:
		return append([]byte{0xda, byte(l >> 8), byte(l)}, s...)
	}
	return append([]byte{0xdb, 0, 0, 0, byte(l >> 24), byte(l >> 16), byte(l >> 8), byte(l)}, s...)
}

func fishMsgpackInt(v int) []byte {
	switch {
	case v >= 0 && v <= 127:
		return []byte{byte(v)}
	case v >= -32 && v < 0:
		return []byte{0xe0 | byte(v)}
	case v >= 0 && v <= 0xff:
		return []byte{0xcc, byte(v)}
	case v >= -128 && v < -32:
		return []byte{0xd0, byte(v)}
	case v >= 0 && v <= 0xffff:
		return []byte{0xcd, byte(v >> 8), byte(v)}
	}
	return []byte{0xd1, byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}

func fishMsgpackArray(items [][]byte) []byte {
	out := []byte{0x90 | byte(len(items))}
	for _, item := range items {
		out = append(out, item...)
	}
	return out
}

func fishMsgpackMap(pairs [][2][]byte) []byte {
	out := []byte{0x80 | byte(len(pairs))}
	for _, kv := range pairs {
		out = append(out, kv[0]...)
		out = append(out, kv[1]...)
	}
	return out
}
