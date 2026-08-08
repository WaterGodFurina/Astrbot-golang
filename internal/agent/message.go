// Package agent - message types for LLM conversations.
// Ported from astrbot/core/agent/message.py
package agent

// MessageRole identifies the sender role in a conversation.
type MessageRole string

const (
	RoleSystem    MessageRole = "system"
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
	RoleTool      MessageRole = "tool"
)

// ContentPart represents a part of a multi-modal message.
type ContentPart struct {
	Type     string        `json:"type"` // "text", "image_url", "audio_url"
	Text     string        `json:"text,omitempty"`
	ImageURL *ImageURLPart `json:"image_url,omitempty"`
	AudioURL *AudioURLPart `json:"audio_url,omitempty"`
}

// ImageURLPart holds an image URL.
type ImageURLPart struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"` // "low", "high", "auto"
}

// AudioURLPart holds an audio URL.
type AudioURLPart struct {
	URL string `json:"url"`
}

// AssistantMessageSegment represents a segment in an assistant response.
type AssistantMessageSegment struct {
	Type     string    `json:"type"` // "text", "tool_call"
	Text     string    `json:"text,omitempty"`
	ToolCall *ToolCall `json:"tool_call,omitempty"`
}

// ToolCall represents a tool/function call from the LLM.
type ToolCall struct {
	ID           string                 `json:"id"`
	Type         string                 `json:"type"` // "function"
	Function     ToolCallFunction       `json:"function"`
	ExtraContent map[string]interface{} `json:"extra_content,omitempty"`
}

// ToolCallFunction holds the function name and arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// ToolCallMessageSegment represents a tool result in a message.
type ToolCallMessageSegment struct {
	Type       string `json:"type"` // "tool_result"
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
}

// Message represents a single message in a conversation.
type Message struct {
	Role      MessageRole               `json:"role"`
	Content   []ContentPart             `json:"content,omitempty"`
	Text      string                    `json:"text,omitempty"`     // simplified text content
	Segments  []AssistantMessageSegment `json:"segments,omitempty"` // for assistant messages
	ToolCalls []ToolCall                `json:"tool_calls,omitempty"`
}

// NewTextMessage creates a simple text message.
func NewTextMessage(role MessageRole, text string) *Message {
	return &Message{
		Role: role,
		Content: []ContentPart{
			{Type: "text", Text: text},
		},
		Text: text,
	}
}

// NewUserMessage creates a user message.
func NewUserMessage(text string) *Message {
	return NewTextMessage(RoleUser, text)
}

// NewSystemMessage creates a system message.
func NewSystemMessage(text string) *Message {
	return NewTextMessage(RoleSystem, text)
}

// NewAssistantMessage creates an assistant message.
func NewAssistantMessage(text string) *Message {
	return NewTextMessage(RoleAssistant, text)
}

// IsCheckpointMessage checks if a message is a checkpoint (tool result).
func IsCheckpointMessage(msg map[string]interface{}) bool {
	role, _ := msg["role"].(string)
	return role == "tool"
}

// DumpMessagesWithCheckpoints serializes messages to JSON-compatible format.
func DumpMessagesWithCheckpoints(messages []Message) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(messages))
	for _, msg := range messages {
		m := map[string]interface{}{
			"role": string(msg.Role),
		}
		if msg.Text != "" && len(msg.Content) == 0 {
			m["content"] = msg.Text
		} else if len(msg.Content) > 0 {
			parts := make([]map[string]interface{}, 0, len(msg.Content))
			for _, p := range msg.Content {
				part := map[string]interface{}{"type": p.Type}
				if p.Text != "" {
					part["text"] = p.Text
				}
				if p.ImageURL != nil {
					part["image_url"] = map[string]interface{}{"url": p.ImageURL.URL}
				}
				if p.AudioURL != nil {
					part["audio_url"] = map[string]interface{}{"url": p.AudioURL.URL}
				}
				parts = append(parts, part)
			}
			m["content"] = parts
		}
		if len(msg.ToolCalls) > 0 {
			calls := make([]map[string]interface{}, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				calls = append(calls, map[string]interface{}{
					"id":   tc.ID,
					"type": tc.Type,
					"function": map[string]interface{}{
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					},
				})
			}
			m["tool_calls"] = calls
		}
		result = append(result, m)
	}
	return result
}
