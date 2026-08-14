package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// TestDoomDeclineClearsPausedAndFlowsThrough: a non-confirm reply from the
// asker clears the paused doom state instead of leaving it forever, and the
// message is let through the normal pipeline (M-10).
func TestDoomDeclineClearsPausedAndFlowsThrough(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{})
	trigger := &core.Event{
		PlainText: "请反复读取该文件",
		Source:    core.EventSource{Platform: "qq", ConvID: "group:1", SenderID: "u1"},
	}
	for i := 0; i < doomLoopThreshold; i++ {
		s.checkDoomLoop(trigger, "read")
	}
	// Substring matches like "是的"/"是不是" must NOT confirm.
	for _, text := range []string{"是的", "是不是", "停止"} {
		reply := &core.Event{
			MessageStr: text,
			Source:     core.EventSource{Platform: "qq", ConvID: "group:1", SenderID: "u1"},
		}
		if got := s.maybeHandleDoomConfirm(reply); got != doomNotConsumed {
			t.Fatalf("%q must not resume the paused tool, got %v", text, got)
		}
		if reply.IsAtOrWakeCommand {
			t.Fatalf("%q must not be marked woken", text)
		}
		if !s.checkDoomLoop(trigger, "read") {
			t.Fatalf("paused state must be cleared after %q", text)
		}
	}
}

// TestDoomConfirmResumesAndWakes: an exact confirm reply resumes the original
// request, clears the paused state, and marks the event as woken so group-chat
// resumes reach the LLM stage (M-10 + M-11).
func TestDoomConfirmResumesAndWakes(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{})
	trigger := &core.Event{
		PlainText: "请帮我查一下资料",
		Source:    core.EventSource{Platform: "qq", ConvID: "group:1", SenderID: "u1"},
	}
	for i := 0; i < doomLoopThreshold; i++ {
		s.checkDoomLoop(trigger, "read")
	}
	asker := &core.Event{
		MessageStr: "继续",
		Source:     core.EventSource{Platform: "qq", ConvID: "group:1", SenderID: "u1"},
	}
	if got := s.maybeHandleDoomConfirm(asker); got != doomResumed {
		t.Fatalf("exact confirm must resume, got %v", got)
	}
	if asker.PlainText != "请帮我查一下资料" || asker.MessageStr != "请帮我查一下资料" {
		t.Fatalf("message must be rewritten to the original request: %q / %q", asker.PlainText, asker.MessageStr)
	}
	if !asker.IsAtOrWakeCommand {
		t.Fatal("resume must mark the event as woken")
	}
	if v, _ := asker.GetExtra("llm_wake").(bool); !v {
		t.Fatal("resume must set llm_wake extra")
	}
	if !s.checkDoomLoop(trigger, "read") {
		t.Fatal("paused state must be cleared after confirmation")
	}
}

// TestToolCallTimeoutApplies: the tool loop's timeout wrapper returns a
// timeout error and the main path is not blocked (M-12).
func TestToolCallTimeoutApplies(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{
		"provider_settings": map[string]interface{}{"tool_call_timeout": 1},
	})
	if got := s.toolCallTimeout(); got != time.Second {
		t.Fatalf("toolCallTimeout = %v, want 1s", got)
	}
	event := &core.Event{
		Source: core.EventSource{Platform: "qq", ConvID: "group:1", SenderID: "u1"},
	}
	start := time.Now()
	result := s.executeToolWithTimeout(event, "local", "astrbot_execute_shell", map[string]interface{}{
		"command": "sleep 3",
	})
	if !strings.Contains(result, "timed out") {
		t.Fatalf("expected a timeout error, got %q", result)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("main path must not block beyond the timeout")
	}
}

// TestExecuteSandboxToolUnconfigured: executeSandboxTool handles the missing
// sandbox case cleanly (M-13 signature/behaviour smoke test).
func TestExecuteSandboxToolUnconfigured(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{})
	if s.sandboxMgr != nil {
		t.Skip("sandbox manager unexpectedly configured")
	}
	result, handled := s.executeSandboxTool(context.Background(), "astrbot_execute_shell", map[string]interface{}{})
	if !handled {
		t.Fatal("sandbox tool must be marked handled")
	}
	if !strings.Contains(result, "not configured") {
		t.Fatalf("expected not-configured result, got %q", result)
	}
}

// mockChatProvider is a ChatProvider stub for chatRound tests.
type mockChatProvider struct {
	provider.BaseProvider
	response *provider.LLMResponse
}

func (m *mockChatProvider) TextChat(ctx context.Context, req *provider.ProviderRequest) (*provider.LLMResponse, error) {
	return m.response, nil
}

func (m *mockChatProvider) TextChatStream(ctx context.Context, req *provider.ProviderRequest) (<-chan *provider.LLMResponse, error) {
	return nil, fmt.Errorf("stream not supported in mock")
}

// TestChatRoundNonStreamingParsesXMLToolCalls: the non-streaming path parses
// Anthropic-style XML tool calls into real tool calls instead of silently
// stripping them (M-14).
func TestChatRoundNonStreamingParsesXMLToolCalls(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{})
	inst := &mockChatProvider{
		response: &provider.LLMResponse{
			Role:           "assistant",
			CompletionText: "<function_calls><invoke name=\"read\"><parameter name=\"path\">/etc/hostname</parameter></invoke></function_calls>\n\n正文",
		},
	}
	resp, err := s.chatRound(context.Background(), inst, &provider.ProviderRequest{}, false, nil)
	if err != nil {
		t.Fatalf("chatRound: %v", err)
	}
	if len(resp.ToolsCallName) != 1 || resp.ToolsCallName[0] != "read" {
		t.Fatalf("expected parsed tool call, got %v", resp.ToolsCallName)
	}
	if len(resp.ToolsCallArgs) != 1 || resp.ToolsCallArgs[0]["path"] != "/etc/hostname" {
		t.Fatalf("expected parsed tool args, got %v", resp.ToolsCallArgs)
	}
	if len(resp.ToolsCallIDs) != 1 || !strings.HasPrefix(resp.ToolsCallIDs[0], "xml_") {
		t.Fatalf("expected xml tool call id, got %v", resp.ToolsCallIDs)
	}
	if strings.Contains(resp.CompletionText, "function_calls") || strings.Contains(resp.CompletionText, "invoke") {
		t.Fatalf("XML markup must be stripped from the reply: %q", resp.CompletionText)
	}
	if !strings.Contains(resp.CompletionText, "正文") {
		t.Fatalf("real reply text must remain: %q", resp.CompletionText)
	}
}
