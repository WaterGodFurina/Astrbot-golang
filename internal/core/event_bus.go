// Package core implements the event bus and pipeline scheduler.
// Ported from astrbot/core/event_bus.py and astrbot/core/pipeline/
package core

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/log"
	"github.com/AstrBotDevs/AstrBot/pkg/message"
)

var logger = log.GetDefault().WithComponent("EventBus")

// EventType classifies incoming events.
type EventType int

const (
	EventMessage EventType = iota
	EventNotice
	EventRequest
	EventMeta
)

// EventSource identifies where an event came from.
type EventSource struct {
	Platform   string
	SelfID     string
	SenderID   string
	SenderName string
	ConvID     string // conversation/group ID
	GroupName  string // group display name, when known
	IsGroup    bool
	IsAtBot    bool
	IsAdmin    bool
}

// MessageObj holds the raw platform message metadata.
type MessageObj struct {
	MessageID   string
	SelfID      string
	SessionID   string
	MessageType string // GroupMessage, FriendMessage, OtherMessage
	Platform    string
	MessageStr  string
	RawMessage  interface{}
	Timestamp   time.Time
}

// Event represents an incoming message event.
// Ported from astrbot/core/platform/astr_message_event.py
type Event struct {
	Type       EventType
	Source     EventSource
	Message    *message.MessageChain
	RawMessage string
	MessageStr string
	Timestamp  time.Time
	Reply      *Event // quoted reply target
	Metadata   map[string]interface{}

	// Pipeline state
	MessageObj        *MessageObj
	PlainText         string
	IsAtOrWakeCommand bool
	WakeCommand       string
	CallLLM           bool
	Role              string // "user", "admin", etc.
	Result            *message.MessageEventResult
	stopped           bool
	HasSendOper       bool
}

// UnifiedMsgOrigin returns platform:conversation_id.
func (e *Event) UnifiedMsgOrigin() string {
	return fmt.Sprintf("%s:%s", e.Source.Platform, e.Source.ConvID)
}

// GetUnifiedMsgOrigin returns platform:conversation_id (alias).
func (e *Event) GetUnifiedMsgOrigin() string {
	return e.UnifiedMsgOrigin()
}

// GetSenderID returns the sender's user ID.
func (e *Event) GetSenderID() string {
	return e.Source.SenderID
}

// GetMessages returns the message chain.
func (e *Event) GetMessages() *message.MessageChain {
	return e.Message
}

// GetMessageType returns "GroupMessage" or "FriendMessage".
func (e *Event) GetMessageType() string {
	if e.Source.IsGroup {
		return "GroupMessage"
	}
	return "FriendMessage"
}

// GetGroupID returns the group ID or empty string.
func (e *Event) GetGroupID() string {
	if e.Source.IsGroup {
		return e.Source.ConvID
	}
	return ""
}

// GetPlatformID returns the platform name.
func (e *Event) GetPlatformID() string {
	return e.Source.Platform
}

// Stop marks the event as stopped (no further stage processing).
func (e *Event) Stop() {
	e.stopped = true
}

// IsStopped returns true if the event was stopped.
func (e *Event) IsStopped() bool {
	return e.stopped
}

// SetResult sets the event result.
func (e *Event) SetResult(result *message.MessageEventResult) {
	e.Result = result
}

// GetResult returns the event result.
func (e *Event) GetResult() *message.MessageEventResult {
	return e.Result
}

// ClearResult clears the event result.
func (e *Event) ClearResult() {
	e.Result = nil
}

// SetExtra sets a metadata key.
func (e *Event) SetExtra(key string, value interface{}) {
	if e.Metadata == nil {
		e.Metadata = make(map[string]interface{})
	}
	e.Metadata[key] = value
}

// GetExtra returns a metadata value.
func (e *Event) GetExtra(key string) interface{} {
	return e.Metadata[key]
}

// HasSendOper returns whether a send operation was performed.
func (e *Event) HasSend() bool {
	return e.HasSendOper
}

// PipelineStage processes events in sequence.
type PipelineStage interface {
	Name() string
	Process(ctx context.Context, event *Event) (*StageResult, error)
}

