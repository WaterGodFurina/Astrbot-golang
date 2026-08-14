// Package provider implements LLM provider entities and management.
// Ported from astrbot/core/provider/entities.py
package provider

import (
	"encoding/json"
	"fmt"
)

// ProviderCapabilityType identifies what a provider can do.
type ProviderCapabilityType string

const (
	CapChatCompletion ProviderCapabilityType = "chat_completion"
	CapSpeechToText   ProviderCapabilityType = "speech_to_text"
	CapTextToSpeech   ProviderCapabilityType = "text_to_speech"
	CapEmbedding      ProviderCapabilityType = "embedding"
	CapRerank         ProviderCapabilityType = "rerank"
)

// ProviderMeta is the basic metadata of a provider instance.
type ProviderMeta struct {
	ID           string                 `json:"id"`
	Model        string                 `json:"model,omitempty"`
	Type         string                 `json:"type"`          // adapter name: openai, ollama, etc.
	ProviderType ProviderCapabilityType `json:"provider_type"` // capability
}

// ProviderMetaData extends ProviderMeta with registration info.
type ProviderMetaData struct {
	ProviderMeta
	Desc                string                 `json:"desc,omitempty"`
	ClsType             interface{}            `json:"-"`
	DefaultConfigTmpl   map[string]interface{} `json:"default_config_tmpl,omitempty"`
	ProviderDisplayName string                 `json:"provider_display_name,omitempty"`
}

// TokenUsage tracks token consumption.
type TokenUsage struct {
	InputOther  int `json:"input_other"`
	InputCached int `json:"input_cached"`
	Output      int `json:"output"`
}

// Total returns the total token count.
func (t TokenUsage) Total() int { return t.InputOther + t.InputCached + t.Output }

// Input returns total input tokens.
func (t TokenUsage) Input() int { return t.InputOther + t.InputCached }

// Add combines two TokenUsage values.
func (t TokenUsage) Add(other TokenUsage) TokenUsage {
	return TokenUsage{
		InputOther:  t.InputOther + other.InputOther,
		InputCached: t.InputCached + other.InputCached,
		Output:      t.Output + other.Output,
	}
}

// LLMResponse represents a response from an LLM provider.
type LLMResponse struct {
	Role                  string                            `json:"role"`
	ResultChain           interface{}                       `json:"result_chain,omitempty"` // *message.MessageChain
	ToolsCallArgs         []map[string]interface{}          `json:"tools_call_args,omitempty"`
	ToolsCallName         []string                          `json:"tools_call_name,omitempty"`
	ToolsCallIDs          []string                          `json:"tools_call_ids,omitempty"`
	ToolsCallExtraContent map[string]map[string]interface{} `json:"tools_call_extra_content,omitempty"`
	ReasoningContent      string                            `json:"reasoning_content,omitempty"`
	ReasoningSignature    string                            `json:"reasoning_signature,omitempty"`
	RawCompletion         interface{}                       `json:"raw_completion,omitempty"`
	CompletionText        string                            `json:"completion_text"`
	IsChunk               bool                              `json:"is_chunk"`
	ID                    string                            `json:"id,omitempty"`
	Usage                 *TokenUsage                       `json:"usage,omitempty"`
}

// NewLLMResponse creates a response with the given role.
func NewLLMResponse(role string, completionText string) *LLMResponse {
	return &LLMResponse{
		Role:           role,
		CompletionText: completionText,
		ToolsCallArgs:  []map[string]interface{}{},
		ToolsCallName:  []string{},
		ToolsCallIDs:   []string{},
	}
}

// ToOpenAIToolCalls converts tool calls to OpenAI format.
// The three parallel slices may have inconsistent lengths; the conversion only
// iterates over their shared prefix to avoid an out-of-range access.
func (r *LLMResponse) ToOpenAIToolCalls() []map[string]interface{} {
	n := len(r.ToolsCallArgs)
	if m := len(r.ToolsCallIDs); m < n {
		n = m
	}
	if m := len(r.ToolsCallName); m < n {
		n = m
	}
	ret := make([]map[string]interface{}, 0, n)
	for idx := 0; idx < n; idx++ {
		argBytes, _ := json.Marshal(r.ToolsCallArgs[idx])
		entry := map[string]interface{}{
			"id":   r.ToolsCallIDs[idx],
			"type": "function",
			"function": map[string]interface{}{
				"name":      r.ToolsCallName[idx],
				"arguments": string(argBytes),
			},
		}
		ret = append(ret, entry)
	}
	return ret
}

// ProviderRequest is the canonical request to an LLM provider.
type ProviderRequest struct {
	Prompt                string                   `json:"prompt,omitempty"`
	SessionID             string                   `json:"session_id,omitempty"`
	ImageURLs             []string                 `json:"image_urls,omitempty"`
	AudioURLs             []string                 `json:"audio_urls,omitempty"`
	ExtraUserContentParts []map[string]interface{} `json:"extra_user_content_parts,omitempty"`
	FuncTool              interface{}              `json:"func_tool,omitempty"` // *ToolSet
	Tools                 []map[string]interface{} `json:"tools,omitempty"`
	Contexts              []map[string]interface{} `json:"contexts,omitempty"`
	SystemPrompt          string                   `json:"system_prompt"`
	Conversation          interface{}              `json:"conversation,omitempty"` // *Conversation
	Model                 string                   `json:"model,omitempty"`
}

// NewProviderRequest creates a default request.
func NewProviderRequest() *ProviderRequest {
	return &ProviderRequest{
		ImageURLs: []string{},
		AudioURLs: []string{},
		Contexts:  []map[string]interface{}{},
	}
}

// AssembleContext builds the user message dict from prompt + media URLs.
func (r *ProviderRequest) AssembleContext() map[string]interface{} {
	contentBlocks := []map[string]interface{}{}

	if r.Prompt != "" {
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type": "text",
			"text": r.Prompt,
		})
	} else if len(r.ImageURLs) > 0 {
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type": "text",
			"text": "[图片]",
		})
	} else if len(r.AudioURLs) > 0 {
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type": "text",
			"text": "[音频]",
		})
	}

	for _, part := range r.ExtraUserContentParts {
		contentBlocks = append(contentBlocks, part)
	}

	for _, imgURL := range r.ImageURLs {
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type":      "image_url",
			"image_url": map[string]interface{}{"url": imgURL},
		})
	}

	for _, audioURL := range r.AudioURLs {
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type":      "audio_url",
			"audio_url": map[string]interface{}{"url": audioURL},
		})
	}

	// Simple format if only one text block
	if len(contentBlocks) == 1 && contentBlocks[0]["type"] == "text" &&
		len(r.ExtraUserContentParts) == 0 && len(r.ImageURLs) == 0 && len(r.AudioURLs) == 0 {
		return map[string]interface{}{
			"role":    "user",
			"content": contentBlocks[0]["text"],
		}
	}

	return map[string]interface{}{
		"role":    "user",
		"content": contentBlocks,
	}
}

// ToUserMessage is an alias for AssembleContext, building the user message dict.
func (r *ProviderRequest) ToUserMessage() map[string]interface{} {
	return r.AssembleContext()
}

// String returns a debug-friendly representation.
func (r *ProviderRequest) String() string {
	return fmt.Sprintf("ProviderRequest(prompt=%s, session_id=%s, image_count=%d, audio_count=%d, system_prompt=%s)",
		r.Prompt, r.SessionID, len(r.ImageURLs), len(r.AudioURLs), r.SystemPrompt)
}

// RerankResult represents a reranking result.
type RerankResult struct {
	Index          int     `json:"index"`
	RelevanceScore float64 `json:"relevance_score"`
}
