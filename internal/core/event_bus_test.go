package core

import (
	"context"
	"testing"
	"time"
)

type captureStage struct{ received chan *Event }

func (c *captureStage) Name() string { return "capture" }

func (c *captureStage) Process(ctx context.Context, event *Event) (*StageResult, error) {
	select {
	case c.received <- event:
	default:
	}
	return &StageResult{Continue: false}, nil
}

func TestPublishDelayed(t *testing.T) {
	bus := NewEventBus(10)
	received := make(chan *Event, 4)
	bus.RegisterScheduler("test", func() *PipelineScheduler {
		s := NewPipelineScheduler("test")
		s.AddStage(&captureStage{received: received})
		return s
	}())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bus.Start(ctx)
	defer bus.Stop()

	// Immediate publish is delivered.
	evt1 := &Event{MessageStr: "immediate"}
	if err := bus.Publish(evt1); err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	// Delayed publish is delivered after the delay.
	evt2 := &Event{MessageStr: "delayed"}
	bus.PublishDelayed(evt2, 80*time.Millisecond)

	select {
	case got := <-received:
		if got != evt1 {
			t.Errorf("expected immediate event, got %q", got.MessageStr)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("immediate event never dispatched")
	}

	start := time.Now()
	select {
	case got := <-received:
		if got != evt2 {
			t.Errorf("expected delayed event, got %q", got.MessageStr)
		}
		if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
			t.Errorf("delayed event dispatched too early: %v", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("delayed event never dispatched")
	}

	// Zero delay publishes immediately.
	bus.PublishDelayed(evt2, 0)
	select {
	case got := <-received:
		if got != evt2 {
			t.Errorf("expected zero-delay event")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("zero-delay event never dispatched")
	}
}
