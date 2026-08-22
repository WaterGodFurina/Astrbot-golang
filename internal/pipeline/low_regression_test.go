package pipeline

import (
	"context"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fogleman/gg"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// --- L-14: executeSandboxTool must not swallow non-sandbox tools when the
// sandbox manager is nil.

func TestExecuteSandboxToolNilOnlyBlocksSandboxTools(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{})
	if s.sandboxMgr != nil {
		t.Skip("sandbox manager unexpectedly configured")
	}
	// Non-sandbox names must fall through unhandled.
	for _, name := range []string{"web_search", "get_current_time", "future_task"} {
		if result, handled := s.executeSandboxTool(context.Background(), name, map[string]interface{}{}); handled {
			t.Fatalf("%s must not be marked handled without a sandbox, got %q", name, result)
		}
	}
	// Sandbox tools report "not configured".
	for _, name := range []string{
		"astrbot_execute_shell", "astrbot_execute_python",
		"astrbot_file_read_tool", "astrbot_file_write_tool",
		"astrbot_file_edit_tool", "astrbot_grep_tool",
	} {
		result, handled := s.executeSandboxTool(context.Background(), name, map[string]interface{}{})
		if !handled || !strings.Contains(result, "not configured") {
			t.Fatalf("%s must report not-configured, got handled=%v result=%q", name, handled, result)
		}
	}
	// Integration: runtime=sandbox without a manager must still let built-in
	// tools execute rather than swallow them.
	event := &core.Event{Source: core.EventSource{Platform: "qq", ConvID: "g:1", SenderID: "u1"}}
	result := s.executeTool(context.Background(), event, "sandbox", "get_current_time", map[string]interface{}{})
	if strings.Contains(result, "Sandbox manager not configured") {
		t.Fatalf("built-in tool was swallowed by the missing sandbox: %q", result)
	}
}

// --- L-15: materializeToolResult sanitizes provider-controlled tool call ids
// so they cannot escape data/temp/tool_results.

func TestMaterializeToolResultSanitizesToolCallID(t *testing.T) {
	inTempDir(t)
	big := strings.Repeat("x", maxInlineToolResultChars+100)
	out := materializeToolResult(big, "../../../etc/evil..name")
	if strings.Contains(out, "..") {
		t.Fatalf("notice must not carry traversal dots: %q", out)
	}
	start := strings.Index(out, "保存到 ")
	end := strings.Index(out, "（共")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("cannot locate saved path in notice: %q", out)
	}
	path := out[start+len("保存到 ") : end]
	wantDir := filepath.Join("data", "temp", "tool_results")
	if !strings.HasPrefix(path, wantDir) {
		t.Fatalf("overflow file escaped tool_results dir: %q", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("overflow file missing at %s: %v", path, err)
	}
}

// --- L-16: compressImageForProvider temp files are identifiable for cleanup.

func TestCompressImageForProviderTempFile(t *testing.T) {
	dir := inTempDir(t)
	img := filepath.Join(dir, "in.png")
	dc := gg.NewContext(64, 64)
	dc.SetRGB(1, 0, 0)
	dc.Clear()
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, dc.Image()); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	s := testProcessStageWithConfig(t, map[string]interface{}{})
	out := s.compressImageForProvider(img)
	defer os.Remove(out)
	if !isCompressTempFile(out) {
		t.Fatalf("expected a compress temp path, got %q", out)
	}
	if !fileExists(out) {
		t.Fatal("temp file must exist")
	}
	if isCompressTempFile(img) {
		t.Fatal("original image must not be treated as a compress temp file")
	}
}

// --- L-17: shell session exit status is race-free and completed sessions are
// reaped after the TTL.

func extractSessionID(t *testing.T, out string) string {
	t.Helper()
	prefix := "session id: "
	idx := strings.Index(out, prefix)
	if idx < 0 {
		t.Fatalf("no session id in output: %q", out)
	}
	id := out[idx+len(prefix):]
	if end := strings.IndexByte(id, ')'); end >= 0 {
		id = id[:end]
	}
	if id == "" {
		t.Fatalf("empty session id in %q", out)
	}
	return id
}

