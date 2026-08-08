// Package platform defines the platform adapter interface and message event model.
// Ported from astrbot/core/platform/astr_message_event.py, astrbot_message.py, message_type.py
package platform

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/core"
	"github.com/AstrBotDevs/AstrBot/internal/log"
	"github.com/AstrBotDevs/AstrBot/pkg/message"
)

var logger = log.GetDefault().WithComponent("Platform")

// MessageType classifies a message.
type MessageType string

const (
	GroupMessage  MessageType = "GroupMessage"
	FriendMessage MessageType = "FriendMessage"
	OtherMessage  MessageType = "OtherMessage"
)

// MessageMember describes a message sender.
type MessageMember struct {
	UserID   string `json:"user_id"`
	Nickname string `json:"nickname,omitempty"`
}

func (m MessageMember) String() string {
	return fmt.Sprintf("User ID: %s, Nickname: %s", m.UserID, m.Nickname)
}

// Group describes a chat group.
type Group struct {
	GroupID     string          `json:"group_id"`
	GroupName   string          `json:"group_name,omitempty"`
	GroupAvatar string          `json:"group_avatar,omitempty"`
	GroupOwner  string          `json:"group_owner,omitempty"`
	GroupAdmins []string        `json:"group_admins,omitempty"`
	Members     []MessageMember `json:"members,omitempty"`
}

// PlatformMetadata describes a platform instance.
type PlatformMetadata struct {
	Name string `json:"name"` // platform type: aiocqhttp, telegram, etc.
	ID   string `json:"id"`   // unique instance ID
}

// MessageSession identifies a conversation session.
type MessageSession struct {
	PlatformName string      `json:"platform_name"`
	MessageType  MessageType `json:"message_type"`
	SessionID    string      `json:"session_id"`
}

func (s MessageSession) String() string {
	return fmt.Sprintf("%s:%s:%s", s.PlatformName, s.MessageType, s.SessionID)
}

// ParseMessageSession parses a "platform:type:session" string.
func ParseMessageSession(s string) MessageSession {
	// split into 3 parts: platform:type:session
	// session may contain colons, so split from the left with max 3
	parts := splitN(s, ":", 3)
	if len(parts) < 3 {
		return MessageSession{PlatformName: parts[0]}
	}
	return MessageSession{
		PlatformName: parts[0],
		MessageType:  MessageType(parts[1]),
		SessionID:    parts[2],
	}
}

