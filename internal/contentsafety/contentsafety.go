// Package contentsafety implements content safety checking.
// Ported from astrbot/core/pipeline/content_safety_check/stage.py
// and astrbot/core/pipeline/content_safety_check/strategies/
package contentsafety

import (
	"regexp"
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

// KeywordChecker blocks text matching any banned keyword.
//
// 对齐 Python keywords.py: `re.search(keyword, content)` —— keyword 是**正则**
// 且**大小写敏感**（不是子串/忽略大小写）。正则预编译缓存，避免每条消息重复
// 编译；非法正则时退化为字面量匹配（不静默丢弃关键词，避免漏拦）。
type KeywordChecker struct {
	mu  sync.RWMutex
	res []*regexp.Regexp
}

// compileKeywords precompiles each keyword as a Go regexp. Go RE2 不支持
// Python re 的回溯/环视等语法，这类非法正则降级为 regexp.QuoteMeta 字面量
// 匹配，保证该关键词仍然生效（fail-closed）而不是被跳过。
func compileKeywords(keywords []string) []*regexp.Regexp {
	res := make([]*regexp.Regexp, 0, len(keywords))
	for _, kw := range keywords {
		if kw == "" {
			continue
		}
		re, err := regexp.Compile(kw)
		if err != nil {
			logger.Error("content_safety 关键词正则 %q 编译失败，回退为字面量匹配: %v", kw, err)
			re = regexp.MustCompile(regexp.QuoteMeta(kw))
		}
		res = append(res, re)
	}
	return res
}

// NewKeywordChecker creates a keyword-based checker.
func NewKeywordChecker(keywords []string) *KeywordChecker {
	return &KeywordChecker{res: compileKeywords(keywords)}
}

// Check returns (ok, info). ok=false if a keyword is found.
func (c *KeywordChecker) Check(text string) (bool, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, re := range c.res {
		if re.MatchString(text) {
			return false, "content blocked by keyword filter"
		}
	}
	return true, ""
}

// AddKeyword adds a banned keyword.
func (c *KeywordChecker) AddKeyword(kw string) {
	if kw == "" {
		return
	}
	compiled := compileKeywords([]string{kw})
	if len(compiled) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.res = append(c.res, compiled...)
}

// SetKeywords replaces all keywords.
func (c *KeywordChecker) SetKeywords(kws []string) {
	compiled := compileKeywords(kws)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.res = compiled
}

// StrategySelector selects the enabled checkers based on config.
// Mirrors the Python StrategySelector: internal_keywords.enable enables the
// keyword strategy and baidu_aip.enable enables the Baidu AI strategy.
type StrategySelector struct {
	checkers []Checker
	enabled  bool
	// degraded reports that a configured strategy is present but not actually
	// enforced (e.g. baidu_aip enabled in a build without the SDK tag).
	degraded bool
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
					s.degraded = true
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

// HasDegradedStrategy reports whether a configured strategy is present but not
// actually enforced (e.g. baidu_aip enabled in a build without the SDK tag, so
// Check fails open). Lets the WebUI surface "configured but not effective".
func (s *StrategySelector) HasDegradedStrategy() bool {
	return s.degraded
}
