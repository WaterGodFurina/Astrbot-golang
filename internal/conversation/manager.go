// Package conversation implements conversation/session management.
// Ported from astrbot/core/db/po.py (Conversation) and
// astrbot/core/conversation_mgr.py (ConversationManager)
package conversation

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/db"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

var logger = log.GetDefault().WithComponent("Conversation")

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
		UserID:           unifiedMsgOrigin,
		PlatformID:       platformID,
		UnifiedMsgOrigin: unifiedMsgOrigin,
		History:          []map[string]interface{}{},
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

// Manager manages conversations, mirroring Python's ConversationManager.
//
// Unlike a single map keyed by unified_msg_origin, a session can hold MANY
// conversations (one per `/new`). Python keeps a per-session "selected
// conversation" pointer (session_conversations[umo] -> conversation_id) and
// stores every conversation in the DB. We mirror that:
//   - byCID  : every known conversation (loaded from DB + created at runtime)
//   - current: umo -> cid of the conversation currently selected for a session
//
// Conversations are lazily created (Python's `_get_session_conv`) and
// persisted to SQLite so history survives restarts.
type Manager struct {
	mu      sync.RWMutex
	db      *db.Database
	byCID   map[string]*Conversation // key = conversation_id
	current map[string]string        // key = unified_msg_origin -> conversation_id
}

// NewManager creates a conversation manager backed by the database. A nil db
// keeps the manager purely in-memory (tests).
func NewManager(database *db.Database) *Manager {
	m := &Manager{
		db:      database,
		byCID:   make(map[string]*Conversation),
		current: make(map[string]string),
	}
	if database != nil {
		m.loadFromDB()
	}
	return m
}

// rowToConversation converts a DB row into a Conversation.
func rowToConversation(row db.ConversationRow) *Conversation {
	conv := &Conversation{
		CID:              row.ConversationID,
		UserID:           row.UserID,
		PlatformID:       row.PlatformID,
		UnifiedMsgOrigin: row.UserID,
		Title:            cleanMentionPrefix(row.Title),
		Persona:          row.PersonaID,
		History:          []map[string]interface{}{},
	}
	if row.Content != "" {
		var hist []map[string]interface{}
		if json.Unmarshal([]byte(row.Content), &hist) == nil && hist != nil {
			conv.History = hist
		}
	}
	if t, err := time.Parse("2006-01-02 15:04:05", row.CreatedAt); err == nil {
		conv.CreatedAt = t
	} else if t, err := time.Parse(time.RFC3339, row.CreatedAt); err == nil {
		conv.CreatedAt = t
	}
	if t, err := time.Parse("2006-01-02 15:04:05", row.UpdatedAt); err == nil {
		conv.UpdatedAt = t
	} else {
		conv.UpdatedAt = conv.CreatedAt
	}
	return conv
}

// loadFromDB loads all persisted conversations into the in-memory cache and
// selects the most recently updated conversation per session as current.
// updated_at has second granularity, so inner_conversation_id breaks ties.
func (m *Manager) loadFromDB() {
	rows, err := m.db.ListConversations()
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	best := make(map[string]*db.ConversationRow) // umo -> best row
	for i := range rows {
		row := &rows[i]
		conv := rowToConversation(*row)
		m.byCID[conv.CID] = conv
		cur := best[conv.UserID]
		if cur == nil || row.UpdatedAt > cur.UpdatedAt ||
			(row.UpdatedAt == cur.UpdatedAt && row.InnerID > cur.InnerID) {
			best[conv.UserID] = row
			m.current[conv.UserID] = conv.CID
		}
	}
}

// GetOrCreateConversation returns the current conversation for a session,
// lazily creating (and persisting) one when it does not exist. Mirrors
// Python's `_get_session_conv`: get_curr_conversation_id -> new_conversation.
func (m *Manager) GetOrCreateConversation(unifiedMsgOrigin, platformID string) *Conversation {
	if conv := m.GetConversation(unifiedMsgOrigin); conv != nil {
		return conv
	}
	return m.NewConversation(unifiedMsgOrigin, platformID)
}

