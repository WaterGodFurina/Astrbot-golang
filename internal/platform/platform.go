// Package platform defines the platform adapter interface and base implementations.
// Ported from astrbot/core/platform/
package platform

import (
	"context"
	"sync"

	"github.com/AstrBotDevs/AstrBot/internal/core"
	"github.com/AstrBotDevs/AstrBot/internal/log"
	"github.com/AstrBotDevs/AstrBot/pkg/message"
)

var logger = log.GetDefault().WithComponent("Platform")

// PlatformAdapter connects a messaging platform (QQ, Telegram, etc.) to AstrBot.
type PlatformAdapter interface {
	ID() string
	Type() string
	Start(ctx context.Context) error
	Stop() error
	Send(sessionID string, chain *message.MessageChain) error
}

// PlatformManager manages all platform adapters.
type PlatformManager struct {
	mu       sync.RWMutex
	adapters map[string]PlatformAdapter
}

// NewPlatformManager creates an empty manager.
func NewPlatformManager() *PlatformManager {
	return &PlatformManager{adapters: make(map[string]PlatformAdapter)}
}

// Register adds a platform adapter.
func (pm *PlatformManager) Register(a PlatformAdapter) {
	pm.mu.Lock()
	pm.adapters[a.ID()] = a
	pm.mu.Unlock()
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

// StartAll starts all registered adapters.
func (pm *PlatformManager) StartAll(ctx context.Context) error {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	for _, a := range pm.adapters {
		if err := a.Start(ctx); err != nil {
			logger.Error("Failed to start platform %s: %v", a.ID(), err)
		}
	}
	return nil
}

// StopAll stops all registered adapters.
func (pm *PlatformManager) StopAll() {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	for _, a := range pm.adapters {
		_ = a.Stop()
	}
}

// BaseAdapter provides common adapter functionality.
type BaseAdapter struct {
	id       string
	platform string
	eventBus *core.EventBus
}

// NewBaseAdapter creates a base adapter.
func NewBaseAdapter(id, platform string, bus *core.EventBus) *BaseAdapter {
	return &BaseAdapter{id: id, platform: platform, eventBus: bus}
}

// ID returns the adapter ID.
func (b *BaseAdapter) ID() string { return b.id }

// Type returns the platform type.
func (b *BaseAdapter) Type() string { return b.platform }

// PublishEvent wraps and publishes an incoming event.
func (b *BaseAdapter) PublishEvent(senderID, senderName, convID string, isGroup bool, msg *message.MessageChain) error {
	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   b.platform,
			SelfID:     b.id,
			SenderID:   senderID,
			SenderName: senderName,
			ConvID:     convID,
			IsGroup:    isGroup,
		},
		Message:   msg,
	}
	return b.eventBus.Publish(event)
}
