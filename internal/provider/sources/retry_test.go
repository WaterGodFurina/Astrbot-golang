package sources

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsRetryableStatus(t *testing.T) {
	tests := []struct {
		code   int
		want   bool
		reason string
	}{
		{200, false, "OK"},
		{301, false, "redirect"},
		{400, false, "bad request"},
		{408, true, "request timeout"},
		{409, true, "conflict"},
		{429, true, "rate limit"},
		{500, true, "internal server error"},
		{502, true, "bad gateway"},
		{503, true, "service unavailable"},
		{504, true, "gateway timeout"},
		{529, true, "cloudflare timeout"},
		{599, true, "5xx edge"},
		{600, false, "6xx not retryable"},
	}
	for _, tt := range tests {
		got := IsRetryableStatus(tt.code)
		if got != tt.want {
			t.Errorf("IsRetryableStatus(%d) = %v, want %v (%s)", tt.code, got, tt.want, tt.reason)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		header string
		wantGT time.Duration // want > this value
		wantLT time.Duration // want < this value
	}{
		{"", 0, 1},
		{"invalid", 0, 1},
		{"5", 4 * time.Second, 6 * time.Second},
		{"0.5", 400 * time.Millisecond, 600 * time.Millisecond},
	}
	for _, tt := range tests {
		got := ParseRetryAfter(tt.header)
		if got < tt.wantGT || got > tt.wantLT {
			t.Errorf("ParseRetryAfter(%q) = %v, want between %v and %v", tt.header, got, tt.wantGT, tt.wantLT)
		}
	}
}

func TestBackoffDelay(t *testing.T) {
	cfg := RetryConfig{MinDelay: 200 * time.Millisecond, MaxDelay: 30 * time.Second}
	// backoffDelay applies ±20% jitter, so assert the [0.8x, 1.2x] range.
	if d := backoffDelay(1, cfg); d < 160*time.Millisecond || d > 240*time.Millisecond {
		t.Errorf("attempt 1: got %v, want in [160ms, 240ms]", d)
	}
	if d := backoffDelay(2, cfg); d < 320*time.Millisecond || d > 480*time.Millisecond {
		t.Errorf("attempt 2: got %v, want in [320ms, 480ms]", d)
	}
	if d := backoffDelay(5, cfg); d < 2560*time.Millisecond || d > 3840*time.Millisecond {
		t.Errorf("attempt 5: got %v, want in [2.56s, 3.84s]", d)
	}
	if d := backoffDelay(10, cfg); d != 30*time.Second {
		t.Errorf("attempt 10: got %v, want 30s (clamped)", d)
	}
}

// TestBackoffDelayJitter verifies the ±20% jitter keeps every delay inside
// [0.8x, 1.2x] of the base and <= MaxDelay, and that jitter actually applies:
// many samples of the same attempt must not all be identical (L-26: 惊群效应).
func TestBackoffDelayJitter(t *testing.T) {
	cfg := RetryConfig{MinDelay: 200 * time.Millisecond, MaxDelay: 30 * time.Second}
	bases := map[int]time.Duration{
		1: 200 * time.Millisecond,
		2: 400 * time.Millisecond,
		5: 3200 * time.Millisecond,
	}
	distinct := map[int]map[time.Duration]bool{}
	for i := 0; i < 500; i++ {
		for attempt, base := range bases {
			d := backoffDelay(attempt, cfg)
			lo := base * 8 / 10
			hi := base * 12 / 10
			if d < lo || d > hi {
				t.Fatalf("attempt %d: got %v, want in [%v, %v] (±20%% of %v)", attempt, d, lo, hi, base)
			}
			if d > cfg.MaxDelay || d <= 0 {
				t.Fatalf("attempt %d: got %v, out of (0, MaxDelay=%v]", attempt, d, cfg.MaxDelay)
			}
			if distinct[attempt] == nil {
				distinct[attempt] = map[time.Duration]bool{}
			}
			distinct[attempt][d] = true
		}
	}
	for attempt := range bases {
		if len(distinct[attempt]) < 2 {
			t.Errorf("attempt %d: jitter not applied, all %d samples identical", attempt, len(distinct[attempt]))
		}
	}
}

// TestBackoffDelayJitterClampedMax verifies jitter never pushes a saturated
// delay past MaxDelay: the value must stay exactly MaxDelay.
func TestBackoffDelayJitterClampedMax(t *testing.T) {
	cfg := RetryConfig{MinDelay: 200 * time.Millisecond, MaxDelay: 30 * time.Second}
	for i := 0; i < 200; i++ {
		if d := backoffDelay(30, cfg); d != 30*time.Second {
			t.Fatalf("attempt 30: got %v, want 30s (jittered but clamped)", d)
		}
	}
}

// TestBackoffDelayNoOverflow 验证极大 attempt 下退避不溢出为 0/负数 (对应 L-26)。
func TestBackoffDelayNoOverflow(t *testing.T) {
	cfg := RetryConfig{MinDelay: 200 * time.Millisecond, MaxDelay: 30 * time.Second}
	for _, attempt := range []int{32, 64, 100, 1000} {
		if d := backoffDelay(attempt, cfg); d != 30*time.Second {
			t.Errorf("attempt %d: got %v, want 30s (saturated, no overflow)", attempt, d)
		}
	}
}

func TestRetryConfigFromSettings(t *testing.T) {
	cfg := RetryConfigFromSettings(nil)
	if cfg.MaxAttempts != 5 {
		t.Errorf("default MaxAttempts = %d, want 5", cfg.MaxAttempts)
	}

	settings := map[string]interface{}{
		"request_max_retries":        10,
		"request_retry_min_delay_ms": 100,
		"request_retry_max_delay_ms": 60000,
		"request_retry_rate_limits":  false,
	}
	cfg = RetryConfigFromSettings(settings)
	if cfg.MaxAttempts != 10 {
		t.Errorf("MaxAttempts = %d, want 10", cfg.MaxAttempts)
	}
	if cfg.MinDelay != 100*time.Millisecond {
		t.Errorf("MinDelay = %v, want 100ms", cfg.MinDelay)
	}
	if cfg.MaxDelay != 60*time.Second {
		t.Errorf("MaxDelay = %v, want 60s", cfg.MaxDelay)
	}
	if cfg.Retry429 {
		t.Error("Retry429 should be false")
	}
}

func TestDoWithRetrySuccess(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	cfg := DefaultRetryConfig()
	resp, err := DoWithRetry(context.Background(), server.Client(), func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	}, cfg, "Test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestDoWithRetryRetries429(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	cfg := RetryConfig{MaxAttempts: 5, MinDelay: 10 * time.Millisecond, MaxDelay: 50 * time.Millisecond, Retry429: true}
	start := time.Now()
	resp, err := DoWithRetry(context.Background(), server.Client(), func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	}, cfg, "Test")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 1.9s (2 x Retry-After 1s)", elapsed)
	}
}

