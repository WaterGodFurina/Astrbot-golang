package sources

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

// newStreamClient returns an http.Client for streaming reads. A generous
// whole-request timeout bounds the read so a dead connection cannot leave the
// streaming goroutine blocked on body reads forever, even if the caller's
// context is never cancelled. Cancellation at token granularity is delegated
// to the caller's context via sseReader.scan.
func newStreamClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Minute}
}

// stripURLQuery returns the URL without its query string, for safe logging.
func stripURLQuery(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.RawQuery = ""
	return u.String()
}

// logger 供 sources 包内各 LLM 提供方的请求/响应摘要 Debug 日志共用，
// 级别默认 INFO，Debug 仅在 DEBUG 级别下输出，不污染 INFO 日志。
var logger = log.GetDefault().WithComponent("Provider")

// configString returns the string value of key, or the fallback when absent.
func configString(config map[string]interface{}, key, fallback string) string {
	if v, ok := config[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

// cloneMap returns a shallow copy of a string-keyed map so callers can apply
// defaults without mutating a shared config map.
func cloneMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// configInt returns the int value of key, or the fallback when absent.
func configInt(config map[string]interface{}, key string, fallback int) int {
	switch v := config[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return fallback
}

// maxImageBytes caps a single image fetched for multimodal providers.
const maxImageBytes = 20 << 20 // 20MB

// maxAudioBytes caps a single audio file downloaded for STT providers.
const maxAudioBytes = 50 << 20 // 50MB

// fetchMediaData resolves a user-supplied media reference (base64 data URL,
// local file path, or remote URL) into raw bytes and a media type. It is used
// by providers that require inline base64 media (Anthropic / Gemini).
func fetchMediaData(raw string) ([]byte, string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, "", fmt.Errorf("empty media reference")
	}
	if strings.HasPrefix(trimmed, "data:") {
		return decodeDataURL(trimmed)
	}
	path := strings.TrimPrefix(trimmed, "file://")
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, "", err
		}
		return data, mediaTypeForExt(path), nil
	}
	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, trimmed, nil)
		if err != nil {
			return nil, "", err
		}
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return nil, "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
		if err != nil {
			return nil, "", err
		}
		if len(data) > maxImageBytes {
			return nil, "", fmt.Errorf("media exceeds %d bytes", maxImageBytes)
		}
		mediaType := resp.Header.Get("Content-Type")
		if mediaType == "" {
			mediaType = mediaTypeForExt(trimmed)
		}
		return data, mediaType, nil
	}
	return nil, "", fmt.Errorf("unsupported image reference: %s", trimmed)
}

// decodeDataURL extracts bytes and media type from a "data:<type>;base64,..."
// data URL.
func decodeDataURL(raw string) ([]byte, string, error) {
	rest := strings.TrimPrefix(raw, "data:")
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return nil, "", fmt.Errorf("malformed data URL")
	}
	meta, payload := rest[:comma], rest[comma+1:]
	mediaType := ""
	for _, part := range strings.Split(meta, ";") {
		if strings.Contains(part, "/") {
			mediaType = part
		}
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, "", err
	}
	return data, mediaType, nil
}

// mediaTypeForExt guesses the media type from a file extension.
func mediaTypeForExt(name string) string {
	base := strings.Split(name, "?")[0]
	if ext := mime.TypeByExtension(strings.ToLower(filepath.Ext(base))); ext != "" {
		return strings.Split(ext, ";")[0]
	}
	return "image/png"
}

// imageToAnthropicBlock converts an image reference into an Anthropic image
// content block (base64 source). Returns nil when the image cannot be loaded.
func imageToAnthropicBlock(raw string) map[string]interface{} {
	data, mediaType, err := fetchMediaData(raw)
	if err != nil {
		logger.Warn("Anthropic: 加载图片 %q 失败: %v", raw, err)
		return nil
	}
	if mediaType == "" {
		mediaType = "image/png"
	}
	return map[string]interface{}{
		"type": "image",
		"source": map[string]interface{}{
			"type":       "base64",
			"media_type": mediaType,
			"data":       base64.StdEncoding.EncodeToString(data),
		},
	}
}

// imageToGeminiPart converts an image reference into a Gemini inline_data
// part. Returns nil when the image cannot be loaded.
func imageToGeminiPart(raw string) map[string]interface{} {
	data, mediaType, err := fetchMediaData(raw)
	if err != nil {
		logger.Warn("Gemini: 加载图片 %q 失败: %v", raw, err)
		return nil
	}
	if mediaType == "" {
		mediaType = "image/png"
	}
	return map[string]interface{}{
		"inline_data": map[string]interface{}{
			"mime_type": mediaType,
			"data":      base64.StdEncoding.EncodeToString(data),
		},
	}
}

// geminiMediaPart converts an audio reference into a Gemini inline_data part.
// Returns nil when the audio cannot be loaded.
func geminiMediaPart(raw string) map[string]interface{} {
	data, mediaType, err := fetchMediaData(raw)
	if err != nil {
		logger.Warn("Gemini: 加载音频 %q 失败: %v", raw, err)
		return nil
	}
	if mediaType == "" {
		mediaType = "audio/mpeg"
	}
	return map[string]interface{}{
		"inline_data": map[string]interface{}{
			"mime_type": mediaType,
			"data":      base64.StdEncoding.EncodeToString(data),
		},
	}
}