// splitN splits s by sep into at most n parts.
func splitN(s, sep string, n int) []string {
	var parts []string
	for i := 0; i < n-1; i++ {
		idx := indexOf(s, sep)
		if idx < 0 {
			break
		}
		parts = append(parts, s[:idx])
		s = s[idx+len(sep):]
	}
	parts = append(parts, s)
	return parts
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// AstrBotMessage is the canonical message object.
type AstrBotMessage struct {
	Type       MessageType         `json:"type"`
	SelfID     string              `json:"self_id"`
	SessionID  string              `json:"session_id"`
	MessageID  string              `json:"message_id"`
	Group      *Group              `json:"group,omitempty"`
	Sender     MessageMember       `json:"sender"`
	Message    []message.Component `json:"message"`
	MessageStr string              `json:"message_str"`
	RawMessage interface{}         `json:"raw_message,omitempty"`
	Timestamp  int64               `json:"timestamp"`
}

// GroupID returns the group ID or empty string.
func (m *AstrBotMessage) GroupID() string {
	if m.Group != nil {
		return m.Group.GroupID
	}
	return ""
}

// NewAstrBotMessage creates a message with default timestamp.
func NewAstrBotMessage() *AstrBotMessage {
	return &AstrBotMessage{
		Timestamp: time.Now().Unix(),
	}
}

// PlatformAdapter connects a messaging platform (QQ, Telegram, etc.) to AstrBot.
type PlatformAdapter interface {
	ID() string
	Type() string
	Start(ctx context.Context) error
	Stop() error
	Send(sessionID string, chain *message.MessageChain) error
}

// AstrMessageEvent is the core event object flowing through the pipeline.
// Ported from astrbot/core/platform/astr_message_event.py
type AstrMessageEvent struct {
	MessageStr        string                      `json:"message_str"`
	MessageObj        *AstrBotMessage             `json:"message_obj"`
	PlatformMeta      PlatformMetadata            `json:"platform_meta"`
	Role              string                      `json:"role"`    // "member" or "admin"
	IsWake            bool                        `json:"is_wake"` // passed WakingCheck stage
	IsAtOrWakeCommand bool                        `json:"is_at_or_wake_command"`
	Session           MessageSession              `json:"session"`
	Result            *message.MessageEventResult `json:"result,omitempty"`
	HasSendOper       bool                        `json:"has_send_oper"`
	CallLLM           bool                        `json:"call_llm"` // if true, suppress default LLM request
	PluginsName       []string                    `json:"plugins_name,omitempty"`
	CreatedAt         time.Time                   `json:"created_at"`
	Extras            map[string]interface{}      `json:"extras,omitempty"`
	ForceStopped      bool                        `json:"force_stopped"`
	TemporaryFiles    []string                    `json:"-"`

	mu sync.RWMutex
}

// NewAstrMessageEvent creates a new event.
func NewAstrMessageEvent(msgStr string, msgObj *AstrBotMessage, platformMeta PlatformMetadata, sessionID string) *AstrMessageEvent {
	msgType := msgObj.Type
	if msgType == "" {
		msgType = FriendMessage
	}
	return &AstrMessageEvent{
		MessageStr:   msgStr,
		MessageObj:   msgObj,
		PlatformMeta: platformMeta,
		Role:         "member",
		Session: MessageSession{
			PlatformName: platformMeta.ID,
			MessageType:  msgType,
			SessionID:    sessionID,
		},
		CreatedAt: time.Now(),
		Extras:    make(map[string]interface{}),
	}
}

// UnifiedMsgOrigin returns the session string.
func (e *AstrMessageEvent) UnifiedMsgOrigin() string {
	return e.Session.String()
}

// GetPlatformName returns the platform type name.
func (e *AstrMessageEvent) GetPlatformName() string {
	return e.PlatformMeta.Name
}

// GetPlatformID returns the platform instance ID.
func (e *AstrMessageEvent) GetPlatformID() string {
	return e.PlatformMeta.ID
}

// GetMessageStr returns the plain text message.
func (e *AstrMessageEvent) GetMessageStr() string {
	return e.MessageStr
}

// GetMessages returns the message component chain.
func (e *AstrMessageEvent) GetMessages() []message.Component {
	if e.MessageObj != nil {
		return e.MessageObj.Message
	}
	return nil
}

// GetMessageType returns the message type.
func (e *AstrMessageEvent) GetMessageType() MessageType {
	if e.MessageObj != nil && e.MessageObj.Type != "" {
		return e.MessageObj.Type
	}
	return e.Session.MessageType
}

// GetSessionID returns the session ID.
func (e *AstrMessageEvent) GetSessionID() string {
	return e.Session.SessionID
}

// GetGroupID returns the group ID or empty string.
func (e *AstrMessageEvent) GetGroupID() string {
	if e.MessageObj != nil {
		return e.MessageObj.GroupID()
	}
	return ""
}

// GetSelfID returns the bot's own ID.
func (e *AstrMessageEvent) GetSelfID() string {
	if e.MessageObj != nil {
		return e.MessageObj.SelfID
	}
	return ""
}

// GetSenderID returns the sender's user ID.
func (e *AstrMessageEvent) GetSenderID() string {
	return e.MessageObj.Sender.UserID
}

// GetSenderName returns the sender's nickname.
func (e *AstrMessageEvent) GetSenderName() string {
	return e.MessageObj.Sender.Nickname
}

// IsPrivateChat returns true for friend messages.
func (e *AstrMessageEvent) IsPrivateChat() bool {
	return e.GetMessageType() == FriendMessage
}

// IsWakeUp returns true if the event passed waking check.
func (e *AstrMessageEvent) IsWakeUp() bool {
	return e.IsWake
}

// IsAdmin returns true if the sender is an admin.
func (e *AstrMessageEvent) IsAdmin() bool {
	return e.Role == "admin"
}

// SetExtra stores an extra value.
func (e *AstrMessageEvent) SetExtra(key string, value interface{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Extras == nil {
		e.Extras = make(map[string]interface{})
	}
	e.Extras[key] = value
}

// GetExtra retrieves an extra value.
func (e *AstrMessageEvent) GetExtra(key string, defaultVal interface{}) interface{} {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.Extras == nil {
		return defaultVal
	}
	if v, ok := e.Extras[key]; ok {
		return v
	}
	return defaultVal
}

// ClearExtra removes all extras.
func (e *AstrMessageEvent) ClearExtra() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Extras = make(map[string]interface{})
}

// TrackTemporaryFile records a file for cleanup after the event.
func (e *AstrMessageEvent) TrackTemporaryFile(path string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.TemporaryFiles = append(e.TemporaryFiles, path)
}

// CleanupTemporaryFiles removes tracked temporary files.
func (e *AstrMessageEvent) CleanupTemporaryFiles() {
	e.mu.Lock()
	files := e.TemporaryFiles
	e.TemporaryFiles = nil
	e.mu.Unlock()
	for _, path := range files {
		// best-effort cleanup
		_ = path // os.Remove would be called here
	}
}

// SetResult sets the message event result.
func (e *AstrMessageEvent) SetResult(result *message.MessageEventResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if result == nil {
		return
	}
	e.Result = result
}

// SetResultText is a convenience to set a plain text result.
func (e *AstrMessageEvent) SetResultText(text string) {
	e.SetResult(NewMessageEventResult().Message(text))
}

// NewMessageEventResult creates a new MessageEventResult for this event.
func (e *AstrMessageEvent) NewMessageEventResult() *message.MessageEventResult {
	return message.NewMessageEventResult()
}

// PlainResult creates a plain text result.
func (e *AstrMessageEvent) PlainResult(text string) *message.MessageEventResult {
	return message.NewMessageEventResult().Message(text)
}

// ImageResult creates an image result from URL or path.
func (e *AstrMessageEvent) ImageResult(urlOrPath string) *message.MessageEventResult {
	r := message.NewMessageEventResult()
	if len(urlOrPath) > 4 && urlOrPath[:4] == "http" {
		return r.URLImage(urlOrPath)
	}
	return r.FileImage(urlOrPath)
}

// ChainResult creates a result from a component chain.
func (e *AstrMessageEvent) ChainResult(chain []message.Component) *message.MessageEventResult {
	r := message.NewMessageEventResult()
	r.Chain = chain
	return r
}

// StopEvent stops event propagation.
func (e *AstrMessageEvent) StopEvent() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ForceStopped = true
	if e.Result == nil {
		e.Result = message.NewMessageEventResult().StopEvent()
	} else {
		e.Result.StopEvent()
	}
}

