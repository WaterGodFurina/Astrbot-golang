// Package core implements the event bus and pipeline scheduler.
// Ported from astrbot/core/event_bus.py and astrbot/core/pipeline/
package core

import (
	"context"
	"fmt"
	"hash/fnv"
	"runtime"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
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
	PlatformID string // platform_id (adapter instance id, config.id), first segment of Python unified_msg_origin
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

	// Trace records agent invocation spans (TracePage /api/v1/trace).
	Trace *log.TraceSpan
}

// UnifiedMsgOrigin returns platform:conversation_id.
func (e *Event) UnifiedMsgOrigin() string {
	return fmt.Sprintf("%s:%s", e.Source.Platform, e.Source.ConvID)
}

// pythonMessageType maps the host message-type value to the Python
// MessageType enum value used in the three-part unified_msg_origin
// ("GroupMessage"/"FriendMessage"/"OtherMessage"). OneBot adapters store the
// raw "group"/"private" values; most others store the platform AstrBotMessage
// type string directly. Falls back to FriendMessage.
func pythonMessageType(messageType string, isGroup bool) string {
	if messageType != "" {
		switch messageType {
		case "GroupMessage", "FriendMessage", "OtherMessage":
			return messageType
		case "group", "Group", "GROUP":
			return "GroupMessage"
		case "private", "Private", "PRIVATE", "friend":
			return "FriendMessage"
		}
	}
	if isGroup {
		return "GroupMessage"
	}
	return "FriendMessage"
}

// PythonUMO returns the Python-sdk style three-part unified_msg_origin
// "platform_id:MessageType:session_id" (mirrors MessageSession.__str__ in the
// original AstrBot). It is used wherever an event must align with the
// Python plugin's unified_msg_origin key (SessionWaitStage, CoreEventToSDK).
// The two-part UnifiedMsgOrigin() is kept for host-internal session keys.
func (e *Event) PythonUMO() string {
	platformID := e.Source.PlatformID
	if platformID == "" {
		platformID = e.Source.Platform
	}
	mt := ""
	if e.MessageObj != nil {
		mt = e.MessageObj.MessageType
	}
	msgType := pythonMessageType(mt, e.Source.IsGroup)
	return fmt.Sprintf("%s:%s:%s", platformID, msgType, e.Source.ConvID)
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
	queueMu  sync.Mutex
	cond     *sync.Cond
	queue    []*Event
	queueCap int
	stopCh   chan struct{}
	done     chan struct{} // closed when the dispatch loop exits
	stopOnce sync.Once     // 保证 stopCh 只 close 一次（Stop 可被重复调用）

	// workerChans is a fixed set of per-worker buffered channels. The dispatch
	// loop hashes each event to a worker so events from the same session (UMO)
	// are processed in order while different sessions run concurrently. A
	// worker drains its channel before exiting on shutdown.
	workerChans []chan *Event
	// workerStop is closed after the dispatch loop exits to tell workers to
	// drain their remaining events and finish.
	workerStop chan struct{}
	// workerWG tracks in-flight worker dispatch so Stop can wait for every
	// event already routed to a worker to finish.
	workerWG sync.WaitGroup
}

// maxQueueCap bounds how large the event queue may grow before Publish blocks.
// Events are pointers, so even 100k buffered events cost only a few MB — the
// bound exists to avoid unbounded growth, not to save memory.
const maxQueueCap = 100000

// minWorkers / maxWorkers clamp the per-session worker pool size.
const (
	minWorkers = 4
	maxWorkers = 8
)

// workerBufferSize is the per-worker channel capacity. The main queue provides
// global back-pressure; the worker buffers only smooth the pop-and-route step.
const workerBufferSize = 64

