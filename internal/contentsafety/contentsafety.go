// Package contentsafety implements content safety checking.
// Ported from astrbot/core/pipeline/content_safety_check/stage.py
package contentsafety

import (
	"strings"
	"sync"
)

// Strategy identifies the content safety check method.
type Strategy string

const (
	StrategyKeyword Strategy = "keyword"
	StrategyLLM     Strategy = "llm"
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

// StrategySelector selects the appropriate checker based on config.
type StrategySelector struct {
	strategy Strategy
	checker  Checker
}

// NewStrategySelector creates a selector from config.
// config is a map with keys: "enable" (bool), "strategy" (string), "keywords" ([]string).
func NewStrategySelector(config map[string]interface{}) *StrategySelector {
	s := &StrategySelector{strategy: StrategyOff}

	if enabled, ok := config["enable"].(bool); !enabled || !ok {
		return s
	}

	if str, ok := config["strategy"].(string); ok {
		switch Strategy(str) {
		case StrategyKeyword:
			s.strategy = StrategyKeyword
			keywords := []string{}
			if kws, ok := config["keywords"].([]interface{}); ok {
				for _, kw := range kws {
					if kwStr, ok := kw.(string); ok && kwStr != "" {
						keywords = append(keywords, kwStr)
					}
				}
			}
			s.checker = NewKeywordChecker(keywords)
		case StrategyLLM:
			s.strategy = StrategyLLM
			// LLM-based checking would require a provider; use keyword fallback
			keywords := []string{}
			if kws, ok := config["keywords"].([]interface{}); ok {
				for _, kw := range kws {
					if kwStr, ok := kw.(string); ok && kwStr != "" {
						keywords = append(keywords, kwStr)
					}
				}
			}
			s.checker = NewKeywordChecker(keywords)
		default:
			s.strategy = StrategyOff
		}
	}

	return s
}

// Check runs the selected strategy.
func (s *StrategySelector) Check(text string) (bool, string) {
	if s.strategy == StrategyOff || s.checker == nil {
		return true, ""
	}
	return s.checker.Check(text)
}

// IsEnabled returns true if content safety checking is active.
func (s *StrategySelector) IsEnabled() bool {
	return s.strategy != StrategyOff
}
