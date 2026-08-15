// Package session provides session waiter functionality.
// Ported from astrbot/core/utils/session_waiter.py
//
// Bug fix for issue #9377: session_waiter not bound to sender.
// The Python DefaultSessionFilter.filter() returned event.unified_msg_origin
// (platform:conversation_id), which in group chats matches ANY member.
// A non-initiating member's message would trigger the handler and consume
// the session. The fix binds the session key to BOTH conversation AND sender.
package session

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// Event is the minimal interface a session waiter needs from an incoming event.
type Event interface {
	GetUnifiedMsgOrigin() string
	GetSenderID() string
	GetMessages() *message.MessageChain
}

// SessionController controls a single session's lifecycle.
type SessionController struct {
	mu            sync.Mutex
	done          chan struct{}
	err           error
	timer         *time.Timer
	historyChains []*message.MessageChain
}

// NewSessionController creates a new controller.
func NewSessionController() *SessionController {
	return &SessionController{
		done: make(chan struct{}),
	}
}

// Stop ends the session immediately.
func (sc *SessionController) Stop(err error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	select {
	case <-sc.done:
		return
	default:
	}
	sc.err = err
	if sc.timer != nil {
		sc.timer.Stop()
	}
	close(sc.done)
}

// Keep extends the session with a timeout. When resetTimeout is true the
// countdown is restarted from now with the given timeout; when false the
// existing countdown is preserved (unless no timer is armed yet, in which case
// one is started). A timeout <= 0 ends the session immediately.
func (sc *SessionController) Keep(timeout time.Duration, resetTimeout bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if timeout <= 0 {
		sc.stopLocked(nil)
		return
	}
	if !resetTimeout && sc.timer != nil {
		// 不重置计时：保留现有倒计时。
		return
	}
	if sc.timer != nil {
		sc.timer.Stop()
	}
	sc.timer = time.AfterFunc(timeout, func() {
		sc.Stop(fmt.Errorf("session timeout"))
	})
}

// Done returns the channel that's closed when the session ends.
func (sc *SessionController) Done() <-chan struct{} { return sc.done }

// Err returns the error that ended the session.
func (sc *SessionController) Err() error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.err
}

// AddHistoryChain records a message chain.
func (sc *SessionController) AddHistoryChain(chain *message.MessageChain) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.historyChains == nil {
		sc.historyChains = make([]*message.MessageChain, 0)
	}
	sc.historyChains = append(sc.historyChains, chain.Clone())
}

// GetHistoryChains returns recorded message chains.
func (sc *SessionController) GetHistoryChains() []*message.MessageChain {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.historyChains
}

func (sc *SessionController) stopLocked(err error) {
	select {
	case <-sc.done:
		return
	default:
	}
	sc.err = err
	if sc.timer != nil {
		sc.timer.Stop()
	}
	close(sc.done)
}

// SessionFilter determines how a session is scoped.
type SessionFilter interface {
	Filter(event Event) string
}

// DefaultSessionFilter uses platform:conversation_id:sender_id as the key.
// FIXED #9377: Previously used only platform:conversation_id, allowing any
// group member to trigger another member's session waiter.
type DefaultSessionFilter struct{}

func (f *DefaultSessionFilter) Filter(event Event) string {
	return fmt.Sprintf("%s:%s", event.GetUnifiedMsgOrigin(), event.GetSenderID())
}

// ConversationFilter uses only conversation ID (for backward compatibility).
type ConversationFilter struct{}

func (f *ConversationFilter) Filter(event Event) string {
	return event.GetUnifiedMsgOrigin()
}

// HandlerFunc is the callback for session waiters.
type HandlerFunc func(ctx context.Context, controller *SessionController, event Event) error

// SessionWaiter manages one waiting session.
type SessionWaiter struct {
	ID         string
	filter     SessionFilter
	handler    HandlerFunc
	controller *SessionController
	reg        *WaiterRegistry
	mu         sync.Mutex
}

// WaiterRegistry scopes session waiters to an explicit instance instead of a
// package-global registry (bug.md 6.8): multi-instance and test scenarios no
// longer share state and cannot cross-interfere. Callers create one registry
// per session-waiter subsystem and hand it to NewSessionWaiter.
type WaiterRegistry struct {
	mu sync.RWMutex
	m  map[string]*SessionWaiter
}

// NewWaiterRegistry creates an empty waiter registry.
func NewWaiterRegistry() *WaiterRegistry {
	return &WaiterRegistry{m: make(map[string]*SessionWaiter)}
}

// NewSessionWaiter creates a waiter with the given filter, registered in reg.
func NewSessionWaiter(reg *WaiterRegistry, filter SessionFilter, sessionID string) *SessionWaiter {
	return &SessionWaiter{ID: sessionID, filter: filter, reg: reg}
}

// RegisterWait starts waiting for external input.
func (sw *SessionWaiter) RegisterWait(ctx context.Context, handler HandlerFunc, timeout time.Duration) error {
	sw.handler = handler
	sw.controller = NewSessionController()

	sw.reg.put(sw)
	defer sw.reg.remove(sw.ID)

	sw.controller.Keep(timeout, true)

	select {
	case <-sw.controller.Done():
		return sw.controller.Err()
	case <-ctx.Done():
		sw.controller.Stop(ctx.Err())
		return ctx.Err()
	}
}

func (r *WaiterRegistry) put(sw *SessionWaiter) {
	r.mu.Lock()
	r.m[sw.ID] = sw
	r.mu.Unlock()
}

func (r *WaiterRegistry) remove(sessionID string) {
	r.mu.Lock()
	delete(r.m, sessionID)
	r.mu.Unlock()
}

// Trigger dispatches an event to a matching session. The handler runs with the
// caller's context (so it can be cancelled), not a detached Background one.
func (r *WaiterRegistry) Trigger(ctx context.Context, sessionID string, event Event) error {
	r.mu.RLock()
	sw, ok := r.m[sessionID]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	select {
	case <-sw.controller.Done():
		return nil
	default:
	}
	sw.mu.Lock()
	defer sw.mu.Unlock()
	select {
	case <-sw.controller.Done():
		return nil
	default:
	}
	sw.controller.AddHistoryChain(event.GetMessages())
	if err := sw.handler(ctx, sw.controller, event); err != nil {
		sw.controller.Stop(err)
		return err
	}
	return nil
}

// TriggerByFilter finds a session matching the filter and event.
func (r *WaiterRegistry) TriggerByFilter(ctx context.Context, event Event) error {
	r.mu.RLock()
	var target *SessionWaiter
	for _, sw := range r.m {
		select {
		case <-sw.controller.Done():
			continue
		default:
		}
		if sw.filter != nil {
			key := sw.filter.Filter(event)
			if key == sw.ID {
				target = sw
				break
			}
		}
	}
	r.mu.RUnlock()
	if target == nil {
		return nil
	}
	// Trigger outside the RLock: it re-acquires the registry lock, and doing so
	// while a writer is queued would deadlock this reader (RLock not reentrant).
	return r.Trigger(ctx, target.ID, event)
}

// IsWaiting checks if a session with the given ID is active in this registry.
func (r *WaiterRegistry) IsWaiting(sessionID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	sw, ok := r.m[sessionID]
	if !ok {
		return false
	}
	select {
	case <-sw.controller.Done():
		return false
	default:
		return true
	}
}
