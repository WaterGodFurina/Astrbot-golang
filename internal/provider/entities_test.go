package provider

import "testing"

// TestToOpenAIToolCallsInconsistentLengths verifies that ToOpenAIToolCalls
// never indexes past the shortest parallel slice (L-46.1a).
func TestToOpenAIToolCallsInconsistentLengths(t *testing.T) {
	r := &LLMResponse{
		ToolsCallArgs: []map[string]interface{}{{"a": "1"}, {"b": "2"}, {"c": "3"}},
		ToolsCallName: []string{"f1", "f2"},
		ToolsCallIDs:  []string{"id1"},
	}
	calls := r.ToOpenAIToolCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call (shared prefix), got %d", len(calls))
	}
	fn, _ := calls[0]["function"].(map[string]interface{})
	if calls[0]["id"] != "id1" || fn["name"] != "f1" {
		t.Fatalf("unexpected call: %v", calls[0])
	}
}

// TestToOpenAIToolCallsEmpty verifies an empty response yields no calls.
func TestToOpenAIToolCallsEmpty(t *testing.T) {
	r := NewLLMResponse("assistant", "")
	calls := r.ToOpenAIToolCalls()
	if len(calls) != 0 {
		t.Fatalf("expected 0 calls, got %d", len(calls))
	}
}

// TestBaseProviderMetaCapability verifies Meta() observes SetCapability
// (the capability field is read under the lock, L-46.1b).
func TestBaseProviderMetaCapability(t *testing.T) {
	b := NewBaseProvider(map[string]interface{}{"id": "x", "type": "openai"}, nil)
	if got := b.Meta().ProviderType; got != CapChatCompletion {
		t.Fatalf("default capability = %v, want chat_completion", got)
	}
	b.SetCapability(CapSpeechToText)
	if got := b.Meta().ProviderType; got != CapSpeechToText {
		t.Fatalf("capability after SetCapability = %v, want speech_to_text", got)
	}
}
