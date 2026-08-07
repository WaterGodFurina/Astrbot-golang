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

	"github.com/AstrBotDevs/AstrBot/pkg/message"
)

// Event is the minimal interface a session waiter needs from an incoming event.
type Event interface {
	GetUnifiedMsgOrigin() string
	GetSenderID() string
	GetMessages() *message.MessageChain
}

// SessionController controls a single session's lifecycle.
type SessionController struct {
	mu           sync.Mutex
	done         chan struct{}
	err          error
	timer        *time.Timer
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

// Keep extends the session with a timeout.
func (sc *SessionController) Keep(timeout time.Duration, resetTimeout bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if timeout <= 0 {
		sc.stopLocked(nil)
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
	mu         sync.Mutex
}

var (
	registryMu sync.RWMutex
	registry   = make(map[string]*SessionWaiter)
)

// RegisterWait starts waiting for external input.
func (sw *SessionWaiter) RegisterWait(ctx context.Context, handler HandlerFunc, timeout time.Duration) error {
	sw.handler = handler
	sw.controller = NewSessionController()

	registryMu.Lock()
	registry[sw.ID] = sw
	registryMu.Unlock()

	sw.controller.Keep(timeout, true)
	defer sw.cleanup()

	select {
	case <-sw.controller.Done():
		return sw.controller.Err()
	case <-ctx.Done():
		sw.controller.Stop(ctx.Err())
		return ctx.Err()
	}
}

func (sw *SessionWaiter) cleanup() {
	registryMu.Lock()
	delete(registry, sw.ID)
	registryMu.Unlock()
	if sw.controller != nil {
		sw.controller.Stop(nil)
	}
}

// Trigger dispatches an event to a matching session.
func Trigger(sessionID string, event Event) error {
	registryMu.RLock()
	sw, ok := registry[sessionID]
	registryMu.RUnlock()
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
	if err := sw.handler(context.Background(), sw.controller, event); err != nil {
		sw.controller.Stop(err)
		return err
	}
	return nil
}

// TriggerByFilter finds a session matching the filter and event.
func TriggerByFilter(event Event) error {
	registryMu.RLock()
	defer registryMu.RUnlock()
	for _, sw := range registry {
		select {
		case <-sw.controller.Done():
			continue
		default:
		}
		if sw.filter != nil {
			key := sw.filter.Filter(event)
			if key == sw.ID {
				return Trigger(sw.ID, event)
			}
		}
	}
	return nil
}

// NewSessionWaiter creates a waiter with the given filter.
func NewSessionWaiter(filter SessionFilter, sessionID string) *SessionWaiter {
	return &SessionWaiter{ID: sessionID, filter: filter}
}

// IsWaiting checks if a session with the given ID is active.
func IsWaiting(sessionID string) bool {
	registryMu.RLock()
	defer registryMu.RUnlock()
	sw, ok := registry[sessionID]
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
