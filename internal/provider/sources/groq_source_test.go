package sources

import "testing"

// TestStripAssistantReasoningFieldsCopiesSharedHistory verifies that stripping
// reasoning fields does not mutate the shared session history maps aliased by
// the request body (L-46.2c).
func TestStripAssistantReasoningFieldsCopiesSharedHistory(t *testing.T) {
	shared := map[string]interface{}{
		"role":              "assistant",
		"content":           "hi",
		"reasoning_content": "thinking",
		"reasoning":         map[string]interface{}{"summary": "s"},
	}
	body := map[string]interface{}{
		"messages": []map[string]interface{}{
			{"role": "user", "content": "q"},
			shared,
			{"role": "assistant", "content": "no reasoning"},
		},
	}

	stripAssistantReasoningFields(body)

	if _, ok := shared["reasoning_content"]; !ok {
		t.Fatal("shared session history map was mutated (reasoning_content deleted)")
	}
	if _, ok := shared["reasoning"]; !ok {
		t.Fatal("shared session history map was mutated (reasoning deleted)")
	}

	msgs := body["messages"].([]map[string]interface{})
	if _, ok := msgs[1]["reasoning_content"]; ok {
		t.Fatal("request body message still carries reasoning_content")
	}
	if _, ok := msgs[1]["reasoning"]; ok {
		t.Fatal("request body message still carries reasoning")
	}
	if msgs[1]["content"] != "hi" {
		t.Fatalf("copied message lost content: %v", msgs[1])
	}
}

// TestXiaomiLongcatConfigNotRewritten verifies the constructors apply defaults
// to a copy instead of mutating the caller's shared config map (L-46.2c).
func TestXiaomiLongcatConfigNotRewritten(t *testing.T) {
	xcfg := map[string]interface{}{"key": "sk-test"}
	src := NewXiaomiSource(xcfg, map[string]interface{}{})
	if _, ok := xcfg["api_base"]; ok {
		t.Fatal("NewXiaomiSource mutated the shared config map (api_base)")
	}
	if src.apiBase != "https://api.xiaomimimo.com/v1" {
		t.Errorf("xiaomi api_base = %q", src.apiBase)
	}

	lcfg := map[string]interface{}{"key": "sk-test"}
	lsrc := NewLongcatSource(lcfg, map[string]interface{}{})
	if _, ok := lcfg["api_base"]; ok {
		t.Fatal("NewLongcatSource mutated the shared config map (api_base)")
	}
	if lsrc.apiBase != "https://api.longcat.chat/openai/v1" {
		t.Errorf("longcat api_base = %q", lsrc.apiBase)
	}
}
