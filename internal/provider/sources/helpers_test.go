package sources

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// TestAnthropicMessageToolHistory verifies that OpenAI-format tool_calls / tool
// messages in tool-loop history are converted to Anthropic tool_use /
// tool_result content blocks (M-37/M-38).
func TestAnthropicMessageToolHistory(t *testing.T) {
	assistant := map[string]interface{}{
		"role":    "assistant",
		"content": "calling now",
		"tool_calls": []interface{}{
			map[string]interface{}{
				"id":   "call_1",
				"type": "function",
				"function": map[string]interface{}{
					"name":      "get_weather",
					"arguments": `{"city":"beijing"}`,
				},
			},
		},
	}
	got := anthropicMessage(assistant)
	if got["role"] != "assistant" {
		t.Fatalf("role = %v", got["role"])
	}
	content := got["content"].([]map[string]interface{})
	if len(content) != 2 {
		t.Fatalf("expected text + tool_use blocks, got %d", len(content))
	}
	if tb := content[0]; tb["type"] != "text" || tb["text"] != "calling now" {
		t.Fatalf("unexpected first block: %v", tb)
	}
	block := content[1]
	if block["type"] != "tool_use" || block["name"] != "get_weather" || block["id"] != "call_1" {
		t.Fatalf("unexpected tool_use block: %v", block)
	}
	input := block["input"].(map[string]interface{})
	if input["city"] != "beijing" {
		t.Fatalf("unexpected tool input: %v", input)
	}

	toolMsg := map[string]interface{}{
		"role":         "tool",
		"tool_call_id": "call_1",
		"content":      "sunny 25c",
	}
	gotTool := anthropicMessage(toolMsg)
	if gotTool["role"] != "user" {
		t.Fatalf("tool_result must be role user, got %v", gotTool["role"])
	}
	blocks := gotTool["content"].([]map[string]interface{})
	if len(blocks) != 1 {
		t.Fatalf("expected 1 tool_result block, got %d", len(blocks))
	}
	tr := blocks[0]
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "call_1" || tr["content"] != "sunny 25c" {
		t.Fatalf("unexpected tool_result block: %v", tr)
	}

	plain := anthropicMessage(map[string]interface{}{"role": "user", "content": "hi"})
	if plain["role"] != "user" {
		t.Fatalf("plain user role changed: %v", plain["role"])
	}
	pc := plain["content"].([]map[string]interface{})
	if len(pc) != 1 {
		t.Fatalf("expected 1 text block, got %d", len(pc))
	}
	if tb := pc[0]; tb["type"] != "text" || tb["text"] != "hi" {
		t.Fatalf("unexpected text block: %v", tb)
	}
}

// TestAnthropicMessageInMemoryFormat covers the in-memory message shapes the
// pipeline produces: tool_calls as []map[string]interface{} and content arrays
// as []map[string]interface{} (not JSON round-tripped []interface{}).
func TestAnthropicMessageInMemoryFormat(t *testing.T) {
	assistant := map[string]interface{}{
		"role":    "assistant",
		"content": "",
		"tool_calls": []map[string]interface{}{
			{
				"id":   "call_m",
				"type": "function",
				"function": map[string]interface{}{
					"name":      "do_thing",
					"arguments": `{"k":1}`,
				},
			},
		},
	}
	got := anthropicMessage(assistant)
	content := got["content"].([]map[string]interface{})
	if len(content) != 1 {
		t.Fatalf("expected 1 tool_use block (empty text skipped), got %d", len(content))
	}
	tu := content[0]
	if tu["type"] != "tool_use" || tu["id"] != "call_m" || tu["name"] != "do_thing" {
		t.Fatalf("unexpected tool_use block: %v", tu)
	}
	args := tu["input"].(map[string]interface{})
	if args["k"] != float64(1) {
		t.Fatalf("unexpected tool input: %v", args)
	}

	toolMsg := map[string]interface{}{
		"role":         "tool",
		"tool_call_id": "call_m",
		"content": []map[string]interface{}{
			{"type": "text", "text": "part1"},
			{"type": "text", "text": "part2"},
		},
	}
	gotTool := anthropicMessage(toolMsg)
	tr := gotTool["content"].([]map[string]interface{})[0]
	if tr["type"] != "tool_result" || tr["content"] != "part1part2" {
		t.Fatalf("unexpected tool_result block: %v", tr)
	}
}

// TestAnthropicImageBlock verifies local image references become base64 image
// content blocks for the Anthropic protocol (M-37d).
func TestAnthropicImageBlock(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "pic.png")
	if err := os.WriteFile(img, []byte{1, 2, 3}, 0o644); err != nil {
		t.Fatal(err)
	}
	block := imageToAnthropicBlock(img)
	if block == nil {
		t.Fatal("expected image block")
	}
	if block["type"] != "image" {
		t.Fatalf("type = %v", block["type"])
	}
	src := block["source"].(map[string]interface{})
	if src["type"] != "base64" || src["media_type"] != "image/png" {
		t.Fatalf("unexpected source: %v", src)
	}
	if data, _ := src["data"].(string); data == "" {
		t.Fatal("expected non-empty base64 data")
	}
}

// TestGeminiBuildRequestBodyMedia verifies multimodal context and ImageURLs /
// AudioURLs are converted into Gemini inline_data parts (M-39).
func TestGeminiBuildRequestBodyMedia(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "pic.png")
	if err := os.WriteFile(img, []byte{1, 2, 3}, 0o644); err != nil {
		t.Fatal(err)
	}
	audio := filepath.Join(dir, "clip.mp3")
	if err := os.WriteFile(audio, []byte{9, 9, 9}, 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewGeminiSource(map[string]interface{}{"key": "k", "model": "gemini-2.0-flash"}, nil)
	req := &provider.ProviderRequest{
		Prompt:    "look",
		ImageURLs: []string{img},
		AudioURLs: []string{audio},
		Contexts: []map[string]interface{}{
			{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "prev"},
					map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": img}},
				},
			},
		},
	}
	body := s.buildRequestBody(req, false)
	contents := body["contents"].([]map[string]interface{})
	if len(contents) != 2 {
		t.Fatalf("expected 2 contents, got %d", len(contents))
	}
	histParts := contents[0]["parts"].([]map[string]interface{})
	if len(histParts) != 2 {
		t.Fatalf("expected 2 history parts, got %d", len(histParts))
	}
	ip := histParts[1]["inline_data"].(map[string]interface{})
	if ip["mime_type"] != "image/png" {
		t.Fatalf("unexpected history mime: %v", ip["mime_type"])
	}
	if _, ok := ip["data"].(string); !ok {
		t.Fatal("expected base64 data in history part")
	}
	userParts := contents[1]["parts"].([]map[string]interface{})
	if len(userParts) != 3 {
		t.Fatalf("expected text+image+audio user parts, got %d", len(userParts))
	}
	if tp := userParts[0]; tp["text"] != "look" {
		t.Fatalf("unexpected text part: %v", tp)
	}
	imgPart := userParts[1]["inline_data"].(map[string]interface{})
	if imgPart["mime_type"] != "image/png" {
		t.Fatalf("unexpected image mime: %v", imgPart["mime_type"])
	}
	audPart := userParts[2]["inline_data"].(map[string]interface{})
	if audPart["mime_type"] != "audio/mpeg" {
		t.Fatalf("unexpected audio mime: %v", audPart["mime_type"])
	}
}
