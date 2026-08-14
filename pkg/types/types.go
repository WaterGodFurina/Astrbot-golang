// Package types defines shared types used across internal packages.
package types

import (
	"time"
)

// ProviderType identifies an LLM provider.
type ProviderType string

const (
	ProviderOpenAI    ProviderType = "openai"
	ProviderAnthropic ProviderType = "anthropic"
	ProviderGemini    ProviderType = "gemini"
	ProviderDashScope ProviderType = "dashscope"
	ProviderCustom    ProviderType = "custom"
)

// PlatformType identifies a messaging platform.
type PlatformType string

const (
	PlatformOneBot11   PlatformType = "aiocqhttp"
	PlatformTelegram   PlatformType = "telegram"
	PlatformDingTalk   PlatformType = "dingtalk"
	PlatformMattermost PlatformType = "mattermost"
	PlatformWebChat    PlatformType = "webchat"
	PlatformKook       PlatformType = "kook"
	PlatformFeishu     PlatformType = "feishu"
	PlatformSlack      PlatformType = "slack"
	PlatformBiliBili   PlatformType = "bilibili"
	PlatformWeCom      PlatformType = "wecom"
	PlatformDiscord    PlatformType = "discord"
	PlatformEmpty      PlatformType = "empty"
)

// EventContext holds metadata for an incoming message event.
type EventContext struct {
	MessageID        string
	UnifiedMsgOrigin string // platform:conversation_id
	SenderID         string
	SenderName       string
	SelfID           string // bot's own ID on this platform
	Platform         PlatformType
	ConversationID   string
	IsGroup          bool
	GroupID          string
	IsPrivate        bool
	IsAdmin          bool
	RawEvent         interface{}
}

// ProviderConfig holds configuration for an LLM provider instance.
type ProviderConfig struct {
	ID         string                 `json:"id"`
	Type       ProviderType           `json:"type"`
	Name       string                 `json:"name"`
	APIBase    string                 `json:"api_base,omitempty"`
	APIKey     string                 `json:"api_key"`
	Model      string                 `json:"model"`
	Proxy      string                 `json:"proxy,omitempty"`
	Timeout    int                    `json:"timeout,omitempty"`
	MaxRetries int                    `json:"max_retries,omitempty"`
	Enable     bool                   `json:"enable"`
	Extra      map[string]interface{} `json:"extra,omitempty"`
}

// PersonaConfig holds a persona definition.
type PersonaConfig struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Prompt    string `json:"prompt"`
	IsDefault bool   `json:"is_default"`
}

// MessageRole is the role of a message in a conversation.
type MessageRole string

const (
	RoleSystem    MessageRole = "system"
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
	RoleTool      MessageRole = "tool"
)

// ConversationMessage is a single message in a conversation history.
type ConversationMessage struct {
	Role       MessageRole `json:"role"`
	Content    string      `json:"content"`
	Components interface{} `json:"components,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	Name       string      `json:"name,omitempty"`
	Timestamp  time.Time   `json:"timestamp"`
}

// Conversation represents a chat session's state.
type Conversation struct {
	ID           string                 `json:"id"`
	Messages     []ConversationMessage  `json:"messages"`
	ProviderID   string                 `json:"provider_id,omitempty"`
	PersonaID    string                 `json:"persona_id,omitempty"`
	SystemPrompt string                 `json:"system_prompt,omitempty"`
	CreatedAt    time.Time              `json:"created_at"`
	UpdatedAt    time.Time              `json:"updated_at"`
	Metadata     map[string]interface{} `json:"metadata,omitempty"`
}

// ToolDefinition describes an LLM-callable tool.
// Fixed per issue #9533: tool names must match ^[a-zA-Z0-9_-]+$ pattern.
type ToolDefinition struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"` // JSON Schema
}

// SanitizeToolName replaces invalid characters in tool names to satisfy
// the ^[a-zA-Z0-9_-]+$ pattern required by LLM APIs.
// Issue #9533: MCP tool names containing "." caused API rejection.
// Processes by rune (not byte) so multi-byte UTF-8 chars become a single _.
func SanitizeToolName(name string) string {
	var out []rune
	for _, r := range name {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' || r == '-' {
			out = append(out, r)
		} else {
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "tool"
	}
	return string(out)
}
