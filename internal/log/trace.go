// Package log - trace span support (mirrors astrbot/core/utils/trace.py).
package log

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// TraceSpan records a named span of actions for one agent invocation. The
// WebUI Trace page groups records by span_id (mirrors Python TraceSpan).
type TraceSpan struct {
	SpanID         string
	Name           string
	UMO            string
	SenderName     string
	MessageOutline string

	// enabled reports whether trace recording is on (config trace_enable).
	enabled func() bool
}

// NewTraceSpan creates a span. enabled returns true when trace_enable is set.
func NewTraceSpan(name, umo, senderName, messageOutline string, enabled func() bool) *TraceSpan {
	span := &TraceSpan{
		SpanID:         randomSpanID(),
		Name:           name,
		UMO:            umo,
		SenderName:     senderName,
		MessageOutline: messageOutline,
		enabled:        enabled,
	}
	return span
}

// randomSpanID generates a uuid-style hex span id.
func randomSpanID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	hexStr := hex.EncodeToString(b)
	return hexStr[0:8] + "-" + hexStr[8:12] + "-" + hexStr[12:16] + "-" + hexStr[16:20] + "-" + hexStr[20:32]
}

// Record publishes one trace event (mirrors TraceSpan.record).
func (s *TraceSpan) Record(action string, fields map[string]interface{}) {
	if s.enabled != nil && !s.enabled() {
		return
	}
	payload := map[string]interface{}{
		"type":            "trace",
		"level":           "TRACE",
		"time":            float64(time.Now().UnixMilli()) / 1000.0,
		"span_id":         s.SpanID,
		"name":            s.Name,
		"umo":             s.UMO,
		"sender_name":     s.SenderName,
		"message_outline": s.MessageOutline,
		"action":          action,
	}
	if fields != nil {
		payload["fields"] = fields
	} else {
		payload["fields"] = map[string]interface{}{}
	}
	GetDefault().PublishTrace(payload)
}
