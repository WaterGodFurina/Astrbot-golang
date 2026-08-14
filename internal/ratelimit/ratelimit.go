// Package ratelimit implements a fixed-window rate limiter.
// Ported from astrbot/core/pipeline/rate_limit_check/stage.py
package ratelimit

import (
	"sync"
	"time"
)

// Strategy defines what happens when rate limit is hit.
type Strategy int

const (
	StrategyStall   Strategy = iota // Stall until next window
	StrategyDiscard                 // Discard the request
)

// RateLimiter implements fixed-window rate limiting per session.
type RateLimiter struct {
	mu             sync.Mutex
	timestamps     map[string][]time.Time // session -> request timestamps
	maxRequests    int
	windowDuration time.Duration
	strategy       Strategy
}

// NewRateLimiter creates a rate limiter.
func NewRateLimiter(maxRequests int, window time.Duration, strategy Strategy) *RateLimiter {
	return &RateLimiter{
		timestamps:     make(map[string][]time.Time),
		maxRequests:    maxRequests,
		windowDuration: window,
		strategy:       strategy,
	}
}

// Allow checks if a request is allowed under the rate limit.
// Returns (allowed, stallDuration).
// If strategy is Stall and not allowed, stallDuration > 0.
func (r *RateLimiter) Allow(sessionID string) (bool, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	r.removeExpired(sessionID, now)

	timestamps := r.timestamps[sessionID]

	if len(timestamps) >= r.maxRequests {
		// Rate limited
		if r.strategy == StrategyDiscard {
			return false, 0
		}
		// Stall: calculate time until oldest timestamp expires
		if len(timestamps) > 0 {
			oldest := timestamps[0]
			stallDuration := r.windowDuration - now.Sub(oldest)
			if stallDuration < 0 {
				stallDuration = 0
			}
			return false, stallDuration
		}
	}

	// Allowed: record timestamp
	r.timestamps[sessionID] = append(r.timestamps[sessionID], now)
	return true, 0
}

// Wait blocks until the session is allowed (for Stall strategy).
func (r *RateLimiter) Wait(sessionID string) bool {
	for {
		allowed, stall := r.Allow(sessionID)
		if allowed {
			return true
		}
		if r.strategy == StrategyDiscard {
			return false
		}
		if stall > 0 {
			time.Sleep(stall)
		}
	}
}

// removeExpired removes timestamps outside the window.
func (r *RateLimiter) removeExpired(sessionID string, now time.Time) {
	timestamps := r.timestamps[sessionID]
	if len(timestamps) == 0 {
		return
	}
	threshold := now.Add(-r.windowDuration)
	idx := 0
	for idx < len(timestamps) && timestamps[idx].Before(threshold) {
		idx++
	}
	if idx >= len(timestamps) {
		// Every timestamp expired: drop the map slot entirely so an idle
		// session does not leave an empty-slice entry behind.
		delete(r.timestamps, sessionID)
	} else if idx > 0 {
		r.timestamps[sessionID] = timestamps[idx:]
	}
}

// Reset clears rate limit state for a session.
func (r *RateLimiter) Reset(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.timestamps, sessionID)
}

// ResetAll clears all rate limit state.
func (r *RateLimiter) ResetAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.timestamps = make(map[string][]time.Time)
}
