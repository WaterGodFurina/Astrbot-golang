// Package log - bounded telemetry values.
// Ported from astrbot/core/utils/telemetry.py (4.29.2)
package log

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// MaxAttributeBytes / MaxBodyBytes bound telemetry text and attribute budgets.
const (
	MaxAttributeBytes = 16 * 1024
	MaxBodyBytes      = 16 * 1024
)

// BoundedText bounds text by UTF-8 bytes without splitting a character.
// Aligned with py telemetry.bounded_text.
func BoundedText(value string, limit int) string {
	if limit < 0 {
		limit = 0
	}
	if len(value) <= limit {
		return value
	}
	// Trim to the last valid rune boundary at or below limit-16, then append a
	// truncation marker.
	cut := limit - 16
	if cut < 0 {
		cut = 0
	}
	if cut > len(value) {
		cut = len(value)
	}
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut] + " [truncated]"
}

// BoundedAttributes converts application fields to bounded attribute values.
// Nested objects become JSON strings; a shared byte budget bounds wide and deep
// inputs. Aligned with py telemetry.bounded_attributes.
func BoundedAttributes(fields map[string]interface{}, maxBytes int) map[string]interface{} {
	if maxBytes <= 0 {
		maxBytes = MaxAttributeBytes
	}
	remaining := maxBytes - 64
	truncated := false
	result := map[string]interface{}{}

	for rawKey, value := range fields {
		key := BoundedText(fmt.Sprint(rawKey), 128)
		if value == nil {
			continue
		}
		switch v := value.(type) {
		case string:
			original := v
			v = BoundedText(v, maxInt(16, remaining-len(key)-16))
			if v != original {
				truncated = true
			}
			value = v
		case bool, int, int64, float64:
			// primitives pass through
		case []interface{}:
			if len(v) <= 128 && uniformPrimitiveSlice(v) {
				orig := make([]interface{}, len(v))
				copy(orig, v)
				for i, item := range v {
					if s, ok := item.(string); ok {
						v[i] = BoundedText(s, 256)
					}
				}
				if !equalSlices(orig, v) {
					truncated = true
				}
				value = v
			} else {
				compacted, wasTruncated := compactForTelemetry(value, 0)
				if wasTruncated {
					truncated = true
				}
				encoded, err := json.Marshal(compacted)
				if err != nil {
					truncated = true
					continue
				}
				value = string(encoded)
			}
		default:
			compacted, wasTruncated := compactForTelemetry(value, 0)
			if wasTruncated {
				truncated = true
			}
			encoded, err := json.Marshal(compacted)
			if err != nil {
				truncated = true
				continue
			}
			value = string(encoded)
		}

		if s, ok := value.(string); ok {
			original := s
			s = BoundedText(s, maxInt(16, remaining-len(key)-16))
			if s != original {
				truncated = true
			}
			value = s
		}

		cost, err := json.Marshal(map[string]interface{}{key: value})
		if err != nil {
			continue
		}
		if len(cost) > remaining {
			result["astrbot.truncated"] = true
			break
		}
		result[key] = value
		remaining -= len(cost)
	}
	if truncated {
		result["astrbot.truncated"] = true
	}
	return result
}

// compactForTelemetry bounds nested values before serialization. The second
// return reports whether anything was truncated.
func compactForTelemetry(item interface{}, depth int) (interface{}, bool) {
	switch v := item.(type) {
	case string:
		bounded := BoundedText(v, 2048)
		return bounded, bounded != v
	case map[string]interface{}:
		truncated := len(v) > 16
		out := map[string]interface{}{}
		n := 0
		for k, val := range v {
			if n >= 16 {
				break
			}
			ck := BoundedText(k, 128)
			cv, t := compactForTelemetry(val, depth+1)
			truncated = truncated || t
			out[ck] = cv
			n++
		}
		return out, truncated
	case []interface{}:
		truncated := len(v) > 16
		out := make([]interface{}, 0, minInt(len(v), 16))
		for i, val := range v {
			if i >= 16 {
				break
			}
			cv, t := compactForTelemetry(val, depth+1)
			truncated = truncated || t
			out = append(out, cv)
		}
		return out, truncated
	}
	if depth >= 3 {
		return BoundedText(fmt.Sprintf("%T", item), 128), true
	}
	switch item.(type) {
	case bool, int, int64, float64, nil:
		return item, false
	}
	bounded := BoundedText(fmt.Sprint(item), 2048)
	return bounded, true
}

// uniformPrimitiveSlice reports whether every element is a string/bool/number
// of the same type (aligned with py's homogeneous primitive list check).
func uniformPrimitiveSlice(v []interface{}) bool {
	if len(v) == 0 {
		return true
	}
	var firstType string
	for i, item := range v {
		var t string
		switch item.(type) {
		case string:
			t = "string"
		case bool:
			t = "bool"
		case int, int64, float64:
			t = "number"
		default:
			return false
		}
		if i == 0 {
			firstType = t
		} else if t != firstType {
			return false
		}
	}
	return true
}

func equalSlices(a, b []interface{}) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
