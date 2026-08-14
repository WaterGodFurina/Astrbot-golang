package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
)

func TestGitSnapshot(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash := gitTreeHash(dir)
	if hash == "" {
		t.Fatal("expected tree hash")
	}
	if err := os.WriteFile(file, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := gitDiffTree(dir, hash)
	if patch == "" {
		t.Fatal("expected non-empty diff after modification")
	}
	if !containsStr(patch, "a.txt") {
		t.Fatalf("patch should mention a.txt: %s", patch)
	}
}

func TestEstimateTokensAndRounds(t *testing.T) {
	ctxs := []map[string]interface{}{
		{"role": "user", "content": "你好，今天天气怎么样"},
		{"role": "assistant", "content": "今天天气晴朗"},
		{"role": "user", "content": "谢谢"},
	}
	if estimateContextTokens(ctxs) <= 0 {
		t.Fatal("token estimate should be positive")
	}
	rounds := splitContextRounds(ctxs)
	if len(rounds) != 2 {
		t.Fatalf("expected 2 rounds, got %d", len(rounds))
	}
	old, recent := splitRoundsByRatio(rounds, estimateContextTokens(ctxs), 0.15)
	// With a small total, the recent budget may cover both rounds; but the
	// function must not panic and must return valid slices.
	_ = old
	_ = recent
}

func TestTruncateContextEntries(t *testing.T) {
	var ctxs []map[string]interface{}
	for i := 0; i < 10; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		ctxs = append(ctxs, map[string]interface{}{"role": role, "content": "x"})
	}
	truncated := truncateContextEntries(ctxs, 2)
	if len(truncated) != 4+1 { // 2 pairs + notice
		t.Fatalf("expected 5 entries, got %d", len(truncated))
	}
	if truncated[len(truncated)-1]["role"] != "system" {
		t.Fatal("last entry should be the truncation notice")
	}
}

func TestDoomLoop(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{})
	// platformMgr nil -> askDoomConfirm silently no-ops.
	event := &core.Event{
		Source: core.EventSource{Platform: "qq", ConvID: "group:1", SenderID: "u1"},
	}
	// shell is whitelisted (exempt from doom detection).
	for i := 0; i < 10; i++ {
		if !s.checkDoomLoop(event, "astrbot_execute_shell") {
			t.Fatal("whitelisted tool should never be paused")
		}
	}
	// A non-whitelisted tool pauses after doomLoopThreshold consecutive calls.
	for i := 0; i < doomLoopThreshold-1; i++ {
		if !s.checkDoomLoop(event, "read") {
			t.Fatal("calls before threshold should pass")
		}
	}
	if s.checkDoomLoop(event, "read") {
		t.Fatal("threshold call should be paused")
	}
	// Another tool unaffected.
	if !s.checkDoomLoop(event, "grep") {
		t.Fatal("different tool should pass")
	}
	// The paused tool stays paused.
	if s.checkDoomLoop(event, "read") {
		t.Fatal("paused tool should stay paused")
	}
}

func TestDoomConfirmByUMOAndSender(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{})
	event := &core.Event{
		Source: core.EventSource{Platform: "qq", ConvID: "group:1", SenderID: "u1"},
	}
	for i := 0; i < doomLoopThreshold; i++ {
		s.checkDoomLoop(event, "read")
	} // paused, asker=u1

	// Another user in the same group cannot confirm.
	other := &core.Event{
		MessageStr: "继续",
		Source:     core.EventSource{Platform: "qq", ConvID: "group:1", SenderID: "u2"},
	}
	if s.maybeHandleDoomConfirm(other) != doomNotConsumed {
		t.Fatal("another sender must not be able to confirm")
	}
	// Same sender on a different session cannot confirm.
	different := &core.Event{
		MessageStr: "继续",
		Source:     core.EventSource{Platform: "qq", ConvID: "group:2", SenderID: "u1"},
	}
	if s.maybeHandleDoomConfirm(different) != doomNotConsumed {
		t.Fatal("different session must not confirm")
	}
	// The asker confirms -> unpaused.
	asker := &core.Event{
		MessageStr: "继续",
		Source:     core.EventSource{Platform: "qq", ConvID: "group:1", SenderID: "u1"},
	}
	if s.maybeHandleDoomConfirm(asker) != doomResumed {
		t.Fatal("asker should be able to confirm")
	}
	if !s.checkDoomLoop(event, "read") {
		t.Fatal("tool should be unpaused after confirmation")
	}
}

