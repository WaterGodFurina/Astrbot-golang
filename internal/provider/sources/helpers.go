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
	"github.com/WaterGodFurina/Astrbot-golang/internal/utils"
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

// configKey returns the API key for key, accepting both the plain-string form
// ("key": "sk-...") and the keyring-array form ("key": ["sk-...", ...]) used
// by cmd_config.json（取首个）。Fallback sources（anthropic/gemini 等）各自
// 内联了数组处理；此处统一抽出供 embedding 等新代码复用。
func configKey(config map[string]interface{}, key string) string {
	if v, ok := config[key].(string); ok && v != "" {
		return v
	}
	if keys, ok := config[key].([]interface{}); ok {
		for _, k := range keys {
			if s, ok := k.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
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

// configBool returns the bool value of key, or the fallback when absent.
// 兼容 WebUI 表单可能提交的字符串/数字形态（对齐 py 布尔配置解析）。
func configBool(config map[string]interface{}, key string, fallback bool) bool {
	switch v := config[key].(type) {
	case bool:
		return v
	case string:
		switch strings.TrimSpace(strings.ToLower(v)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	case float64:
		return v != 0
	case int:
		return v != 0
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
	// base64://<payload>（自家出站/插件会产出该形态，对齐 py media_utils 的
	// base64:// 分支）：解码后按 magic bytes 嗅探 MIME，容忍换行/空白与缺失 padding。
	if strings.HasPrefix(trimmed, "base64://") {
		payload := strings.Join(strings.Fields(strings.TrimPrefix(trimmed, "base64://")), "")
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			if d, rawErr := base64.RawStdEncoding.DecodeString(payload); rawErr == nil {
				data, err = d, nil
			}
		}
		if err != nil {
			return nil, "", fmt.Errorf("invalid base64 media payload: %w", err)
		}
		if len(data) > maxImageBytes {
			return nil, "", fmt.Errorf("media exceeds %d bytes", maxImageBytes)
		}
		mt := sniffMediaType(data)
		if mt == "" {
			mt = "image/jpeg"
		}
		return data, mt, nil
	}
	// file:// URI 与裸本地路径都归一为文件系统路径（兼容 Windows 三斜杠与
	// percent-encoding）；非本地引用（http(s) 等）原样返回，走下面的下载分支。
	path := utils.FileURIToPath(trimmed)
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		f, err := os.Open(path)
		if err != nil {
			return nil, "", err
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, maxImageBytes+1))
		if err != nil {
			return nil, "", err
		}
		if len(data) > maxImageBytes {
			return nil, "", fmt.Errorf("media exceeds %d bytes", maxImageBytes)
		}
		// 优先按内容嗅探 MIME，其次按扩展名，最后回退 jpeg（对齐 py 默认）。
		mt := sniffMediaType(data)
		if mt == "" {
			mt = mediaTypeForExt(path)
		}
		if mt == "" {
			mt = "image/jpeg"
		}
		return data, mt, nil
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
		// 优先按内容嗅探 MIME（对齐 py detect_image_mime_type），其次响应头，
		// 再按扩展名，最后回退 jpeg。
		mediaType := sniffMediaType(data)
		if mediaType == "" {
			mediaType = strings.Split(resp.Header.Get("Content-Type"), ";")[0]
		}
		if mediaType == "" {
			mediaType = mediaTypeForExt(trimmed)
		}
		if mediaType == "" {
			mediaType = "image/jpeg"
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
	if len(payload) > base64.StdEncoding.EncodedLen(maxImageBytes) {
		return nil, "", fmt.Errorf("media exceeds %d bytes", maxImageBytes)
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
	// 未知时不臆断 png，交给调用方用内容嗅探/默认值（对齐 py）。
	return ""
}

// sniffMediaType 按 magic bytes 检测图片 MIME（对齐 py
// _detect_image_mime_type / detect_image_mime_type）：未知返回空串。
func sniffMediaType(data []byte) string {
	if len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n" {
		return "image/png"
	}
	if len(data) >= 2 && data[0] == 0xff && data[1] == 0xd8 {
		return "image/jpeg"
	}
	if len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a") {
		return "image/gif"
	}
	if len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp"
	}
	if ct := http.DetectContentType(data); strings.HasPrefix(ct, "image/") {
		return ct
	}
	return ""
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
		mediaType = "image/jpeg"
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
		mediaType = "image/jpeg"
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
		// 历史回传思考块（对齐 Python anthropic_source._prepare_payload）：从
		// content 的 "think" part 抽取 reasoning_content 与 encrypted 签名，
		// 两者都存在时在 content 首位插入 Anthropic thinking block，否则
		// tool-use 循环第二轮起会缺少 signature 导致 API 400。
		var reasoningContent, thinkingSignature string
		converted := []map[string]interface{}{}
		for _, b := range contentAsBlocks(msg["content"]) {
			if t, _ := b["type"].(string); t == "think" {
				// 只取最后一个 think part（对齐 Python 注释 "only pick the last"）。
				if th, ok := b["think"].(string); ok {
					reasoningContent = th
				}
				if sig, ok := b["encrypted"].(string); ok {
					thinkingSignature = sig
				}
				continue
			}
			converted = append(converted, b)
		}
		content := anthropicContentBlocks(converted)
		if reasoningContent != "" && thinkingSignature != "" {
			content = append([]map[string]interface{}{{
				"type":      "thinking",
				"thinking":  reasoningContent,
				"signature": thinkingSignature,
			}}, content...)
		}
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

// anthropicMergeConsecutiveMessages 合并相邻同角色消息（对齐 Python
// anthropic_source._merge_consecutive_anthropic_messages）。合并 user 消息时
// tool_result 块移到其他块之前，满足 Anthropic 的块顺序要求。
func anthropicMergeConsecutiveMessages(messages []map[string]interface{}) []map[string]interface{} {
	merged := make([]map[string]interface{}, 0, len(messages))
	for _, msg := range messages {
		role, _ := msg["role"].(string)
		if role == "" {
			merged = append(merged, msg)
			continue
		}
		if len(merged) == 0 {
			merged = append(merged, msg)
			continue
		}
		prev := merged[len(merged)-1]
		prevRole, _ := prev["role"].(string)
		if prevRole != role {
			merged = append(merged, msg)
			continue
		}
		// 重新拼装到新切片，避免 append 复用共享历史的底层数组。
		combined := make([]map[string]interface{}, 0, len(prev)+1)
		combined = append(combined, contentAsBlocks(prev["content"])...)
		combined = append(combined, contentAsBlocks(msg["content"])...)
		if role == "user" {
			var results, rest []map[string]interface{}
			for _, b := range combined {
				if t, _ := b["type"].(string); t == "tool_result" {
					results = append(results, b)
				} else {
					rest = append(rest, b)
				}
			}
			if len(results) > 0 {
				combined = append(results, rest...)
			}
		}
		out := map[string]interface{}{"role": role, "content": combined}
		for k, v := range prev {
			if k != "role" && k != "content" {
				out[k] = v
			}
		}
		merged[len(merged)-1] = out
	}
	return merged
}

// anthropicSanitizeMessages 清理 Anthropic 消息中的孤儿工具块（对齐 Python
// anthropic_source._sanitize_assistant_messages）：先合并相邻同角色消息，再
// 丢弃前面没有含对应 tool_use(id) 的 assistant 消息的 tool_result（孤儿
// tool_result 会让 API 400）；孤儿 tool_use 按 Python 语义保留；最后再次合并，
// 使并行多工具的多条 tool_result 落入同一条 user 消息的不同 content 块。
func anthropicSanitizeMessages(messages []map[string]interface{}) []map[string]interface{} {
	merged := anthropicMergeConsecutiveMessages(messages)
	sanitized := make([]map[string]interface{}, 0, len(merged))
	pendingToolUseIDs := map[string]bool{}
	for _, msg := range merged {
		role, _ := msg["role"].(string)
		if role == "assistant" {
			pendingToolUseIDs = map[string]bool{}
			for _, b := range contentAsBlocks(msg["content"]) {
				if t, _ := b["type"].(string); t == "tool_use" {
					if id, _ := b["id"].(string); id != "" {
						pendingToolUseIDs[id] = true
					}
				}
			}
			sanitized = append(sanitized, msg)
			continue
		}
		if role == "user" {
			// 对齐 Python：仅对 list 形态的 content 做块级清理，其他形态原样保留。
			var blocks []map[string]interface{}
			switch c := msg["content"].(type) {
			case []map[string]interface{}:
				blocks = c
			case []interface{}:
				blocks = contentAsBlocks(c)
			default:
				sanitized = append(sanitized, msg)
				pendingToolUseIDs = map[string]bool{}
				continue
			}
			var results, others []map[string]interface{}
			for _, b := range blocks {
				if t, _ := b["type"].(string); t == "tool_result" {
					id, _ := b["tool_use_id"].(string)
					if pendingToolUseIDs[id] {
						results = append(results, b)
						delete(pendingToolUseIDs, id)
					}
					// 孤儿 tool_result：直接丢弃。
					continue
				}
				others = append(others, b)
			}
			cleaned := append(results, others...)
			if len(cleaned) > 0 {
				out := map[string]interface{}{"role": "user", "content": cleaned}
				for k, v := range msg {
					if k != "role" && k != "content" {
						out[k] = v
					}
				}
				sanitized = append(sanitized, out)
			}
			pendingToolUseIDs = map[string]bool{}
			continue
		}
		sanitized = append(sanitized, msg)
		pendingToolUseIDs = map[string]bool{}
	}
	return anthropicMergeConsecutiveMessages(sanitized)
}
