// Package contentsafety implements content safety checking.
// Ported from astrbot/core/pipeline/content_safety_check/stage.py
// and astrbot/core/pipeline/content_safety_check/strategies/
package contentsafety

import (
	"strings"
	"sync"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

var logger = log.GetDefault().WithComponent("ContentSafety")

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
			return false, "content blocked by keyword filter"
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
				// 只有在 baidu_aip 选项开启时才提示缺少 SDK（默认构建不下载
				// baidu-aip 依赖，也不报错）。
				if !baiduAipSDKEnabled {
					logger.Error("baidu_aip 已开启，但未编译官方 golang-sdk。请执行 `go get github.com/Baidu-AIP/golang-sdk` 后以 `-tags baidu_aip` 重新编译。当前将 fail-open 不拦截消息。")
				}
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
