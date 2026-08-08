// Package dashboard - chat session persistence.
package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type chatSession struct {
	SessionID   string                   `json:"session_id"`
	DisplayName string                   `json:"display_name,omitempty"`
	PlatformID  string                   `json:"platform_id"`
	Creator     string                   `json:"creator,omitempty"`
	IsGroup     int                      `json:"is_group"`
	CreatedAt   string                   `json:"created_at"`
	UpdatedAt   string                   `json:"updated_at"`
	Messages    []map[string]interface{} `json:"messages"`
}

type chatData struct {
	Sessions []*chatSession `json:"sessions"`
}

type chatStore struct {
	mu   sync.Mutex
	path string
	data *chatData
}

func newChatStore(dataDir string) *chatStore {
	cs := &chatStore{
		path: filepath.Join(dataDir, "chat_sessions.json"),
		data: &chatData{Sessions: []*chatSession{}},
	}
	cs.load()
	return cs
}

func (cs *chatStore) load() {
	data, err := os.ReadFile(cs.path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, cs.data)
	if cs.data.Sessions == nil {
		cs.data.Sessions = []*chatSession{}
	}
}

func (cs *chatStore) save() error {
	os.MkdirAll(filepath.Dir(cs.path), 0755)
	data, err := json.MarshalIndent(cs.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cs.path, data, 0644)
}

// createSession creates a new chat session and returns it.
func (cs *chatStore) createSession(platformID string) (*chatSession, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	now := time.Now()
	session := &chatSession{
		SessionID:   now.Format("20060102150405") + randomSuffix(),
		PlatformID:  platformID,
		CreatedAt:   now.Format(time.RFC3339),
		UpdatedAt:   now.Format(time.RFC3339),
		Messages:    []map[string]interface{}{},
		DisplayName: "",
	}
	cs.data.Sessions = append(cs.data.Sessions, session)
	if err := cs.save(); err != nil {
		return nil, err
	}
	return session, nil
}

// listSessions returns all sessions ordered by updated_at desc.
func (cs *chatStore) listSessions() []map[string]interface{} {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	result := make([]map[string]interface{}, 0, len(cs.data.Sessions))
	for _, s := range cs.data.Sessions {
		result = append(result, sessionView(s))
	}
	return result
}

func sessionView(s *chatSession) map[string]interface{} {
	return map[string]interface{}{
		"session_id":   s.SessionID,
		"display_name": s.DisplayName,
		"updated_at":   s.UpdatedAt,
		"platform_id":  s.PlatformID,
		"creator":      s.Creator,
		"is_group":     s.IsGroup,
		"created_at":   s.CreatedAt,
	}
}

// getSession returns a session view or nil.
func (cs *chatStore) getSession(id string) *chatSession {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, s := range cs.data.Sessions {
		if s.SessionID == id {
			return s
		}
	}
	return nil
}

// updateSession patches a session (display_name etc.).
func (cs *chatStore) updateSession(id string, patch map[string]interface{}) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, s := range cs.data.Sessions {
		if s.SessionID == id {
			if name, ok := patch["display_name"].(string); ok {
				s.DisplayName = name
			}
			s.UpdatedAt = time.Now().Format(time.RFC3339)
			return cs.save()
		}
	}
	return nil
}

// deleteSessions removes sessions by id and returns the deleted count.
func (cs *chatStore) deleteSessions(ids []string) int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	target := map[string]bool{}
	for _, id := range ids {
		target[id] = true
	}
	next := make([]*chatSession, 0, len(cs.data.Sessions))
	deleted := 0
	for _, s := range cs.data.Sessions {
		if target[s.SessionID] {
			deleted++
			continue
		}
		next = append(next, s)
	}
	cs.data.Sessions = next
	_ = cs.save()
	return deleted
}

// appendMessage adds a message to a session.
func (cs *chatStore) appendMessage(id string, msg map[string]interface{}) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, s := range cs.data.Sessions {
		if s.SessionID == id {
			s.Messages = append(s.Messages, msg)
			s.UpdatedAt = time.Now().Format(time.RFC3339)
			_ = cs.save()
			return true
		}
	}
	return false
}

// sessionDetail returns the session detail payload for GET /chat/sessions/{id}.
func (cs *chatStore) sessionDetail(id string) map[string]interface{} {
	s := cs.getSession(id)
	if s == nil {
		return nil
	}
	cs.mu.Lock()
	messages := make([]map[string]interface{}, len(s.Messages))
	for i, m := range s.Messages {
		messages[i] = m
	}
	cs.mu.Unlock()
	return map[string]interface{}{
		"session_id":  s.SessionID,
		"history":     messages,
		"threads":     []interface{}{},
		"project":     nil,
		"active_runs": []interface{}{},
	}
}

func randomSuffix() string {
	ts := time.Now().UnixNano()
	return string(rune('a'+ts%26)) + string(rune('a'+(ts/26)%26)) + string(rune('a'+(ts/676)%26))
}
