package pipeline

import (
	"strings"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// TestApplyLLMSafetyMode: enabled safety mode prefixes the system prompt.
func TestApplyLLMSafetyMode(t *testing.T) {
	s := &ProcessStage{providerConf: &ProviderSettings{LLMSafetyMode: true, SafetyModeStrategy: "system_prompt"}}
	out := s.applyLLMSafetyMode("你是助手")
	if !strings.HasPrefix(out, llmSafetyModeSystemPrompt) {
		t.Error("safety mode must prefix the system prompt")
	}
	if !strings.Contains(out, "你是助手") {
		t.Error("original prompt must be preserved")
	}
	// Disabled: unchanged.
	s2 := &ProcessStage{providerConf: &ProviderSettings{LLMSafetyMode: false}}
	if out := s2.applyLLMSafetyMode("x"); out != "x" {
		t.Error("disabled safety mode must not change the prompt")
	}
}

// TestUnsupportedStreamingTurnOff: turn_off disables streaming.
func TestUnsupportedStreamingTurnOff(t *testing.T) {
	s := &ProcessStage{config: map[string]interface{}{
		"provider_settings": map[string]interface{}{"unsupported_streaming_strategy": "turn_off"},
	}}
	if !s.unsupportedStreamingStrategyIsTurnOff() {
		t.Error("turn_off must disable streaming")
	}
	s2 := &ProcessStage{config: map[string]interface{}{
		"provider_settings": map[string]interface{}{"unsupported_streaming_strategy": "realtime_segmenting"},
	}}
	if s2.unsupportedStreamingStrategyIsTurnOff() {
		t.Error("realtime_segmenting must keep streaming")
	}
}

// TestSanitizeContextByModalities: image blocks are replaced with placeholders
// when the provider has no image modality.
func TestSanitizeContextByModalities(t *testing.T) {
	ctx := []map[string]interface{}{
		{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "hi"},
				map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "x"}},
			},
		},
		{"role": "tool", "tool_call_id": "t1", "content": "result"},
		{"role": "assistant", "content": "ok", "tool_calls": []interface{}{map[string]interface{}{"id": "1"}}},
	}
	out := sanitizeContextByModalities(ctx, []string{"text"})
	if len(out) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(out))
	}
	// Image -> [Image] placeholder
	parts, _ := out[0]["content"].([]interface{})
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	imgPart, _ := parts[1].(map[string]interface{})
	if imgPart["text"] != "[Image]" {
		t.Errorf("image must become placeholder, got %v", imgPart)
	}
	// Tool message -> user with placeholder content.
	if out[1]["role"] != "user" {
		t.Error("tool message must become user role without tool_use support")
	}
	// Assistant tool_calls removed.
	if _, has := out[2]["tool_calls"]; has {
		t.Error("assistant tool_calls must be removed without tool_use support")
	}
}

// TestQuotedMessageParserDepth: nested quote depth is capped.
func TestQuotedMessageParserDepth(t *testing.T) {
	cfg := map[string]interface{}{
		"provider_settings": map[string]interface{}{
			"quoted_message_parser": map[string]interface{}{
				"max_component_chain_depth": 2,
			},
		},
	}
	// Build a 4-level nested quote.
	var inner *message.Reply
	for i := 0; i < 4; i++ {
		inner = &message.Reply{MessageID: "m", Chain: []message.Component{&message.Plain{Text: "x"}}}
	}
	ev := &core.Event{
		Message: &message.MessageChain{Chain: []message.Component{&message.Reply{MessageID: "top", Chain: []message.Component{inner}}}},
	}
	applyQuotedMessageParser(cfg, ev)
	// Walk depth: should be capped at 2.
	reply, ok := ev.Message.Chain[0].(*message.Reply)
	if !ok {
		t.Fatal("top component must be Reply")
	}
	depth := 1
	for len(reply.Chain) > 0 {
		next, ok := reply.Chain[0].(*message.Reply)
		if !ok {
			break
		}
		reply = next
		depth++
	}
	if depth > 2 {
		t.Errorf("quote depth must be capped at 2, got %d", depth)
	}
}

// TestQuotedMessageParserImageCap: quoted images are capped.
func TestQuotedMessageParserImageCap(t *testing.T) {
	cfg := map[string]interface{}{
		"provider_settings": map[string]interface{}{
			"quoted_message_parser": map[string]interface{}{
				"max_quoted_fallback_images": 2,
			},
		},
	}
	chain := []message.Component{
		&message.Image{URL: "1"},
		&message.Image{URL: "2"},
		&message.Image{URL: "3"},
		&message.Plain{Text: "text"},
	}
	out := sanitizeQuotedChain(chain, resolveQuotedMessageParserSettings(cfg))
	imgs := 0
	for _, c := range out {
		if _, ok := c.(*message.Image); ok {
			imgs++
		}
	}
	if imgs != 2 {
		t.Errorf("images must be capped at 2, got %d", imgs)
	}
}

// TestToolStatusMessages: status builders produce the expected text.
func TestToolStatusMessages(t *testing.T) {
	if got := toolStatusCall("web_fetch"); got != "🔨 调用工具: web_fetch" {
		t.Errorf("call status: %q", got)
	}
	long := strings.Repeat("x", 100)
	if got := toolStatusResult(long); len([]rune(got)) > 85 {
		t.Errorf("result status must be truncated, runes=%d", len([]rune(got)))
	}
	if !strings.Contains(toolStatusResult(long), "...") {
		t.Error("truncated result must carry an ellipsis")
	}
}
