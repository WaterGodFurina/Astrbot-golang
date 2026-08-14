package log

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPublishTraceSubscriber: subscribers receive the trace entry.
func TestPublishTraceSubscriber(t *testing.T) {
	ch := GetDefault().Subscribe(10)
	defer GetDefault().Unsubscribe(ch)

	GetDefault().PublishTrace(map[string]interface{}{
		"type": "trace", "span_id": "s1", "action": "astr_agent_prepare",
		"fields": map[string]interface{}{"persona_id": "default"},
	})
	select {
	case entry := <-ch:
		if !entry.IsTrace() {
			t.Fatal("entry must be a trace event")
		}
		if entry.Trace["action"] != "astr_agent_prepare" || entry.Trace["span_id"] != "s1" {
			t.Errorf("trace payload: %v", entry.Trace)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not receive trace")
	}
}

// TestPublishTraceHistory: trace entries appear in History().
func TestPublishTraceHistory(t *testing.T) {
	before := len(GetDefault().History())
	GetDefault().PublishTrace(map[string]interface{}{"type": "trace", "span_id": "h1", "action": "agent_tool_call"})
	found := false
	for _, e := range GetDefault().History() {
		if e.IsTrace() && e.Trace["span_id"] == "h1" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("trace entry not in history (before=%d)", before)
	}
}

// TestTraceSpanRecord: enabled respects the flag.
func TestTraceSpanRecord(t *testing.T) {
	ch := GetDefault().Subscribe(10)
	defer GetDefault().Unsubscribe(ch)

	enabled := true
	span := NewTraceSpan("main", "telegram:g1", "user", "hello", func() bool { return enabled })
	span.Record("agent_tool_call", map[string]interface{}{"tool_name": "web_fetch"})
	select {
	case entry := <-ch:
		if !entry.IsTrace() || entry.Trace["action"] != "agent_tool_call" {
			t.Errorf("trace: %v", entry.Trace)
		}
		if entry.Trace["name"] != "main" || entry.Trace["sender_name"] != "user" {
			t.Errorf("span fields: %v", entry.Trace)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no trace received")
	}

	// Disabled: no publish.
	enabled = false
	span.Record("agent_tool_result", map[string]interface{}{})
	select {
	case <-ch:
		t.Error("disabled trace must not publish")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestSpanIDUnique: span ids are unique.
func TestSpanIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := randomSpanID()
		if seen[id] {
			t.Fatal("duplicate span id")
		}
		seen[id] = true
	}
}

// TestConcurrentPublish: concurrent publishes do not panic.
func TestConcurrentPublish(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			GetDefault().PublishTrace(map[string]interface{}{"type": "trace", "span_id": "c", "action": "x"})
		}(i)
	}
	wg.Wait()
}

// TestLogHistorySurvivesTraceBytes is a regression test for M-36: trace
// entries are charged to historyBytes by their serialized payload length, so
// eviction must subtract the same amount. Previously a log() call evicted
// trace entries at zero bytes, inflating historyBytes until every log call
// flushed the entire history.
func TestLogHistorySurvivesTraceBytes(t *testing.T) {
	l := &Logger{out: io.Discard, history: []LogEntry{}}

	// Large trace payloads so the serialized size dwarfs any message length.
	big := strings.Repeat("x", 64<<10) // 64 KiB
	for i := 0; i < 40; i++ {
		l.PublishTrace(map[string]interface{}{"type": "trace", "span_id": "big", "fields": big})
	}

	l.Info("hello after trace flood")

	hist := l.History()
	if len(hist) == 0 {
		t.Fatal("history was emptied by trace-accounting drift")
	}
	found := false
	for _, e := range hist {
		if !e.IsTrace() && e.Message == "hello after trace flood" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("the log entry itself was evicted by trace-accounting drift")
	}
}