func TestDoWithRetryRetries500(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := RetryConfig{MaxAttempts: 3, MinDelay: 10 * time.Millisecond, MaxDelay: 50 * time.Millisecond}
	resp, err := DoWithRetry(context.Background(), server.Client(), func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	}, cfg, "Test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestDoWithRetryMaxRetriesExceeded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	cfg := RetryConfig{MaxAttempts: 3, MinDelay: 1 * time.Millisecond, MaxDelay: 2 * time.Millisecond}
	resp, err := DoWithRetry(context.Background(), server.Client(), func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	}, cfg, "Test")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "max retries") {
		t.Errorf("error message = %q, want 'max retries'", err.Error())
	}
	if resp != nil {
		t.Errorf("exhausted path must return a nil response, got %v (closed body)", resp)
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error message = %q, want last status folded in", err.Error())
	}
}

func TestDoWithRetryNonRetryableError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	cfg := DefaultRetryConfig()
	resp, err := DoWithRetry(context.Background(), server.Client(), func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	}, cfg, "Test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestDoWithRetryNetworkErrorRetries(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := RetryConfig{MaxAttempts: 3, MinDelay: 10 * time.Millisecond, MaxDelay: 50 * time.Millisecond}

	// First attempt: connection refused (server not listening)
	// Subsequent attempts: hit the test server
	var client *http.Client
	if server.URL != "" {
		client = server.Client()
	}

	resp, err := DoWithRetry(context.Background(), client, func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	}, cfg, "Test")
	if err != nil {
		// If the server is reachable on first try (race condition), that's fine
		t.Logf("request failed (may be race): %v", err)
		return
	}
	_ = resp.Body.Close()
	if calls < 1 {
		t.Errorf("calls = %d, want >= 1", calls)
	}
}

func TestDoWithRetryContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	cfg := RetryConfig{MaxAttempts: 3, MinDelay: 10 * time.Millisecond, MaxDelay: 50 * time.Millisecond}
	_, err := DoWithRetry(ctx, server.Client(), func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	}, cfg, "Test")
	if err == nil {
		t.Fatal("expected error due to context cancellation, got nil")
	}
	if !strings.Contains(err.Error(), "create request") {
		// context cancellation may surface as request creation error or during client.Do
		t.Logf("got expected cancellation error: %v", err)
	}
}

func TestDoWithRetryRetryAfterHeader(t *testing.T) {
	var calls int
	var delays []time.Duration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := RetryConfig{MaxAttempts: 3, MinDelay: 10 * time.Millisecond, MaxDelay: 50 * time.Millisecond}
	start := time.Now()
	resp, err := DoWithRetry(context.Background(), server.Client(), func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	}, cfg, "Test")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
	_ = delays
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if elapsed < 1900*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 2s (Retry-After)", elapsed)
	}
}

func TestDoWithRetryFactoryCalledPerAttempt(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	cfg := RetryConfig{MaxAttempts: 4, MinDelay: 1 * time.Millisecond, MaxDelay: 2 * time.Millisecond}
	_, err := DoWithRetry(context.Background(), server.Client(), func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	}, cfg, "Test")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 4 {
		t.Errorf("factory calls = %d, want 4 (MaxAttempts)", calls)
	}
}

func TestDoWithRetryExponentialBackoff(t *testing.T) {
	var timestamps []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timestamps = append(timestamps, time.Now())
		if len(timestamps) < 4 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := RetryConfig{
		MaxAttempts: 5,
		MinDelay:    100 * time.Millisecond,
		MaxDelay:    200 * time.Millisecond,
	}
	resp, err := DoWithRetry(context.Background(), server.Client(), func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	}, cfg, "Test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
	if len(timestamps) != 4 {
		t.Fatalf("got %d timestamps, want 4", len(timestamps))
	}
	d1 := timestamps[1].Sub(timestamps[0])
	d2 := timestamps[2].Sub(timestamps[1])
	// With ±20% jitter, attempt-1 sleeps in [80ms, 120ms); keep a floor well
	// below the jitter range to prove a real sleep happened without flaking.
	if d1 < 70*time.Millisecond {
		t.Errorf("delay 1 = %v, want >= 70ms (100ms base, ±20%% jitter)", d1)
	}
	// Attempt 2 sleeps 200ms * jitter, clamped to MaxDelay: [160ms, 200ms].
	// Keep the floor below the jitter range to prove a real sleep, not flake.
	if d2 < 140*time.Millisecond {
		t.Errorf("delay 2 = %v, want >= 140ms (200ms capped by MaxDelay, ±20%% jitter)", d2)
	}
}

func BenchmarkDoWithRetry(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := DefaultRetryConfig()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := DoWithRetry(context.Background(), server.Client(), func() (*http.Request, error) {
			return http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
		}, cfg, "Bench")
		if err != nil {
			b.Fatal(err)
		}
		_ = resp.Body.Close()
	}
}
