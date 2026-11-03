// Package tracestore implements an independent, bounded SQLite store for local
// trace history. Ported in spirit from astrbot/core/utils/trace_store.py
// (4.29.2): a single background writer with bounded, nonblocking admission so
// trace persistence never blocks message processing.
package tracestore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// TraceStore persists trace records to a separate SQLite database.
type TraceStore struct {
	db   *sql.DB
	path string

	ch      chan map[string]interface{}
	dropped atomic.Int64
	closed  bool

	mu   sync.Mutex
	wg   sync.WaitGroup
	done chan struct{}
}

// Open opens (or creates) the trace database at path and starts the writer.
func Open(path string) (*TraceStore, error) {
	dsn := fmt.Sprintf("%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(30000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open trace store: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &TraceStore{
		db:   db,
		path: path,
		ch:   make(chan map[string]interface{}, 4096),
		done: make(chan struct{}),
	}
	if err := s.initSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	s.wg.Add(1)
	go s.drain()
	return s, nil
}

func (s *TraceStore) initSchema() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS traces (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			time REAL NOT NULL,
			span_id TEXT,
			name TEXT,
			umo TEXT,
			sender_name TEXT,
			message_outline TEXT,
			action TEXT,
			fields TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS traces_time ON traces(time DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS traces_span ON traces(span_id)`,
	}
	for _, stmt := range statements {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("trace store schema: %w", err)
		}
	}
	return nil
}

// Append queues an immutable record snapshot without blocking. It returns false
// when the queue is full and the record was dropped.
func (s *TraceStore) Append(payload map[string]interface{}) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	select {
	case s.ch <- payload:
		s.mu.Unlock()
		return true
	default:
		s.dropped.Add(1)
		s.mu.Unlock()
		return false
	}
}

// drain writes queued records in small batches until the queue is closed.
func (s *TraceStore) drain() {
	defer s.wg.Done()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	batch := make([]map[string]interface{}, 0, 128)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.writeBatch(batch); err != nil {
			// Persisting failures must not propagate into the message pipeline.
			_ = err
		}
		batch = batch[:0]
	}

	for {
		select {
		case rec, ok := <-s.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, rec)
			if len(batch) >= 128 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (s *TraceStore) writeBatch(batch []map[string]interface{}) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(
		`INSERT INTO traces(time, span_id, name, umo, sender_name, message_outline, action, fields)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, rec := range batch {
		fieldsJSON := "{}"
		if f, ok := rec["fields"]; ok && f != nil {
			if raw, err := json.Marshal(f); err == nil {
				fieldsJSON = string(raw)
			}
		}
		t, _ := rec["time"].(float64)
		_, err := stmt.Exec(
			t,
			str(rec["span_id"]),
			str(rec["name"]),
			str(rec["umo"]),
			str(rec["sender_name"]),
			str(rec["message_outline"]),
			str(rec["action"]),
			fieldsJSON,
		)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// Flush waits up to timeout for queued records to be written.
func (s *TraceStore) Flush(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(s.ch) == 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return len(s.ch) == 0
}

// Close flushes pending records and closes the database.
func (s *TraceStore) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.ch)
	s.mu.Unlock()
	s.wg.Wait()
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	_ = s.db.Close()
}

// Path returns the database file path.
func (s *TraceStore) Path() string { return s.path }

// Dropped returns the number of records dropped because the queue was full.
func (s *TraceStore) Dropped() int64 { return s.dropped.Load() }

// ClearHistory deletes all stored trace records and returns how many were removed.
func (s *TraceStore) ClearHistory() (int, error) {
	// Drain pending writes first so the count reflects persisted records.
	s.Flush(2 * time.Second)
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM traces").Scan(&count); err != nil {
		return 0, err
	}
	if _, err := s.db.Exec("DELETE FROM traces"); err != nil {
		return 0, err
	}
	_, _ = s.db.Exec("VACUUM")
	return count, nil
}

// TraceRow is one stored trace record.
type TraceRow struct {
	ID             int64                  `json:"id"`
	Time           float64                `json:"time"`
	SpanID         string                 `json:"span_id"`
	Name           string                 `json:"name"`
	UMO            string                 `json:"umo"`
	SenderName     string                 `json:"sender_name"`
	MessageOutline string                 `json:"message_outline"`
	Action         string                 `json:"action"`
	Fields         map[string]interface{} `json:"fields"`
}

// ListTraces returns recent trace records ordered newest first.
func (s *TraceStore) ListTraces(limit, offset int) ([]TraceRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.Query(
		`SELECT id, time, span_id, name, umo, sender_name, message_outline, action, fields
		 FROM traces ORDER BY time DESC, id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTraceRows(rows)
}

// GetTrace returns all records for a span id, oldest first.
func (s *TraceStore) GetTrace(spanID string) ([]TraceRow, error) {
	rows, err := s.db.Query(
		`SELECT id, time, span_id, name, umo, sender_name, message_outline, action, fields
		 FROM traces WHERE span_id = ? ORDER BY time, id`, spanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTraceRows(rows)
}

func scanTraceRows(rows *sql.Rows) ([]TraceRow, error) {
	var out []TraceRow
	for rows.Next() {
		var r TraceRow
		var fieldsJSON string
		if err := rows.Scan(&r.ID, &r.Time, &r.SpanID, &r.Name, &r.UMO, &r.SenderName,
			&r.MessageOutline, &r.Action, &fieldsJSON); err != nil {
			return nil, err
		}
		r.Fields = map[string]interface{}{}
		if fieldsJSON != "" {
			_ = json.Unmarshal([]byte(fieldsJSON), &r.Fields)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func str(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}
