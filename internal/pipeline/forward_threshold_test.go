package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

func decorateStage(threshold int) *ResultDecorateStage {
	s := &ResultDecorateStage{forwardThreshold: threshold}
	return s
}

// TestForwardThresholdConvertsToNode: on aiocqhttp a reply longer than the
// threshold becomes a single Node and @/quote decorations are skipped.
func TestForwardThresholdConvertsToNode(t *testing.T) {
	s := decorateStage(10)
	long := strings.Repeat("内容内容", 10) // 40 runes > 10
	ev := &core.Event{
		Source: core.EventSource{Platform: "aiocqhttp", SelfID: "bot", SenderID: "u1", IsGroup: true},
		Result: message.NewMessageEventResult(),
	}
	ev.Result.Chain = []message.Component{&message.Plain{Text: long}}
	s.replyWithMention = true
	s.replyWithQuote = true
	ev.MessageObj = &core.MessageObj{MessageID: "m1"}
	if _, err := s.Process(context.Background(), ev); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(ev.Result.Chain) != 1 {
		t.Fatalf("chain must be a single Node, got %d components", len(ev.Result.Chain))
	}
	node, ok := ev.Result.Chain[0].(*message.Node)
	if !ok {
		t.Fatalf("expected *message.Node, got %T", ev.Result.Chain[0])
	}
	if node.Name != "AstrBot" {
		t.Errorf("node name: want AstrBot, got %q", node.Name)
	}
}

// TestForwardThresholdBelowKeepsChain: short replies keep the original chain
// and @/quote decorations still apply.
func TestForwardThresholdBelowKeepsChain(t *testing.T) {
	s := decorateStage(1000)
	ev := &core.Event{
		Source: core.EventSource{Platform: "aiocqhttp", SelfID: "bot", SenderID: "u1", IsGroup: true},
		Result: message.NewMessageEventResult(),
	}
	ev.Result.Chain = []message.Component{&message.Plain{Text: "短回复"}}
	s.replyWithMention = true
	ev.MessageObj = &core.MessageObj{MessageID: "m1"}
	if _, err := s.Process(context.Background(), ev); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if _, ok := ev.Result.Chain[0].(*message.At); !ok {
		t.Error("short reply should still be decorated with @mention")
	}
}

// TestForwardThresholdPlatformGate: other platforms never convert to a node.
func TestForwardThresholdPlatformGate(t *testing.T) {
	s := decorateStage(5)
	ev := &core.Event{
		Source: core.EventSource{Platform: "telegram", SelfID: "bot", SenderID: "u1"},
		Result: message.NewMessageEventResult(),
	}
	ev.Result.Chain = []message.Component{&message.Plain{Text: strings.Repeat("x", 50)}}
	if _, err := s.Process(context.Background(), ev); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if _, ok := ev.Result.Chain[0].(*message.Node); ok {
		t.Error("non-aiocqhttp platforms must not convert to forward nodes")
	}
}
