package sources

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/provider"
	"github.com/gorilla/websocket"
)

const (
	edgeTTSDefaultWSURL = "wss://speech.platform.bing.com/consumer/speech/synthesize/readaloud/edge/v1"
	edgeTTSToken        = "6A5AA1D4EAFF4E9FB37E23D68491D6F4"
	edgeTTSDefaultVoice = "zh-CN-XiaoxiaoNeural"
)

// EdgeTTSSource synthesizes speech using the free Microsoft Edge
// readaloud WebSocket service (edge-tts protocol).
// Ported from astrbot/core/provider/sources/edge_tts_source.py
// Note: the Python version converts the resulting mp3 to wav via ffmpeg;
// this Go port saves the mp3 stream directly.
type EdgeTTSSource struct {
	provider.BaseProvider
	voice   string
	rate    string
	volume  string
	pitch   string
	wsURL   string
	timeout time.Duration
}

// NewEdgeTTSSource creates an Edge TTS provider.
func NewEdgeTTSSource(config, settings map[string]interface{}) *EdgeTTSSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &EdgeTTSSource{
		BaseProvider: bp,
		voice:        configString(config, "edge-tts-voice", edgeTTSDefaultVoice),
		rate:         configString(config, "rate", "+0%"),
		volume:       configString(config, "volume", "+0%"),
		pitch:        configString(config, "pitch", "+0Hz"),
		wsURL:        configString(config, "edge-tts-ws-url", edgeTTSDefaultWSURL),
		timeout:      time.Duration(configInt(config, "timeout", 30)) * time.Second,
	}
	if s.rate == "" {
		s.rate = "+0%"
	}
	if s.volume == "" {
		s.volume = "+0%"
	}
	if s.pitch == "" {
		s.pitch = "+0Hz"
	}
	s.SetModel("edge_tts")
	s.SetCapability(provider.CapTextToSpeech)
	return s
}

// buildURL appends the TrustedClientToken and ConnectionId query params.
func (s *EdgeTTSSource) buildURL() (string, error) {
	u, err := url.Parse(s.wsURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("TrustedClientToken", edgeTTSToken)
	q.Set("ConnectionId", ttsUUID())
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// GetAudio synthesizes speech over WebSocket and returns the mp3 file path.
func (s *EdgeTTSSource) GetAudio(ctx context.Context, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("text is empty")
	}
	wsURL, err := s.buildURL()
	if err != nil {
		return "", err
	}
	dialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: s.timeout,
	}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return "", fmt.Errorf("edge_tts 连接失败: %w", err)
	}
	defer conn.Close()

	ts := time.Now().Format(time.RFC3339)
	reqID := ttsUUID()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(
		fmt.Sprintf("X-Timestamp:%s\r\nContent-Type:application/json; charset=utf-8\r\nPath:speech.config\r\n\r\n%s",
			ts, `{"context":{"synthesis":{"audio":{"metadataoptions":{"sentenceBoundaryEnabled":"false","wordBoundaryEnabled":"false"},"outputFormat":"audio-24khz-48kbitrate-mono-mp3"}}}}`),
	)); err != nil {
		return "", fmt.Errorf("edge_tts 发送speech.config失败: %w", err)
	}

	ssml := fmt.Sprintf("<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis' xml:lang='zh-CN'><voice name='%s'><prosody pitch='%s' rate='%s' volume='%s'>%s</prosody></voice></speak>",
		html.EscapeString(s.voice), html.EscapeString(s.pitch), html.EscapeString(s.rate), html.EscapeString(s.volume), html.EscapeString(text))
	if err := conn.WriteMessage(websocket.TextMessage, []byte(
		fmt.Sprintf("X-RequestId:%s\r\nContent-Type:application/ssml+xml\r\nX-Timestamp:%s\r\nPath:ssml\r\n\r\n%s", reqID, ts, ssml),
	)); err != nil {
		return "", fmt.Errorf("edge_tts 发送ssml失败: %w", err)
	}

	var audio bytes.Buffer
	for {
		mt, msg, rerr := conn.ReadMessage()
		if rerr != nil {
			return "", fmt.Errorf("edge_tts 读取失败: %w", rerr)
		}
		if mt == websocket.BinaryMessage {
			if chunk := edgeParseAudioFrame(msg); len(chunk) > 0 {
				audio.Write(chunk)
			}
			continue
		}
		if strings.Contains(string(msg), "Path:turn.end") {
			break
		}
	}
	if audio.Len() == 0 {
		return "", fmt.Errorf("edge_tts 未返回音频数据")
	}
	return ttsSaveAudio(bytes.NewReader(audio.Bytes()), "edge_tts", "mp3")
}

// edgeParseAudioFrame strips the 2-byte big-endian header length and header
// text from a binary audio frame, returning the raw audio bytes.
func edgeParseAudioFrame(b []byte) []byte {
	if len(b) < 2 {
		return nil
	}
	headerLen := int(binary.BigEndian.Uint16(b[:2]))
	if 2+headerLen > len(b) {
		return nil
	}
	return b[2+headerLen:]
}

// SupportStream reports whether the provider supports streaming audio output.
func (s *EdgeTTSSource) SupportStream() bool { return false }

// Test verifies the provider configuration.
func (s *EdgeTTSSource) Test(ctx context.Context) error {
	return nil
}
