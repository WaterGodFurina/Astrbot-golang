// Package message defines the core message components used across all platform adapters.
// Ported from astrbot/core/message/components.py
package message

import (
	"fmt"
	"time"
)

// ComponentType identifies the kind of message component.
type ComponentType string

const (
	CompPlain     ComponentType = "Plain"
	CompAt        ComponentType = "At"
	CompReply     ComponentType = "Reply"
	CompImage     ComponentType = "Image"
	CompVoice     ComponentType = "Voice"
	CompFile      ComponentType = "File"
	CompVideo     ComponentType = "Video"
	CompEmoji     ComponentType = "Emoji"
	CompNode      ComponentType = "Node"
	CompNodes     ComponentType = "Nodes"
	CompPoke      ComponentType = "Poke"
	CompMusic     ComponentType = "Music"
	CompForward   ComponentType = "Forward"
	CompUnknown   ComponentType = "Unknown"
)

// Component is the interface every message component implements.
type Component interface {
	Type() ComponentType
	String() string
	Clone() Component
}

// Plain represents a plain text segment.
type Plain struct {
	Text string `json:"text"`
}

func (p *Plain) Type() ComponentType { return CompPlain }
func (p *Plain) String() string      { return p.Text }
func (p *Plain) Clone() Component     { return &Plain{Text: p.Text} }

// At represents a mention of a user.
type At struct {
	TargetID string `json:"target_id"`
	Name     string `json:"name,omitempty"`
}

func (a *At) Type() ComponentType { return CompAt }
func (a *At) String() string {
	if a.Name != "" {
		return fmt.Sprintf("@%s", a.Name)
	}
	return fmt.Sprintf("@%s", a.TargetID)
}
func (a *At) Clone() Component { return &At{TargetID: a.TargetID, Name: a.Name} }

// Reply represents a quoted/replied message.
// Fixed per issue #9463: DingTalk quoted messages now include full content.
type Reply struct {
	MessageID  string      `json:"message_id,omitempty"`
	SenderID   string      `json:"sender_id,omitempty"`
	SenderNick string      `json:"sender_nick,omitempty"`
	Chain      []Component `json:"chain,omitempty"`
	CreatedAt  time.Time   `json:"created_at,omitempty"`
}

func (r *Reply) Type() ComponentType { return CompReply }
func (r *Reply) String() string {
	var parts string
	for _, c := range r.Chain {
		parts += c.String()
	}
	return fmt.Sprintf("[Reply:%s] %s", r.SenderNick, parts)
}
func (r *Reply) Clone() Component {
	chain := make([]Component, len(r.Chain))
	for i, c := range r.Chain {
		chain[i] = c.Clone()
	}
	return &Reply{
		MessageID:  r.MessageID,
		SenderID:   r.SenderID,
		SenderNick: r.SenderNick,
		Chain:      chain,
		CreatedAt:  r.CreatedAt,
	}
}

// Image represents an image component.
type Image struct {
	URL      string `json:"url,omitempty"`
	Path     string `json:"path,omitempty"`
	Base64   string `json:"base64,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

func (img *Image) Type() ComponentType { return CompImage }
func (img *Image) String() string     { return "[Image]" }
func (img *Image) Clone() Component {
	return &Image{
		URL: img.URL, Path: img.Path, Base64: img.Base64,
		FileID: img.FileID, Width: img.Width, Height: img.Height,
	}
}

// Voice represents a voice/audio component.
type Voice struct {
	URL    string `json:"url,omitempty"`
	Path   string `json:"path,omitempty"`
	Base64 string `json:"base64,omitempty"`
	FileID string `json:"file_id,omitempty"`
}

func (v *Voice) Type() ComponentType { return CompVoice }
func (v *Voice) String() string     { return "[Voice]" }
func (v *Voice) Clone() Component {
	return &Voice{URL: v.URL, Path: v.Path, Base64: v.Base64, FileID: v.FileID}
}

// File represents a file component.
type File struct {
	URL    string `json:"url,omitempty"`
	Path   string `json:"path,omitempty"`
	FileID string `json:"file_id,omitempty"`
	Name   string `json:"name,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

func (f *File) Type() ComponentType { return CompFile }
func (f *File) String() string     { return "[File]" }
func (f *File) Clone() Component {
	return &File{URL: f.URL, Path: f.Path, FileID: f.FileID, Name: f.Name, Size: f.Size}
}

// Video represents a video component.
type Video struct {
	URL    string `json:"url,omitempty"`
	Path   string `json:"path,omitempty"`
	FileID string `json:"file_id,omitempty"`
}

func (v *Video) Type() ComponentType { return CompVideo }
func (v *Video) String() string      { return "[Video]" }
func (v *Video) Clone() Component {
	return &Video{URL: v.URL, Path: v.Path, FileID: v.FileID}
}

// Emoji represents a custom emoji/sticker.
type Emoji struct {
	ID  string `json:"id,omitempty"`
	URL string `json:"url,omitempty"`
}

func (e *Emoji) Type() ComponentType { return CompEmoji }
func (e *Emoji) String() string      { return "[Emoji]" }
func (e *Emoji) Clone() Component    { return &Emoji{ID: e.ID, URL: e.URL} }

// MessageChain is an ordered list of components forming a complete message.
type MessageChain struct {
	Components []Component `json:"components"`
}

// NewChain creates a MessageChain from the given components.
func NewChain(comps ...Component) *MessageChain {
	return &MessageChain{Components: comps}
}

// PlainChain creates a chain from a plain text string.
func PlainChain(text string) *MessageChain {
	return NewChain(&Plain{Text: text})
}

// Append adds a component to the chain.
func (mc *MessageChain) Append(c Component) {
	mc.Components = append(mc.Components, c)
}

// PlainText extracts all plain text from the chain.
func (mc *MessageChain) PlainText() string {
	var sb []byte
	for _, c := range mc.Components {
		if p, ok := c.(*Plain); ok {
			sb = append(sb, p.Text...)
		}
	}
	return string(sb)
}

// HasImage returns true if the chain contains at least one Image component.
func (mc *MessageChain) HasImage() bool {
	for _, c := range mc.Components {
		if _, ok := c.(*Image); ok {
			return true
		}
	}
	return false
}

// HasAt returns true if the chain contains an At component targeting the given ID.
func (mc *MessageChain) HasAt(targetID string) bool {
	for _, c := range mc.Components {
		if at, ok := c.(*At); ok && at.TargetID == targetID {
			return true
		}
	}
	return false
}

// Clone creates a deep copy of the message chain.
func (mc *MessageChain) Clone() *MessageChain {
	comps := make([]Component, len(mc.Components))
	for i, c := range mc.Components {
		comps[i] = c.Clone()
	}
	return &MessageChain{Components: comps}
}

// GetFirst returns the first component of the given type, or nil.
func (mc *MessageChain) GetFirst(t ComponentType) Component {
	for _, c := range mc.Components {
		if c.Type() == t {
			return c
		}
	}
	return nil
}

// GetAll returns all components of the given type.
func (mc *MessageChain) GetAll(t ComponentType) []Component {
	var result []Component
	for _, c := range mc.Components {
		if c.Type() == t {
			result = append(result, c)
		}
	}
	return result
}
