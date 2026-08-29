package sources

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
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

// TestSSEReaderStopsOnContextCancel verifies the scan loop honors context
// cancellation at token boundaries: cancelling mid-stream stops the scan at
// the next boundary without consuming the remaining tokens.
func TestSSEReaderStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events []string
	reader := newSSEReader(ctx, &http.Response{
		Body: io.NopCloser(strings.NewReader("data: one\n\ndata: two\n\ndata: three\n\n")),
	}, func(data string) bool {
		events = append(events, data)
		cancel()
		return false
	})
	if err := reader.scan(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(events) != 1 || events[0] != "one" {
		t.Fatalf("expected scan to stop at the boundary after cancel, got %v", events)
	}
}

// TestSSEReaderBlockedOnReadHonorsCancel verifies cancellation is observed
// even when the scanner is blocked waiting for the next token on a live
// connection: the scan must stop at the next token boundary (the blocked read
// itself cannot be interrupted, but the loop must not continue past it).
func TestSSEReaderBlockedOnReadHonorsCancel(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// events 由 scan goroutine 的 onData 回调追加，测试主 goroutine 在轮询与
	// 最终断言中读取，需互斥锁同步（race detector 曾在此报 DATA RACE）。
	var (
		events []string
		mu     sync.Mutex
	)
	eventsSnapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), events...)
	}
	reader := newSSEReader(ctx, &http.Response{Body: pr}, func(data string) bool {
		mu.Lock()
		events = append(events, data)
		mu.Unlock()
		return false
	})

	done := make(chan error, 1)
	go func() { done <- reader.scan() }()

	// Feed one event and wait until it has been dispatched, so the scanner is
	// guaranteed to be blocked in Scan() on the still-open pipe.
	if _, err := pw.Write([]byte("data: one\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(eventsSnapshot()) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("first event was not dispatched")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	// Deliver the next token; scan must observe the cancellation at this
	// boundary and stop without dispatching it.
	if _, err := pw.Write([]byte("data: two\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scan did not stop after context cancellation at token boundary")
	}
	got := eventsSnapshot()
	if len(got) != 1 || got[0] != "one" {
		t.Fatalf("expected only the pre-cancel event, got %v", got)
	}
}

// TestSSEReaderCancelBeforeStart verifies a pre-cancelled context stops the
// scan immediately without reading the stream.
func TestSSEReaderCancelBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := newSSEReader(ctx, &http.Response{
		Body: io.NopCloser(strings.NewReader("data: one\n\n")),
	}, func(data string) bool {
		t.Fatal("onData must not be called for a pre-cancelled context")
		return false
	})
	if err := reader.scan(); err != nil {
		t.Fatalf("scan: %v", err)
	}
}
