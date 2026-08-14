// Package sources - HTTP request retry with exponential backoff.
// Ported from astrbot/core/provider/sources/request_retry.py
package sources

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

var retryLogger = log.GetDefault().WithComponent("ProviderRetry")

// RetryableStatusCodes are HTTP status codes that trigger a retry.
// Ported from REQUEST_RETRY_STATUS_CODES in request_retry.py
var RetryableStatusCodes = map[int]struct{}{
	http.StatusRequestTimeout:      {}, // 408
	http.StatusConflict:            {}, // 409
	http.StatusTooManyRequests:     {}, // 429
	http.StatusInternalServerError: {}, // 500
	http.StatusBadGateway:          {}, // 502
	http.StatusServiceUnavailable:  {}, // 503
	http.StatusGatewayTimeout:      {}, // 504
	529:                            {}, // 529 - Network Connect Timeout Error
}

// IsRetryableStatus reports whether an HTTP status code should trigger a retry.
// Any 5xx is retryable; the above 4xx codes are explicitly retryable.
func IsRetryableStatus(code int) bool {
	if _, ok := RetryableStatusCodes[code]; ok {
		return true
	}
	return code >= 500 && code < 600
}

// RetryConfig controls retry behavior.
// Ported from REQUEST_RETRY_ATTEMPTS / wait_exponential in request_retry.py
type RetryConfig struct {
	MaxAttempts int
	MinDelay    time.Duration
	MaxDelay    time.Duration
	Retry429    bool // whether 429 rate-limit counts as retryable
}

// DefaultRetryConfig returns the default retry configuration.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts: 5,
		MinDelay:    200 * time.Millisecond,
		MaxDelay:    30 * time.Second,
		Retry429:    true,
	}
}

// RetryConfigFromSettings reads retry configuration from provider_settings.
// Keys (matching Python's config names):
//   - request_max_retries (default 5, min 1)
//   - request_retry_min_delay_ms (default 200)
//   - request_retry_max_delay_ms (default 30000)
//   - request_retry_rate_limits (default true; set false to skip 429)
func RetryConfigFromSettings(settings map[string]interface{}) RetryConfig {
	cfg := DefaultRetryConfig()
	if settings == nil {
		return cfg
	}
	if v := configInt(settings, "request_max_retries", 0); v > 0 {
		cfg.MaxAttempts = v
	}
	if v := configInt(settings, "request_retry_min_delay_ms", 0); v > 0 {
		cfg.MinDelay = time.Duration(v) * time.Millisecond
	}
	if v := configInt(settings, "request_retry_max_delay_ms", 0); v > 0 {
		cfg.MaxDelay = time.Duration(v) * time.Millisecond
	}
	if v, ok := settings["request_retry_rate_limits"].(bool); ok && !v {
		cfg.Retry429 = false
	}
	return cfg
}

// ParseRetryAfter parses the Retry-After header value (seconds or HTTP-date).
// Returns 0 if the header is absent or unparseable.
func ParseRetryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	if sec, err := parseRetryAfterSeconds(header); err == nil {
		return sec
	}
	if t, err := http.ParseTime(header); err == nil {
		return time.Until(t)
	}
	return 0
}

func parseRetryAfterSeconds(s string) (time.Duration, error) {
	var sec float64
	if _, err := fmt.Sscanf(s, "%f", &sec); err != nil {
		return 0, err
	}
	if sec < 0 {
		return 0, fmt.Errorf("negative retry-after")
	}
	return time.Duration(sec * float64(time.Second)), nil
}

// backoffDelay computes exponential backoff for the given attempt.
// Matches Python's wait_exponential(multiplier=1, min=0.2, max=30).
func backoffDelay(attempt int, cfg RetryConfig) time.Duration {
	exp := attempt - 1
	if exp < 0 {
		exp = 0
	}
	// 饱和退避: 钳制移位指数, 防止 MaxAttempts 配置过大时 1<<(attempt-1)
	// 溢出为 0/负数, 使退避延迟归零退化为热循环。
	if exp > 30 {
		exp = 30
	}
	delay := cfg.MinDelay * time.Duration(1<<exp)
	if delay > cfg.MaxDelay || delay <= 0 {
		return cfg.MaxDelay
	}
	return delay
}

// sleepWithContext sleeps for d or until ctx is done, whichever comes first.
func sleepWithContext(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// isRetryableError reports whether the HTTP request error is transient.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "temporary failure") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "use of closed network") ||
		strings.Contains(msg, "server closed")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// RequestFactory creates a new HTTP request for each retry attempt.
// This is necessary because request bodies can only be read once.
type RequestFactory func() (*http.Request, error)

// DoWithRetry executes an HTTP request with retry logic.
// It retries on connection errors and retryable HTTP status codes.
// The retry delay respects the Retry-After header for 429 responses.
//
// The factory is called once per attempt to create a fresh request.
// The caller is responsible for closing the response body on success.
//
// Ported from astrbot/core/provider/sources/request_retry.py
// (tenacity.AsyncRetrying + retry_if_exception + wait_exponential)
func DoWithRetry(
	ctx context.Context,
	client *http.Client,
	factory RequestFactory,
	cfg RetryConfig,
	providerLabel string,
) (*http.Response, error) {
	var lastResp *http.Response
	var lastErr error

	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		req, err := factory()
		if err != nil {
			return nil, fmt.Errorf("[%s] create request: %w", providerLabel, err)
		}

		resp, err := client.Do(req)
		if err != nil {
			lastResp = nil
			lastErr = err
			if isRetryableError(err) {
				retryLogger.Debug("[%s] HTTP request error (attempt %d/%d): %v",
					providerLabel, attempt, cfg.MaxAttempts, err)
			} else {
				return nil, fmt.Errorf("[%s] %w", providerLabel, err)
			}
		} else {
			status := resp.StatusCode
			retryable := (status == http.StatusTooManyRequests && cfg.Retry429) || IsRetryableStatus(status)
			if status == http.StatusOK || status == http.StatusCreated || !retryable {
				return resp, nil
			}
			// Retryable error status: consume body and retry.
			io.ReadAll(resp.Body)
			resp.Body.Close()
			lastResp = resp
			lastErr = fmt.Errorf("HTTP %d", status)
			retryLogger.Warn("[%s] Retryable status %d (attempt %d/%d): %s",
				providerLabel, status, attempt, cfg.MaxAttempts,
				truncate(lastErr.Error(), 200))
			// Respect Retry-After for 429.
			if status == http.StatusTooManyRequests {
				if d := ParseRetryAfter(resp.Header.Get("Retry-After")); d > 0 {
					retryLogger.Debug("[%s] Retry-After: waiting %v before next attempt", providerLabel, d)
					sleepWithContext(ctx, d)
					continue
				}
			}
		}

		if attempt < cfg.MaxAttempts {
			delay := backoffDelay(attempt, cfg)
			retryLogger.Debug("[%s] Retrying in %v (attempt %d/%d)",
				providerLabel, delay, attempt, cfg.MaxAttempts)
			sleepWithContext(ctx, delay)
		}
	}

	// On the exhausted path the last retryable response body was already
	// consumed and closed, so only its status is folded into the error and a
	// nil response is returned (never a closed body).
	if lastResp != nil {
		lastErr = fmt.Errorf("last status %d", lastResp.StatusCode)
	}
	return nil, fmt.Errorf("[%s] max retries (%d) exceeded: %w", providerLabel, cfg.MaxAttempts, lastErr)
}
