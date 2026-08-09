// Package sources - Groq OpenAI-compatible chat provider.
// Ported from astrbot/core/provider/sources/groq_source.py
package sources

import (
	"context"

	"github.com/AstrBotDevs/AstrBot/internal/provider"
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
// messages; Groq rejects them in the chat history.
func stripAssistantReasoningFields(body map[string]interface{}) {
	messages, ok := body["messages"].([]map[string]interface{})
	if !ok {
		return
	}
	for _, msg := range messages {
		if msg["role"] == "assistant" {
			delete(msg, "reasoning_content")
			delete(msg, "reasoning")
		}
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
