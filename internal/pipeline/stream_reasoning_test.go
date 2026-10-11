package pipeline

import (
	"context"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// reasoningCaptureAdapter 记录 SendByEvent 发出的链，用于回归推理片段直发行为。
type reasoningCaptureAdapter struct {
	platform.PlatformAdapter
	chains []*message.MessageChain
}

func (a *reasoningCaptureAdapter) ID() string   { return "reasoning_capture" }
func (a *reasoningCaptureAdapter) Type() string { return "qq" }

func (a *reasoningCaptureAdapter) SendByEvent(sessionID string, chain *message.MessageChain, event *core.Event) error {
	a.chains = append(a.chains, chain)
	return nil
}

// reasoningStreamProvider 产出正文与推理交替的流式 chunk。
type reasoningStreamProvider struct {
	provider.BaseProvider
	chunks []*provider.LLMResponse
}

func (m *reasoningStreamProvider) TextChat(ctx context.Context, req *provider.ProviderRequest) (*provider.LLMResponse, error) {
	return nil, context.Canceled
}

func (m *reasoningStreamProvider) TextChatStream(ctx context.Context, req *provider.ProviderRequest) (<-chan *provider.LLMResponse, error) {
	ch := make(chan *provider.LLMResponse, len(m.chunks))
	for _, c := range m.chunks {
		ch <- c
	}
	close(ch)
	return ch, nil
}

// 回归：群聊 + showReasoning + 流式 → 推理不得直发平台、sent 必须为 false。
// 否则 finalizeAgentReply 标记 streamed，RespondStage 跳过最终全文，用户只收到
// 推理片段、收不到答案（对齐 py stream_to_general 语义：推理跟随正文同一通道，
// 正文群聊不增量发时推理也不增量发）。
func TestStreamingReasoningNotSentInGroup(t *testing.T) {
	pm := platform.NewPlatformManager()
	adapter := &reasoningCaptureAdapter{}
	pm.Register(adapter)
	s := testProcessStageWithConfig(t, map[string]interface{}{})
	s.platformMgr = pm

	event := &core.Event{Source: core.EventSource{Platform: "qq", ConvID: "g:1", SenderID: "u1", IsGroup: true}}
	streamer := newStreamSender(s, event)
	if streamer.frag != nil || streamer.segMode {
		t.Fatalf("群聊不得启用增量发送：frag=%v segMode=%v", streamer.frag != nil, streamer.segMode)
	}
	inst := &reasoningStreamProvider{chunks: []*provider.LLMResponse{
		{Role: "assistant", IsChunk: true, CompletionText: "思考中。", ReasoningContent: "先想一下，"},
		{Role: "assistant", IsChunk: true, CompletionText: "想完了。", ReasoningContent: "再想一下。"},
		{Role: "assistant", CompletionText: "最终答案。"},
	}}
	resp, err := s.chatRound(context.Background(), inst, &provider.ProviderRequest{}, true, true, streamer)
	if err != nil {
		t.Fatalf("chatRound: %v", err)
	}
	streamer.flush()
	if streamer.sentAny() {
		t.Fatalf("群聊推理不得置 sent=true，否则最终全文会被 RespondStage 跳过")
	}
	if len(adapter.chains) != 0 {
		t.Fatalf("群聊推理片段不得直发平台，实际发送了 %d 段", len(adapter.chains))
	}
	if resp.ReasoningContent == "" {
		t.Fatalf("推理内容必须仍进 LLMResponse（供 _llm_reasoning_content 注入）")
	}
}

// 私聊不支持原生流式 → segMode + showReasoning：推理按句直发、sent=true 合法
// （正文同样逐句发，答案不会丢）。
func TestStreamingReasoningSentInPrivateSegMode(t *testing.T) {
	pm := platform.NewPlatformManager()
	adapter := &reasoningCaptureAdapter{}
	pm.Register(adapter)
	s := testProcessStageWithConfig(t, map[string]interface{}{})
	s.platformMgr = pm

	event := &core.Event{Source: core.EventSource{Platform: "qq", ConvID: "f:1", SenderID: "u1"}}
	streamer := newStreamSender(s, event)
	if !streamer.segMode {
		t.Fatalf("私聊无原生流式平台应进入 segMode")
	}
	inst := &reasoningStreamProvider{chunks: []*provider.LLMResponse{
		{Role: "assistant", IsChunk: true, CompletionText: "开始回答。", ReasoningContent: "思考第一句。"},
		{Role: "assistant", CompletionText: "正文答案。"},
	}}
	if _, err := s.chatRound(context.Background(), inst, &provider.ProviderRequest{}, true, true, streamer); err != nil {
		t.Fatalf("chatRound: %v", err)
	}
	streamer.flush()
	var reasoning, plain int
	for _, chain := range adapter.chains {
		if chain.Type == "reasoning" {
			reasoning++
		} else {
			plain++
		}
	}
	if reasoning == 0 {
		t.Fatalf("segMode 私聊推理应按句直发")
	}
	if !streamer.sentAny() {
		t.Fatalf("segMode 私聊推理/正文直发后 sent 应为 true")
	}
	if plain == 0 {
		t.Fatalf("segMode 私聊正文应按句直发")
	}
}