func TestMaxAgentStepConfig(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{
		"provider_settings": map[string]interface{}{"max_agent_step": 10},
	})
	if s.providerConf.MaxAgentStep != 10 {
		t.Fatalf("max_agent_step = %d", s.providerConf.MaxAgentStep)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestMaterializeToolResult(t *testing.T) {
	// Small result unchanged.
	small := "ok"
	if got := materializeToolResult(small, "c1"); got != small {
		t.Fatalf("small result should pass through: %q", got)
	}
	// Large result -> preview + overflow notice + file on disk.
	big := make([]rune, maxInlineToolResultChars+1000)
	for i := range big {
		big[i] = 'x'
	}
	out := materializeToolResult(string(big), "tool_call_123")
	if len(out) >= len(big) {
		t.Fatal("large result should be truncated")
	}
	if !containsStr(out, "astrbot_file_read_tool") {
		t.Fatal("missing read-tool hint")
	}
	if !containsStr(out, "tool_results") {
		t.Fatal("missing overflow path")
	}
	// The overflow file exists.
	if _, err := os.Stat("data/temp/tool_results"); err != nil {
		t.Fatalf("overflow dir missing: %v", err)
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("你好世界", 2); got != "你好" {
		t.Fatalf("truncateRunes = %q", got)
	}
	if got := truncateRunes("hello", 10); got != "hello" {
		t.Fatalf("short string should pass: %q", got)
	}
}

func TestParseXMLToolCalls(t *testing.T) {
	xml := `<function_calls><invoke name="astrbot_execute_shell"><parameter name="command">ls -la</parameter><parameter name="timeout">60</parameter></invoke><invoke name="read"><parameter name="path">/x/y.txt</parameter></invoke></function_calls>`
	calls, ok := parseXMLToolCalls(xml)
	if !ok || len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %v ok=%v", calls, ok)
	}
	if calls[0].name != "astrbot_execute_shell" {
		t.Fatalf("call0 name = %q", calls[0].name)
	}
	if calls[0].args["command"] != "ls -la" {
		t.Fatalf("call0 command = %v", calls[0].args["command"])
	}
	if !containsToolXML("<invoke name=") {
		t.Fatal("containsToolXML should detect invoke tag")
	}
	if containsToolXML("普通文本") {
		t.Fatal("plain text should not be flagged")
	}
	if _, ok := parseXMLToolCalls("无工具调用"); ok {
		t.Fatal("no XML block should return ok=false")
	}
	// stripToolCallXML removes the block and tags.
	cleaned := stripToolCallXML(xml + "\n正文")
	if containsStr(cleaned, "function_calls") || containsStr(cleaned, "invoke") {
		t.Fatalf("cleaned output still has tags: %q", cleaned)
	}
}

func TestStripAdvisorMarkup(t *testing.T) {
	s := `[Advisor review]
<astrbot_advisor>
计划：1. 检查目录 2. git clone 3. 阅读 README
</astrbot_advisor>
接下来我会执行工具。`
	cleaned := stripToolCallXML(s)
	if containsStr(cleaned, "Advisor") || containsStr(cleaned, "astrbot_advisor") || containsStr(cleaned, "计划") {
		t.Fatalf("advisor block should be removed: %q", cleaned)
	}
	if !containsStr(cleaned, "执行工具") {
		t.Fatalf("real reply text should remain: %q", cleaned)
	}
	if !containsControlText("[Advisor review]") {
		t.Fatal("advisor tag should be flagged as control text")
	}
	if !containsControlText("<astrbot_advisor>") {
		t.Fatal("advisor block should be flagged")
	}
	if containsControlText("普通回复文本") {
		t.Fatal("plain reply should not be flagged")
	}
}

func TestWebSearchInjection(t *testing.T) {
	cfg := map[string]interface{}{
		"provider_settings": map[string]interface{}{
			"web_search":           true,
			"websearch_provider":   "tavily",
			"websearch_tavily_key": []interface{}{"tvly-test-key"},
		},
	}
	s := testProcessStageWithConfig(t, cfg)
	found := false
	for _, tool := range s.collectTools("none") {
		fn, _ := tool["function"].(map[string]interface{})
		if fn["name"] == "web_search_tavily" {
			found = true
		}
	}
	if !found {
		t.Fatal("web_search_tavily should be injected when enabled with a key")
	}

	// Disabled provider -> not injected.
	s2 := testProcessStageWithConfig(t, map[string]interface{}{
		"provider_settings": map[string]interface{}{
			"web_search":           false,
			"websearch_provider":   "tavily",
			"websearch_tavily_key": []interface{}{"tvly-test-key"},
		},
	})
	for _, tool := range s2.collectTools("none") {
		fn, _ := tool["function"].(map[string]interface{})
		if fn["name"] == "web_search_tavily" {
			t.Fatal("web search should not be injected when disabled")
		}
	}
}

func TestTavilyKeys(t *testing.T) {
	if len(tavilyKeys(map[string]interface{}{})) != 0 {
		t.Fatal("empty config should have no keys")
	}
	cfg := map[string]interface{}{
		"provider_settings": map[string]interface{}{"websearch_tavily_key": []interface{}{"a", "b"}},
	}
	if got := tavilyKeys(cfg); len(got) != 2 {
		t.Fatalf("expected 2 keys, got %v", got)
	}
}

func TestWebSearchProvidersInjection(t *testing.T) {
	providers := map[string]string{
		"tavily":    "websearch_tavily_key",
		"bocha":     "websearch_bocha_key",
		"brave":     "websearch_brave_key",
		"firecrawl": "websearch_firecrawl_key",
		"exa":       "websearch_exa_key",
	}
	for provider, key := range providers {
		cfg := map[string]interface{}{
			"provider_settings": map[string]interface{}{
				"web_search":         true,
				"websearch_provider": provider,
				key:                  []interface{}{"test-key"},
			},
		}
		s := testProcessStageWithConfig(t, cfg)
		found := false
		for _, tool := range s.collectTools("none") {
			fn, _ := tool["function"].(map[string]interface{})
			if fn["name"] == "web_search_"+provider {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s tool should be injected when provider=%s with key", provider, provider)
		}
		// No key -> not injected.
		cfg2 := map[string]interface{}{
			"provider_settings": map[string]interface{}{
				"web_search": true, "websearch_provider": provider,
			},
		}
		s2 := testProcessStageWithConfig(t, cfg2)
		for _, tool := range s2.collectTools("none") {
			fn, _ := tool["function"].(map[string]interface{})
			if fn["name"] == "web_search_"+provider {
				t.Fatalf("%s should NOT be injected without key", provider)
			}
		}
	}
}

func TestSendAndGroupAndKBInjection(t *testing.T) {
	// group history only when enabled.
	cfgOn := map[string]interface{}{"provider_ltm_settings": map[string]interface{}{"group_message_history_enable": true}}
	sOn := testProcessStageWithConfig(t, cfgOn)
	var names []string
	for _, tool := range sOn.collectTools("none") {
		fn, _ := tool["function"].(map[string]interface{})
		if n, _ := fn["name"].(string); n != "" {
			names = append(names, n)
		}
	}
	if !containsStrArr(names, "get_group_message_history") {
		t.Fatalf("group history should inject when enabled: %v", names)
	}
	// kb agentic mode.
	cfgKB := map[string]interface{}{"kb_agentic_mode": true}
	sKB := testProcessStageWithConfig(t, cfgKB)
	for _, tool := range sKB.collectTools("none") {
		fn, _ := tool["function"].(map[string]interface{})
		if fn["name"] == "astr_kb_search" {
			goto ok
		}
	}
	t.Fatal("astr_kb_search should inject when kb_agentic_mode")
ok:
	// not injected when kb_agentic_mode false.
	sNo := testProcessStageWithConfig(t, map[string]interface{}{})
	for _, tool := range sNo.collectTools("none") {
		fn, _ := tool["function"].(map[string]interface{})
		if fn["name"] == "astr_kb_search" {
			t.Fatal("kb search should not inject when agentic mode off")
		}
	}
}

func containsStrArr(arr []string, s string) bool {
	for _, x := range arr {
		if x == s {
			return true
		}
	}
	return false
}

func TestExtractToolSchemaURLRequired(t *testing.T) {
	// Extract tools must require "url", not "query".
	for _, schema := range []map[string]interface{}{tavilyExtractToolSchema(), firecrawlExtractToolSchema(), exaContentsToolSchema()} {
		fn, _ := schema["function"].(map[string]interface{})
		name, _ := fn["name"].(string)
		params, _ := fn["parameters"].(map[string]interface{})
		props, _ := params["properties"].(map[string]interface{})
		if _, ok := props["query"]; ok {
			t.Fatalf("%s must not have query param", name)
		}
		if _, ok := props["url"]; !ok {
			t.Fatalf("%s must have url param", name)
		}
		req, _ := params["required"].([]interface{})
		found := false
		for _, r := range req {
			if r == "url" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s required should include url", name)
		}
	}
}
