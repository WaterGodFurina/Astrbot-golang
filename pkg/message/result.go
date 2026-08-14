// Package message defines MessageEventResult and related types.
// Ported from astrbot/core/message/message_event_result.py
package message

// EventResultType describes whether an event continues or stops propagation.
type EventResultType int

const (
	EventResultContinue EventResultType = iota
	EventResultStop
)

// ResultContentType describes the kind of content in an event result.
type ResultContentType int

const (
	ResultGeneralResult ResultContentType = iota
	ResultLLMResult
	ResultAgentRunnerError
	ResultStreamingResult
	ResultStreamingFinish
)

// MessageChain describes a complete message with ordered components.
type MessageChain struct {
	Chain       []Component `json:"chain"`
	UseT2I      *bool       `json:"use_t2i,omitempty"`      // nil = follow config
	UseMarkdown *bool       `json:"use_markdown,omitempty"` // nil = follow platform default
	Type        string      `json:"type,omitempty"`         // message type tag
}

// NewMessageChain creates a MessageChain from components.
func NewMessageChain(comps ...Component) *MessageChain {
	return &MessageChain{Chain: comps}
}

// PlainChain creates a chain from a text string.
func PlainChain(text string) *MessageChain {
	return NewMessageChain(&Plain{Text: text})
}

// Append adds a component.
func (mc *MessageChain) Append(c Component) {
	mc.Chain = append(mc.Chain, c)
}

// Message adds a plain text segment (builder pattern).
func (mc *MessageChain) Message(text string) *MessageChain {
	mc.Chain = append(mc.Chain, &Plain{Text: text})
	return mc
}

// At adds an At segment.
func (mc *MessageChain) At(name, qq string) *MessageChain {
	mc.Chain = append(mc.Chain, &At{TargetID: qq, Name: name})
	return mc
}

// AtAll adds an AtAll segment.
func (mc *MessageChain) AtAll() *MessageChain {
	mc.Chain = append(mc.Chain, &AtAll{})
	return mc
}

// URLImage adds an image from URL.
func (mc *MessageChain) URLImage(url string) *MessageChain {
	mc.Chain = append(mc.Chain, ImageFromURL(url))
	return mc
}

// FileImage adds an image from local file path.
func (mc *MessageChain) FileImage(path string) *MessageChain {
	mc.Chain = append(mc.Chain, ImageFromFile(path))
	return mc
}

// Base64Image adds an image from base64 data.
func (mc *MessageChain) Base64Image(b64 string) *MessageChain {
	mc.Chain = append(mc.Chain, ImageFromBase64(b64))
	return mc
}

// UseT2ISet sets the text-to-image flag.
func (mc *MessageChain) UseT2ISet(use bool) *MessageChain {
	mc.UseT2I = &use
	return mc
}

// UseMarkdownSet sets the markdown flag.
func (mc *MessageChain) UseMarkdownSet(use bool) *MessageChain {
	mc.UseMarkdown = &use
	return mc
}

// Derive creates a new MessageChain inheriting metadata.
func (mc *MessageChain) Derive(chain []Component) *MessageChain {
	newChain := &MessageChain{Chain: chain}
	newChain.UseT2I = mc.UseT2I
	newChain.UseMarkdown = mc.UseMarkdown
	newChain.Type = mc.Type
	return newChain
}

