package core

import (
	"context"
	"fmt"
	"sync"
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

// blockingStage records arrival order and optionally blocks a specific UMO.
type blockingStage struct {
	started   sync.Once
	startedCh chan struct{} // closed when the blockUMO event enters Process
	block     chan struct{}
	blockUMO  string
	mu        sync.Mutex
	order     []string
}

func newBlockingStage(blockUMO string, block chan struct{}) *blockingStage {
	return &blockingStage{startedCh: make(chan struct{}), block: block, blockUMO: blockUMO}
}

func (b *blockingStage) Name() string { return "blocking" }

func (b *blockingStage) Process(ctx context.Context, event *Event) (*StageResult, error) {
	b.mu.Lock()
	b.order = append(b.order, event.MessageStr)
	b.mu.Unlock()
	if b.blockUMO != "" && event.UnifiedMsgOrigin() == b.blockUMO {
		b.started.Do(func() { close(b.startedCh) })
		<-b.block
	}
	return &StageResult{Continue: false}, nil
}

func (b *blockingStage) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.order))
	copy(out, b.order)
	return out
}

func waitForOrder(t *testing.T, b *blockingStage, want int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		o := b.snapshot()
		if len(o) >= want {
			return o
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("only %d of %d events dispatched: %v", len(b.snapshot()), want, b.snapshot())
	return nil
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

// TestDispatchWorkerPreservesSessionOrder ensures events sharing one UMO are
// processed in publish order (they hash to the same worker).
func TestDispatchWorkerPreservesSessionOrder(t *testing.T) {
	bus := NewEventBus(10)
	stage := newBlockingStage("", make(chan struct{}))
	bus.RegisterScheduler("test", func() *PipelineScheduler {
		s := NewPipelineScheduler("test")
		s.AddStage(stage)
		return s
	}())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bus.Start(ctx)
	defer bus.Stop()

	const n = 20
	for i := 0; i < n; i++ {
		evt := &Event{
			MessageStr: fmt.Sprintf("m%d", i),
			Source:     EventSource{Platform: "plat", ConvID: "same"},
		}
		if err := bus.Publish(evt); err != nil {
			t.Fatalf("publish %d failed: %v", i, err)
		}
	}
	order := waitForOrder(t, stage, n)
	for i, got := range order {
		if want := fmt.Sprintf("m%d", i); got != want {
			t.Fatalf("expected %s at position %d, got %s (order %v)", want, i, got, order)
		}
	}
}

// TestDispatchWorkerSurvivesPanic ensures a panic in dispatch (here: a broken
// completion signal with a nil channel, which panics in close) is recovered by
// the worker goroutine instead of crashing the process, and the same worker
// keeps processing subsequent events of that session.
func TestDispatchWorkerSurvivesPanic(t *testing.T) {
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

	// Same UMO so both events land on the same worker.
	source := EventSource{Platform: "plat", ConvID: "same"}
	broken := &Event{
		MessageStr: "broken",
		Source:     source,
		Metadata:   map[string]interface{}{MetadataPipelineDone: &PipelineDone{ch: nil}},
	}
	if err := bus.Publish(broken); err != nil {
		t.Fatalf("publish broken failed: %v", err)
	}
	ok := &Event{MessageStr: "ok", Source: source}
	if err := bus.Publish(ok); err != nil {
		t.Fatalf("publish ok failed: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case got := <-received:
			if got == ok {
				return
			}
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("worker did not dispatch the next event after recovering from a panic")
}

// TestDispatchWorkerConcurrentAcrossSessions verifies a slow event blocked in
// one session does not stall a different session's event.
func TestDispatchWorkerConcurrentAcrossSessions(t *testing.T) {
	bus := NewEventBus(10)
	block := make(chan struct{})
	stage := newBlockingStage("plat:slow", block)
	bus.RegisterScheduler("test", func() *PipelineScheduler {
		s := NewPipelineScheduler("test")
		s.AddStage(stage)
		return s
	}())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bus.Start(ctx)
	defer bus.Stop()

	slowShard := eventShardKey(&Event{Source: EventSource{Platform: "plat", ConvID: "slow"}})
	other := "other0"
	for i := 1; ; i++ {
		if eventShardKey(&Event{Source: EventSource{Platform: "plat", ConvID: other}}) != slowShard {
			break
		}
		other = fmt.Sprintf("other%d", i)
	}

	if err := bus.Publish(&Event{
		MessageStr: "slow",
		Source:     EventSource{Platform: "plat", ConvID: "slow"},
	}); err != nil {
		t.Fatalf("publish slow failed: %v", err)
	}
	select {
	case <-stage.startedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("slow event never entered processing")
	}

	if err := bus.Publish(&Event{
		MessageStr: "other",
		Source:     EventSource{Platform: "plat", ConvID: other},
	}); err != nil {
		t.Fatalf("publish other failed: %v", err)
	}
	order := waitForOrder(t, stage, 2)
	if order[1] != "other" {
		t.Fatalf("expected other-session event to complete while slow is blocked, got %v", order)
	}

	close(block)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(stage.snapshot()) == 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("slow event never completed after release: %v", stage.snapshot())
}
