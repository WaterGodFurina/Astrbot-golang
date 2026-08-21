package sources

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- md5 用于 Azure 在线 TTS（OTTS）签名字段，属厂商协议强制要求，非密码学用途
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

const (
	azureTokenURLFormat    = "https://%s.api.cognitive.microsoft.com/sts/v1.0/issuetoken"
	azureEndpointURLFormat = "https://%s.tts.speech.microsoft.com/cognitiveservices/v1"
	azureOutputFormat      = "riff-48khz-16bit-mono-pcm"
)

var (
	azureKeyPattern     = regexp.MustCompile(`^[a-zA-Z0-9]{32}$|^[a-zA-Z0-9]{84}$`)
	azureOTTSJSONRegexp = regexp.MustCompile(`(?s)other\[(.*)\]`)
)

// azureOTTSConfig holds settings for the third-party OTTS backend (other[...]).
type azureOTTSConfig struct {
	skey        string
	apiURL      string
	authTimeURL string
}

// AzureTTSProvider synthesizes speech via Microsoft Azure Speech REST.
// Ported from astrbot/core/provider/sources/azure_tts_source.py
// Two backends are supported, selected by the subscription key format:
//   - native Azure: key is a 32/84-char subscription key, uses the standard
//     Microsoft Speech synthesis endpoint with a bearer token.
//   - OTTS: key is "other[{...}]" JSON pointing at a third-party OTTS service
//     (signature based auth).
type AzureTTSProvider struct {
	*provider.BaseProvider
	client  *http.Client
	initErr error

	// mu 保护 token/tokenExpire（native 模式）与 timeOffset/lastSync（OTTS
	// 模式）的读写，GetAudio 可能被并发调用。
	mu sync.Mutex

	// native Azure mode
	subscriptionKey string
	region          string
	endpoint        string
	tokenURL        string
	token           string
	tokenExpire     time.Time

	// OTTS mode
	otts *azureOTTSConfig
	// OTTS time sync state
	timeOffset int64
	lastSync   time.Time

	voice  string
	style  string
	role   string
	rate   string
	volume string
}

// NewAzureTTSSource creates an Azure TTS provider.
func NewAzureTTSSource(config, settings map[string]interface{}) *AzureTTSProvider {
	bp := provider.NewBaseProvider(config, settings)
	s := &AzureTTSProvider{
		BaseProvider: bp,
		client:       &http.Client{Timeout: 30 * time.Second},
		region:       strings.TrimSpace(configString(config, "azure_tts_region", "eastus")),
		voice:        configString(config, "azure_tts_voice", "zh-CN-YunxiaNeural"),
		style:        configString(config, "azure_tts_style", "cheerful"),
		role:         configString(config, "azure_tts_role", "Boy"),
		rate:         configString(config, "azure_tts_rate", "1"),
		volume:       configString(config, "azure_tts_volume", "100"),
	}
	s.endpoint = fmt.Sprintf(azureEndpointURLFormat, s.region)
	s.tokenURL = fmt.Sprintf(azureTokenURLFormat, s.region)

	keyValue := strings.TrimSpace(configString(config, "azure_tts_subscription_key", ""))
	switch {
	case strings.HasPrefix(strings.ToLower(keyValue), "other["):
		otts, err := s.parseOTTS(keyValue)
		if err != nil {
			s.initErr = err
		} else {
			s.otts = &otts
		}
	case azureKeyPattern.MatchString(keyValue):
		s.subscriptionKey = keyValue
	default:
		s.initErr = fmt.Errorf("无效的Azure订阅密钥: 应为32位或84位字母数字, 或 other[{...}] 格式")
	}

	if s.GetModel() == "" {
		s.SetModel("azure_tts")
	}
	s.SetCapability(provider.CapTextToSpeech)
	return s
}

func (s *AzureTTSProvider) parseOTTS(keyValue string) (azureOTTSConfig, error) {
	var cfg azureOTTSConfig
	match := azureOTTSJSONRegexp.FindStringSubmatch(keyValue)
	if len(match) < 2 {
		return cfg, fmt.Errorf("无效的other[...]格式，应形如 other[{...}]")
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(match[1])), &raw); err != nil {
		return cfg, fmt.Errorf("OTTS JSON解析失败: %w", err)
	}
	cfg.skey = configString(raw, "OTTS_SKEY", "")
	cfg.apiURL = configString(raw, "OTTS_URL", "")
	cfg.authTimeURL = configString(raw, "OTTS_AUTH_TIME", "")
	if cfg.skey == "" || cfg.apiURL == "" || cfg.authTimeURL == "" {
		return cfg, fmt.Errorf("缺少OTTS参数: 需要 OTTS_SKEY, OTTS_URL, OTTS_AUTH_TIME")
	}
	return cfg, nil
}

