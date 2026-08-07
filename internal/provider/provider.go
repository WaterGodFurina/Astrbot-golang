// Package provider implements LLM provider management and request decoration.
// Ported from astrbot/core/provider/ and astrbot/core/astr_main_agent.py
//
// Bug fix for issue #9573: UMO max_context_length not taking effect.
// The Python code used `or` short-circuit:
//   cfg = config.provider_settings or plugin_context.get_config(umo=...).get("provider_settings", {})
// Since global provider_settings is always a non-empty dict, the UMO-specific
// config was never fetched. Fix: merge global + UMO configs, with UMO overriding.
package provider

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ProviderType identifies an LLM provider type.
type ProviderType string

const (
	ProviderOpenAI    ProviderType = "openai"
	ProviderAnthropic ProviderType = "anthropic"
	ProviderGemini    ProviderType = "gemini"
	ProviderDashScope ProviderType = "dashscope"
	ProviderCustom    ProviderType = "custom"
)

// Provider is the interface every LLM provider implements.
type Provider interface {
	ID() string
	Type() ProviderType
	Chat(ctx context.Context, req *Request) (*Response, error)
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Request is an LLM request.
type Request struct {
	Model          string
	SystemPrompt   string
	Messages       []Message
	Tools          []ToolDefinition
	MaxTokens      int
	Temperature    float64
	Stream         bool
	SessionID      string
	ImageURLs      []string
	AudioURLs      []string
	ExtraHeaders   map[string]string
}

// Message is a single message in the conversation.
type Message struct {
	Role       string // "system", "user", "assistant", "tool"
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
	Timestamp  time.Time
}

// ToolCall represents a tool invocation by the LLM.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // JSON string
}

// ToolDefinition describes an LLM-callable tool.
// FIXED #9533: tool names are sanitized to match ^[a-zA-Z0-9_-]+$
type ToolDefinition struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

// SanitizeToolName replaces invalid characters in tool names.
// Issue #9533: MCP tool names containing "." caused LLM API rejection.
func SanitizeToolName(name string) string {
	var out []byte
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '_' || c == '-' {
			out = append(out, c)
		} else {
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "tool"
	}
	return string(out)
}

// Response is an LLM response.
type Response struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
}

// Usage tracks token usage.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens       int
}

// ProviderSettings contains per-provider configuration.
type ProviderSettings struct {
	MaxContextLength    int      `json:"max_context_length"`
	DequeueContextLength int     `json:"dequeue_context_length"`
	PromptPrefix        string   `json:"prompt_prefix"`
	EnableWebSearch     bool     `json:"enable_web_search"`
	WebSearchProviders  []string `json:"web_search_providers"`
	RequestMaxRetries   int      `json:"request_max_retries"`
}

// ProviderConfig represents the configuration for a provider instance.
type ProviderConfig struct {
	ID             string
	Type           ProviderType
	APIKey         string
	APIBase        string
	Model          string
	ProxyURL       string
	CACertPath     string
	Timeout        time.Duration
	Settings       ProviderSettings
}

// Manager manages all provider instances.
type Manager struct {
	mu        sync.RWMutex
	providers map[string]Provider
	defaultID string
}

// NewManager creates an empty provider manager.
func NewManager() *Manager {
	return &Manager{providers: make(map[string]Provider)}
}

// Register adds a provider.
func (m *Manager) Register(p Provider) {
	m.mu.Lock()
	m.providers[p.ID()] = p
	if m.defaultID == "" {
		m.defaultID = p.ID()
	}
	m.mu.Unlock()
}

// Get returns a provider by ID.
func (m *Manager) Get(id string) Provider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.providers[id]
}

// GetDefault returns the default provider.
func (m *Manager) GetDefault() Provider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.defaultID == "" {
		return nil
	}
	return m.providers[m.defaultID]
}

// All returns all providers.
func (m *Manager) All() []Provider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]Provider, 0, len(m.providers))
	for _, p := range m.providers {
		result = append(result, p)
	}
	return result
}

// MergeProviderSettings merges global and UMO-specific settings.
// UMO settings override global ones.
// FIXED #9573: Python code used `or` short-circuit which always chose global.
func MergeProviderSettings(global, umo ProviderSettings) ProviderSettings {
	merged := global // start with global

	// UMO overrides global for non-zero/non-empty values
	if umo.MaxContextLength != 0 {
		merged.MaxContextLength = umo.MaxContextLength
	}
	if umo.DequeueContextLength != 0 {
		merged.DequeueContextLength = umo.DequeueContextLength
	}
	if umo.PromptPrefix != "" {
		merged.PromptPrefix = umo.PromptPrefix
	}
	if umo.EnableWebSearch {
		merged.EnableWebSearch = umo.EnableWebSearch
	}
	if len(umo.WebSearchProviders) > 0 {
		merged.WebSearchProviders = umo.WebSearchProviders
	}
	if umo.RequestMaxRetries != 0 {
		merged.RequestMaxRetries = umo.RequestMaxRetries
	}
	return merged
}

// DecorateLLMRequest applies configuration to an LLM request.
// FIXED #9573: Merge global + UMO settings instead of `or` short-circuit.
func DecorateLLMRequest(req *Request, globalSettings, umoSettings ProviderSettings) {
	// Merge settings: UMO overrides global (fix for #9573)
	cfg := MergeProviderSettings(globalSettings, umoSettings)

	// Apply prompt prefix
	if cfg.PromptPrefix != "" {
		req.SystemPrompt = cfg.PromptPrefix + "\n" + req.SystemPrompt
	}

	// Sanitize tool names (fix for #9533)
	for i := range req.Tools {
		req.Tools[i].Name = SanitizeToolName(req.Tools[i].Name)
	}
}

// EnforceMaxTurns trims the message history to maxTurns.
// If maxTurns is -1, no limit is applied.
func EnforceMaxTurns(messages []Message, maxTurns int) []Message {
	if maxTurns < 0 || maxTurns >= len(messages) {
		return messages
	}
	// Keep the system message if present
	var system []Message
	var rest []Message
	for _, msg := range messages {
		if msg.Role == "system" {
			system = append(system, msg)
		} else {
			rest = append(rest, msg)
		}
	}
	if maxTurns < len(rest) {
		rest = rest[len(rest)-maxTurns:]
	}
	return append(system, rest...)
}

// FormatContext generates the system prompt with context.
func FormatContext(messages []Message, maxTokens int) string {
	var parts []string
	for _, msg := range messages {
		role := msg.Role
		content := msg.Content
		if role == "system" {
			parts = append(parts, content)
		} else {
			parts = append(parts, fmt.Sprintf("%s: %s", role, content))
		}
	}
	return joinStrings(parts, "\n")
}

func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	result := parts[0]
	for _, p := range parts[1:] {
		result += sep + p
	}
	return result
}