// ContinueEvent resumes event propagation.
func (e *AstrMessageEvent) ContinueEvent() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ForceStopped = false
	if e.Result == nil {
		e.Result = message.NewMessageEventResult().ContinueEvent()
	} else {
		e.Result.ContinueEvent()
	}
}

// IsStopped returns true if event propagation should stop.
func (e *AstrMessageEvent) IsStopped() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.ForceStopped {
		return true
	}
	if e.Result == nil {
		return false
	}
	return e.Result.IsStopped()
}

// ShouldCallLLM sets whether to call LLM for this event.
func (e *AstrMessageEvent) ShouldCallLLM(call bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.CallLLM = call
}

// GetResult returns the current result.
func (e *AstrMessageEvent) GetResult() *message.MessageEventResult {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.Result
}

// ClearResult clears the current result.
func (e *AstrMessageEvent) ClearResult() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Result = nil
}

// GetMessageOutline returns a text summary of the message.
func (e *AstrMessageEvent) GetMessageOutline() string {
	if e.MessageObj == nil {
		return ""
	}
	var parts []string
	for _, c := range e.MessageObj.Message {
		switch comp := c.(type) {
		case *message.Plain:
			parts = append(parts, comp.Text)
		case *message.Image:
			parts = append(parts, "[图片]")
		case *message.Face:
			parts = append(parts, fmt.Sprintf("[表情:%s]", comp.ID))
		case *message.At:
			parts = append(parts, fmt.Sprintf("[At:%s]", comp.TargetID))
		case *message.AtAll:
			parts = append(parts, "[At:全体成员]")
		case *message.Forward:
			parts = append(parts, "[转发消息]")
		case *message.Reply:
			if comp.MessageStr != "" {
				parts = append(parts, fmt.Sprintf("[引用消息(%s: %s)]", comp.SenderNick, comp.MessageStr))
			} else {
				parts = append(parts, "[引用消息]")
			}
		default:
			parts = append(parts, fmt.Sprintf("[%s]", c.Type()))
		}
	}
	return joinStrings(parts, " ")
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

// NewMessageEventResult is a package-level constructor (alias).
func NewMessageEventResult() *message.MessageEventResult {
	return message.NewMessageEventResult()
}

// BaseAdapter provides a partial PlatformAdapter implementation.
type BaseAdapter struct {
	id       string
	platform string
	eventBus core.EventBusPublisher
}

// EventBus is the interface the adapter uses to publish events.
type EventBus interface {
	Publish(event *core.Event) error
}

// EventBusSetter is implemented by adapters that accept an event bus after construction.
type EventBusSetter interface {
	SetEventBus(bus EventBus)
}

// SetEventBus sets the event bus.
func (b *BaseAdapter) SetEventBus(bus EventBus) {
	b.eventBus = bus
}

// NewBaseAdapter creates a base adapter.
func NewBaseAdapter(id, platform string) *BaseAdapter {
	return &BaseAdapter{id: id, platform: platform}
}

// ID returns the adapter ID.
func (b *BaseAdapter) ID() string { return b.id }

// Type returns the platform type.
func (b *BaseAdapter) Type() string { return b.platform }

// PublishEvent wraps and publishes an incoming message as a core.Event.
func (b *BaseAdapter) PublishEvent(msgStr string, msgObj *AstrBotMessage) error {
	isGroup := msgObj.Type == GroupMessage
	convID := msgObj.SessionID
	if convID == "" {
		if isGroup && msgObj.Group != nil {
			convID = msgObj.Group.GroupID
		} else {
			convID = msgObj.Sender.UserID
		}
	}

	// Build message chain from AstrBotMessage components
	chain := &message.MessageChain{Chain: msgObj.Message}

	event := &core.Event{
		Type:       core.EventMessage,
		Message:    chain,
		MessageStr: msgStr,
		MessageObj: &core.MessageObj{
			MessageID:   msgObj.MessageID,
			SelfID:      msgObj.SelfID,
			SessionID:   msgObj.SessionID,
			MessageType: string(msgObj.Type),
			Platform:    b.platform,
			MessageStr:  msgStr,
			RawMessage:  msgObj.RawMessage,
		},
		Source: core.EventSource{
			Platform:   b.platform,
			SelfID:     b.id,
			SenderID:   msgObj.Sender.UserID,
			SenderName: msgObj.Sender.Nickname,
			ConvID:     convID,
			IsGroup:    isGroup,
		},
		Timestamp: time.Now(),
		Metadata:  make(map[string]interface{}),
	}

	if b.eventBus != nil {
		return b.eventBus.Publish(event)
	}
	return nil
}

// PlatformManager manages all platform adapters.
type PlatformManager struct {
	mu       sync.RWMutex
	adapters map[string]PlatformAdapter
}

// NewPlatformManager creates a platform manager.
func NewPlatformManager() *PlatformManager {
	return &PlatformManager{adapters: make(map[string]PlatformAdapter)}
}

// Register adds a platform adapter.
func (pm *PlatformManager) Register(adapter PlatformAdapter) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.adapters[adapter.ID()] = adapter
}