func TestShellSessionStatusAndCleanup(t *testing.T) {
	inTempDir(t)
	old := shellSessionTTL
	shellSessionTTL = 100 * time.Millisecond
	defer func() { shellSessionTTL = old }()

	umo := "bg:cleanup"
	out := executeLocalShell(umo, umo, "exit 3", true, 0)
	id := extractSessionID(t, out)

	// Status eventually reports completed with the exit code.
	deadline := time.Now().Add(3 * time.Second)
	var st map[string]interface{}
	for time.Now().Before(deadline) {
		shellSessionsMu.Lock()
		s := shellSessions[id]
		shellSessionsMu.Unlock()
		if s == nil {
			t.Fatal("session missing before reap")
		}
		st = s.status()
		if st["status"] == "completed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st == nil || st["status"] != "completed" {
		t.Fatalf("session never completed: %v", st)
	}
	if st["exit_code"] != 3 {
		t.Fatalf("exit code = %v, want 3", st["exit_code"])
	}

	// After the TTL the completed session is reaped from the table.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		shellSessionsMu.Lock()
		_, ok := shellSessions[id]
		shellSessionsMu.Unlock()
		if !ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("completed session was not reaped after the TTL")
}

// --- L-18: doom counters reset at each agent request while preserving the
// paused tool.

func TestDoomLoopResetPerRequest(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{})
	event := &core.Event{Source: core.EventSource{Platform: "qq", ConvID: "group:1", SenderID: "u1"}}
	for i := 0; i < 3; i++ {
		s.checkDoomLoop(event, "read")
	}
	// A new request resets the counters.
	s.resetDoomLoopCount(event.UnifiedMsgOrigin())
	for i := 0; i < 2; i++ {
		if !s.checkDoomLoop(event, "read") {
			t.Fatal("reset must clear the previous request's count; tool paused too early")
		}
	}
}

func TestDoomLoopResetPreservesPausedTool(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{})
	event := &core.Event{Source: core.EventSource{Platform: "qq", ConvID: "group:1", SenderID: "u1"}}
	for i := 0; i < doomLoopThreshold; i++ {
		s.checkDoomLoop(event, "read")
	}
	s.resetDoomLoopCount(event.UnifiedMsgOrigin())
	s.doomMu.Lock()
	tr := s.doomTrackers[event.UnifiedMsgOrigin()]
	paused := tr != nil && tr.pausedTool == "read"
	s.doomMu.Unlock()
	if !paused {
		t.Fatal("reset must preserve the paused tool state")
	}
	if s.checkDoomLoop(event, "read") {
		t.Fatal("paused tool must stay paused after reset")
	}
}

// --- L-19: subagent tool names are provider-safe and resolvable back to the
// original agent.

func TestSubAgentToolNameSanitized(t *testing.T) {
	agents := []*SubAgent{
		{Name: "翻译助手", Enabled: true},
		{Name: "coder", Enabled: true},
	}
	for _, sc := range subAgentToolSchemas(agents) {
		fn, _ := sc["function"].(map[string]interface{})
		n, _ := fn["name"].(string)
		if !isValidToolName(n) {
			t.Fatalf("tool name %q must be provider-safe", n)
		}
	}

	s := testProcessStageWithConfig(t, map[string]interface{}{
		"subagent_orchestrator": map[string]interface{}{
			"main_enable": true,
			"agents": []interface{}{
				map[string]interface{}{"name": "翻译助手", "enabled": true},
				map[string]interface{}{"name": "coder", "enabled": true},
			},
		},
	})
	safe := subAgentToolName("翻译助手")
	agent := s.findSubAgentByName(safe)
	if agent == nil || agent.Name != "翻译助手" {
		t.Fatalf("sanitized tool name %q must resolve to the original agent, got %+v", safe, agent)
	}
	// executeSubAgent recognizes the sanitized name (handled=true proves the
	// agent was found; the provider setup then fails without a config).
	event := &core.Event{Source: core.EventSource{Platform: "qq", ConvID: "g:1", SenderID: "u1"}}
	r, handled := s.executeSubAgent(event, safe, map[string]interface{}{"input": "hello"})
	if !handled {
		t.Fatal("sanitized subagent call must be handled")
	}
	if !strings.Contains(r, "翻译助手") {
		t.Fatalf("result should reference the original agent name: %q", r)
	}
}

