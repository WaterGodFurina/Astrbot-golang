package core

import (
	"context"
	"testing"
)

func newTestEvent(umo string) *Event {
	e := &Event{MessageStr: "hi"}
	e.Source.PlatformID = "p"
	e.Source.ConvID = umo
	e.Source.IsGroup = false
	e.MessageObj = &MessageObj{MessageType: "FriendMessage"}
	return e
}

func TestActiveEventRegistryRequestAgentStopAll(t *testing.T) {
	r := &ActiveEventRegistry{events: map[string]map[*Event]struct{}{}, stopCallbacks: map[*Event]context.CancelFunc{}}
	e1 := newTestEvent("s1")
	e2 := newTestEvent("s1")
	ctx2, cancel2 := context.WithCancel(context.Background())
	r.Register(e1)
	r.Register(e2)
	r.RegisterStopCallback(e2, cancel2)

	umo1 := e1.UnifiedMsgOrigin()
	if n := r.RequestAgentStopAll(umo1, e1); n != 1 {
		t.Fatalf("RequestAgentStopAll = %d, want 1", n)
	}
	select {
	case <-ctx2.Done():
	default:
		t.Fatal("stop callback was not invoked")
	}
	if v, _ := e2.GetExtra("agent_stop_requested").(bool); !v {
		t.Fatal("agent_stop_requested flag not set")
	}
	if e2.IsStopped() {
		t.Fatal("RequestAgentStopAll must not Stop the event")
	}
	if n := r.RequestAgentStopAll(umo1, e1); n != 1 {
		t.Fatalf("second RequestAgentStopAll = %d, want 1", n)
	}
}

func TestActiveEventRegistryStopAllAndUnregister(t *testing.T) {
	r := &ActiveEventRegistry{events: map[string]map[*Event]struct{}{}, stopCallbacks: map[*Event]context.CancelFunc{}}
	e1 := newTestEvent("s2")
	e2 := newTestEvent("s2")
	r.Register(e1)
	r.Register(e2)
	umo2 := e1.UnifiedMsgOrigin()
	if n := r.StopAll(umo2, e1); n != 1 {
		t.Fatalf("StopAll = %d, want 1", n)
	}
	if !e2.IsStopped() || e1.IsStopped() {
		t.Fatal("StopAll must stop only non-excluded events")
	}
	r.Unregister(e1)
	r.Unregister(e2)
	if n := r.StopAll(umo2, nil); n != 0 {
		t.Fatalf("StopAll after unregister = %d, want 0", n)
	}
}
