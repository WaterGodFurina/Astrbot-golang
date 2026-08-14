// Package dashboard - Weixin OC (微信开放平台) one-click registration.
// Ported 1:1 from
// astrbot/core/platform/sources/weixin_oc/login_registration.py: request a
// login QR code (ilink/bot/get_bot_qrcode) then poll its status
// (ilink/bot/get_qrcode_status) until the user scans and confirms, returning
// the bot token / account id for the platform config.
package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	weixinOCDefaultBaseURL      = "https://ilinkai.weixin.qq.com"
	weixinOCDefaultCDNBaseURL   = "https://novac2c.cdn.weixin.qq.com/c2c"
	weixinOCDefaultBotType      = "3"
	weixinOCDefaultQRInterval   = 1
	weixinOCDefaultLongPollMS   = 35000
	weixinOCDefaultAPITimeoutMS = 15000
)

// weixinOCStringField reads a string field from an API payload (Python
// _string_field: strips whitespace).
func weixinOCStringField(data map[string]interface{}, key string) string {
	if v, ok := data[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// weixinOCIntConfig parses an int config with a default and minimum
// (Python _int_config).
func weixinOCIntConfig(value interface{}, def, minimum int) int {
	parsed := def
	switch v := value.(type) {
	case float64:
		parsed = int(v)
	case int:
		parsed = v
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			parsed = n
		}
	}
	if parsed < minimum {
		return minimum
	}
	return parsed
}

// weixinOCConfig resolves the config fields used by registration.
type weixinOCConfig struct {
	baseURL    string
	botType    string
	qrInterval int
	longPollMS int
	apiTimeout time.Duration
}

func resolveWeixinOCConfig(platformConfig map[string]interface{}) weixinOCConfig {
	c := weixinOCConfig{
		baseURL:    weixinOCDefaultBaseURL,
		botType:    weixinOCDefaultBotType,
		qrInterval: weixinOCDefaultQRInterval,
		longPollMS: weixinOCDefaultLongPollMS,
		apiTimeout: time.Duration(weixinOCDefaultAPITimeoutMS) * time.Millisecond,
	}
	if v := weixinOCStringField(platformConfig, "weixin_oc_base_url"); v != "" {
		c.baseURL = strings.TrimRight(v, "/")
	}
	if v := weixinOCStringField(platformConfig, "weixin_oc_bot_type"); v != "" {
		c.botType = v
	}
	c.qrInterval = weixinOCIntConfig(platformConfig["weixin_oc_qr_poll_interval"], weixinOCDefaultQRInterval, 1)
	c.longPollMS = weixinOCIntConfig(platformConfig["weixin_oc_long_poll_timeout_ms"], weixinOCDefaultLongPollMS, 1000)
	if v := weixinOCIntConfig(platformConfig["weixin_oc_api_timeout_ms"], weixinOCDefaultAPITimeoutMS, 1000); v > 0 {
		c.apiTimeout = time.Duration(v) * time.Millisecond
	}
	return c
}

// weixinOCRequestJSON performs an ilink/bot/* API request (mirrors
// WeixinOCClient.request_json; no token needed for QR flows).
func weixinOCRequestJSON(ctx context.Context, baseURL, endpoint string, params url.Values, timeout time.Duration) (map[string]interface{}, error) {
	u := baseURL + "/" + strings.TrimLeft(endpoint, "/")
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("X-WECHAT-UIN", base64URLSafe(8))
	if endpoint == "ilink/bot/get_qrcode_status" {
		req.Header.Set("iLink-App-ClientVersion", "1")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s failed: %d %s", endpoint, resp.StatusCode, string(body))
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return map[string]interface{}{}, nil
	}
	var out map[string]interface{}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode %s: %w", endpoint, err)
	}
	return out, nil
}

// weixinOCLoginResult maps the qrcode_status response to the registration
// result (Python weixin_oc_login_result).
func weixinOCLoginResult(data map[string]interface{}, defaultBaseURL string) map[string]interface{} {
	rawStatus := weixinOCStringField(data, "status")
	if rawStatus == "" {
		rawStatus = "wait"
	}
	switch rawStatus {
	case "confirmed":
		botToken := weixinOCStringField(data, "bot_token")
		if botToken == "" {
			return map[string]interface{}{"status": "error", "message": "登录成功但未返回 token"}
		}
		baseURL := weixinOCStringField(data, "baseurl")
		if baseURL == "" {
			baseURL = defaultBaseURL
		}
		return map[string]interface{}{
			"status":               "created",
			"qr_status":            rawStatus,
			"weixin_oc_token":      botToken,
			"weixin_oc_account_id": weixinOCStringField(data, "ilink_bot_id"),
			"weixin_oc_base_url":   strings.TrimRight(baseURL, "/"),
			"weixin_oc_user_id":    weixinOCStringField(data, "ilink_user_id"),
			"platform_id_suffix":   randomPlatformIDSuffix(),
		}
	case "expired":
		return map[string]interface{}{"status": "expired", "qr_status": rawStatus, "message": "二维码已过期"}
	case "cancel", "canceled", "denied":
		return map[string]interface{}{"status": "denied", "qr_status": rawStatus, "message": "用户取消登录"}
	default:
		return map[string]interface{}{"status": "pending", "qr_status": rawStatus}
	}
}

// weixinOCStart requests a login QR code (Python request_weixin_oc_login_qr).
func weixinOCStart(platformConfig map[string]interface{}) (map[string]interface{}, error) {
	cfg := resolveWeixinOCConfig(platformConfig)
	ctx := context.Background()
	data, err := weixinOCRequestJSON(ctx, cfg.baseURL, "ilink/bot/get_bot_qrcode",
		url.Values{"bot_type": []string{cfg.botType}}, 15*time.Second)
	if err != nil {
		return nil, err
	}
	qrcode := weixinOCStringField(data, "qrcode")
	imgContent := weixinOCStringField(data, "qrcode_img_content")
	if qrcode == "" || imgContent == "" {
		return nil, fmt.Errorf("个人微信二维码响应格式异常")
	}
	return map[string]interface{}{
		"status":             "pending",
		"registration_code":  qrcode,
		"qrcode":             qrcode,
		"qrcode_img_content": imgContent,
		"interval":           cfg.qrInterval,
	}, nil
}

// weixinOCPoll polls the QR status (Python poll_weixin_oc_login_once).
func weixinOCPoll(platformConfig map[string]interface{}, qrcode string) (map[string]interface{}, error) {
	qrcode = strings.TrimSpace(qrcode)
	if qrcode == "" {
		return nil, fmt.Errorf("缺少 qrcode")
	}
	cfg := resolveWeixinOCConfig(platformConfig)
	ctx := context.Background()
	data, err := weixinOCRequestJSON(ctx, cfg.baseURL, "ilink/bot/get_qrcode_status",
		url.Values{"qrcode": []string{qrcode}}, cfg.apiTimeout)
	if err != nil {
		return nil, err
	}
	return weixinOCLoginResult(data, cfg.baseURL), nil
}

// weixinOCRegistration dispatches the one-click registration for weixin_oc.
func (s *Server) weixinOCRegistration(action string, platformConfig map[string]interface{}, qrcode string) (map[string]interface{}, error) {
	switch action {
	case "start":
		return weixinOCStart(platformConfig)
	case "poll":
		return weixinOCPoll(platformConfig, qrcode)
	}
	return nil, fmt.Errorf("不支持的 action: %s", action)
}

// randomPlatformIDSuffix mirrors Python random_platform_id_suffix (8 hex chars).
func randomPlatformIDSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func base64URLSafe(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
