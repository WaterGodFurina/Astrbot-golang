// Package sources - Groq OpenAI-compatible chat provider.
// Ported from astrbot/core/provider/sources/groq_source.py
package sources

import (
	"context"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// GroqSource is an OpenAI-compatible chat provider for Groq.
type GroqSource struct {
	*OpenAISource
}

// NewGroqSource creates a Groq provider.
func NewGroqSource(config, settings map[string]interface{}) *GroqSource {
	s := &GroqSource{OpenAISource: NewOpenAISource(config, settings)}
	s.postProcessBody = stripAssistantReasoningFields
	return s
}

// stripAssistantReasoningFields removes reasoning fields from assistant
// messages; Groq rejects them in the chat history. The messages slice belongs
// to the freshly built request body, but each element aliases the shared
// session history, so assistant entries are shallow-copied before editing.
func stripAssistantReasoningFields(body map[string]interface{}) {
	messages, ok := body["messages"].([]map[string]interface{})
	if !ok {
		return
	}
	for i, msg := range messages {
		if msg["role"] != "assistant" {
			continue
		}
		if _, hasReasoningContent := msg["reasoning_content"]; !hasReasoningContent {
			if _, hasReasoning := msg["reasoning"]; !hasReasoning {
				continue
			}
		}
		copied := make(map[string]interface{}, len(msg))
		for k, v := range msg {
			copied[k] = v
		}
		delete(copied, "reasoning_content")
		delete(copied, "reasoning")
		messages[i] = copied
	}
}

// TextChat strips reasoning fields via postProcessBody, then delegates.
func (s *GroqSource) TextChat(ctx context.Context, req *provider.ProviderRequest) (*provider.LLMResponse, error) {
	return s.OpenAISource.TextChat(ctx, req)
}

// TextChatStream strips reasoning fields via postProcessBody, then delegates.
func (s *GroqSource) TextChatStream(ctx context.Context, req *provider.ProviderRequest) (<-chan *provider.LLMResponse, error) {
	return s.OpenAISource.TextChatStream(ctx, req)
}

var _ provider.ChatProvider = (*GroqSource)(nil)
