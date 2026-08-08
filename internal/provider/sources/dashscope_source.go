// Package sources - DashScope (Aliyun Qwen) provider.
// Ported from astrbot/core/provider/sources/dashscope_source.py
package sources

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

// DashScopeSource is an Aliyun DashScope (Qwen) chat provider.
type DashScopeSource struct {
	provider.BaseProvider
	apiBase string
	apiKey  string
	client  *http.Client
}

// NewDashScopeSource creates a DashScope provider.
func NewDashScopeSource(config, settings map[string]interface{}) *DashScopeSource {
	bp := provider.NewBaseProvider(config, settings)
	s := &DashScopeSource{
		BaseProvider: bp,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
	s.apiBase, _ = config["api_base"].(string)
	if s.apiBase == "" {
		s.apiBase = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	}
	if key, ok := config["key"].(string); ok {
		s.apiKey = key
	}
	if keys, ok := config["key"].([]interface{}); ok && len(keys) > 0 {
		if k, ok := keys[0].(string); ok {
			s.apiKey = k
		}
	}
	return s
}

// TextChat sends a non-streaming chat request (OpenAI-compatible endpoint).
func (s *DashScopeSource) TextChat(ctx context.Context, req *provider.ProviderRequest) (*provider.LLMResponse, error) {
	// DashScope compatible mode uses OpenAI format
	openai := &OpenAISource{
		BaseProvider: s.BaseProvider,
		apiBase:      s.apiBase,
		apiKey:       s.apiKey,
		client:       s.client,
	}
	return openai.TextChat(ctx, req)
}

// TextChatStream sends a streaming chat request.
func (s *DashScopeSource) TextChatStream(ctx context.Context, req *provider.ProviderRequest) (<-chan *provider.LLMResponse, error) {
	openai := &OpenAISource{
		BaseProvider: s.BaseProvider,
		apiBase:      s.apiBase,
		apiKey:       s.apiKey,
		client:       s.client,
	}
	return openai.TextChatStream(ctx, req)
}

// Test verifies the provider.
func (s *DashScopeSource) Test(ctx context.Context) error {
	req := &provider.ProviderRequest{
		Prompt:    "REPLY `PONG` ONLY",
		SessionID: "test",
	}
	resp, err := s.TextChat(ctx, req)
	if err != nil {
		return err
	}
	if resp.Role == "err" {
		return fmt.Errorf("provider test failed: %s", resp.CompletionText)
	}
	return nil
}
