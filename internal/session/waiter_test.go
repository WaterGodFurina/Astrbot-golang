package session

import (
	"testing"

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
