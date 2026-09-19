// Package core implements the event bus and pipeline scheduler.
// Ported from astrbot/core/event_bus.py and astrbot/core/pipeline/
package core

import (
	"context"
	"fmt"
	"hash/fnv"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
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

// MessageMember identifies a chat member (defined here so MessageObj can
// reference Group without an import cycle; platform package aliases it).
type MessageMember struct {
	UserID   string `json:"user_id"`
	Nickname string `json:"nickname,omitempty"`
}

// String implements the historical platform.MessageMember String method.
func (m MessageMember) String() string {
	return fmt.Sprintf("User ID: %s, Nickname: %s", m.UserID, m.Nickname)
}

// Group describes a chat group.
type Group struct {
	GroupID     string          `json:"group_id"`
	GroupName   string          `json:"group_name,omitempty"`
	GroupAvatar string          `json:"group_avatar,omitempty"`
	GroupOwner  string          `json:"group_owner,omitempty"`
	GroupAdmins []string        `json:"group_admins,omitempty"`
	Members     []MessageMember `json:"members,omitempty"`
	// MemberCount is the total member count, available even when the member
	// list is incomplete (aligned with Python MessageGroup.member_count).
	MemberCount *int `json:"member_count,omitempty"`
}
type MessageObj struct {
	MessageID   string
	SelfID      string
	SessionID   string
	MessageType string // GroupMessage, FriendMessage, OtherMessage
	Platform    string
	MessageStr  string
	RawMessage  interface{}
	Timestamp   time.Time
	// Group carries inbound group metadata (aligned with Python
	// AstrBotMessage.group); adapters fill what the platform provides.
	Group *Group
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

	// Metadata 承载平台附加字段（如 temporary_file_paths、extra_data）与管线
	// 运行期写入的键。事件发布后管线可能在其它 goroutine 中原地写入该 map，
	// 读取方（如 Misskey 发送时读取 extra_data）必须经 SetExtra/GetExtra/
	// MetadataSnapshot 访问，否则与管线写入并发触发 map race。
	// metadataMu 仅保护 map 的装载与键值访问，不递归保护值内部结构。
	metadataMu sync.RWMutex
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

// UnifiedMsgOrigin returns the three-part unified_msg_origin
// "platform_id:MessageType:session_id" (mirrors MessageSession.__str__ in the
// original AstrBot). The host uses this form everywhere as the session key —
// conversations, session rules, cron targets, session waiter and plugin SDK
// all share the same format, so no conversion is needed at any boundary.
func (e *Event) UnifiedMsgOrigin() string {
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
// (alias of UnifiedMsgOrigin — the host session key IS the three-part form).
func (e *Event) PythonUMO() string {
	return e.UnifiedMsgOrigin()
}

// GetUnifiedMsgOrigin returns the three-part unified_msg_origin (alias).
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

// GetGroup returns group information (aligned with Python
// AstrMessageEvent.get_group default implementation): the inbound group data
// when it matches, or an ID-only group object for an explicit query. Platform
// event enrichments (member lists, avatars, APIs) are layered on top by
// adapters owning the event.
func (e *Event) GetGroup(groupID string) *Group {
	resolved := groupID
	if resolved == "" {
		resolved = e.GetGroupID()
	}
	if resolved == "" {
		return nil
	}
	resolved = toStringValue(resolved)
	if e.MessageObj != nil && e.MessageObj.Group != nil && e.MessageObj.Group.GroupID == resolved {
		return e.MessageObj.Group
	}
	return &Group{GroupID: resolved}
}

// toStringValue normalizes an ID string (identity for strings; kept as a hook
// so numeric ID types stay toString per project convention).
func toStringValue(s string) string {
	return s
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

// SetExtra sets a metadata key. 加锁以与事件发布后的并发读取（GetExtra /
// MetadataSnapshot）同步。
func (e *Event) SetExtra(key string, value interface{}) {
	e.metadataMu.Lock()
	defer e.metadataMu.Unlock()
	if e.Metadata == nil {
		e.Metadata = make(map[string]interface{})
	}
	e.Metadata[key] = value
}

// GetExtra returns a metadata value. 加读锁以与 SetExtra 的并发写入同步。
func (e *Event) GetExtra(key string) interface{} {
	e.metadataMu.RLock()
	defer e.metadataMu.RUnlock()
	return e.Metadata[key]
}

// MetadataSnapshot 在锁内复制事件 Metadata 的顶层键值，返回给需要在事件
// 发布后（管线可能仍在原地写 map）安全读取的调用方。返回的 map 为快照，
// 对其修改不会影响事件本身。
func (e *Event) MetadataSnapshot() map[string]interface{} {
	e.metadataMu.RLock()
	defer e.metadataMu.RUnlock()
	if e.Metadata == nil {
		return nil
	}
	out := make(map[string]interface{}, len(e.Metadata))
	for k, v := range e.Metadata {
		out[k] = v
	}
	return out
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
	// confResolver 按事件的 UMO 解析其归属的配置 ID（多配置/多 abconf 路由）。
	// 对齐 Python event_bus.py dispatch：get_conf_info(unified_msg_origin) →
	// pipeline_scheduler_mapping[conf_id]，事件只进入其配置对应的调度器。
	// 未设置时按单配置模式处理。由 lifecycle 在启动时注入（SetConfResolver）。
	confResolver atomic.Value
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

// SetConfResolver injects the UMO → config-ID resolver used by dispatch to
// route each event to the scheduler of its owning config profile (Python:
// astrbot_config_mgr.get_conf_info(event.unified_msg_origin)). The resolver
// is read atomically; passing nil disables routing (single-config mode).
func (bus *EventBus) SetConfResolver(fn func(umo string) string) {
	bus.confResolver.Store(fn)
}

// resolveConfID resolves the config ID for an event UMO. Returns "" when no
// resolver is wired or the resolver has no opinion (caller falls back).
func (bus *EventBus) resolveConfID(umo string) string {
	if fn, ok := bus.confResolver.Load().(func(string) string); ok && fn != nil {
		return fn(umo)
	}
	return ""
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
		bus.queue[0] = nil // release the head reference so the backing array can be GC'd
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
	// 掩码去掉符号位：Sum32 在 32 位平台上 int(uint32) 可能变为负数，
	// 直接作为索引会越界 panic，这里保证结果恒为非负。
	return int(h.Sum32() & 0x7fffffff)
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

// dispatch routes the event to the pipeline scheduler of the config profile
// it belongs to (Python event_bus.py dispatch: get_conf_info(unified_msg_origin)
// → pipeline_scheduler_mapping[conf_id]). 多配置时按 UMO 路由，避免多个调度器
// 各自跑一遍同一事件（或 map 随机序首个兜住的错误路由）。无 resolver（单配置）
// 或解析不到归属时回退到 default 调度器，保持旧行为。The scheduler set is
// snapshotted under a read lock, then Process is invoked outside the lock so a
// slow pipeline (up to minutes) can never stall RegisterScheduler /
// ReloadPipelineScheduler (which need the write lock).
func (bus *EventBus) dispatch(ctx context.Context, event *Event) {
	scheduler := bus.pickScheduler(event)
	if scheduler == nil {
		logger.Error("EventBus: 事件 %q 未找到可用的管线调度器，已忽略", event.UnifiedMsgOrigin())
		if event.Metadata != nil {
			if done, ok := event.Metadata[MetadataPipelineDone].(*PipelineDone); ok {
				done.Signal()
			}
		}
		return
	}
	logger.Debug("EventBus: 正在分发消息 %q（调度器=%s）", event.MessageStr, scheduler.ConfID())

	result, err := scheduler.Process(ctx, event)
	if err != nil {
		logger.Error("Pipeline task failed: %v", err)
	} else if result != nil && !result.Continue {
		logger.Debug("EventBus: 调度器 %s 拦截了事件 %q", scheduler.ConfID(), event.MessageStr)
	}
	// Signal completion so publishers that enqueued the event (e.g. dashboard
	// chat) can observe that the pipeline run finished.
	if event.Metadata != nil {
		if done, ok := event.Metadata[MetadataPipelineDone].(*PipelineDone); ok {
			done.Signal()
		}
	}
}

// pickScheduler resolves the scheduler for an event: 1) UMO 路由表命中其配置 ID
// 且该调度器已注册 → 用之；2) 回退 default 调度器；3) 再回退已注册调度器中
// 稳定序（按配置 ID 排序）的首个，避免 map 迭代随机性。返回 nil 表示没有任何
// 调度器可用。
func (bus *EventBus) pickScheduler(event *Event) *PipelineScheduler {
	bus.mu.RLock()
	defer bus.mu.RUnlock()
	if len(bus.schedulers) == 0 {
		return nil
	}
	// 1) UMO → conf_id 路由（Python get_conf_info 语义）。
	if confID := bus.resolveConfID(event.UnifiedMsgOrigin()); confID != "" {
		if scheduler, ok := bus.schedulers[confID]; ok {
			return scheduler
		}
		logger.Error("EventBus: UMO %q 路由到配置 %s，但该配置的调度器未注册，回退 default", event.UnifiedMsgOrigin(), confID)
	}
	// 2) default 调度器兜底。
	if scheduler, ok := bus.schedulers["default"]; ok {
		return scheduler
	}
	// 3) 稳定序首个（单配置场景即唯一那个）。
	ids := make([]string, 0, len(bus.schedulers))
	for id := range bus.schedulers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return bus.schedulers[ids[0]]
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

// ConfID returns the config profile this scheduler was built for.
func (s *PipelineScheduler) ConfID() string { return s.confID }

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
		// 对齐 Python scheduler.py：每个阶段执行后检查事件是否已被 Stop，
		// 已停止则不再执行后续阶段（避免 Stop 后仍继续跑完管线并 Respond）。
		if event.IsStopped() {
			logger.Debug("阶段 %s 之后事件 %q 已停止传播", stage.Name(), event.MessageStr)
			return &StageResult{Continue: false}, nil
		}
	}
	logger.I18nInfo("流水线: 事件 %q 已通过所有阶段", event.MessageStr)
	return &StageResult{Continue: false}, nil
}
