package ratelimit

import (
	"testing"
	"time"
)

// TestRemoveExpiredDeletesEmptySlot verifies that once all of a session's
// timestamps fall outside the window, the map slot is deleted instead of
// leaving an empty slice behind.
func TestRemoveExpiredDeletesEmptySlot(t *testing.T) {
	r := NewRateLimiter(10, time.Minute, StrategyStall)
	r.mu.Lock()
	r.timestamps["s1"] = []time.Time{time.Now().Add(-2 * time.Hour), time.Now().Add(-time.Hour)}
	r.removeExpired("s1", time.Now())
	_, exists := r.timestamps["s1"]
	r.mu.Unlock()

	if exists {
		t.Error("session slot with fully-expired timestamps was not deleted")
	}
}

// TestRemoveExpiredKeepsLiveTimestamps verifies that timestamps still inside
// the window are retained (and the leading expired ones trimmed).
func TestRemoveExpiredKeepsLiveTimestamps(t *testing.T) {
	r := NewRateLimiter(10, time.Minute, StrategyStall)
	now := time.Now()
	r.mu.Lock()
	r.timestamps["s2"] = []time.Time{now.Add(-2 * time.Hour), now.Add(-time.Second), now}
	r.removeExpired("s2", now)
	ts := r.timestamps["s2"]
	r.mu.Unlock()

	if len(ts) != 2 {
		t.Fatalf("expected 2 live timestamps, got %d", len(ts))
	}
}