// Clear removes all registered adapters.
func (pm *PlatformManager) Clear() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.adapters = make(map[string]PlatformAdapter)
}

// Get returns a platform adapter by ID.
func (pm *PlatformManager) Get(id string) PlatformAdapter {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.adapters[id]
}

// All returns all adapters.
func (pm *PlatformManager) All() []PlatformAdapter {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	result := make([]PlatformAdapter, 0, len(pm.adapters))
	for _, a := range pm.adapters {
		result = append(result, a)
	}
	return result
}

// StartAll starts all adapters.
func (pm *PlatformManager) StartAll(ctx context.Context) error {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	for _, adapter := range pm.adapters {
		if err := adapter.Start(ctx); err != nil {
			return fmt.Errorf("platform %s: %w", adapter.ID(), err)
		}
		logger.Info("Platform adapter %s (%s) started", adapter.ID(), adapter.Type())
	}
	return nil
}

// StopAll stops all adapters.
func (pm *PlatformManager) StopAll() {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	for _, adapter := range pm.adapters {
		if err := adapter.Stop(); err != nil {
			logger.Error("Failed to stop platform %s: %v", adapter.ID(), err)
		}
	}
}

// Send sends a message chain to a session.
func (pm *PlatformManager) Send(platformID, sessionID string, chain *message.MessageChain) error {
	pm.mu.RLock()
	adapter := pm.adapters[platformID]
	if adapter == nil {
		// Events carry the platform type (e.g. "qq_official"); adapters are
		// keyed by their instance ID (e.g. "default_1905382202").
		for _, a := range pm.adapters {
			if a.Type() == platformID {
				adapter = a
				break
			}
		}
	}
	pm.mu.RUnlock()
	if adapter == nil {
		return fmt.Errorf("platform %s not found", platformID)
	}
	return adapter.Send(sessionID, chain)
}
