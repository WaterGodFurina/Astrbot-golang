// Package star - plugin manager stub.
package star

import (
	"github.com/AstrBotDevs/AstrBot/internal/db"
)

// Manager manages plugins and commands.
type Manager struct {
	db *db.Database
}

// NewManager creates a plugin manager.
func NewManager(database *db.Database) *Manager {
	return &Manager{db: database}
}