// syncTime queries the OTTS server time and caches the offset. A sync failure
// is only fatal when the last successful sync is older than one hour.
func (s *AzureTTSProvider) syncTime(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.otts.authTimeURL, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if s.syncAge() > time.Hour {
			return fmt.Errorf("时间同步失败: %w", err)
		}
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if s.syncAge() > time.Hour {
			return fmt.Errorf("时间同步失败: 状态码 %d", resp.StatusCode)
		}
		return nil
	}
	var m struct {
		Timestamp int64 `json:"timestamp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		if s.syncAge() > time.Hour {
			return fmt.Errorf("时间同步响应解析失败: %w", err)
		}
		return nil
	}
	s.mu.Lock()
	s.timeOffset = m.Timestamp - time.Now().Unix()
	s.lastSync = time.Now()
	s.mu.Unlock()
	return nil
}

// syncAge returns how long ago the last successful OTTS time sync happened.
func (s *AzureTTSProvider) syncAge() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastSync)
}

// generateSignature builds the OTTS signature: {timestamp}-{nonce}-0-{md5}.
func (s *AzureTTSProvider) generateSignature(ctx context.Context) (string, error) {
	if err := s.syncTime(ctx); err != nil {
		return "", err
	}
	s.mu.Lock()
	offset := s.timeOffset
	s.mu.Unlock()
	timestamp := time.Now().Unix() + offset
	nonce := ttsRandomNonce(10)
	path := "/"
	if u, err := url.Parse(s.otts.apiURL); err == nil && u.Path != "" {
		path = u.Path
	}
	digest := md5.Sum([]byte(fmt.Sprintf("%s-%d-%s-0-%s", path, timestamp, nonce, s.otts.skey))) // #nosec G401 -- Azure OTTS 协议规定的签名字符串格式（对应 Python 原版实现），非密码学哈希用途; nosemgrep: go.lang.security.audit.crypto.use_of_weak_crypto.use-of-md5
	return fmt.Sprintf("%d-%s-0-%s", timestamp, nonce, hex.EncodeToString(digest[:])), nil
}

// refreshToken obtains a new bearer token from the STS endpoint.
func (s *AzureTTSProvider) refreshToken(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Ocp-Apim-Subscription-Key", s.subscriptionKey)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("azure token获取失败 %d: %s", resp.StatusCode, string(data))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.token = strings.TrimSpace(string(data))
	s.tokenExpire = time.Now().Add(540 * time.Second)
	s.mu.Unlock()
	return nil
}

// nativeTokenValid reports whether a cached native bearer token is present and
// not yet expired.
func (s *AzureTTSProvider) nativeTokenValid() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token != "" && time.Now().Before(s.tokenExpire)
}

// getNativeAudio synthesizes via the Microsoft Speech REST endpoint.
func (s *AzureTTSProvider) getNativeAudio(ctx context.Context, text string) (string, error) {
	if !s.nativeTokenValid() {
		if err := s.refreshToken(ctx); err != nil {
			return "", err
		}
	}
	s.mu.Lock()
	token := s.token
	s.mu.Unlock()
	if token == "" {
		return "", fmt.Errorf("azure token 为空")
	}
	ssml := azureBuildSSML(s.voice, s.style, s.role, s.rate, s.volume, text)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader([]byte(ssml)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/ssml+xml")
	req.Header.Set("X-Microsoft-OutputFormat", azureOutputFormat)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "AstrBot/Go")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("azure TTS API error %d: %s", resp.StatusCode, string(data))
	}
	return ttsSaveAudio(resp.Body, "azure_tts", "wav")
}

// getOTTSAudio synthesizes via the third-party OTTS service.
func (s *AzureTTSProvider) getOTTSAudio(ctx context.Context, text string) (string, error) {
	sig, err := s.generateSignature(ctx)
	if err != nil {
		return "", err
	}
	form := url.Values{}
	form.Set("text", text)
	form.Set("voice", s.voice)
	form.Set("style", s.style)
	form.Set("role", s.role)
	form.Set("rate", s.rate)
	form.Set("volume", s.volume)
	apiURL := s.otts.apiURL + "?sign=" + url.QueryEscape(sig)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "AstrBot/Go")
	req.Header.Set("UAK", "AstrBot/AzureTTS")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("OTTS API error %d: %s", resp.StatusCode, string(data))
	}
	return ttsSaveAudio(resp.Body, "azure_tts", "wav")
}

// azureBuildSSML builds the SSML document sent to the Speech service.
func azureBuildSSML(voice, style, role, rate, volume, text string) string {
	esc := html.EscapeString
	return fmt.Sprintf(`<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis'
            xmlns:mstts='http://www.w3.org/2001/mstts' xml:lang='zh-CN'>
            <voice name='%s'>
                <mstts:express-as style='%s'
                    role='%s'>
                    <prosody rate='%s'
                        volume='%s'>
                        %s
                    </prosody>
                </mstts:express-as>
            </voice>
        </speak>`, esc(voice), esc(style), esc(role), esc(rate), esc(volume), esc(text))
}

// GetAudio synthesizes speech and returns the path to the generated wav file.
func (s *AzureTTSProvider) GetAudio(ctx context.Context, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("text is empty")
	}
	if s.initErr != nil {
		return "", s.initErr
	}
	if s.otts != nil {
		return s.getOTTSAudio(ctx, text)
	}
	return s.getNativeAudio(ctx, text)
}

// SupportStream reports whether the provider supports streaming audio output.
func (s *AzureTTSProvider) SupportStream() bool { return false }

// Test verifies the provider configuration.
func (s *AzureTTSProvider) Test(ctx context.Context) error {
	return s.initErr
}
