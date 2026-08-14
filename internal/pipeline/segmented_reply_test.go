package pipeline

import (
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// TestSegmentedReplyRegex: regex split mode splits on sentence terminators.
func TestSegmentedReplyRegex(t *testing.T) {
	s := &ResultDecorateStage{}
	s.Initialize(&PipelineContext{AstrbotConfig: map[string]interface{}{
		"platform_settings": map[string]interface{}{
			"segmented_reply": map[string]interface{}{
				"enable": true, "only_llm_result": false,
				"split_mode": "regex", "regex": ".*?[。！？]+|.+$",
				"words_count_threshold": 150, "split_words": []interface{}{},
				"content_cleanup_rule": "",
			},
		},
	}})
	ev := &core.Event{Source: core.EventSource{Platform: "telegram"}}
	ev.Result = message.NewMessageEventResult()
	ev.Result.Chain = []message.Component{&message.Plain{Text: "你好呀。今天天气不错！明天呢？"}}
	s.applySegmentedReply(ev)
	if len(ev.Result.Chain) != 3 {
		t.Fatalf("expected 3 segments, got %d: %v", len(ev.Result.Chain), ev.Result.Chain)
	}
	for _, c := range ev.Result.Chain {
		p, ok := c.(*message.Plain)
		if !ok || p.Text == "" {
			t.Errorf("segment must be non-empty Plain: %#v", c)
		}
	}
}

// TestSegmentedReplyWords: words split mode splits on the word list.
func TestSegmentedReplyWords(t *testing.T) {
	s := &ResultDecorateStage{}
	s.Initialize(&PipelineContext{AstrbotConfig: map[string]interface{}{
		"platform_settings": map[string]interface{}{
			"segmented_reply": map[string]interface{}{
				"enable": true, "only_llm_result": false,
				"split_mode":            "words",
				"split_words":           []interface{}{"。", "！"},
				"words_count_threshold": 150, "content_cleanup_rule": "",
			},
		},
	}})
	ev := &core.Event{Source: core.EventSource{Platform: "telegram"}}
	ev.Result = message.NewMessageEventResult()
	ev.Result.Chain = []message.Component{&message.Plain{Text: "第一段。第二段！第三段"}}
	s.applySegmentedReply(ev)
	if len(ev.Result.Chain) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(ev.Result.Chain))
	}
	texts := []string{}
	for _, c := range ev.Result.Chain {
		texts = append(texts, c.(*message.Plain).Text)
	}
	if texts[0] != "第一段" || texts[1] != "第二段" {
		t.Errorf("words split mismatch: %v", texts)
	}
}

// TestSegmentedReplyThreshold: long text over the threshold is not split.
func TestSegmentedReplyThreshold(t *testing.T) {
	s := &ResultDecorateStage{}
	s.Initialize(&PipelineContext{AstrbotConfig: map[string]interface{}{
		"platform_settings": map[string]interface{}{
			"segmented_reply": map[string]interface{}{
				"enable": true, "only_llm_result": false,
				"split_mode": "regex", "regex": ".*?[。！？]+|.+$",
				"words_count_threshold": 10, "split_words": []interface{}{},
				"content_cleanup_rule": "",
			},
		},
	}})
	ev := &core.Event{Source: core.EventSource{Platform: "telegram"}}
	ev.Result = message.NewMessageEventResult()
	ev.Result.Chain = []message.Component{&message.Plain{Text: "这是一个很长的消息超过十个字不应该被分段呀。"}}
	s.applySegmentedReply(ev)
	if len(ev.Result.Chain) != 1 {
		t.Errorf("long text must not be split, got %d segments", len(ev.Result.Chain))
	}
}

// TestSegmentedReplyCleanupRule: content cleanup removes matched chars.
func TestSegmentedReplyCleanupRule(t *testing.T) {
	s := &ResultDecorateStage{}
	s.Initialize(&PipelineContext{AstrbotConfig: map[string]interface{}{
		"platform_settings": map[string]interface{}{
			"segmented_reply": map[string]interface{}{
				"enable": true, "only_llm_result": false,
				"split_mode": "regex", "regex": ".*?[。！？]+|.+$",
				"words_count_threshold": 150, "split_words": []interface{}{},
				"content_cleanup_rule": "[。！？]",
			},
		},
	}})
	ev := &core.Event{Source: core.EventSource{Platform: "telegram"}}
	ev.Result = message.NewMessageEventResult()
	ev.Result.Chain = []message.Component{&message.Plain{Text: "你好。世界！"}}
	s.applySegmentedReply(ev)
	if len(ev.Result.Chain) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(ev.Result.Chain))
	}
	if p := ev.Result.Chain[0].(*message.Plain); p.Text != "你好" {
		t.Errorf("cleanup rule must strip punctuation: %q", p.Text)
	}
}

// TestSegmentedReplyOnlyLLMResult: only_llm_result=true skips non-model results.
func TestSegmentedReplyOnlyLLMResult(t *testing.T) {
	s := &ResultDecorateStage{}
	s.Initialize(&PipelineContext{AstrbotConfig: map[string]interface{}{
		"platform_settings": map[string]interface{}{
			"segmented_reply": map[string]interface{}{
				"enable": true, "only_llm_result": true,
				"split_mode": "regex", "regex": ".*?[。！？]+|.+$",
				"words_count_threshold": 150, "split_words": []interface{}{},
				"content_cleanup_rule": "",
			},
		},
	}})
	ev := &core.Event{Source: core.EventSource{Platform: "telegram"}}
	ev.Result = message.NewMessageEventResult() // general result, not model
	ev.Result.Chain = []message.Component{&message.Plain{Text: "插件回复。应该整体发送。"}}
	s.applySegmentedReply(ev)
	if len(ev.Result.Chain) != 1 {
		t.Errorf("non-model result must not be split with only_llm_result, got %d", len(ev.Result.Chain))
	}
}

// TestSegmentedReplyPlatformBlacklist: blacklisted platforms never split.
func TestSegmentedReplyPlatformBlacklist(t *testing.T) {
	s := &ResultDecorateStage{}
	if !s.isSegmentedReplyPlatform("telegram") {
		t.Error("telegram must support segmentation")
	}
	if s.isSegmentedReplyPlatform("dingtalk") {
		t.Error("dingtalk must be blacklisted")
	}
}

// TestWordCount: ASCII words vs CJK alnum runes.
func TestWordCount(t *testing.T) {
	if n := wordCount("hello world foo"); n != 3 {
		t.Errorf("ascii word count: want 3, got %d", n)
	}
	if n := wordCount("你好世界"); n != 4 {
		t.Errorf("cjk count: want 4, got %d", n)
	}
}
