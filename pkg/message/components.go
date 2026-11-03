// Package message defines the core message components used across all platform adapters.
// Ported from astrbot/core/message/components.py
package message

import (
	"fmt"
	"sync"
	"time"
)

// ComponentType identifies the kind of message component.
type ComponentType string

const (
	CompPlain    ComponentType = "Plain"
	CompAt       ComponentType = "At"
	CompAtAll    ComponentType = "AtAll"
	CompReply    ComponentType = "Reply"
	CompImage    ComponentType = "Image"
	CompRecord   ComponentType = "Record" // voice/audio
	CompFile     ComponentType = "File"
	CompVideo    ComponentType = "Video"
	CompFace     ComponentType = "Face" // QQ emoji
	CompEmoji    ComponentType = "Emoji"
	CompNode     ComponentType = "Node"
	CompNodes    ComponentType = "Nodes"
	CompPoke     ComponentType = "Poke"
	CompMusic    ComponentType = "Music"
	CompForward  ComponentType = "Forward"
	CompJson     ComponentType = "Json"
	CompShare    ComponentType = "Share"
	CompContact  ComponentType = "Contact"
	CompLocation ComponentType = "Location"
	CompShake    ComponentType = "Shake"
	CompDice     ComponentType = "Dice"
	CompRPS      ComponentType = "RPS"
	CompUnknown  ComponentType = "Unknown"

	// Portable interactive components (aligned with Python v4.29.x).
	CompActionRow         ComponentType = "ActionRow"         // a row of interactive controls
	CompButton            ComponentType = "Button"            // an interactive button
	CompButtonInteraction ComponentType = "ButtonInteraction" // an inbound button click

	// Deprecated aliases for backward compat
	CompVoice ComponentType = "Record"
)

// JSONValue is any JSON-compatible value (aligned with Python JSONValue).
type JSONValue = interface{}

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
func (p *Plain) Clone() Component    { return &Plain{Text: p.Text} }

// ButtonStyle is the portable visual intent for a button.
// Ported from Python Button.Style.
type ButtonStyle string

const (
	ButtonStyleDefault ButtonStyle = "default"
	ButtonStylePrimary ButtonStyle = "primary"
	ButtonStyleSuccess ButtonStyle = "success"
	ButtonStyleDanger  ButtonStyle = "danger"
)

// ButtonActionType distinguishes callback and url button actions.
type ButtonActionType string

const (
	ButtonActionCallback ButtonActionType = "callback"
	ButtonActionURL      ButtonActionType = "url"
)

// ButtonAction describes what happens when a portable button is clicked.
// Aligned with Python Button.CallbackAction / Button.UrlAction.
type ButtonAction struct {
	Type ButtonActionType `json:"type"`
	Data JSONValue        `json:"data,omitempty"`
	URL  string           `json:"url,omitempty"`
}

// NewCallbackButtonAction creates a callback action carrying optional context.
func NewCallbackButtonAction(data JSONValue) ButtonAction {
	return ButtonAction{Type: ButtonActionCallback, Data: data}
}

// NewURLButtonAction creates a url action opening the given link.
func NewURLButtonAction(url string) ButtonAction {
	return ButtonAction{Type: ButtonActionURL, URL: url}
}

// Button is a portable interactive button.
// Ported from Python message/components.py Button.
type Button struct {
	ID     string       `json:"id"`
	Label  string       `json:"label"`
	Action ButtonAction `json:"action"`
	Style  ButtonStyle  `json:"style,omitempty"`
}

func (b *Button) Type() ComponentType { return CompButton }
func (b *Button) String() string      { return fmt.Sprintf("[Button:%s]", b.Label) }
func (b *Button) Clone() Component {
	nb := *b
	nb.Action.Data = deepCopyJSONValue(b.Action.Data)
	return &nb
}

// ToDict serializes the button using the public message component format
// (aligned with Python Button.toDict).
func (b *Button) ToDict() map[string]interface{} {
	style := b.Style
	if style == "" {
		style = ButtonStyleDefault
	}
	action := map[string]interface{}{"type": string(b.Action.Type)}
	if b.Action.Type == ButtonActionCallback {
		if b.Action.Data != nil {
			action["data"] = b.Action.Data
		}
	} else {
		action["url"] = b.Action.URL
	}
	return map[string]interface{}{
		"type": "button",
		"data": map[string]interface{}{
			"id":     b.ID,
			"label":  b.Label,
			"action": action,
			"style":  string(style),
		},
	}
}

