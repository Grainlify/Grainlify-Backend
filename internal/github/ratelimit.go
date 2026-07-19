// Package github provides a GitHub API client with automatic rate-limit backoff.
//
// RateLimitTransport wraps any http.RoundTripper and transparently retries
// requests that fail with GitHub primary (403/429 + X-RateLimit-Remaining: 0)
// or secondary (Retry-After header) rate limits.
package github

import (
	cryptorand "crypto/rand"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultMaxRetries = 3
	DefaultMaxWait    = 60 * time.Second
	DefaultBaseWait   = time.Second
)

// RateLimitTransport is an http.RoundTripper that retries rate-limited GitHub
// API responses (403 / 429) by honoring X-RateLimit-Reset and Retry-After
// headers. Both retry count and maximum wait are configurable.
type RateLimitTransport struct {
	Base       http.RoundTripper // underlying transport; defaults to http.DefaultTransport
	MaxRetries int               // max retry attempts per request
	MaxWait    time.Duration     // cap on any single sleep
}

// NewRateLimitTransport returns a RateLimitTransport with the given base
// transport and default limits.
func NewRateLimitTransport(base http.RoundTripper) *RateLimitTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &RateLimitTransport{
		Base:       base,
		MaxRetries: DefaultMaxRetries,
		MaxWait:    DefaultMaxWait,
	}
}

// RoundTrip executes the request, sleeping and retrying on rate-limit responses.
// It never logs or exposes the Authorization header value.
func (t *RateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	maxRetries := t.MaxRetries
	if maxRetries <= 0 {
		maxRetries = DefaultMaxRetries
	}
	maxWait := t.MaxWait
	if maxWait <= 0 {
		maxWait = DefaultMaxWait
	}

	var (
		resp *http.Response
		err  error
	)

	for attempt := 0; attempt <= maxRetries; attempt++ {
		// Clone the request so the body can be re-read on retry.
		clone := req.Clone(req.Context())

		resp, err = t.Base.RoundTrip(clone)
		if err != nil {
			return nil, err
		}

		if !isRateLimited(resp) {
			return resp, nil
		}

		// Last attempt — return the rate-limit response to the caller.
		if attempt == maxRetries {
			break
		}

		wait := rateLimitWait(resp, maxWait)
		resp.Body.Close()

		// Respect context cancellation during the sleep.
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(wait):
		}
	}

	return resp, nil
}

// isRateLimited reports whether the response is a GitHub rate-limit error.
// Primary limit: 403 or 429 with X-RateLimit-Remaining == 0.
// Secondary limit: 403 or 429 with a Retry-After header present.
func isRateLimited(resp *http.Response) bool {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return false
	}
	if resp.Header.Get("Retry-After") != "" {
		return true
	}
	if v := strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")); v == "0" {
		return true
	}
	return false
}

// rateLimitWait computes how long to sleep before retrying, capped at maxWait.
// Priority: Retry-After (seconds) > X-RateLimit-Reset (Unix timestamp) > jittered fallback.
func rateLimitWait(resp *http.Response, maxWait time.Duration) time.Duration {
	now := time.Now()

	// Secondary rate limit: Retry-After (seconds).
	if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil && secs >= 0 {
			return capWait(time.Duration(secs)*time.Second, maxWait)
		}
	}

	// Primary rate limit: X-RateLimit-Reset (Unix timestamp).
	if v := strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset")); v != "" {
		if unix, err := strconv.ParseInt(v, 10, 64); err == nil {
			resetAt := time.Unix(unix, 0)
			d := resetAt.Sub(now)
			if d <= 0 {
				return capWait(DefaultBaseWait, maxWait)
			}
			return capWait(d, maxWait)
		}
	}

	// Fallback uses full jitter over the bounded base delay to avoid
	// synchronized retries when multiple workers hit an unheadered limit.
	return fullJitterDelay(DefaultBaseWait, maxWait)
}

func capWait(d, maxWait time.Duration) time.Duration {
	if maxWait <= 0 {
		maxWait = DefaultMaxWait
	}
	if d > maxWait {
		return maxWait
	}
	return d
}

func fullJitterDelay(baseWait, maxWait time.Duration) time.Duration {
	if baseWait <= 0 {
		baseWait = DefaultBaseWait
	}
	if maxWait <= 0 {
		maxWait = DefaultMaxWait
	}
	limit := baseWait
	if limit > maxWait {
		limit = maxWait
	}
	if limit <= 0 {
		return 0
	}

	n, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(limit)+1))
	if err != nil {
		return limit
	}
	return time.Duration(n.Int64())
}