// EventBusPublisher is the interface for publishing events (used by platform adapters).
type EventBusPublisher interface {
	Publish(event *Event) error
}

// StageResult controls pipeline flow.
type StageResult struct {
	Continue   bool   // true = continue to next stage, false = stop
	Reply      string // immediate reply text (if any)
	ReplyChain *message.MessageChain
	Error      error
}

// EventBus dispatches events to pipeline schedulers.
type EventBus struct {
	mu         sync.RWMutex
	schedulers map[string]*PipelineScheduler // keyed by config_id
	queue      chan *Event
	stopCh     chan struct{}
}

// NewEventBus creates a new event bus.
func NewEventBus(bufferSize int) *EventBus {
	if bufferSize <= 0 {
		bufferSize = 1000
	}
	return &EventBus{
		schedulers: make(map[string]*PipelineScheduler),
		queue:      make(chan *Event, bufferSize),
		stopCh:     make(chan struct{}),
	}
}

// RegisterScheduler adds a pipeline scheduler.
func (bus *EventBus) RegisterScheduler(confID string, scheduler *PipelineScheduler) {
	bus.mu.Lock()
	bus.schedulers[confID] = scheduler
	bus.mu.Unlock()
}

// Start begins dispatching events.
func (bus *EventBus) Start(ctx context.Context) error {
	for {
		select {
		case event := <-bus.queue:
			bus.dispatch(ctx, event)
		case <-ctx.Done():
			return ctx.Err()
		case <-bus.stopCh:
			return nil
		}
	}
}

// Publish enqueues an event for processing.
func (bus *EventBus) Publish(event *Event) error {
	select {
	case bus.queue <- event:
		return nil
	default:
		return fmt.Errorf("event bus queue full")
	}
}

// PublishDelayed re-enqueues an event after the given delay. Used by the
// rate-limit stall strategy so an over-window message is processed once the
// window frees up instead of being dropped. If the queue is still full when
// the timer fires, the event is dropped.
func (bus *EventBus) PublishDelayed(event *Event, delay time.Duration) {
	if delay <= 0 {
		if err := bus.Publish(event); err != nil {
			logger.Warn("Delayed publish (immediate) failed: %v", err)
		}
		return
	}
	time.AfterFunc(delay, func() {
		if err := bus.Publish(event); err != nil {
			logger.Warn("Delayed publish failed (queue full, event dropped): %v", err)
		}
	})
}

// Stop shuts down the event bus.
func (bus *EventBus) Stop() {
	close(bus.stopCh)
}

func (bus *EventBus) dispatch(ctx context.Context, event *Event) {
	logger.Info("EventBus: dispatching message %q (schedulers=%d)", event.MessageStr, len(bus.schedulers))
	bus.mu.RLock()
	defer bus.mu.RUnlock()

	for _, scheduler := range bus.schedulers {
		result, err := scheduler.Process(ctx, event)
		if err != nil {
			logger.Error("Pipeline task failed: %v", err)
		}
		if result != nil && !result.Continue {
			break
		}
	}
}

// PipelineScheduler runs events through a chain of stages.
type PipelineScheduler struct {
	confID string
	stages []PipelineStage
}

// NewPipelineScheduler creates a scheduler.
func NewPipelineScheduler(confID string) *PipelineScheduler {
	return &PipelineScheduler{confID: confID}
}

// AddStage appends a processing stage.
func (s *PipelineScheduler) AddStage(stage PipelineStage) {
	s.stages = append(s.stages, stage)
}

// Process runs the event through all stages.
func (s *PipelineScheduler) Process(ctx context.Context, event *Event) (result *StageResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Pipeline panic while processing event %q: %v", event.MessageStr, r)
			result = &StageResult{Continue: true}
			err = nil
		}
	}()
	for _, stage := range s.stages {
		result, err = stage.Process(ctx, event)
		if err != nil {
			return nil, fmt.Errorf("stage %s: %w", stage.Name(), err)
		}
		if result != nil && !result.Continue {
			logger.Info("Pipeline: stage %s stopped event %q", stage.Name(), event.MessageStr)
			return result, nil
		}
	}
	logger.Info("Pipeline: all stages passed for %q", event.MessageStr)
	return &StageResult{Continue: false}, nil
}
