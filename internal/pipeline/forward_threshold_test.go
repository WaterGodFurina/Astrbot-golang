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

// TestForwardThresholdIncludesReasoning: 开启思考展示时，合并转发的 Node 内容
// 必须包含思考前缀（否则思考落在转发节点之外、被平台适配器丢弃）。
func TestForwardThresholdIncludesReasoning(t *testing.T) {
	s := decorateStage(10)
	s.showReasoning = true
	ev := &core.Event{
		Source: core.EventSource{Platform: "aiocqhttp", SelfID: "bot", SenderID: "u1", IsGroup: true},
		Result: message.NewMessageEventResult(),
	}
	ev.Result.Chain = []message.Component{&message.Plain{Text: strings.Repeat("内容", 20)}} // 40 runes > 10
	ev.SetExtra("_llm_reasoning_content", "这是思考过程")
	if _, err := s.Process(context.Background(), ev); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(ev.Result.Chain) != 1 {
		t.Fatalf("chain must be a single Node, got %d", len(ev.Result.Chain))
	}
	node, ok := ev.Result.Chain[0].(*message.Node)
	if !ok {
		t.Fatalf("expected *message.Node, got %T", ev.Result.Chain[0])
	}
	if len(node.Content) == 0 {
		t.Fatal("node content is empty")
	}
	p, ok := node.Content[0].(*message.Plain)
	if !ok || !strings.Contains(p.Text, "这是思考过程") {
		t.Fatalf("reasoning must be inside the forward node content, got %#v", node.Content[0])
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