// NewConversation creates a fresh conversation and makes it the selected one
// for the session (persisted). The previous conversation stays in the DB.
func (m *Manager) NewConversation(unifiedMsgOrigin, platformID string) *Conversation {
	conv := NewConversation(unifiedMsgOrigin, platformID)
	if platformID == "" {
		if parts := splitPlatform(unifiedMsgOrigin); parts != "" {
			conv.PlatformID = parts
		}
	}
	m.mu.Lock()
	m.byCID[conv.CID] = conv
	m.current[unifiedMsgOrigin] = conv.CID
	m.mu.Unlock()
	m.persist(conv)
	return conv
}

// GetConversation returns the current conversation for a session, loading it
// from the database on a cache miss.
func (m *Manager) GetConversation(unifiedMsgOrigin string) *Conversation {
	m.mu.RLock()
	cid := m.current[unifiedMsgOrigin]
	conv := m.byCID[cid]
	m.mu.RUnlock()
	if conv != nil || m.db == nil {
		return conv
	}
	row, found, err := m.db.GetConversationByUserID(unifiedMsgOrigin)
	if err != nil || !found {
		return nil
	}
	loaded := rowToConversation(row)
	m.mu.Lock()
	m.byCID[loaded.CID] = loaded
	m.current[unifiedMsgOrigin] = loaded.CID
	m.mu.Unlock()
	return loaded
}

// AllConversations returns all conversations.
func (m *Manager) AllConversations() []*Conversation {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*Conversation, 0, len(m.byCID))
	for _, conv := range m.byCID {
		if !conv.IsDeleted {
			result = append(result, conv)
		}
	}
	return result
}

// GetAllConversations returns conversations serialized in the shape the
// dashboard WebUI expects (mirrors Python's conversation_service
// _serialize_conversation), ordered by updated_at desc.
func (m *Manager) GetAllConversations() []interface{} {
	m.mu.RLock()
	convs := make([]*Conversation, 0, len(m.byCID))
	for _, conv := range m.byCID {
		if !conv.IsDeleted {
			convs = append(convs, conv)
		}
	}
	m.mu.RUnlock()
	sort.Slice(convs, func(i, j int) bool { return convs[i].UpdatedAt.After(convs[j].UpdatedAt) })

	result := make([]interface{}, 0, len(convs))
	for _, conv := range convs {
		result = append(result, serializeConversation(conv))
	}
	return result
}

// GetConversationByCID returns a serialized conversation by its cid, or nil.
func (m *Manager) GetConversationByCID(cid string) map[string]interface{} {
	m.mu.RLock()
	conv := m.byCID[cid]
	m.mu.RUnlock()
	if conv != nil && !conv.IsDeleted {
		return serializeConversation(conv)
	}
	if m.db != nil {
		row, found, err := m.db.GetConversationByID(cid)
		if err == nil && found {
			return serializeConversation(rowToConversation(row))
		}
	}
	return nil
}

// serializeConversation renders a conversation in the dashboard API shape.
func serializeConversation(conv *Conversation) map[string]interface{} {
	history := make([]interface{}, 0, len(conv.History))
	for _, h := range conv.History {
		history = append(history, h)
	}
	return map[string]interface{}{
		"cid":         conv.CID,
		"platform_id": conv.PlatformID,
		"user_id":     conv.UserID,
		"title":       conv.Title,
		"persona_id":  conv.Persona,
		"token_usage": 0,
		"created_at":  conv.CreatedAt.Format(time.RFC3339),
		"updated_at":  conv.UpdatedAt.Format(time.RFC3339),
		"umo_info":    buildUMOInfo(conv.UnifiedMsgOrigin),
		"history":     history,
		"is_deleted":  conv.IsDeleted,
	}
}

// buildUMOInfo mirrors Python's _build_umo_info + parse_umo.
func buildUMOInfo(umo string) map[string]interface{} {
	parts := strings.SplitN(umo, ":", 3)
	platform := "unknown"
	messageType := "unknown"
	sessionID := umo
	if len(parts) >= 1 && parts[0] != "" {
		platform = parts[0]
	}
	if len(parts) >= 2 && parts[1] != "" {
		messageType = parts[1]
	}
	if len(parts) >= 3 {
		sessionID = parts[2]
	}
	return map[string]interface{}{
		"umo":          umo,
		"platform":     platform,
		"message_type": messageType,
		"session_id":   sessionID,
		"auto_name":    "",
		"user_alias":   "",
		"display_name": umo,
	}
}

