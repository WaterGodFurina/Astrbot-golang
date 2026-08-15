package platform

import (
	"context"
	"strings"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// fakeAdapter is a minimal PlatformAdapter recording Send calls.
type fakeAdapter struct {
	id   string
	typ  string
	sent []string
}

func (f *fakeAdapter) ID() string   { return f.id }
func (f *fakeAdapter) Type() string { return f.typ }
func (f *fakeAdapter) Start(ctx context.Context) error {
	return nil
}
func (f *fakeAdapter) Stop() error { return nil }
func (f *fakeAdapter) Send(sessionID string, chain *message.MessageChain) error {
	f.sent = append(f.sent, sessionID)
	return nil
}

// TestSendRejectsEmptyPlatform verifies that an empty platformID is explicitly
// rejected with a clear error instead of silently falling through routing.
func TestSendRejectsEmptyPlatform(t *testing.T) {
	pm := NewPlatformManager()
	a := &fakeAdapter{id: "inst_1", typ: "mock"}
	pm.Register(a)

	err := pm.Send("", "session-1", message.NewMessageChain())
	if err == nil {
		t.Fatal("Send with empty platformID must return an error")
	}
	if !strings.Contains(err.Error(), "platformID 为空") {
		t.Errorf("expected a clear empty-platformID error, got: %v", err)
	}
	if len(a.sent) != 0 {
		t.Errorf("Send with empty platformID must not reach any adapter, got %v", a.sent)
	}

	// Non-empty platform (instance ID and type fallback) must still route.
	if err := pm.Send("inst_1", "s", message.NewMessageChain()); err != nil {
		t.Errorf("Send by instance ID failed: %v", err)
	}
	if err := pm.Send("mock", "s2", message.NewMessageChain()); err != nil {
		t.Errorf("Send by type fallback failed: %v", err)
	}
	if len(a.sent) != 2 {
		t.Errorf("expected 2 routed sends, got %v", a.sent)
	}
}

// TestSendUnknownPlatform verifies the error message quotes the platform so an
// empty value is distinguishable from a misspelled one.
func TestSendUnknownPlatform(t *testing.T) {
	pm := NewPlatformManager()
	err := pm.Send("nonexistent", "s", message.NewMessageChain())
	if err == nil {
		t.Fatal("Send for unknown platform must fail")
	}
	if !strings.Contains(err.Error(), `"nonexistent"`) {
		t.Errorf("expected quoted platform in error, got: %v", err)
	}
}

// TestReactRejectsEmptyPlatform mirrors the Send guard for the Reactor path.
func TestReactRejectsEmptyPlatform(t *testing.T) {
	pm := NewPlatformManager()
	pm.Register(&fakeAdapter{id: "inst_1", typ: "mock"})

	err := pm.React("", "s", "m", "👍")
	if err == nil || !strings.Contains(err.Error(), "platformID 为空") {
		t.Errorf("expected clear empty-platformID error, got: %v", err)
	}
}

// fakeReactor implements both PlatformAdapter and Reactor.
type fakeReactor struct {
	*fakeAdapter
	reacted []string
}

func (f *fakeReactor) React(sessionID, messageID, emoji string) error {
	f.reacted = append(f.reacted, emoji)
	return nil
}

// TestReactRoutesToReactor verifies React still works for registered platforms.
func TestReactRoutesToReactor(t *testing.T) {
	pm := NewPlatformManager()
	r := &fakeReactor{fakeAdapter: &fakeAdapter{id: "inst_1", typ: "mock"}}
	pm.Register(r)

	if err := pm.React("mock", "s", "m", "👍"); err != nil {
		t.Fatalf("React failed: %v", err)
	}
	if len(r.reacted) != 1 || r.reacted[0] != "👍" {
		t.Errorf("expected 1 reaction, got %v", r.reacted)
	}

	// Registered adapter that is not a Reactor must report an error.
	pm.Register(&fakeAdapter{id: "inst_2", typ: "plain"})
	err := pm.React("plain", "s", "m", "👍")
	if err == nil || !strings.Contains(err.Error(), "does not support reactions") {
		t.Errorf("expected no-reaction error, got: %v", err)
	}
}
