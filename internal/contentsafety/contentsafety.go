// Package contentsafety implements content safety checking.
// Ported from astrbot/core/pipeline/content_safety_check/stage.py
// and astrbot/core/pipeline/content_safety_check/strategies/
package contentsafety

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Strategy identifies the content safety check method.
type Strategy string

const (
	StrategyKeyword Strategy = "keyword"
	StrategyBaidu   Strategy = "baidu_aip"
	StrategyOff     Strategy = "off"
)

// Checker is the interface for content safety strategies.
type Checker interface {
	Check(text string) (bool, string)
}

// KeywordChecker blocks text containing banned keywords.
type KeywordChecker struct {
	mu       sync.RWMutex
	keywords []string
}

// NewKeywordChecker creates a keyword-based checker.
func NewKeywordChecker(keywords []string) *KeywordChecker {
	return &KeywordChecker{keywords: keywords}
}

// Check returns (ok, info). ok=false if a keyword is found.
func (c *KeywordChecker) Check(text string) (bool, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	lower := strings.ToLower(text)
	for _, kw := range c.keywords {
		if kw == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(kw)) {
			return false, "keyword matched: " + kw
		}
	}
	return true, ""
}

// AddKeyword adds a banned keyword.
func (c *KeywordChecker) AddKeyword(kw string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keywords = append(c.keywords, kw)
}

// SetKeywords replaces all keywords.
func (c *KeywordChecker) SetKeywords(kws []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keywords = kws
}

// BaiduAipChecker checks text via the Baidu AI content moderation API
// (port of the baidu-aip SDK's textCensorUserDefined, which the Python
// adapter uses through the aip_content_censor module).
type BaiduAipChecker struct {
	appID    string
	apiKey   string
	secretKey string
	client   *http.Client

	tokenMu   sync.RWMutex
	token     string
	tokenExp  time.Time
}

// NewBaiduAipChecker creates a Baidu AI content moderation checker.
func NewBaiduAipChecker(appID, apiKey, secretKey string) *BaiduAipChecker {
	return &BaiduAipChecker{
		appID:     appID,
		apiKey:    apiKey,
		secretKey: secretKey,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// accessToken returns a cached OAuth token, refreshing it when expired.
func (c *BaiduAipChecker) accessToken(ctx context.Context) (string, error) {
	c.tokenMu.RLock()
	if c.token != "" && time.Now().Before(c.tokenExp) {
		t := c.token
		c.tokenMu.RUnlock()
		return t, nil
	}
	c.tokenMu.RUnlock()

	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExp) {
		return c.token, nil
	}

	u := "https://aip.baidubce.com/oauth/2.0/token?grant_type=client_credentials" +
		"&client_id=" + url.QueryEscape(c.apiKey) +
		"&client_secret=" + url.QueryEscape(c.secretKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("baidu aip token error: %s %s", body.Error, body.ErrorDesc)
	}
	exp := body.ExpiresIn
	if exp <= 0 {
		exp = 2592000 // default 30 days
	}
	c.token = body.AccessToken
	c.tokenExp = time.Now().Add(time.Duration(exp) * time.Second).Add(-time.Hour)
	return c.token, nil
}

// Check returns (ok, info). ok=false if the text is flagged.
func (c *BaiduAipChecker) Check(text string) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	token, err := c.accessToken(ctx)
	if err != nil {
		// Fail open on transport errors so a transient API outage does not
		// block all messages (Python logs the exception and returns ok).
		return true, "baidu aip token error: " + err.Error()
	}

	form := url.Values{}
	form.Set("text", text)
	u := "https://aip.baidubce.com/rest/2.0/solution/v1/text_censor/user_defined?access_token=" +
		url.QueryEscape(token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return true, "baidu aip request error: " + err.Error()
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.client.Do(req)
	if err != nil {
		return true, "baidu aip request error: " + err.Error()
	}
	defer resp.Body.Close()

	var body struct {
		Conclusion string `json:"conclusion"`
		ConclusionType int  `json:"conclusionType"`
		ErrorCode  int    `json:"error_code"`
		ErrorMsg   string `json:"error_msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return true, "baidu aip decode error: " + err.Error()
	}
	if body.ErrorCode != 0 {
		return true, fmt.Sprintf("baidu aip error(%d): %s", body.ErrorCode, body.ErrorMsg)
	}
	if body.Conclusion == "不合规" || body.ConclusionType == 2 {
		return false, "baidu aip flagged: " + body.Conclusion
	}
	return true, ""
}

// StrategySelector selects the enabled checkers based on config.
// Mirrors the Python StrategySelector: internal_keywords.enable enables the
// keyword strategy and baidu_aip.enable enables the Baidu AI strategy.
type StrategySelector struct {
	checkers []Checker
	enabled  bool
}

// NewStrategySelector creates a selector from the content_safety config map.
func NewStrategySelector(config map[string]interface{}) *StrategySelector {
	s := &StrategySelector{}

	if ik, ok := config["internal_keywords"].(map[string]interface{}); ok {
		if enabled, _ := ik["enable"].(bool); enabled {
			keywords := []string{}
			if kws, ok := ik["extra_keywords"].([]interface{}); ok {
				for _, kw := range kws {
					if kwStr, ok := kw.(string); ok && kwStr != "" {
						keywords = append(keywords, kwStr)
					}
				}
			}
			if len(keywords) > 0 {
				s.checkers = append(s.checkers, NewKeywordChecker(keywords))
				s.enabled = true
			}
		}
	}

	if ba, ok := config["baidu_aip"].(map[string]interface{}); ok {
		if enabled, _ := ba["enable"].(bool); enabled {
			appID, _ := ba["app_id"].(string)
			apiKey, _ := ba["api_key"].(string)
			secretKey, _ := ba["secret_key"].(string)
			if appID != "" || apiKey != "" {
				s.checkers = append(s.checkers, NewBaiduAipChecker(appID, apiKey, secretKey))
				s.enabled = true
			}
		}
	}

	return s
}

// Check runs all enabled strategies; all must pass.
func (s *StrategySelector) Check(text string) (bool, string) {
	for _, checker := range s.checkers {
		ok, info := checker.Check(text)
		if !ok {
			return false, info
		}
	}
	return true, ""
}

// IsEnabled returns true if any content safety strategy is active.
func (s *StrategySelector) IsEnabled() bool {
	return s.enabled
}
