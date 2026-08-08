// Package star - plugin manager.
package star

import (
	"github.com/AstrBotDevs/AstrBot/internal/db"
)

// Manager manages plugins, handlers, and commands.
type Manager struct {
	db       *db.Database
	registry *StarRegistry
	handlers *StarHandlerRegistry
}

// NewManager creates a plugin manager.
func NewManager(database *db.Database) *Manager {
	return &Manager{
		db:       database,
		registry: NewStarRegistry(),
		handlers: NewStarHandlerRegistry(),
	}
}

// NewManagerSimple creates a plugin manager without database.
func NewManagerSimple() *Manager {
	return &Manager{
		registry: NewStarRegistry(),
		handlers: NewStarHandlerRegistry(),
	}
}

// Registry returns the star registry.
func (m *Manager) Registry() *StarRegistry { return m.registry }

// Handlers returns the handler registry.
func (m *Manager) Handlers() *StarHandlerRegistry { return m.handlers }
