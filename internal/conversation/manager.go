// Package conversation implements conversation/session management.
// Ported from astrbot/core/db/po.py (Conversation) and
// astrbot/core/star/session_llm_manager.py (SessionServiceManager)
package conversation

import (
	"fmt"
	"sync"
	"time"
)

// Conversation represents a chat conversation.
type Conversation struct {
	CID              string                   `json:"cid"`
	UserID           string                   `json:"user_id"`
	PlatformID       string                   `json:"platform_id"`
	UnifiedMsgOrigin string                   `json:"unified_msg_origin"`
	History          []map[string]interface{} `json:"history,omitempty"`
	Persona          string                   `json:"persona_id,omitempty"`
	Title            string                   `json:"title,omitempty"`
	CreatedAt        time.Time                `json:"created_at"`
	UpdatedAt        time.Time                `json:"updated_at"`
	IsDeleted        bool                     `json:"is_deleted"`
}

// NewConversation creates a conversation.
func NewConversation(unifiedMsgOrigin, platformID string) *Conversation {
	now := time.Now()
	return &Conversation{
		CID:              fmt.Sprintf("conv_%d", now.UnixNano()),
		PlatformID:       platformID,
		UnifiedMsgOrigin: unifiedMsgOrigin,
		History:          []map[string]interface{}{},
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

// Manager manages conversations.
type Manager struct {
	mu            sync.RWMutex
	conversations map[string]*Conversation // key = unified_msg_origin
}

// NewManager creates a conversation manager.
func NewManager() *Manager {
	return &Manager{conversations: make(map[string]*Conversation)}
}

// NewConversation creates and stores a new conversation.
func (m *Manager) NewConversation(unifiedMsgOrigin, platformID string) *Conversation {
	conv := NewConversation(unifiedMsgOrigin, platformID)
	m.mu.Lock()
	m.conversations[unifiedMsgOrigin] = conv
	m.mu.Unlock()
	return conv
}

// GetConversation returns the conversation for a session.
func (m *Manager) GetConversation(unifiedMsgOrigin string) *Conversation {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.conversations[unifiedMsgOrigin]
}

// AllConversations returns all conversations.
func (m *Manager) AllConversations() []*Conversation {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*Conversation, 0, len(m.conversations))
	for _, conv := range m.conversations {
		result = append(result, conv)
	}
	return result
}

// GetCurrConversationID returns the conversation ID for a session.
func (m *Manager) GetCurrConversationID(unifiedMsgOrigin string) string {
	conv := m.GetConversation(unifiedMsgOrigin)
	if conv == nil {
		return ""
	}
	return conv.CID
}

// UpdateConversation updates the conversation.
func (m *Manager) UpdateConversation(conv *Conversation) {
	m.mu.Lock()
	defer m.mu.Unlock()
	conv.UpdatedAt = time.Now()
	m.conversations[conv.UnifiedMsgOrigin] = conv
}

// DeleteConversation soft-deletes a conversation.
func (m *Manager) DeleteConversation(unifiedMsgOrigin string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if conv, ok := m.conversations[unifiedMsgOrigin]; ok {
		conv.IsDeleted = true
		conv.UpdatedAt = time.Now()
	}
}

// AppendHistory appends a message to conversation history.
func (m *Manager) AppendHistory(unifiedMsgOrigin string, role, content string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	conv, ok := m.conversations[unifiedMsgOrigin]
	if !ok {
		return
	}
	conv.History = append(conv.History, map[string]interface{}{
		"role":    role,
		"content": content,
	})
	conv.UpdatedAt = time.Now()
}

// ClearHistory clears conversation history.
func (m *Manager) ClearHistory(unifiedMsgOrigin string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	conv, ok := m.conversations[unifiedMsgOrigin]
	if !ok {
		return
	}
	conv.History = []map[string]interface{}{}
	conv.UpdatedAt = time.Now()
}

// SetPersona sets the persona for a conversation.
func (m *Manager) SetPersona(unifiedMsgOrigin, personaID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	conv, ok := m.conversations[unifiedMsgOrigin]
	if !ok {
		return
	}
	conv.Persona = personaID
	conv.UpdatedAt = time.Now()
}

// SessionServiceManager tracks whether sessions are enabled.
type SessionServiceManager struct {
	mu       sync.RWMutex
	disabled map[string]bool // key = unified_msg_origin
}

// NewSessionServiceManager creates a session service manager.
func NewSessionServiceManager() *SessionServiceManager {
	return &SessionServiceManager{disabled: make(map[string]bool)}
}

// IsSessionEnabled returns true if the session is enabled.
func (s *SessionServiceManager) IsSessionEnabled(unifiedMsgOrigin string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.disabled[unifiedMsgOrigin]
}

// DisableSession disables a session.
func (s *SessionServiceManager) DisableSession(unifiedMsgOrigin string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disabled[unifiedMsgOrigin] = true
}

// EnableSession enables a session.
func (s *SessionServiceManager) EnableSession(unifiedMsgOrigin string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.disabled, unifiedMsgOrigin)
}