// --- L-20: RemoveSession no longer removes the lock table entry while
// concurrently holding two different locks for the same umo.

func TestGroupContextRemoveSessionConcurrent(t *testing.T) {
	g := NewGroupChatContext(groupCtxConfig())
	ev := groupEvent("群消息")
	umo := ev.UnifiedMsgOrigin()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			g.HandleMessage(groupEvent("x"))
		}()
		go func() {
			defer wg.Done()
			g.RemoveSession(umo)
		}()
		go func() {
			defer wg.Done()
			g.OnReqLLM(groupEvent("y"), &provider.ProviderRequest{})
		}()
	}
	wg.Wait()
	// The umo must still be usable afterwards.
	g.HandleMessage(ev)
	req := &provider.ProviderRequest{}
	g.OnReqLLM(ev, req)
}

// --- L-21: control markers split across stream chunks must not leak to the
// user, while surrounding normal text is preserved.

type streamChunkProvider struct {
	provider.BaseProvider
	chunks []*provider.LLMResponse
}

func (m *streamChunkProvider) TextChat(ctx context.Context, req *provider.ProviderRequest) (*provider.LLMResponse, error) {
	return nil, fmt.Errorf("stream only")
}

func (m *streamChunkProvider) TextChatStream(ctx context.Context, req *provider.ProviderRequest) (<-chan *provider.LLMResponse, error) {
	ch := make(chan *provider.LLMResponse, len(m.chunks))
	for _, c := range m.chunks {
		ch <- c
	}
	close(ch)
	return ch, nil
}

func TestStreamingControlTextSplitAcrossChunks(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{})
	inst := &streamChunkProvider{chunks: []*provider.LLMResponse{
		{Role: "assistant", IsChunk: true, CompletionText: "好的，我来查一下。\n<function"},
		{Role: "assistant", IsChunk: true, CompletionText: "_calls><invoke name=\"read\">"},
		{Role: "assistant", IsChunk: true, CompletionText: "</invoke></function_calls>"},
		{Role: "assistant", IsChunk: true, CompletionText: "执行完成。"},
	}}
	event := &core.Event{Source: core.EventSource{Platform: "qq", ConvID: "g:1", SenderID: "u1"}}
	streamer := newStreamSender(s, event)
	resp, err := s.chatRound(context.Background(), inst, &provider.ProviderRequest{}, true, streamer)
	if err != nil {
		t.Fatalf("chatRound: %v", err)
	}
	streamed := streamer.pending.String()
	if strings.Contains(streamed, "function_calls") || strings.Contains(streamed, "invoke") {
		t.Fatalf("split control markers leaked to the user: %q", streamed)
	}
	if !strings.Contains(streamed, "我来查一下") {
		t.Fatalf("normal text before the marker was lost: %q", streamed)
	}
	if !strings.Contains(streamed, "执行完成") {
		t.Fatalf("normal text after the marker was lost: %q", streamed)
	}
	if len(resp.ToolsCallName) != 1 || resp.ToolsCallName[0] != "read" {
		t.Fatalf("the split XML block must still be parsed as a tool call, got %v", resp.ToolsCallName)
	}
}