// BuildUMOInfo is the exported form of buildUMOInfo.
func BuildUMOInfo(umo string) map[string]interface{} {
	return buildUMOInfo(umo)
}

// CountConversations returns the number of active (non-deleted) conversations.
func (m *Manager) CountConversations() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, conv := range m.byCID {
		if !conv.IsDeleted {
			count++
		}
	}
	return count
}

// ActiveUMOs returns the distinct session UMOs that have conversations
// (mirrors Python's list_known_umos over the conversations table).
func (m *Manager) ActiveUMOs() []string {
	m.mu.RLock()
	set := make(map[string]bool)
	for _, conv := range m.byCID {
		if !conv.IsDeleted && conv.UserID != "" {
			set[conv.UserID] = true
		}
	}
	m.mu.RUnlock()
	umos := make([]string, 0, len(set))
	for u := range set {
		umos = append(umos, u)
	}
	sort.Strings(umos)
	return umos
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
	conv.UpdatedAt = time.Now()
	m.byCID[conv.CID] = conv
	m.mu.Unlock()
	m.persist(conv)
}

// DeleteConversation removes the current conversation of a session (persisted).
func (m *Manager) DeleteConversation(unifiedMsgOrigin string) {
	m.mu.Lock()
	cid := m.current[unifiedMsgOrigin]
	conv := m.byCID[cid]
	if conv == nil {
		m.mu.Unlock()
		return
	}
	delete(m.byCID, cid)
	delete(m.current, unifiedMsgOrigin)
	m.mu.Unlock()
	if m.db != nil {
		_ = m.db.DeleteConversationByID(cid)
	}
}

// FindByCID returns a conversation by its cid, or nil.
func (m *Manager) FindByCID(cid string) *Conversation {
	m.mu.RLock()
	conv := m.byCID[cid]
	m.mu.RUnlock()
	if conv != nil && !conv.IsDeleted {
		return conv
	}
	return nil
}

// SetTitleByCID sets a conversation title by cid. Returns false if not found.
func (m *Manager) SetTitleByCID(cid, title string) bool {
	m.mu.Lock()
	conv := m.byCID[cid]
	if conv == nil || conv.IsDeleted {
		m.mu.Unlock()
		return false
	}
	conv.Title = title
	conv.UpdatedAt = time.Now()
	m.mu.Unlock()
	if m.db != nil {
		_ = m.db.UpdateConversationTitle(cid, title)
	}
	return true
}

// SetPersonaByCID sets a conversation persona by cid. Returns false if not found.
func (m *Manager) SetPersonaByCID(cid, personaID string) bool {
	m.mu.Lock()
	conv := m.byCID[cid]
	if conv == nil || conv.IsDeleted {
		m.mu.Unlock()
		return false
	}
	conv.Persona = personaID
	conv.UpdatedAt = time.Now()
	m.mu.Unlock()
	if m.db != nil {
		_ = m.db.UpdateConversationPersona(cid, personaID)
	}
	return true
}

// ReplaceHistoryByCID replaces a conversation's history by cid.
// Returns false if not found.
func (m *Manager) ReplaceHistoryByCID(cid string, history []map[string]interface{}) bool {
	m.mu.Lock()
	conv := m.byCID[cid]
	if conv == nil || conv.IsDeleted {
		m.mu.Unlock()
		return false
	}
	conv.History = history
	conv.UpdatedAt = time.Now()
	m.mu.Unlock()
	m.persist(conv)
	return true
}

