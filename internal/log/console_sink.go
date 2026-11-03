// Package log - bounded console sink.
// Ported from astrbot/core/utils/console_sink.py BoundedConsoleSink (4.29.4)
package log

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// BoundedConsoleSink writes console records on one background goroutine with
// bounded, nonblocking admission. When the queue is full the console copy of a
// record is dropped (the record itself is never lost elsewhere) and the drops
// are reported once output recovers. This keeps slow console IO from blocking
// the application's hot path (aligned with py BoundedConsoleSink).
type BoundedConsoleSink struct {
	stream   io.Writer
	capacity int
	ch       chan string
	dropped  atomic.Int64
	closed   bool

	mu   sync.Mutex
	done chan struct{}
}

// NewBoundedConsoleSink starts a console writer around stream. capacity is the
// maximum number of pending records (<=0 falls back to 512).
func NewBoundedConsoleSink(stream io.Writer, capacity int) *BoundedConsoleSink {
	if capacity <= 0 {
		capacity = 512
	}
	s := &BoundedConsoleSink{
		stream:   stream,
		capacity: capacity,
		ch:       make(chan string, capacity),
		done:     make(chan struct{}),
	}
	go s.drain()
	return s
}

// Write queues a record, dropping only its console copy when full. It never
// blocks and never returns an error so callers (fmt.Fprint) proceed normally.
func (s *BoundedConsoleSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return len(p), nil
	}
	select {
	case s.ch <- string(p):
	default:
		s.dropped.Add(1)
	}
	s.mu.Unlock()
	return len(p), nil
}

// drain isolates stream IO and reports dropped records after output recovers.
func (s *BoundedConsoleSink) drain() {
	defer close(s.done)
	var reportedDropped int64
	for msg := range s.ch {
		dropped := s.dropped.Load()
		if dropped > reportedDropped {
			msg += fmt.Sprintf("[AstrBot] Skipped %d console log records because the console queue was full.\n", dropped-reportedDropped)
			reportedDropped = dropped
		}
		_, _ = io.WriteString(s.stream, msg)
	}
}

// Drain waits for queued write attempts within a fixed budget. Returns whether
// all queued records finished (regardless of IO errors).
func (s *BoundedConsoleSink) Drain(timeout time.Duration) bool {
	if timeout <= 0 {
		return len(s.ch) == 0
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.done:
		return true
	case <-timer.C:
		return len(s.ch) == 0
	}
}

// GetStatus snapshots process-lifetime counters without waiting on console IO.
func (s *BoundedConsoleSink) GetStatus() map[string]interface{} {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	return map[string]interface{}{
		"capacity":        s.capacity,
		"queued_records":  len(s.ch),
		"dropped_records": s.dropped.Load(),
		"closed":          closed,
	}
}

// Close allows a short drain at exit, without waiting on an unread console.
// It is safe to call multiple times.
func (s *BoundedConsoleSink) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	// Closing the channel under the lock lets the drain goroutine emit any
	// buffered records before exiting; concurrent Write calls observe closed
	// and never send on the closed channel.
	close(s.ch)
	s.mu.Unlock()

	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-s.done:
	case <-timer.C:
	}
}
