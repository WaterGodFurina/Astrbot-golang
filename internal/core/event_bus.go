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
	IsGroup    bool
	IsAtBot    bool
	IsAdmin    bool
}

// Event represents an incoming message event.
type Event struct {
	Type        EventType
	Source      EventSource
	Message     *message.MessageChain
	RawMessage  string
	MessageStr  string
	Timestamp   time.Time
	Reply       *Event // quoted reply target
	Metadata    map[string]interface{}
}

// GetUnifiedMsgOrigin returns platform:conversation_id.
func (e *Event) GetUnifiedMsgOrigin() string {
	return fmt.Sprintf("%s:%s", e.Source.Platform, e.Source.ConvID)
}

// GetSenderID returns the sender's user ID.
func (e *Event) GetSenderID() string {
	return e.Source.SenderID
}

// GetMessages returns the message chain.
func (e *Event) GetMessages() *message.MessageChain {
	return e.Message
}

// PipelineStage processes events in sequence.
type PipelineStage interface {
	Name() string
	Process(ctx context.Context, event *Event) (*StageResult, error)
}

// StageResult controls pipeline flow.
type StageResult struct {
	Continue  bool   // true = continue to next stage, false = stop
	Reply     string // immediate reply text (if any)
	ReplyChain *message.MessageChain
	Error     error
}

// EventBus dispatches events to pipeline schedulers.
type EventBus struct {
	mu        sync.RWMutex
	schedulers map[string]*PipelineScheduler // keyed by config_id
	queue     chan *Event
	stopCh    chan struct{}
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

// Stop shuts down the event bus.
func (bus *EventBus) Stop() {
	close(bus.stopCh)
}

func (bus *EventBus) dispatch(ctx context.Context, event *Event) {
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
func (s *PipelineScheduler) Process(ctx context.Context, event *Event) (*StageResult, error) {
	for _, stage := range s.stages {
		result, err := stage.Process(ctx, event)
		if err != nil {
			return nil, fmt.Errorf("stage %s: %w", stage.Name(), err)
		}
		if result != nil && !result.Continue {
			return result, nil
		}
	}
	return &StageResult{Continue: false}, nil
}
