// Package sources - HTTP request retry with exponential backoff.
// Ported from astrbot/core/provider/sources/request_retry.py
package sources

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2" // nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used -- #nosec G404: 仅用于退避重试抖动（防惊群），非安全随机
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

	// 429 专用重试（provider_settings.request_retry_429_*）：
	//   Retry429Enabled: 总开关，false 时遇 429 不重试（默认 true）
	//   Retry429Max:     遇 429 时最多重试次数（默认 5，<=0 视为 1）
	//   Retry429Fixed:   是否按固定秒数等待（默认 false → 指数退避）
	//   Retry429FixedSeconds: 固定等待秒数（默认 10）
	Retry429Enabled      bool
	Retry429Max          int
	Retry429Fixed        bool
	Retry429FixedSeconds int
}

// DefaultRetryConfig returns the default retry configuration.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts: 5,
		MinDelay:    200 * time.Millisecond,
		MaxDelay:    30 * time.Second,
		Retry429:    true,

		Retry429Enabled:      true,
		Retry429Max:          5,
		Retry429Fixed:        false,
		Retry429FixedSeconds: 10,
	}
}

// RetryConfigFromSettings reads retry configuration from provider_settings.
// Keys (matching Python's config names):
//   - request_max_retries (default 5, min 1)
//   - request_retry_min_delay_ms (default 200)
//   - request_retry_max_delay_ms (default 30000)
//   - request_retry_rate_limits (default true; set false to skip 429)
//   - request_retry_429_enabled (default true; set false to skip 429 retry)
//   - request_retry_429_max (default 5; <=0 coerced to 1)
//   - request_retry_429_strategy ("exponential" | "fixed", default exponential)
//   - request_retry_429_fixed_seconds (default 10)
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
	// 429 专用配置。
	if v, ok := settings["request_retry_429_enabled"].(bool); ok {
		cfg.Retry429Enabled = v
	}
	if v := configInt(settings, "request_retry_429_max", 0); v > 0 {
		cfg.Retry429Max = v
	}
	if cfg.Retry429Max <= 0 {
		cfg.Retry429Max = 1
	}
	if v, ok := settings["request_retry_429_strategy"].(string); ok {
		cfg.Retry429Fixed = v != "exponential"
	}
	if v := configInt(settings, "request_retry_429_fixed_seconds", 0); v > 0 {
		cfg.Retry429FixedSeconds = v
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

// backoffDelay computes exponential backoff for the given attempt with ±20%
// random jitter. Matches Python's wait_exponential(multiplier=1, min=0.2,
// max=30), plus jitter to break the thundering-herd effect: without jitter,
// clients that fail at the same instant retry in perfect lockstep, hammering
// the already-struggling server.
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
	// 抖动: 以 [0.8, 1.2) 随机缩放延迟, 打散并发客户端的同步重试。
	delay = time.Duration(float64(delay) * (0.8 + 0.4*rand.Float64())) // #nosec G404 -- 重试退避抖动（打散惊群），非安全场景，无需 crypto/rand
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
			if status == http.StatusTooManyRequests {
				if !cfg.Retry429Enabled || !cfg.Retry429 {
					return resp, nil
				}
				// 429 独立重试：固定或指数，最多 Retry429Max 次
				resp, err = retry429(ctx, client, factory, resp, cfg, providerLabel)
				if err != nil {
					return nil, err
				}
				return resp, nil
			}
			retryable := IsRetryableStatus(status)
			if status == http.StatusOK || status == http.StatusCreated || !retryable {
				return resp, nil
			}
			// Retryable error status: consume body and retry.
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			lastResp = resp
			lastErr = fmt.Errorf("HTTP %d", status)
			retryLogger.Warn("[%s] Retryable status %d (attempt %d/%d): %s",
				providerLabel, status, attempt, cfg.MaxAttempts,
				truncate(lastErr.Error(), 200))
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

// retry429 performs a dedicated 429 rate-limit retry loop. It consumes and
// closes each 429 response, then re-issues the request up to
// cfg.Retry429Max times. The wait between attempts honors the Retry-After
// header when present; otherwise it is fixed
// (cfg.Retry429FixedSeconds) when cfg.Retry429Fixed, or exponential backoff
// otherwise. Returns the first non-429 response (body intact) or an error
// after the budget is exhausted.
func retry429(
	ctx context.Context,
	client *http.Client,
	factory RequestFactory,
	initial *http.Response,
	cfg RetryConfig,
	providerLabel string,
) (*http.Response, error) {
	resp := initial
	for attempt := 0; ; attempt++ {
		status := resp.StatusCode
		// 成功或非 429（例如临时改判 502→结果继续走通用重试）直接原样返回，
		// body 保持未消费，调用方负责读取/关闭。
		if status == http.StatusOK || status == http.StatusCreated || status != http.StatusTooManyRequests {
			return resp, nil
		}
		// 429：消费并关闭当前响应体，准备重试。
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if attempt >= cfg.Retry429Max {
			return nil, fmt.Errorf("[%s] HTTP 429 rate-limit max retries (%d) exceeded", providerLabel, cfg.Retry429Max)
		}
		var delay time.Duration
		if cfg.Retry429Fixed {
			delay = time.Duration(cfg.Retry429FixedSeconds) * time.Second
			if ra := ParseRetryAfter(resp.Header.Get("Retry-After")); ra > 0 {
				delay = ra
			}
		} else if ra := ParseRetryAfter(resp.Header.Get("Retry-After")); ra > 0 {
			delay = ra
		} else {
			delay = backoffDelay(attempt+1, cfg)
		}
		if delay > cfg.MaxDelay {
			delay = cfg.MaxDelay
		}
		retryLogger.Warn("[%s] HTTP 429 rate-limit (attempt %d/%d), retrying in %v",
			providerLabel, attempt+1, cfg.Retry429Max, delay)
		sleepWithContext(ctx, delay)
		req, err := factory()
		if err != nil {
			return nil, fmt.Errorf("[%s] create request for 429 retry: %w", providerLabel, err)
		}
		resp, err = client.Do(req)
		if err != nil {
			if isRetryableError(err) {
				retryLogger.Debug("[%s] 429-retry HTTP error (attempt %d/%d): %v",
					providerLabel, attempt+1, cfg.Retry429Max, err)
				continue
			}
			return nil, fmt.Errorf("[%s] %w", providerLabel, err)
		}
	}
}