// geminiPartsFromContent converts an OpenAI content value (string or array of
// text/image_url/audio_url blocks) into Gemini parts.
func geminiPartsFromContent(content interface{}) []map[string]interface{} {
	parts := []map[string]interface{}{}
	for _, b := range contentAsBlocks(content) {
		typ, _ := b["type"].(string)
		switch typ {
		case "text":
			if text, ok := b["text"].(string); ok && text != "" {
				parts = append(parts, map[string]interface{}{"text": text})
			}
		case "image_url":
			var raw string
			switch u := b["image_url"].(type) {
			case map[string]interface{}:
				raw, _ = u["url"].(string)
			case string:
				raw = u
			}
			if raw == "" {
				continue
			}
			if part := imageToGeminiPart(raw); part != nil {
				parts = append(parts, part)
			}
		case "audio_url":
			var raw string
			switch u := b["audio_url"].(type) {
			case map[string]interface{}:
				raw, _ = u["url"].(string)
			case string:
				raw = u
			}
			if raw == "" {
				continue
			}
			if part := geminiMediaPart(raw); part != nil {
				parts = append(parts, part)
			}
		}
	}
	return parts
}

// contentAsBlocks normalizes a message content value (string or array of
// OpenAI blocks) into a slice of block maps. The pipeline builds in-memory
// content arrays as []map[string]interface{}; history round-tripped through
// JSON arrives as []interface{}. Both are accepted.
func contentAsBlocks(content interface{}) []map[string]interface{} {
	switch v := content.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []map[string]interface{}{{"type": "text", "text": v}}
	case []map[string]interface{}:
		return v
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(v))
		for _, bAny := range v {
			if bm, ok := bAny.(map[string]interface{}); ok {
				out = append(out, bm)
			}
		}
		return out
	default:
		return nil
	}
}

// toolCallsSlice normalizes a message tool_calls value ([]map[string]interface{}
// in-memory or []interface{} after JSON round-trip) into a slice of block maps.
func toolCallsSlice(v interface{}) []map[string]interface{} {
	switch tc := v.(type) {
	case []map[string]interface{}:
		return tc
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(tc))
		for _, a := range tc {
			if m, ok := a.(map[string]interface{}); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

// anthropicContentBlocks converts OpenAI content blocks (text / image_url /
// audio_url) into Anthropic content blocks (text / base64 image). Unsupported
// blocks are skipped.
func anthropicContentBlocks(blocks []map[string]interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(blocks))
	for _, b := range blocks {
		typ, _ := b["type"].(string)
		switch typ {
		case "text":
			if text, ok := b["text"].(string); ok && text != "" {
				out = append(out, map[string]interface{}{"type": "text", "text": text})
			}
		case "image_url":
			var raw string
			switch u := b["image_url"].(type) {
			case map[string]interface{}:
				raw, _ = u["url"].(string)
			case string:
				raw = u
			}
			if raw == "" {
				continue
			}
			if block := imageToAnthropicBlock(raw); block != nil {
				out = append(out, block)
			}
		}
	}
	return out
}

// anthropicToolResultContent extracts the plain text of a tool message content
// (string or array of text blocks).
func anthropicToolResultContent(content interface{}) string {
	blocks := contentAsBlocks(content)
	var b strings.Builder
	for _, bm := range blocks {
		if t, ok := bm["text"].(string); ok {
			b.WriteString(t)
		}
	}
	return b.String()
}

// anthropicMessage converts one OpenAI-format context message into the
// Anthropic Messages protocol shape: assistant messages with tool_calls gain
// tool_use content blocks, tool messages become tool_result blocks (role
// "user"), and image_url content blocks become base64 image blocks. Messages
// without OpenAI-specific shape are passed through unchanged. It is shared by
// the Anthropic and Kimi providers so both translate tool-loop history.
func anthropicMessage(msg map[string]interface{}) map[string]interface{} {
	role, _ := msg["role"].(string)
	switch role {
	case "assistant":
		out := map[string]interface{}{"role": "assistant"}
		content := anthropicContentBlocks(contentAsBlocks(msg["content"]))
		for _, tc := range toolCallsSlice(msg["tool_calls"]) {
			fn, _ := tc["function"].(map[string]interface{})
			if fn == nil {
				continue
			}
			name, _ := fn["name"].(string)
			if name == "" {
				continue
			}
			var input map[string]interface{}
			if argsRaw, ok := fn["arguments"].(string); ok && argsRaw != "" {
				_ = json.Unmarshal([]byte(argsRaw), &input)
			}
			if input == nil {
				input = map[string]interface{}{}
			}
			id, _ := tc["id"].(string)
			content = append(content, map[string]interface{}{
				"type":  "tool_use",
				"id":    id,
				"name":  name,
				"input": input,
			})
		}
		out["content"] = content
		return out
	case "tool":
		toolCallID, _ := msg["tool_call_id"].(string)
		return map[string]interface{}{
			"role": "user",
			"content": []map[string]interface{}{
				{
					"type":        "tool_result",
					"tool_use_id": toolCallID,
					"content":     anthropicToolResultContent(msg["content"]),
				},
			},
		}
	case "user":
		return map[string]interface{}{
			"role":    "user",
			"content": anthropicContentBlocks(contentAsBlocks(msg["content"])),
		}
	default:
		return msg
	}
}
