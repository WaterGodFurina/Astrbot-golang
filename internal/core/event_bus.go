// Package core implements the event bus and pipeline scheduler.
// Ported from astrbot/core/event_bus.py and astrbot/core/pipeline/
package core

import (
	"context"
	"fmt"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
	"sync"
	"time"
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

	// Ctx, when set, is the execution context for this event's pipeline run
	// (e.g. a WebSocket chat session). Stages use it in place of the dispatch
	// loop's context so a caller can cancel just this run (interrupt).
	Ctx context.Context

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
	// queue is a growable slice guarded by queueMu. It replaces the previous
	// fixed-capacity channel: Publish never drops events (it blocks once the
	// queue reaches maxQueueCap) and the queue auto-grows under bursts.
	queueMu   sync.Mutex
	cond      *sync.Cond
	queue     []*Event
	queueCap  int
	stopCh    chan struct{}
	done      chan struct{} // closed when the dispatch loop exits
	stopOnce  sync.Once     // 保证 stopCh 只 close 一次（Stop 可被重复调用）
}

// maxQueueCap bounds how large the event queue may grow before Publish blocks.
// Events are pointers, so even 100k buffered events cost only a few MB — the
// bound exists to avoid unbounded growth, not to save memory.
const maxQueueCap = 100000

// NewEventBus creates a new event bus.
func NewEventBus(bufferSize int) *EventBus {
	if bufferSize <= 0 {
		bufferSize = 1000
	}
	bus := &EventBus{
		schedulers: make(map[string]*PipelineScheduler),
		queueCap:   bufferSize,
		stopCh:     make(chan struct{}),
		done:       make(chan struct{}),
	}
	bus.cond = sync.NewCond(&bus.queueMu)
	return bus
}

// RegisterScheduler adds a pipeline scheduler.
func (bus *EventBus) RegisterScheduler(confID string, scheduler *PipelineScheduler) {
	bus.mu.Lock()
	bus.schedulers[confID] = scheduler
	bus.mu.Unlock()
}

// GetScheduler returns the pipeline scheduler registered for a config ID.
func (bus *EventBus) GetScheduler(confID string) *PipelineScheduler {
	bus.mu.RLock()
	defer bus.mu.RUnlock()
	return bus.schedulers[confID]
}

// Start begins dispatching events.
func (bus *EventBus) Start(ctx context.Context) error {
	defer close(bus.done)
	// Wake a cond.Wait() blocked with an empty queue when the context is
	// cancelled (shutdown); Stop() already broadcasts via stopCh.
	go func() {
		select {
		case <-ctx.Done():
			bus.queueMu.Lock()
			bus.cond.Broadcast()
			bus.queueMu.Unlock()
		case <-bus.done:
		}
	}()
	for {
		bus.queueMu.Lock()
		for len(bus.queue) == 0 && !bus.isStopped() {
			bus.cond.Wait()
			select {
			case <-ctx.Done():
				bus.queueMu.Unlock()
				return ctx.Err()
			default:
			}
		}
		if len(bus.queue) == 0 {
			// stopped and drained
			bus.queueMu.Unlock()
			return nil
		}
		event := bus.queue[0]
		bus.queue = bus.queue[1:]
		bus.cond.Broadcast() // wake blocked publishers
		bus.queueMu.Unlock()
		bus.dispatch(ctx, event)
	}
}

// Publish enqueues an event for processing. It never drops events: the queue
// auto-grows under bursts, and once it reaches maxQueueCap Publish blocks until
// a slot frees (back-pressure) or the bus stops.
func (bus *EventBus) Publish(event *Event) error {
	bus.queueMu.Lock()
	defer bus.queueMu.Unlock()
	for len(bus.queue) >= bus.queueCap && bus.queueCap >= maxQueueCap && !bus.isStopped() {
		bus.cond.Wait()
	}
	if bus.isStopped() {
		return fmt.Errorf("event bus stopped")
	}
	if len(bus.queue) >= bus.queueCap && bus.queueCap < maxQueueCap {
		old := bus.queueCap
		bus.queueCap *= 2
		if bus.queueCap > maxQueueCap {
			bus.queueCap = maxQueueCap
		}
		logger.I18nWarn("EventBus 队列已自动扩容: %d → %d（事件 %q）", old, bus.queueCap, event.MessageStr)
	}
	bus.queue = append(bus.queue, event)
	bus.cond.Signal()
	return nil
}

