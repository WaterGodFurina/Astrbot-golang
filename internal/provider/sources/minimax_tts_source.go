package sources

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

// MiniMaxTTSSource synthesizes speech via the MiniMax /v1/t2a_v2 endpoint
// (streaming SSE response carrying hex-encoded audio).
// Ported from astrbot/core/provider/sources/minimax_tts_api_source.py
type MiniMaxTTSSource struct {
	provider.BaseProvider
	client         *http.Client
	apiKey         string
	apiBase        string
	groupID        string
	langBoost      string
	isTimberWeight bool
	timberWeight   []interface{}
	voiceSetting   map[string]interface{}
	audioSetting   map[string]interface{}
}

// NewMiniMaxTTSSource creates a MiniMax TTS provider.
func NewMiniMaxTTSSource(config, settings map[string]interface{}) *MiniMaxTTSSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &MiniMaxTTSSource{
		BaseProvider:   bp,
		apiBase:        strings.TrimSuffix(configString(config, "api_base", "https://api.minimax.chat/v1/t2a_v2"), "/"),
		apiKey:         configString(config, "api_key", ""),
		groupID:        configString(config, "minimax-group-id", ""),
		langBoost:      configString(config, "minimax-langboost", "auto"),
		isTimberWeight: ttsConfigBool(config, "minimax-is-timber-weight", false),
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
	s.SetModel(configString(config, "model", ""))

	// timber weights (voice cloning), JSON string or structured value.
	if v, ok := config["minimax-timber-weight"].([]interface{}); ok {
		s.timberWeight = v
	} else if raw := configString(config, "minimax-timber-weight", ""); raw != "" {
		var tw []interface{}
		if err := json.Unmarshal([]byte(raw), &tw); err != nil {
			tw = minimaxDefaultTimberWeight()
		}
		s.timberWeight = tw
	} else {
		s.timberWeight = minimaxDefaultTimberWeight()
	}

	voiceID := configString(config, "minimax-voice-id", "")
	if s.isTimberWeight {
		voiceID = ""
	}
	s.voiceSetting = map[string]interface{}{
		"speed":                 ttsConfigFloat(config, "minimax-voice-speed", 1.0),
		"vol":                   ttsConfigFloat(config, "minimax-voice-vol", 1.0),
		"pitch":                 ttsConfigFloat(config, "minimax-voice-pitch", 0),
		"voice_id":              voiceID,
		"latex_read":            ttsConfigBool(config, "minimax-voice-latex", false),
		"english_normalization": ttsConfigBool(config, "minimax-voice-english-normalization", false),
	}
	if emotion := configString(config, "minimax-voice-emotion", "auto"); emotion != "auto" {
		s.voiceSetting["emotion"] = emotion
	}

	s.audioSetting = map[string]interface{}{
		"sample_rate": 32000,
		"bitrate":     128000,
		"format":      "wav",
	}

	s.SetCapability(provider.CapTextToSpeech)
	return s
}

func minimaxDefaultTimberWeight() []interface{} {
	return []interface{}{
		map[string]interface{}{"voice_id": "Chinese (Mandarin)_Warm_Girl", "weight": 1},
	}
}

// buildStreamBody builds the streaming request JSON body.
func (s *MiniMaxTTSSource) buildStreamBody(text string) ([]byte, error) {
	body := map[string]interface{}{
		"model":          s.GetModel(),
		"text":           text,
		"stream":         true,
		"language_boost": s.langBoost,
		"voice_setting":  s.voiceSetting,
		"audio_setting":  s.audioSetting,
	}
	if s.isTimberWeight {
		body["timber_weights"] = s.timberWeight
	}
	return json.Marshal(body)
}

// GetAudio synthesizes speech and returns the path to the generated wav file.
func (s *MiniMaxTTSSource) GetAudio(ctx context.Context, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("text is empty")
	}
	payload, err := s.buildStreamBody(text)
	if err != nil {
		return "", err
	}
	url := s.apiBase + "?GroupId=" + url.QueryEscape(s.groupID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("MiniMax TTS API error %d: %s", resp.StatusCode, string(data))
	}

	var audio bytes.Buffer
	rd := bufio.NewReader(resp.Body)
	for {
		line, rerr := rd.ReadString('\n')
		if line != "" {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data: ") {
				if err := s.collectAudioMessage([]byte(strings.TrimPrefix(line, "data: ")), &audio); err != nil {
					return "", err
				}
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return "", rerr
		}
	}
	if audio.Len() == 0 {
		return "", fmt.Errorf("MiniMax TTS API returned empty audio data. 请检查配置, 尤其是 'group_id' 参数")
	}
	return ttsSaveAudio(bytes.NewReader(audio.Bytes()), "minimax_tts_api", "wav")
}

// collectAudioMessage extracts hex-encoded audio from an SSE data message.
func (s *MiniMaxTTSSource) collectAudioMessage(data []byte, audio *bytes.Buffer) error {
	var msg struct {
		ExtraInfo json.RawMessage `json:"extra_info"`
		Data      struct {
			Audio string `json:"audio"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil
	}
	if len(msg.ExtraInfo) > 0 {
		return nil
	}
	if chunk := strings.TrimSpace(msg.Data.Audio); chunk != "" {
		b, err := hex.DecodeString(chunk)
		if err == nil {
			audio.Write(b)
		}
	}
	return nil
}

// SupportStream reports whether the provider supports streaming audio output.
func (s *MiniMaxTTSSource) SupportStream() bool { return false }

// Test verifies the provider configuration.
func (s *MiniMaxTTSSource) Test(ctx context.Context) error {
	return nil
}
