package dashboard

import "testing"

func TestProviderNameToProviderType(t *testing.T) {
	cases := map[string]string{
		"openai_chat_completion":    "chat_completion",
		"anthropic_chat_completion": "chat_completion",
		"deepseek_chat":             "chat_completion",
		"openai_whisper":            "speech_to_text",
		"openai_tts":                "text_to_speech",
		"openai_embedding":          "embedding",
		"bailian_rerank":            "rerank",
		"dify":                      "agent_runner",
		"some_random_provider":      "",
		"":                          "",
	}
	for in, want := range cases {
		if got := providerNameToProviderType(in); got != want {
			t.Errorf("providerNameToProviderType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCapabilityToProviderType(t *testing.T) {
	cases := map[string]string{
		"chat":            "chat_completion",
		"chat_completion": "chat_completion",
		"agent":           "agent_runner",
		"stt":             "speech_to_text",
		"tts":             "text_to_speech",
		"embedding":       "embedding",
		"rerank":          "rerank",
		"unknown":         "unknown",
	}
	for in, want := range cases {
		if got := capabilityToProviderType(in); got != want {
			t.Errorf("capabilityToProviderType(%q) = %q, want %q", in, got, want)
		}
	}
}