// isStopped 报告事件总线是否已停止（非阻塞检查 stopCh 是否已关闭）。
func (bus *EventBus) isStopped() bool {
	select {
	case <-bus.stopCh:
		return true
	default:
		return false
	}
}

// PublishDelayed re-enqueues an event after the given delay. Used by the
// rate-limit stall strategy so an over-window message is processed once the
// window frees up instead of being dropped.
func (bus *EventBus) PublishDelayed(event *Event, delay time.Duration) {
	if delay <= 0 {
		if err := bus.Publish(event); err != nil {
			logger.I18nWarn("延迟发布（立即）失败: %v", err)
		}
		return
	}
	time.AfterFunc(delay, func() {
		if bus.isStopped() {
			return
		}
		if err := bus.Publish(event); err != nil {
			logger.I18nWarn("延迟发布失败（事件被丢弃）: %v", err)
		}
	})
}

// Stop shuts down the event bus and waits for the dispatch loop to exit (with
// a bounded timeout). Callers must stop the bus before closing the database so
// no in-flight event can touch storage after it is closed.
func (bus *EventBus) Stop() {
	bus.stopOnce.Do(func() {
		bus.queueMu.Lock()
		close(bus.stopCh)
		bus.cond.Broadcast()
		bus.queueMu.Unlock()
	})
	select {
	case <-bus.done:
	case <-time.After(eventBusStopTimeout):
		logger.I18nWarn("EventBus 调度循环在关闭时未在 %v 内退出", eventBusStopTimeout)
	}
}

// eventBusStopTimeout bounds how long Stop waits for the dispatch loop. The
// loop normally exits promptly once its context is cancelled (LLM calls carry
// the context; plugin RPCs fail fast after the plugin processes are killed);
// the timeout guards against a permanently stuck stage.
const eventBusStopTimeout = 30 * time.Second

// dispatch runs the event through every registered scheduler. The scheduler
// set is snapshotted under a read lock, then Process is invoked outside the
// lock so a slow pipeline (up to minutes) can never stall RegisterScheduler /
// ReloadPipelineScheduler (which need the write lock).
func (bus *EventBus) dispatch(ctx context.Context, event *Event) {
	bus.mu.RLock()
	schedulers := make([]*PipelineScheduler, 0, len(bus.schedulers))
	for _, scheduler := range bus.schedulers {
		schedulers = append(schedulers, scheduler)
	}
	bus.mu.RUnlock()
	logger.I18nInfo("EventBus: 正在分发消息 %q（调度器=%d）", event.MessageStr, len(schedulers))

	for _, scheduler := range schedulers {
		result, err := scheduler.Process(ctx, event)
		if err != nil {
			logger.Error("Pipeline task failed: %v", err)
		}
		if result != nil && !result.Continue {
			break
		}
	}
	// Signal completion so publishers that enqueued the event (e.g. dashboard
	// chat) can observe that the pipeline run finished.
	if event.Metadata != nil {
		if done, ok := event.Metadata[MetadataPipelineDone].(*PipelineDone); ok {
			done.Signal()
		}
	}
}

// PipelineDone is a completion signal closed exactly once when an event
// finishes dispatching. Publishers stash a *PipelineDone in Event.Metadata
// under MetadataPipelineDone to observe pipeline completion (e.g. dashboard
// chat streaming). It is idempotent so a rate-limit re-publish of the same
// event cannot double-close the channel.
type PipelineDone struct {
	once sync.Once
	ch   chan struct{}
}

// NewPipelineDone creates a fresh completion signal.
func NewPipelineDone() *PipelineDone {
	return &PipelineDone{ch: make(chan struct{})}
}

// Done returns the channel closed when the pipeline finishes processing.
func (d *PipelineDone) Done() <-chan struct{} { return d.ch }

// Signal marks the event as fully processed (safe to call more than once).
func (d *PipelineDone) Signal() {
	d.once.Do(func() { close(d.ch) })
}

// MetadataPipelineDone is the Event.Metadata key under which a *PipelineDone
// completion signal is stored.
const MetadataPipelineDone = "__pipeline_done"

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
			logger.I18nInfo("流水线: 阶段 %s 拦截了事件 %q", stage.Name(), event.MessageStr)
			return result, nil
		}
	}
	logger.I18nInfo("流水线: 事件 %q 已通过所有阶段", event.MessageStr)
	return &StageResult{Continue: false}, nil
}