// NewEventBus creates a new event bus.
func NewEventBus(bufferSize int) *EventBus {
	if bufferSize <= 0 {
		bufferSize = 1000
	}
	bus := &EventBus{
		schedulers:  make(map[string]*PipelineScheduler),
		queueCap:    bufferSize,
		stopCh:      make(chan struct{}),
		done:        make(chan struct{}),
		workerStop:  make(chan struct{}),
		workerChans: make([]chan *Event, 0, workerCount()),
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

// Start begins dispatching events. Events are routed to per-session workers
// so one slow pipeline (up to minutes) cannot block other sessions' events.
func (bus *EventBus) Start(ctx context.Context) error {
	defer close(bus.done)
	// Workers must only drain-and-exit after the dispatch loop (their only
	// producer) has exited, otherwise they would miss events still being
	// routed during shutdown.
	defer close(bus.workerStop)
	bus.startWorkers(ctx, workerCount())
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
		if !bus.enqueueToWorker(ctx, event) {
			// Context cancelled: remaining queued events are dropped, matching
			// the pre-worker behaviour of the dispatch loop returning on ctx.
			return ctx.Err()
		}
	}
}

// workerCount returns the number of dispatch workers, clamped to a sane range.
func workerCount() int {
	n := runtime.NumCPU()
	if n < minWorkers {
		n = minWorkers
	}
	if n > maxWorkers {
		n = maxWorkers
	}
	return n
}

// startWorkers launches the worker goroutines. Each worker consumes events
// from its shard channel sequentially, preserving per-session ordering. On
// workerStop it drains any events still buffered before returning.
func (bus *EventBus) startWorkers(ctx context.Context, n int) {
	if len(bus.workerChans) == 0 {
		bus.workerChans = make([]chan *Event, n)
		for i := range bus.workerChans {
			bus.workerChans[i] = make(chan *Event, workerBufferSize)
		}
	}
	for _, ch := range bus.workerChans {
		bus.workerWG.Add(1)
		go func(ch chan *Event) {
			defer bus.workerWG.Done()
			for {
				select {
				case event := <-ch:
					bus.dispatchSafely(ctx, event)
				case <-bus.workerStop:
					// Producer (dispatch loop) has exited; process whatever is
					// still buffered for this worker, then finish.
					for {
						select {
						case event := <-ch:
							bus.dispatchSafely(ctx, event)
						default:
							return
						}
					}
				}
			}
		}(ch)
	}
}

// dispatchSafely runs dispatch with panic recovery so a panic while
// dispatching one event (e.g. the scheduler snapshot, dispatch logging or a
// broken completion signal) cannot crash the whole process; the worker logs
// it and keeps processing subsequent events. Stage panics are already
// recovered inside PipelineScheduler.Process, so this only guards panics in
// dispatch itself and anything else outside the scheduler's recover.
func (bus *EventBus) dispatchSafely(ctx context.Context, event *Event) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("EventBus worker recovered from panic while dispatching event: %v", r)
		}
	}()
	bus.dispatch(ctx, event)
}

// enqueueToWorker routes an event to the worker owning its session shard,
// blocking until a slot frees up (per-shard back-pressure). It returns false
// only when the context is cancelled, in which case the caller abandons the
// remaining queue.
func (bus *EventBus) enqueueToWorker(ctx context.Context, event *Event) bool {
	shard := eventShardKey(event) % len(bus.workerChans)
	select {
	case bus.workerChans[shard] <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

// eventShardKey derives a stable shard key for an event from its session
// origin so all events of one session land on the same worker.
func eventShardKey(event *Event) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(event.UnifiedMsgOrigin()))
	return int(h.Sum32())
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
		// The callback runs on a timer goroutine: recover so a panic here
		// (e.g. inside Publish) cannot crash the process.
		defer func() {
			if r := recover(); r != nil {
				logger.Error("EventBus recovered from panic in delayed publish: %v", r)
			}
		}()
		if bus.isStopped() {
			return
		}
		if err := bus.Publish(event); err != nil {
			logger.I18nWarn("延迟发布失败（事件被丢弃）: %v", err)
		}
	})
}

// Stop shuts down the event bus and waits for the dispatch loop and all
// dispatch workers to exit (with a bounded timeout). Callers must stop the
// bus before closing the database so no in-flight event can touch storage
// after it is closed.
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
	// Wait for workers to finish events already routed to them.
	done := make(chan struct{})
	go func() {
		bus.workerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(eventBusStopTimeout):
		logger.I18nWarn("EventBus 分发 worker 在关闭时未在 %v 内退出", eventBusStopTimeout)
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
	logger.Debug("EventBus: 正在分发消息 %q（调度器=%d）", event.MessageStr, len(schedulers))

	for _, scheduler := range schedulers {
		result, err := scheduler.Process(ctx, event)
		if err != nil {
			logger.Error("Pipeline task failed: %v", err)
			break
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
			logger.Debug("流水线: 阶段 %s 拦截了事件 %q", stage.Name(), event.MessageStr)
			return result, nil
		}
	}
	logger.I18nInfo("流水线: 事件 %q 已通过所有阶段", event.MessageStr)
	return &StageResult{Continue: false}, nil
}