// ActionRow is a group of buttons displayed on one row when possible.
// Ported from Python message/components.py ActionRow.
type ActionRow struct {
	Buttons      []*Button `json:"buttons"`
	FallbackText string    `json:"fallback_text,omitempty"`
	// ActionTimeout is the shared callback lifetime in seconds; nil = unlimited.
	ActionTimeout *float64 `json:"action_timeout,omitempty"`

	mu              sync.Mutex
	actionExpiresAt *float64
}

func (r *ActionRow) Type() ComponentType { return CompActionRow }
func (r *ActionRow) String() string      { return "[ActionRow]" }
func (r *ActionRow) Clone() Component {
	buttons := make([]*Button, len(r.Buttons))
	for i, b := range r.Buttons {
		if b != nil {
			buttons[i] = b.Clone().(*Button)
		}
	}
	var timeout *float64
	if r.ActionTimeout != nil {
		v := *r.ActionTimeout
		timeout = &v
	}
	return &ActionRow{Buttons: buttons, FallbackText: r.FallbackText, ActionTimeout: timeout}
}

// ActionExpiresAt returns the shared callback deadline, starting on first
// rendering (aligned with Python ActionRow.action_expires_at). Returns nil when
// the row has no timeout.
func (r *ActionRow) ActionExpiresAt() *float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ActionTimeout != nil && r.actionExpiresAt == nil {
		exp := float64(time.Now().UnixNano())/1e9 + *r.ActionTimeout
		r.actionExpiresAt = &exp
	}
	return r.actionExpiresAt
}

// ToDict serializes the row using the public message component format
// (aligned with Python ActionRow.toDict).
func (r *ActionRow) ToDict() map[string]interface{} {
	buttons := make([]map[string]interface{}, 0, len(r.Buttons))
	for _, b := range r.Buttons {
		if b == nil {
			continue
		}
		if data, ok := b.ToDict()["data"].(map[string]interface{}); ok {
			buttons = append(buttons, data)
		}
	}
	data := map[string]interface{}{"buttons": buttons}
	if r.FallbackText != "" {
		data["fallback_text"] = r.FallbackText
	}
	if r.ActionTimeout != nil {
		data["action_timeout"] = *r.ActionTimeout
	}
	return map[string]interface{}{"type": "actionrow", "data": data}
}

// ButtonInteraction is the normalized inbound event produced by a callback
// button click. Ported from Python message/components.py ButtonInteraction.
type ButtonInteraction struct {
	ActionID        string    `json:"action_id"`
	Data            JSONValue `json:"data,omitempty"`
	InteractionID   string    `json:"interaction_id"`
	SourceMessageID string    `json:"source_message_id,omitempty"`
	// CallbackData retains the opaque token for expiry validation before
	// plugin dispatch (aligned with Python callback_data).
	CallbackData string `json:"callback_data,omitempty"`
}

func (b *ButtonInteraction) Type() ComponentType { return CompButtonInteraction }
func (b *ButtonInteraction) String() string {
	return fmt.Sprintf("[ButtonInteraction:%s]", b.ActionID)
}
func (b *ButtonInteraction) Clone() Component {
	nb := *b
	nb.Data = deepCopyJSONValue(b.Data)
	return &nb
}

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

// AtAll represents @all.
type AtAll struct{}

func (a *AtAll) Type() ComponentType { return CompAtAll }
func (a *AtAll) String() string      { return "@全体成员" }
func (a *AtAll) Clone() Component    { return &AtAll{} }

// Reply represents a quoted/replied message.
type Reply struct {
	MessageID  string      `json:"message_id,omitempty"`
	SenderID   string      `json:"sender_id,omitempty"`
	SenderNick string      `json:"sender_nick,omitempty"`
	Chain      []Component `json:"chain,omitempty"`
	MessageStr string      `json:"message_str,omitempty"`
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
		MessageStr: r.MessageStr,
		CreatedAt:  r.CreatedAt,
	}
}

