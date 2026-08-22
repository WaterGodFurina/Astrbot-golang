package pipeline

import (
	"regexp"
	"strings"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// defaultSegRegex mirrors the Python fallback segmentation regex.
var defaultSegRegex = regexp.MustCompile(`.*?[。？！~…]+|.+$`)

// segPlatformBlacklist mirrors the Python platforms that never segment.
var segPlatformBlacklist = map[string]bool{
	"qq_official_webhook":     true,
	"weixin_official_account": true,
	"dingtalk":                true,
}

// isSegmentedReplyPlatform reports whether the platform supports segmented
// replies (mirrors the Python blacklist).
func (s *ResultDecorateStage) isSegmentedReplyPlatform(platform string) bool {
	return !segPlatformBlacklist[platform]
}

// applySegmentedReply splits long Plain components into multiple Plain
// segments using the configured split_mode (regex or words), applies the
// content cleanup rule, and rebuilds the chain (mirrors result_decorate/stage.py).
func (s *ResultDecorateStage) applySegmentedReply(event *core.Event) {
	if s.segOnlyLLMResult && !event.Result.IsModelResult() {
		return
	}
	newChain := []message.Component{}
	for _, comp := range event.Result.Chain {
		plain, ok := comp.(*message.Plain)
		if !ok {
			// Non-Plain segments are never split.
			newChain = append(newChain, comp)
			continue
		}
		if len([]rune(plain.Text)) > s.segWordsThreshold {
			// Over the threshold: send whole, do not segment.
			newChain = append(newChain, comp)
			continue
		}

		// Choose the split strategy.
		var segments []string
		if s.segSplitMode == "words" && s.segWordsPattern != nil {
			segments = s.splitTextByWords(plain.Text)
		} else {
			re := defaultSegRegex
			if s.segRegexCompiled != nil {
				re = s.segRegexCompiled
			}
			segments = re.FindAllString(plain.Text, -1)
		}

		if len(segments) == 0 {
			newChain = append(newChain, comp)
			continue
		}
		for _, seg := range segments {
			if s.segContentCleanupRule != nil {
				seg = s.segContentCleanupRule.ReplaceAllString(seg, "")
			}
			seg = strings.TrimSpace(seg)
			if seg != "" {
				newChain = append(newChain, &message.Plain{Text: seg})
			}
		}
	}
	event.Result.Chain = newChain
}

// splitTextByWords splits text on the configured split-word list
// (mirrors _split_text_by_words: findall of "(.*?(w1|w2)|.+$)").
func (s *ResultDecorateStage) splitTextByWords(text string) []string {
	matches := s.segWordsPattern.FindAllStringSubmatch(text, -1)
	result := []string{}
	for _, m := range matches {
		content := ""
		if len(m) > 1 {
			content = m[1]
		} else if len(m) > 0 {
			content = m[0]
		}
		if content == "" && len(m) > 0 {
			content = m[0]
		}
		// Strip a trailing split word (Python removes the matched word).
		for _, w := range s.segSplitWords {
			if strings.HasSuffix(content, w) {
				content = content[:len(content)-len(w)]
				break
			}
		}
		if strings.TrimSpace(content) != "" {
			result = append(result, content)
		}
	}
	if len(result) == 0 && strings.TrimSpace(text) != "" {
		result = append(result, text)
	}
	return result
}