func TestStreamingLegitAngleNotSwallowed(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{})
	inst := &streamChunkProvider{chunks: []*provider.LLMResponse{
		{Role: "assistant", IsChunk: true, CompletionText: "price <"},
		{Role: "assistant", IsChunk: true, CompletionText: " 5"},
	}}
	event := &core.Event{Source: core.EventSource{Platform: "qq", ConvID: "g:1", SenderID: "u1"}}
	streamer := newStreamSender(s, event)
	if _, err := s.chatRound(context.Background(), inst, &provider.ProviderRequest{}, true, streamer); err != nil {
		t.Fatalf("chatRound: %v", err)
	}
	if got := streamer.pending.String(); got != "price < 5" {
		t.Fatalf("legitimate text with '<' must be preserved, got %q", got)
	}
}

func TestControlTextPendingLen(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"<function", len("<function")},
		{"</function_calls>", 0}, // complete marker, not a pending prefix
		{"hello", 0},
		{"hello <", 1},
		{"< 5", 0},
		{"[Advisor", len("[Advisor")},
		{"plain [", 1},
	}
	for _, c := range cases {
		if got := controlTextPendingLen(c.in); got != c.want {
			t.Errorf("controlTextPendingLen(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// --- L-22: a timed-out local shell kills the whole process group, not just
// the direct child.

func TestCommandTimeoutKillsProcessGroup(t *testing.T) {
	dir := inTempDir(t)
	marker := filepath.Join(dir, "child.txt")
	// Foreground sleep keeps sh alive past the 1s timeout; a background
	// subshell would write the marker at ~2s if it survived the kill.
	cmd := fmt.Sprintf("(sleep 2; echo survived > %s) & sleep 8", marker)
	out := executeLocalShell("t:c", "t:c", cmd, false, 1)
	if !strings.Contains(out, "timed out") {
		t.Fatalf("expected a timeout, got %q", out)
	}
	// Wait past the marker-write time; a surviving grandchild would have
	// written it.
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("grandchild survived the timeout (process group not killed)")
	}
}

// --- L-23: dashboard selected_provider/selected_model metadata overrides the
// resolved provider config on a copy.

func TestSelectedProviderModelMetadata(t *testing.T) {
	cfg := map[string]interface{}{
		"provider": []interface{}{
			map[string]interface{}{"id": "p1", "type": "openai_chat_completion", "model": "gpt-4", "enable": true},
			map[string]interface{}{"id": "p2", "type": "openai_chat_completion", "model": "claude-x", "enable": true},
		},
	}
	s := testProcessStageWithConfig(t, cfg)
	providerCfg, _, err := s.resolveProvider()
	if err != nil {
		t.Fatalf("resolveProvider: %v", err)
	}
	if providerCfg["id"] != "p1" {
		t.Fatalf("default provider = %v, want p1", providerCfg["id"])
	}

	// Provider + model override.
	event := &core.Event{Metadata: map[string]interface{}{"selected_provider": "p2", "selected_model": "custom-model"}}
	got := s.applySelectedProviderModel(event, providerCfg)
	if got["id"] != "p2" {
		t.Fatalf("provider must switch to p2, got %v", got["id"])
	}
	if got["model"] != "custom-model" {
		t.Fatalf("model must be overridden, got %v", got["model"])
	}

	// Model-only override keeps the provider.
	event2 := &core.Event{Metadata: map[string]interface{}{"selected_model": "m3"}}
	got2 := s.applySelectedProviderModel(event2, providerCfg)
	if got2["id"] != "p1" || got2["model"] != "m3" {
		t.Fatalf("model-only override wrong: %v", got2)
	}

	// No metadata -> unchanged.
	got3 := s.applySelectedProviderModel(&core.Event{}, providerCfg)
	if got3["id"] != "p1" || got3["model"] != "gpt-4" {
		t.Fatalf("no metadata must leave providerCfg unchanged: %v", got3)
	}

	// The shared config must never be mutated.
	providers, _ := cfg["provider"].([]interface{})
	orig, _ := providers[0].(map[string]interface{})
	if orig["model"] != "gpt-4" {
		t.Fatal("shared provider config must not be mutated")
	}
}