// Image represents an image component.
type Image struct {
	URL      string `json:"url,omitempty"`
	Path     string `json:"path,omitempty"`
	File     string `json:"file,omitempty"`
	Base64   string `json:"base64,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	Filename string `json:"filename,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

func (img *Image) Type() ComponentType { return CompImage }
func (img *Image) String() string      { return "[图片]" }
func (img *Image) Clone() Component {
	return &Image{
		URL: img.URL, Path: img.Path, File: img.File, Base64: img.Base64,
		FileID: img.FileID, Filename: img.Filename, Width: img.Width, Height: img.Height,
	}
}

// FromURL creates an Image from a URL.
func ImageFromURL(url string) *Image { return &Image{URL: url} }

// FromFile creates an Image from a file path.
func ImageFromFile(path string) *Image { return &Image{File: path, Path: path} }

// FromBase64 creates an Image from base64 data.
func ImageFromBase64(b64 string) *Image { return &Image{Base64: b64} }

// Record represents a voice/audio component.
type Record struct {
	URL    string `json:"url,omitempty"`
	Path   string `json:"path,omitempty"`
	File   string `json:"file,omitempty"`
	Base64 string `json:"base64,omitempty"`
	FileID string `json:"file_id,omitempty"`
	Text   string `json:"text,omitempty"`   // text representation for TTS
	Format string `json:"format,omitempty"` // 音频格式（ogg/mp3/m4a/flac/wav），用于选择 sendVoice/sendAudio
	Mime   string `json:"mime,omitempty"`   // 原始 mime_type
}

func (r *Record) Type() ComponentType { return CompRecord }
func (r *Record) String() string      { return "[语音]" }
func (r *Record) Clone() Component {
	return &Record{URL: r.URL, Path: r.Path, File: r.File, Base64: r.Base64, FileID: r.FileID, Text: r.Text, Format: r.Format, Mime: r.Mime}
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
func (f *File) String() string      { return "[文件]" }
func (f *File) Clone() Component {
	return &File{URL: f.URL, Path: f.Path, FileID: f.FileID, Name: f.Name, Size: f.Size}
}

// Video represents a video component.
type Video struct {
	URL    string `json:"url,omitempty"`
	Path   string `json:"path,omitempty"`
	FileID string `json:"file_id,omitempty"`
	// Cover 视频预览封面（对应 Python Video.cover）：发送侧优先用它作为
	// preview image，缺省时再由平台侧从视频抽帧。
	Cover string `json:"cover,omitempty"`
}

func (v *Video) Type() ComponentType { return CompVideo }
func (v *Video) String() string      { return "[视频]" }
func (v *Video) Clone() Component {
	return &Video{URL: v.URL, Path: v.Path, FileID: v.FileID, Cover: v.Cover}
}

// Face represents a QQ emoji.
type Face struct {
	ID string `json:"id,omitempty"`
}

func (f *Face) Type() ComponentType { return CompFace }
func (f *Face) String() string      { return fmt.Sprintf("[表情:%s]", f.ID) }
func (f *Face) Clone() Component    { return &Face{ID: f.ID} }

// Emoji represents a custom emoji/sticker.
type Emoji struct {
	ID  string `json:"id,omitempty"`
	URL string `json:"url,omitempty"`
}

func (e *Emoji) Type() ComponentType { return CompEmoji }
func (e *Emoji) String() string      { return "[Emoji]" }
func (e *Emoji) Clone() Component    { return &Emoji{ID: e.ID, URL: e.URL} }

// Node represents a single forward message node.
type Node struct {
	UIN     string      `json:"uin,omitempty"`
	Name    string      `json:"name,omitempty"`
	Content []Component `json:"content,omitempty"`
}

func (n *Node) Type() ComponentType { return CompNode }
func (n *Node) String() string      { return "[转发节点]" }
func (n *Node) Clone() Component {
	content := make([]Component, len(n.Content))
	for i, c := range n.Content {
		content[i] = c.Clone()
	}
	return &Node{UIN: n.UIN, Name: n.Name, Content: content}
}

// Nodes represents multiple forward message nodes. ForwardIDs carries the
// remote combined-forward message ids (OneBot get_forward_msg) when the
// node list itself has not been fetched yet.
type Nodes struct {
	Nodes      []*Node  `json:"nodes,omitempty"`
	ForwardIDs []string `json:"forward_ids,omitempty"`
}

// IDs returns the remote forward-message ids.
func (n *Nodes) IDs() []string {
	if len(n.ForwardIDs) > 0 {
		return n.ForwardIDs
	}
	return nil
}

func (n *Nodes) Type() ComponentType { return CompNodes }
func (n *Nodes) String() string      { return "[合并转发]" }
func (n *Nodes) Clone() Component {
	nodes := make([]*Node, len(n.Nodes))
	for i, node := range n.Nodes {
		if node != nil {
			nodes[i] = node.Clone().(*Node)
		}
	}
	forwardIDs := make([]string, len(n.ForwardIDs))
	copy(forwardIDs, n.ForwardIDs)
	return &Nodes{Nodes: nodes, ForwardIDs: forwardIDs}
}

// Poke represents a poke action.
type Poke struct {
	Target   string `json:"target,omitempty"`
	PokeType string `json:"poke_type,omitempty"`
	Name     string `json:"name,omitempty"`
}

func (p *Poke) Type() ComponentType { return CompPoke }
func (p *Poke) String() string      { return "[戳一戳]" }
func (p *Poke) Clone() Component    { return &Poke{Target: p.Target, PokeType: p.PokeType, Name: p.Name} }

// Music represents a music share.
type Music struct {
	MusicType string `json:"_type,omitempty"` // "custom" or platform-specific
	ID        string `json:"id,omitempty"`
	URL       string `json:"url,omitempty"`
	Audio     string `json:"audio,omitempty"`
	Title     string `json:"title,omitempty"`
}

func (m *Music) Type() ComponentType { return CompMusic }
func (m *Music) String() string      { return "[音乐分享]" }
func (m *Music) Clone() Component {
	return &Music{MusicType: m.MusicType, ID: m.ID, URL: m.URL, Audio: m.Audio, Title: m.Title}
}

// Forward represents a forwarded message reference.
type Forward struct {
	ID string `json:"id,omitempty"`
}

func (f *Forward) Type() ComponentType { return CompForward }
func (f *Forward) String() string      { return "[转发消息]" }
func (f *Forward) Clone() Component    { return &Forward{ID: f.ID} }

// Json represents a JSON card message.
type Json struct {
	Data map[string]interface{} `json:"data,omitempty"`
}

func (j *Json) Type() ComponentType { return CompJson }
func (j *Json) String() string      { return fmt.Sprintf("[Json:%v]", j.Data) }
func (j *Json) Clone() Component {
	return &Json{Data: deepCopyJSONMap(j.Data)}
}

// deepCopyJSONMap recursively copies a JSON card payload so the clone never
// shares map or slice storage with the original.
func deepCopyJSONMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = deepCopyJSONValue(v)
	}
	return out
}

func deepCopyJSONValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		return deepCopyJSONMap(t)
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, e := range t {
			out[i] = deepCopyJSONValue(e)
		}
		return out
	default:
		return v
	}
}

// Share represents a link share.
type Share struct {
	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`
}

func (s *Share) Type() ComponentType { return CompShare }
func (s *Share) String() string      { return "[分享]" }
func (s *Share) Clone() Component    { return &Share{URL: s.URL, Title: s.Title} }

// Contact represents a recommended contact or group.
type Contact struct {
	ContactType string `json:"_type,omitempty"` // "qq" or "group"
	ID          string `json:"id,omitempty"`
}

func (c *Contact) Type() ComponentType { return CompContact }
func (c *Contact) String() string      { return "[推荐]" }
func (c *Contact) Clone() Component    { return &Contact{ContactType: c.ContactType, ID: c.ID} }

// Location represents a location message.
type Location struct {
	Lat float64 `json:"lat,omitempty"`
	Lon float64 `json:"lon,omitempty"`
}

func (l *Location) Type() ComponentType { return CompLocation }
func (l *Location) String() string      { return "[位置]" }
func (l *Location) Clone() Component    { return &Location{Lat: l.Lat, Lon: l.Lon} }

// Shake represents a window shake/poke.
type Shake struct{}

func (s *Shake) Type() ComponentType { return CompShake }
func (s *Shake) String() string      { return "[窗口抖动]" }
func (s *Shake) Clone() Component    { return &Shake{} }

// Dice represents a dice emoji.
type Dice struct{}

func (d *Dice) Type() ComponentType { return CompDice }
func (d *Dice) String() string      { return "[骰子]" }
func (d *Dice) Clone() Component    { return &Dice{} }

// RPS represents a rock-paper-scissors emoji.
type RPS struct{}

func (r *RPS) Type() ComponentType { return CompRPS }
func (r *RPS) String() string      { return "[猜拳]" }
func (r *RPS) Clone() Component    { return &RPS{} }

// Unknown represents an unrecognized message component.
type Unknown struct {
	Text string `json:"text,omitempty"`
}

func (u *Unknown) Type() ComponentType { return CompUnknown }
func (u *Unknown) String() string      { return u.Text }
func (u *Unknown) Clone() Component    { return &Unknown{Text: u.Text} }