// DeleteConversationByCID removes a conversation by cid. Returns false if not found.
func (m *Manager) DeleteConversationByCID(cid string) bool {
	m.mu.Lock()
	conv := m.byCID[cid]
	if conv == nil || conv.IsDeleted {
		m.mu.Unlock()
		return false
	}
	delete(m.byCID, cid)
	// Clear the per-session pointer if it pointed at this conversation.
	for umo, cur := range m.current {
		if cur == cid {
			delete(m.current, umo)
		}
	}
	m.mu.Unlock()
	if m.db != nil {
		_ = m.db.DeleteConversationByID(cid)
	}
	return true
}

// AppendHistory appends a message to the current conversation of a session.
// The conversation is lazily created when missing (Python's `_get_session_conv`
// behavior).
func (m *Manager) AppendHistory(unifiedMsgOrigin string, role, content string) {
	m.mu.Lock()
	conv := m.byCID[m.current[unifiedMsgOrigin]]
	if conv == nil {
		conv = NewConversation(unifiedMsgOrigin, splitPlatform(unifiedMsgOrigin))
		m.byCID[conv.CID] = conv
		m.current[unifiedMsgOrigin] = conv.CID
	}
	conv.History = append(conv.History, map[string]interface{}{
		"role":    role,
		"content": content,
	})
	conv.UpdatedAt = time.Now()
	if conv.Title == "" && role == "user" {
		conv.Title = deriveTitle(content)
	}
	m.mu.Unlock()
	m.persist(conv)
}

// deriveTitle builds a conversation title from a user message, stripping
// platform mentions (e.g. "@qq_official/...") and trimming to a sane length.
func deriveTitle(content string) string {
	title := cleanMentionPrefix(content)
	title = strings.TrimSpace(title)
	if title == "" {
		title = "新对话"
	}
	if r := []rune(title); len(r) > 30 {
		title = string(r[:30])
	}
	return title
}

// cleanMentionPrefix removes a leading "@<mention>/" or "@<mention> " prefix.
func cleanMentionPrefix(s string) string {
	if !strings.HasPrefix(s, "@") {
		return s
	}
	rest := s[1:]
	if i := strings.IndexAny(rest, "/ \t"); i >= 0 {
		rest = rest[i+1:]
	}
	return strings.TrimLeft(rest, "/ \t")
}

// ClearHistory clears the current conversation's history (persisted).
func (m *Manager) ClearHistory(unifiedMsgOrigin string) {
	m.mu.Lock()
	conv := m.byCID[m.current[unifiedMsgOrigin]]
	if conv == nil {
		m.mu.Unlock()
		return
	}
	conv.History = []map[string]interface{}{}
	conv.UpdatedAt = time.Now()
	m.mu.Unlock()
	if m.db != nil {
		_ = m.db.UpdateConversationContent(conv.CID, "[]")
	}
}

// SetPersona sets the persona for the current conversation (persisted).
func (m *Manager) SetPersona(unifiedMsgOrigin, personaID string) {
	m.mu.Lock()
	conv := m.byCID[m.current[unifiedMsgOrigin]]
	if conv == nil {
		m.mu.Unlock()
		return
	}
	conv.Persona = personaID
	conv.UpdatedAt = time.Now()
	m.mu.Unlock()
	if m.db != nil {
		_ = m.db.UpdateConversationPersona(conv.CID, personaID)
	}
}

// persist writes a conversation to the database. Uses UpsertConversation
// (single transaction) to avoid the TOCTOU race between the existence check
// and INSERT/UPDATE that the previous Get+Create/Update sequence had.
func (m *Manager) persist(conv *Conversation) {
	if m.db == nil {
		return
	}
	historyJSON, err := json.Marshal(conv.History)
	if err != nil {
		logger.Error("persist conversation %s: marshal history: %v", conv.CID, err)
		return
	}
	if err := m.db.UpsertConversation(conv.CID, conv.UserID, conv.PlatformID, string(historyJSON), conv.Title, conv.Persona); err != nil {
		logger.Error("persist conversation %s: %v", conv.CID, err)
	}
}

// splitPlatform extracts the platform id from a unified_msg_origin
// ("platform:type:session_id" -> "platform").
func splitPlatform(umo string) string {
	for i := 0; i < len(umo); i++ {
		if umo[i] == ':' {
			return umo[:i]
		}
	}
	return ""
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
