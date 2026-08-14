package session

import (
	"context"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

type mockEvent struct {
	origin   string
	senderID string
	messages *message.MessageChain
}

func (m *mockEvent) GetUnifiedMsgOrigin() string        { return m.origin }
func (m *mockEvent) GetSenderID() string                { return m.senderID }
func (m *mockEvent) GetMessages() *message.MessageChain { return m.messages }

// TestIssue9377_SessionBoundToSender verifies the fix for issue #9377:
// In a group chat, a session_waiter should only trigger for the original
// sender, not for any other group member.
func TestIssue9377_SessionBoundToSender(t *testing.T) {
	originalSender := &mockEvent{
		origin:   "aiocqhttp:group_12345",
		senderID: "user_alice",
		messages: &message.MessageChain{Chain: []message.Component{}},
	}
	otherMember := &mockEvent{
		origin:   "aiocqhttp:group_12345",
		senderID: "user_bob",
		messages: &message.MessageChain{Chain: []message.Component{}},
	}

	// Create a session with DefaultSessionFilter
	filter := DefaultSessionFilter{}
	sessionKey := filter.Filter(originalSender)
	if sessionKey == "" {
		t.Fatal("filter returned empty session key")
	}

	// The key should contain BOTH conversation ID and sender ID
	// (This is the fix - the old code only used conversation ID)
	otherKey := filter.Filter(otherMember)

	if sessionKey == otherKey {
		t.Errorf(
			"BUG #9377: session key is same for different senders in same group!\n"+
				"  alice key: %s\n"+
				"  bob key:   %s\n"+
				"  These must differ - otherwise any group member can trigger\n"+
				"  another member's session_waiter.",
			sessionKey, otherKey,
		)
	}

	// Verify the key contains the sender ID
	if !containsStr(sessionKey, "user_alice") {
		t.Errorf(
			"session key should contain sender ID 'user_alice': %s",
			sessionKey,
		)
	}
}

// TestDefaultSessionFilter_PrivateChat verifies that in private chat,
// the filter still works correctly.
func TestDefaultSessionFilter_PrivateChat(t *testing.T) {
	privateEvent := &mockEvent{
		origin:   "aiocqhttp:private_user_alice",
		senderID: "user_alice",
	}

	filter := DefaultSessionFilter{}
	key := filter.Filter(privateEvent)

	if key == "" {
		t.Error("filter returned empty key for private chat")
	}
}

func containsStr(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestKeepResetTimeoutFalsePreservesTimer verifies that Keep(..., false) does
// not reset the countdown: the session must still end on the originally-armed
// (shorter) deadline.
func TestKeepResetTimeoutFalsePreservesTimer(t *testing.T) {
	sc := NewSessionController()
	sc.Keep(60*time.Millisecond, true)
	time.Sleep(20 * time.Millisecond)

	// This must NOT reset the 60ms timer (i.e. must not extend the deadline).
	sc.Keep(time.Hour, false)

	select {
	case <-sc.Done():
		// Ended on the original timer: correct.
	case <-time.After(time.Second):
		t.Fatal("session did not time out on the original deadline: resetTimeout=false must not reset the timer")
	}
}

// TestKeepResetTimeoutTrueResetsTimer verifies that Keep(..., true) restarts
// the countdown, extending the deadline.
func TestKeepResetTimeoutTrueResetsTimer(t *testing.T) {
	sc := NewSessionController()
	sc.Keep(60*time.Millisecond, true)
	time.Sleep(20 * time.Millisecond)

	sc.Keep(time.Hour, true)
	select {
	case <-sc.Done():
		t.Fatal("session ended prematurely: resetTimeout=true should have restarted the countdown")
	case <-time.After(500 * time.Millisecond):
	}
	sc.Stop(nil)
}

// TestKeepTimeoutNonPositiveStops verifies a non-positive timeout ends the
// session immediately regardless of resetTimeout.
func TestKeepTimeoutNonPositiveStops(t *testing.T) {
	sc := NewSessionController()
	sc.Keep(time.Hour, true)
	sc.Keep(0, false)
	select {
	case <-sc.Done():
	case <-time.After(time.Second):
		t.Fatal("Keep(0, false) should stop the session immediately")
	}
}

// TestTriggerByFilterNoSelfDeadlock verifies TriggerByFilter finds and
// dispatches to a matching waiter without deadlocking (regression for the
// recursive RLock window where Trigger was called while holding the registry
// RLock).
func TestTriggerByFilterNoSelfDeadlock(t *testing.T) {
	originalSender := &mockEvent{
		origin:   "aiocqhttp:group_12345",
		senderID: "user_alice",
		messages: &message.MessageChain{Chain: []message.Component{}},
	}

	sw := NewSessionWaiter(&DefaultSessionFilter{}, "aiocqhttp:group_12345:user_alice")
	triggered := make(chan struct{}, 1)
	sw.handler = func(ctx context.Context, controller *SessionController, event Event) error {
		triggered <- struct{}{}
		return nil
	}
	sw.controller = NewSessionController()
	sw.controller.Keep(time.Minute, true)

	registryMu.Lock()
	registry[sw.ID] = sw
	registryMu.Unlock()
	defer func() {
		registryMu.Lock()
		delete(registry, sw.ID)
		registryMu.Unlock()
	}()

	done := make(chan error, 1)
	go func() {
		done <- TriggerByFilter(originalSender)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("TriggerByFilter returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("TriggerByFilter deadlocked")
	}
	select {
	case <-triggered:
	default:
		t.Error("matching waiter was not triggered")
	}
}
