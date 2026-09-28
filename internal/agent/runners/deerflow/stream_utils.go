package deerflow

// 移植 Python deerflow_stream_utils.py。

import (
	"fmt"
	"strings"
)

// extractText 对齐 Python extract_text。
func extractText(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case map[string]interface{}:
		if s, ok := v["text"].(string); ok {
			return s
		}
		if c, ok := v["content"]; ok {
			return extractText(c)
		}
		if kw, ok := v["kwargs"].(map[string]interface{}); ok {
			return extractText(kw["content"])
		}
	case []interface{}:
		parts := []string{}
		for _, item := range v {
			switch it := item.(type) {
			case string:
				parts = append(parts, it)
			case map[string]interface{}:
				if it["type"] == "text" {
					if s, ok := it["text"].(string); ok {
						parts = append(parts, s)
					}
				} else if c, ok := it["content"]; ok {
					parts = append(parts, extractText(c))
				}
			}
		}
		filtered := []string{}
		for _, p := range parts {
			if p != "" {
				filtered = append(filtered, p)
			}
		}
		return strings.TrimSpace(strings.Join(filtered, "\n"))
	}
	if content == nil {
		return ""
	}
	return fmt.Sprint(content)
}

// extractMessagesFromValuesData 对齐 Python extract_messages_from_values_data。
func extractMessagesFromValuesData(data interface{}) []interface{} {
	candidates := []map[string]interface{}{}
	switch v := data.(type) {
	case map[string]interface{}:
		candidates = append(candidates, v)
		if vals, ok := v["values"].(map[string]interface{}); ok {
			candidates = append(candidates, vals)
		}
	case []interface{}:
		for _, x := range v {
			if m, ok := x.(map[string]interface{}); ok {
				candidates = append(candidates, m)
			}
		}
	}
	for _, item := range candidates {
		if msgs, ok := item["messages"].([]interface{}); ok {
			return msgs
		}
	}
	return nil
}

// isAIMessage 对齐 Python is_ai_message。
func isAIMessage(message map[string]interface{}) bool {
	role := strings.ToLower(fmt.Sprint(message["role"]))
	if role == "assistant" || role == "ai" {
		return true
	}
	msgType := strings.ToLower(fmt.Sprint(message["type"]))
	switch msgType {
	case "ai", "assistant", "aimessage", "aimessagechunk":
		return true
	}
	if strings.Contains(msgType, "ai") &&
		!strings.Contains(msgType, "human") &&
		!strings.Contains(msgType, "tool") &&
		!strings.Contains(msgType, "system") {
		return true
	}
	return false
}

// extractLatestAIText 对齐 Python extract_latest_ai_text。
func extractLatestAIText(messages []interface{}) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]interface{})
		if !ok {
			continue
		}
		if isAIMessage(msg) {
			if text := extractText(msg["content"]); text != "" {
				return text
			}
		}
	}
	return ""
}

// extractLatestAIMessage 对齐 Python extract_latest_ai_message。
func extractLatestAIMessage(messages []interface{}) map[string]interface{} {
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]interface{})
		if !ok {
			continue
		}
		if isAIMessage(msg) {
			return msg
		}
	}
	return nil
}

// isClarificationToolMessage 对齐 Python。
func isClarificationToolMessage(message map[string]interface{}) bool {
	msgType := strings.ToLower(fmt.Sprint(message["type"]))
	toolName := strings.ToLower(fmt.Sprint(message["name"]))
	return msgType == "tool" && toolName == "ask_clarification"
}

// extractLatestClarificationText 对齐 Python。
func extractLatestClarificationText(messages []interface{}) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]interface{})
		if !ok {
			continue
		}
		if isClarificationToolMessage(msg) {
			if text := extractText(msg["content"]); text != "" {
				return text
			}
		}
	}
	return ""
}

// getMessageID 对齐 Python get_message_id。
func getMessageID(message interface{}) string {
	m, ok := message.(map[string]interface{})
	if !ok {
		return ""
	}
	if s, ok := m["id"].(string); ok {
		return s
	}
	return ""
}

// extractEventMessageObj 对齐 Python extract_event_message_obj。
func extractEventMessageObj(data interface{}) map[string]interface{} {
	msgObj := data
	if l, ok := data.([]interface{}); ok && len(l) > 0 {
		msgObj = l[0]
	}
	if m, ok := msgObj.(map[string]interface{}); ok {
		if inner, ok := m["data"].(map[string]interface{}); ok {
			msgObj = inner
		}
	}
	if m, ok := msgObj.(map[string]interface{}); ok {
		return m
	}
	return nil
}

// extractAIDeltaFromEventData 对齐 Python。
func extractAIDeltaFromEventData(data interface{}) string {
	msgObj := extractEventMessageObj(data)
	if msgObj == nil {
		return ""
	}
	if isAIMessage(msgObj) {
		return extractText(msgObj["content"])
	}
	return ""
}

// extractClarificationFromEventData 对齐 Python。
func extractClarificationFromEventData(data interface{}) string {
	msgObj := extractEventMessageObj(data)
	if msgObj == nil {
		return ""
	}
	if isClarificationToolMessage(msgObj) {
		return extractText(msgObj["content"])
	}
	return ""
}

// iterCustomEventItems 对齐 Python _iter_custom_event_items。
func iterCustomEventItems(data interface{}) []map[string]interface{} {
	items := []map[string]interface{}{}
	switch v := data.(type) {
	case map[string]interface{}:
		return []map[string]interface{}{v}
	case []interface{}:
		for _, item := range v {
			switch it := item.(type) {
			case map[string]interface{}:
				items = append(items, it)
			case []interface{}:
				for _, nested := range it {
					if m, ok := nested.(map[string]interface{}); ok {
						items = append(items, m)
					}
				}
			}
		}
	}
	return items
}

// extractTaskFailuresFromCustomEvent 对齐 Python。
func extractTaskFailuresFromCustomEvent(data interface{}) []string {
	failures := []string{}
	for _, item := range iterCustomEventItems(data) {
		eventType := strings.ToLower(fmt.Sprint(item["type"]))
		if eventType != "task_failed" && eventType != "task_timed_out" {
			continue
		}
		taskID := strings.TrimSpace(fmt.Sprint(item["task_id"]))
		errorText := strings.TrimSpace(extractText(item["error"]))
		switch {
		case taskID != "" && taskID != "<nil>" && errorText != "":
			failures = append(failures, taskID+": "+errorText)
		case errorText != "":
			failures = append(failures, errorText)
		case taskID != "" && taskID != "<nil>":
			failures = append(failures, taskID+": unknown error")
		default:
			failures = append(failures, "unknown task failure")
		}
	}
	return failures
}

// buildTaskFailureSummary 对齐 Python。
func buildTaskFailureSummary(failures []string) string {
	if len(failures) == 0 {
		return ""
	}
	deduped := []string{}
	seen := map[string]bool{}
	for _, f := range failures {
		if !seen[f] {
			seen[f] = true
			deduped = append(deduped, f)
		}
	}
	if len(deduped) == 1 {
		return "DeerFlow subtask failed: " + deduped[0]
	}
	limit := len(deduped)
	if limit > 5 {
		limit = 5
	}
	lines := []string{}
	for _, d := range deduped[:limit] {
		lines = append(lines, "- "+d)
	}
	return "DeerFlow subtasks failed:\n" + strings.Join(lines, "\n")
}
