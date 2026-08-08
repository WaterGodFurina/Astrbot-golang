// Package persona implements persona/prompt template management.
// Ported from astrbot/core/persona_mgr.py
package persona

import (
	"fmt"
	"sync"
)

// Persona represents a personality configuration for the LLM.
type Persona struct {
	ID         string                            `json:"id"`
	Name       string                            `json:"name"`
	Prompt     string                            `json:"prompt"`
	IsDefault  bool                              `json:"is_default"`
	IsHidden   bool                              `json:"is_hidden"`
	Tools      []string                          `json:"tools,omitempty"`
	MCPConfigs map[string]map[string]interface{} `json:"mcp_configs,omitempty"`
}

// PersonaManager manages personas.
type PersonaManager struct {
	mu        sync.RWMutex
	byID      map[string]*Persona
	byName    map[string]*Persona
	defaultID string
}

// NewPersonaManager creates a persona manager.
func NewPersonaManager() *PersonaManager {
	return &PersonaManager{
		byID:   make(map[string]*Persona),
		byName: make(map[string]*Persona),
	}
}

// Register adds or updates a persona.
func (m *PersonaManager) Register(p *Persona) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.ID == "" {
		p.ID = fmt.Sprintf("persona_%d", len(m.byID)+1)
	}
	m.byID[p.ID] = p
	m.byName[p.Name] = p
	if p.IsDefault {
		m.defaultID = p.ID
	}
}

// Get retrieves a persona by ID.
func (m *PersonaManager) Get(id string) *Persona {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byID[id]
}

// GetByName retrieves a persona by name.
func (m *PersonaManager) GetByName(name string) *Persona {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byName[name]
}

// Default returns the default persona.
func (m *PersonaManager) Default() *Persona {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.defaultID != "" {
		return m.byID[m.defaultID]
	}
	for _, p := range m.byID {
		return p
	}
	return nil
}

// All returns all personas.
func (m *PersonaManager) All() []*Persona {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*Persona, 0, len(m.byID))
	for _, p := range m.byID {
		result = append(result, p)
	}
	return result
}

// Delete removes a persona by ID.
func (m *PersonaManager) Delete(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.byID[id]
	if !ok {
		return false
	}
	delete(m.byID, id)
	delete(m.byName, p.Name)
	return true
}

// SetDefault sets the default persona.
func (m *PersonaManager) SetDefault(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.byID[id]
	if !ok {
		return false
	}
	if m.defaultID != "" {
		if old, ok := m.byID[m.defaultID]; ok {
			old.IsDefault = false
		}
	}
	p.IsDefault = true
	m.defaultID = id
	return true
}
