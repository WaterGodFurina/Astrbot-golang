package sources

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// collectSSEData scans an SSE body and returns the dispatched events.
func collectSSEData(t *testing.T, body string) ([]string, error) {
	t.Helper()
	var events []string
	reader := newSSEReader(context.Background(), &http.Response{
		Body: io.NopCloser(strings.NewReader(body)),
	}, func(data string) bool {
		events = append(events, data)
		return false
	})
	err := reader.scan()
	return events, err
}

// TestSSEReaderJoinsMultiLineData verifies that `data:` lines of one event are
// joined with a newline and dispatched once, per the SSE spec (L-46.2d).
func TestSSEReaderJoinsMultiLineData(t *testing.T) {
	body := "event: message\r\ndata: {\"a\": 1,\r\ndata: \"b\": 2}\r\n\r\n" +
		": keepalive comment\r\n" +
		"data: second\r\n\r\n" +
		"data: last"
	events, err := collectSSEData(t, body)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d: %v", len(events), events)
	}
	if events[0] != "{\"a\": 1,\n\"b\": 2}" {
		t.Errorf("multi-line data not joined with newline: %q", events[0])
	}
	if events[1] != "second" {
		t.Errorf("second event = %q, want second", events[1])
	}
	if events[2] != "last" {
		t.Errorf("trailing event without blank line lost: %q", events[2])
	}
}

// TestSSEReaderSingleLineCompatibility verifies the common single-line event
// shape used by all streaming providers still dispatches one payload per event.
func TestSSEReaderSingleLineCompatibility(t *testing.T) {
	body := "data: {\"choices\":[]}\n\n" +
		"data: {\"choices\":[]}\n\n" +
		"data: [DONE]\n\n"
	events, err := collectSSEData(t, body)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events before [DONE], got %d: %v", len(events), events)
	}
}

// TestSSEReaderStopsOnData verifies onData returning true stops the scan.
func TestSSEReaderStopsOnData(t *testing.T) {
	var events []string
	reader := newSSEReader(context.Background(), &http.Response{
		Body: io.NopCloser(strings.NewReader("data: one\n\ndata: two\n\n")),
	}, func(data string) bool {
		events = append(events, data)
		return data == "one"
	})
	if err := reader.scan(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(events) != 1 || events[0] != "one" {
		t.Fatalf("expected scan to stop after 'one', got %v", events)
	}
}