// PlainText extracts all plain text joined by space.
func (mc *MessageChain) PlainText() string {
	var parts []string
	for _, c := range mc.Chain {
		if p, ok := c.(*Plain); ok {
			parts = append(parts, p.Text)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	result := parts[0]
	for _, p := range parts[1:] {
		result += " " + p
	}
	return result
}

// PlainTextConcat concatenates all plain text without separator.
func (mc *MessageChain) PlainTextConcat() string {
	var result string
	for _, c := range mc.Chain {
		if p, ok := c.(*Plain); ok {
			result += p.Text
		}
	}
	return result
}

// HasImage returns true if chain contains an Image.
func (mc *MessageChain) HasImage() bool {
	for _, c := range mc.Chain {
		if _, ok := c.(*Image); ok {
			return true
		}
	}
	return false
}

// HasAt returns true if chain contains an At targeting the given ID.
func (mc *MessageChain) HasAt(targetID string) bool {
	for _, c := range mc.Chain {
		if at, ok := c.(*At); ok && at.TargetID == targetID {
			return true
		}
	}
	return false
}

// Clone deep-copies the message chain.
func (mc *MessageChain) Clone() *MessageChain {
	comps := make([]Component, len(mc.Chain))
	for i, c := range mc.Chain {
		comps[i] = c.Clone()
	}
	return &MessageChain{Chain: comps, UseT2I: mc.UseT2I, UseMarkdown: mc.UseMarkdown, Type: mc.Type}
}

// GetFirst returns the first component of a given type, or nil.
func (mc *MessageChain) GetFirst(t ComponentType) Component {
	for _, c := range mc.Chain {
		if c.Type() == t {
			return c
		}
	}
	return nil
}

// GetAll returns all components of a given type.
func (mc *MessageChain) GetAll(t ComponentType) []Component {
	var result []Component
	for _, c := range mc.Chain {
		if c.Type() == t {
			result = append(result, c)
		}
	}
	return result
}

// MessageEventResult describes a message event's result with components and control flow.
type MessageEventResult struct {
	Chain             []Component       `json:"chain"`
	UseT2I            *bool             `json:"use_t2i,omitempty"`
	UseMarkdown       *bool             `json:"use_markdown,omitempty"`
	Type              string            `json:"type,omitempty"`
	ResultType        EventResultType   `json:"result_type"`
	ResultContentType ResultContentType `json:"result_content_type"`
}

// NewMessageEventResult creates a default MessageEventResult.
func NewMessageEventResult() *MessageEventResult {
	return &MessageEventResult{
		Chain:             []Component{},
		ResultType:        EventResultContinue,
		ResultContentType: ResultGeneralResult,
	}
}

// Message adds plain text (builder).
func (r *MessageEventResult) Message(text string) *MessageEventResult {
	r.Chain = append(r.Chain, &Plain{Text: text})
	return r
}

// At adds an At segment.
func (r *MessageEventResult) At(name, qq string) *MessageEventResult {
	r.Chain = append(r.Chain, &At{TargetID: qq, Name: name})
	return r
}

// URLImage adds an image from URL.
func (r *MessageEventResult) URLImage(url string) *MessageEventResult {
	r.Chain = append(r.Chain, ImageFromURL(url))
	return r
}

// FileImage adds an image from local file.
func (r *MessageEventResult) FileImage(path string) *MessageEventResult {
	r.Chain = append(r.Chain, ImageFromFile(path))
	return r
}

// UseT2ISet sets the text-to-image flag.
func (r *MessageEventResult) UseT2ISet(use bool) *MessageEventResult {
	r.UseT2I = &use
	return r
}

// UseMarkdownSet sets the markdown flag.
func (r *MessageEventResult) UseMarkdownSet(use bool) *MessageEventResult {
	r.UseMarkdown = &use
	return r
}

// StopEvent stops event propagation.
func (r *MessageEventResult) StopEvent() *MessageEventResult {
	r.ResultType = EventResultStop
	return r
}

// ContinueEvent continues event propagation.
func (r *MessageEventResult) ContinueEvent() *MessageEventResult {
	r.ResultType = EventResultContinue
	return r
}

// IsStopped returns true if event propagation should stop.
func (r *MessageEventResult) IsStopped() bool {
	return r.ResultType == EventResultStop
}

// IsLLMResult returns true if this is a result from LLM.
func (r *MessageEventResult) IsLLMResult() bool {
	return r.ResultContentType == ResultLLMResult
}

// IsModelResult returns true if result comes from model execution.
func (r *MessageEventResult) IsModelResult() bool {
	return r.ResultContentType == ResultLLMResult || r.ResultContentType == ResultAgentRunnerError
}

// SetResultContentType sets the content type.
func (r *MessageEventResult) SetResultContentType(t ResultContentType) *MessageEventResult {
	r.ResultContentType = t
	return r
}

// GetPlainText returns concatenated plain text.
func (r *MessageEventResult) GetPlainText() string {
	var result string
	for _, c := range r.Chain {
		if p, ok := c.(*Plain); ok {
			result += p.Text
		}
	}
	return result
}

// ToMessageChain converts to a MessageChain.
func (r *MessageEventResult) ToMessageChain() *MessageChain {
	return &MessageChain{
		Chain:       r.Chain,
		UseT2I:      r.UseT2I,
		UseMarkdown: r.UseMarkdown,
		Type:        r.Type,
	}
}

// Derive creates a MessageChain from a subset of components.
func (r *MessageEventResult) Derive(chain []Component) *MessageChain {
	return &MessageChain{
		Chain:       chain,
		UseT2I:      r.UseT2I,
		UseMarkdown: r.UseMarkdown,
		Type:        r.Type,
	}
}
