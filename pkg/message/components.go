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
        CompPlain   ComponentType = "Plain"
        CompAt      ComponentType = "At"
        CompAtAll   ComponentType = "AtAll"
        CompReply   ComponentType = "Reply"
        CompImage   ComponentType = "Image"
        CompRecord  ComponentType = "Record" // voice/audio
        CompFile    ComponentType = "File"
        CompVideo   ComponentType = "Video"
        CompFace    ComponentType = "Face" // QQ emoji
        CompEmoji   ComponentType = "Emoji"
        CompNode   ComponentType = "Node"
        CompNodes  ComponentType = "Nodes"
        CompPoke   ComponentType = "Poke"
        CompMusic  ComponentType = "Music"
        CompForward ComponentType = "Forward"
        CompJson   ComponentType = "Json"
        CompShare  ComponentType = "Share"
        CompContact ComponentType = "Contact"
        CompLocation ComponentType = "Location"
        CompShake  ComponentType = "Shake"
        CompDice   ComponentType = "Dice"
        CompRPS    ComponentType = "RPS"
        CompUnknown ComponentType = "Unknown"

        // Deprecated aliases for backward compat
        CompVoice ComponentType = "Record"
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

// AtAll represents @all.
type AtAll struct{}

func (a *AtAll) Type() ComponentType { return CompAtAll }
func (a *AtAll) String() string      { return "@全体成员" }
func (a *AtAll) Clone() Component     { return &AtAll{} }

// Reply represents a quoted/replied message.
type Reply struct {
        MessageID    string      `json:"message_id,omitempty"`
        SenderID    string      `json:"sender_id,omitempty"`
        SenderNick  string      `json:"sender_nick,omitempty"`
        Chain       []Component `json:"chain,omitempty"`
        MessageStr  string      `json:"message_str,omitempty"`
        CreatedAt   time.Time   `json:"created_at,omitempty"`
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
                MessageID:   r.MessageID,
                SenderID:    r.SenderID,
                SenderNick:  r.SenderNick,
                Chain:       chain,
                MessageStr:  r.MessageStr,
                CreatedAt:   r.CreatedAt,
        }
}

// Image represents an image component.
type Image struct {
        URL      string `json:"url,omitempty"`
        Path     string `json:"path,omitempty"`
        File     string `json:"file,omitempty"`
        Base64   string `json:"base64,omitempty"`
        FileID   string `json:"file_id,omitempty"`
        Width    int    `json:"width,omitempty"`
        Height   int    `json:"height,omitempty"`
}

func (img *Image) Type() ComponentType { return CompImage }
func (img *Image) String() string      { return "[图片]" }
func (img *Image) Clone() Component {
        return &Image{
                URL: img.URL, Path: img.Path, File: img.File, Base64: img.Base64,
                FileID: img.FileID, Width: img.Width, Height: img.Height,
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
        Text   string `json:"text,omitempty"` // text representation for TTS
}

func (r *Record) Type() ComponentType { return CompRecord }
func (r *Record) String() string      { return "[语音]" }
func (r *Record) Clone() Component {
        return &Record{URL: r.URL, Path: r.Path, File: r.File, Base64: r.Base64, FileID: r.FileID, Text: r.Text}
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
}

func (v *Video) Type() ComponentType { return CompVideo }
func (v *Video) String() string      { return "[视频]" }
func (v *Video) Clone() Component {
        return &Video{URL: v.URL, Path: v.Path, FileID: v.FileID}
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
        return &Nodes{Nodes: nodes}
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
func (m *Music) Clone() Component    { return &Music{MusicType: m.MusicType, ID: m.ID, URL: m.URL, Audio: m.Audio, Title: m.Title} }

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
func (j *Json) Clone() Component    { return &Json{Data: j.Data} }

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
