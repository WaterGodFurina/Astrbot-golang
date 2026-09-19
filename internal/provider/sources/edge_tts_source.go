package sources

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	"github.com/gorilla/websocket"
)

const (
	edgeTTSDefaultWSURL = "wss://speech.platform.bing.com/consumer/speech/synthesize/readaloud/edge/v1"
	edgeTTSToken        = "6A5AA1D4EAFF4E9FB37E23D68491D6F4"
	edgeTTSDefaultVoice = "zh-CN-XiaoxiaoNeural"
	// edgeTTSDefaultChromiumVersion 用于 Sec-MS-GEC-Version（1-<chromium 版本>）。
	// 微软会随 Edge 更新提升该版本号，可通过 edge-tts-sec-ms-gec-version 配置覆盖。
	edgeTTSDefaultChromiumVersion = "130.0.2849.68"
	// edgeTTSTicksPer5Min 是 Sec-MS-GEC 时间戳向下取整窗口（5 分钟，单位 100ns）。
	edgeTTSTicksPer5Min = int64(5 * 60 * 10000000)
)

// EdgeTTSSource synthesizes speech using the free Microsoft Edge
// readaloud WebSocket service (edge-tts protocol).
// Ported from astrbot/core/provider/sources/edge_tts_source.py
// Note: the Python version converts the resulting mp3 to wav via ffmpeg;
// this Go port saves the mp3 stream directly.
type EdgeTTSSource struct {
	*provider.BaseProvider
	voice           string
	rate            string
	volume          string
	pitch           string
	wsURL           string
	timeout         time.Duration
	secMSGECVersion string
}

// NewEdgeTTSSource creates an Edge TTS provider.
func NewEdgeTTSSource(config, settings map[string]interface{}) *EdgeTTSSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &EdgeTTSSource{
		BaseProvider:    bp,
		voice:           configString(config, "edge-tts-voice", edgeTTSDefaultVoice),
		rate:            configString(config, "rate", "+0%"),
		volume:          configString(config, "volume", "+0%"),
		pitch:           configString(config, "pitch", "+0Hz"),
		wsURL:           configString(config, "edge-tts-ws-url", edgeTTSDefaultWSURL),
		timeout:         time.Duration(configInt(config, "timeout", 30)) * time.Second,
		secMSGECVersion: configString(config, "edge-tts-sec-ms-gec-version", edgeTTSDefaultChromiumVersion),
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

// buildURL appends the TrustedClientToken, Sec-MS-GEC（DRM 校验）与
// ConnectionId query params.
func (s *EdgeTTSSource) buildURL() (string, error) {
	u, err := url.Parse(s.wsURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("TrustedClientToken", edgeTTSToken)
	// 微软 2024 起要求 Sec-MS-GEC/Sec-MS-GEC-Version，缺失会 403。
	q.Set("Sec-MS-GEC", generateSecMSGEC())
	q.Set("Sec-MS-GEC-Version", "1-"+s.secMSGECVersion)
	q.Set("ConnectionId", ttsUUID())
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// generateSecMSGEC 计算 Sec-MS-GEC（对齐 edge-tts DRM.generate_sec_ms_gec）：
// 当前时间转 Windows FILETIME ticks（100ns），向下取整到 5 分钟，拼接
// TrustedClientToken 后取 SHA256 大写十六进制。
func generateSecMSGEC() string {
	// Unix 纳秒 /100 = 100ns ticks；11644473600s = 1601-01-01 到 1970-01-01。
	ticks := time.Now().UnixNano()/100 + 11644473600*10000000
	ticks -= ticks % edgeTTSTicksPer5Min
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d%s", ticks, edgeTTSToken)))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// generateEdgeDate 生成 edge-tts 协议使用的 UTC 时间戳字符串
// （对齐 edge-tts generate_date，必须为 UTC，本地时区会被服务端拒绝）。
func generateEdgeDate() string {
	return time.Now().UTC().Format("Mon Jan 02 2006 15:04:05 GMT+0000 (Coordinated Universal Time)")
}

// GetAudio synthesizes speech over WebSocket and returns the mp3 file path.
func (s *EdgeTTSSource) GetAudio(ctx context.Context, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("text is empty")
	}
	dialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: s.timeout,
	}
	var conn *websocket.Conn
	var err error
	// Sec-MS-GEC 会随时间窗/版本失效导致握手 403：重新生成后重试一次。
	for attempt := 0; attempt < 2; attempt++ {
		var wsURL string
		wsURL, err = s.buildURL()
		if err != nil {
			return "", err
		}
		conn, _, err = dialer.DialContext(ctx, wsURL, nil)
		if err == nil {
			break
		}
		if attempt == 0 && strings.Contains(err.Error(), "403") {
			logger.Warn("edge_tts 握手 403（Sec-MS-GEC 过期或版本不符），重新生成后重试")
			continue
		}
		return "", fmt.Errorf("edge_tts 连接失败: %w", err)
	}
	if conn == nil {
		return "", fmt.Errorf("edge_tts 连接失败: %w", err)
	}
	defer conn.Close()

	ts := generateEdgeDate()
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

	// 服务端挂起时 ReadMessage 会永久阻塞：一是 ctx 取消后由 goroutine 关闭
	// 连接解除阻塞，二是每条消息后续期 SetReadDeadline 兜底，防止无 ctx 取消
	// 的调用方被无限挂住。
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	var audio bytes.Buffer
	for {
		if err := conn.SetReadDeadline(time.Now().Add(s.timeout)); err != nil {
			return "", fmt.Errorf("edge_tts 设置读超时失败: %w", err)
		}
		mt, msg, rerr := conn.ReadMessage()
		if rerr != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("edge_tts 读取失败: %w", ctx.Err())
			}
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
