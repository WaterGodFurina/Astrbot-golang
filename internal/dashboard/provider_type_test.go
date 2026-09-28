package dashboard

import "testing"

func TestCapabilityToProviderType(t *testing.T) {
	cases := map[string]string{
		"chat":      "chat_completion",
		"stt":       "speech_to_text",
		"tts":       "text_to_speech",
		"embedding": "embedding",
		"rerank":    "rerank",
		// 未知值原样透传（对齐 Python CAPABILITY_TO_PROVIDER_TYPE.get(capability, capability)）
		"chat_completion": "chat_completion",
		"agent":           "agent",
		"agent_runner":    "agent_runner",
		"unknown":         "unknown",
	}
	for in, want := range cases {
		if got := capabilityToProviderType(in); got != want {
			t.Errorf("capabilityToProviderType(%q) = %q, want %q", in, got, want)
		}
	}
}
