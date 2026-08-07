// Package pipeline implements the message processing stages.
// Ported from astrbot/core/pipeline/
package pipeline

import (
	"context"
	"strings"
	"sync"

	"github.com/AstrBotDevs/AstrBot/internal/core"
	"github.com/AstrBotDevs/AstrBot/internal/log"
	"github.com/AstrBotDevs/AstrBot/pkg/message"
)

var logger = log.GetDefault().WithComponent("Pipeline")

// StageResult controls pipeline flow after a stage.
type StageResult = core.StageResult

// WhitelistCheckStage filters events by whitelist/blacklist.
type WhitelistCheckStage struct {
	whitelist map[string]bool
	blacklist map[string]bool
}

// NewWhitelistCheckStage creates the stage.
func NewWhitelistCheckStage() *WhitelistCheckStage {
	return &WhitelistCheckStage{
		whitelist: make(map[string]bool),
		blacklist: make(map[string]bool),
	}
}

// Name returns the stage name.
func (s *WhitelistCheckStage) Name() string { return "whitelist_check" }

// SetWhitelist replaces the whitelist.
func (s *WhitelistCheckStage) SetWhitelist(ids []string) {
	s.whitelist = make(map[string]bool, len(ids))
	for _, id := range ids {
		s.whitelist[id] = true
	}
}

// SetBlacklist replaces the blacklist.
func (s *WhitelistCheckStage) SetBlacklist(ids []string) {
	s.blacklist = make(map[string]bool, len(ids))
	for _, id := range ids {
		s.blacklist[id] = true
	}
}

// Process checks if the event's sender is allowed.
func (s *WhitelistCheckStage) Process(ctx context.Context, event *core.Event) (*StageResult, error) {
	// Whitelist is not yet configured -> allow all
	if len(s.whitelist) == 0 && len(s.blacklist) == 0 {
		return &StageResult{Continue: true}, nil
	}

	senderKey := event.Source.Platform + ":" + event.Source.SenderID
	if s.blacklist[senderKey] {
		return &StageResult{Continue: false}, nil
	}
	if len(s.whitelist) > 0 && !s.whitelist[senderKey] {
		return &StageResult{Continue: false}, nil
	}
	return &StageResult{Continue: true}, nil
}

// WakingCheckStage determines if the bot should respond (wake prefix, @mention).
type WakingCheckStage struct {
	mu          sync.RWMutex
	wakePrefix  string
	botID       string
}

// NewWakingCheckStage creates the stage.
func NewWakingCheckStage(botID, wakePrefix string) *WakingCheckStage {
	return &WakingCheckStage{
		botID:      botID,
		wakePrefix: wakePrefix,
	}
}

// Name returns the stage name.
func (s *WakingCheckStage) Name() string { return "waking_check" }

// Process checks wake conditions.
func (s *WakingCheckStage) Process(ctx context.Context, event *core.Event) (*StageResult, error) {
	if event.Source.IsAdmin {
		event.Source.IsAtBot = true
		return &StageResult{Continue: true}, nil
	}

	// Check @bot
	if event.Message != nil {
		for _, comp := range event.Message.Components {
			if at, ok := comp.(*message.At); ok && at.TargetID == s.botID {
				event.Source.IsAtBot = true
				return &StageResult{Continue: true}, nil
			}
		}
	}

	// Check wake prefix
	msgStr := strings.TrimSpace(event.MessageStr)
	s.mu.RLock()
	prefix := s.wakePrefix
	s.mu.RUnlock()

	if prefix != "" && strings.HasPrefix(msgStr, prefix) {
		event.Source.IsAtBot = true
		event.MessageStr = strings.TrimPrefix(msgStr, prefix)
		return &StageResult{Continue: true}, nil
	}

	// Private message always wakes
	if !event.Source.IsGroup {
		event.Source.IsAtBot = true
		return &StageResult{Continue: true}, nil
	}

	return &StageResult{Continue: false}, nil
}

// CommandStage processes commands registered by plugins.
type CommandStage struct {
	commandMap map[string]CommandHandler
}

// CommandHandler is a function that handles a command.
type CommandHandler func(ctx context.Context, event *core.Event, args []string) (*StageResult, error)

// NewCommandStage creates the stage.
func NewCommandStage() *CommandStage {
	return &CommandStage{commandMap: make(map[string]CommandHandler)}
}

// Name returns the stage name.
func (s *CommandStage) Name() string { return "command" }

// Register adds a command.
func (s *CommandStage) Register(name string, handler CommandHandler) {
	s.commandMap[strings.ToLower(name)] = handler
}

// Process handles command messages.
func (s *CommandStage) Process(ctx context.Context, event *core.Event) (*StageResult, error) {
	if !event.Source.IsAtBot {
		return &StageResult{Continue: true}, nil
	}

	msgStr := strings.TrimSpace(event.MessageStr)
	if msgStr == "" {
		return &StageResult{Continue: true}, nil
	}

	parts := strings.Fields(msgStr)
	if len(parts) == 0 {
		return &StageResult{Continue: true}, nil
	}

	cmdName := strings.ToLower(parts[0])
	handler, ok := s.commandMap[cmdName]
	if !ok {
		return &StageResult{Continue: true}, nil
	}

	return handler(ctx, event, parts[1:])
}

// ProcessStage handles LLM request/response.
type ProcessStage struct {
	llmHandler func(ctx context.Context, event *core.Event) (*StageResult, error)
}

// NewProcessStage creates the stage.
func NewProcessStage(handler func(ctx context.Context, event *core.Event) (*StageResult, error)) *ProcessStage {
	return &ProcessStage{llmHandler: handler}
}

// Name returns the stage name.
func (s *ProcessStage) Name() string { return "process" }

// Process delegates to the LLM handler.
func (s *ProcessStage) Process(ctx context.Context, event *core.Event) (*StageResult, error) {
	if s.llmHandler == nil {
		return &StageResult{Continue: true}, nil
	}
	return s.llmHandler(ctx, event)
}

// ResultStage sends the reply back to the platform.
type ResultStage struct {
	sender func(sessionID string, text string) error
}

// NewResultStage creates the stage.
func NewResultStage(sender func(string, string) error) *ResultStage {
	return &ResultStage{sender: sender}
}

// Name returns the stage name.
func (s *ResultStage) Name() string { return "result" }

// Process sends any accumulated reply.
func (s *ResultStage) Process(ctx context.Context, event *core.Event) (*StageResult, error) {
	// The reply would be set by the process stage on the event metadata
	// For now this is a pass-through
	return &StageResult{Continue: false}, nil
}
