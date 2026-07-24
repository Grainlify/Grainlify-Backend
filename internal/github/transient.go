package github

// TransientRetryTransport retries GitHub API requests that fail with transient
// 5xx HTTP errors or network-level failures (connection reset, temporary DNS
// failure, i/o timeout, etc.).
//
// It is intentionally separate from RateLimitTransport so each concern stays
// single-responsibility and both layers remain independently testable.
//
// Transport composition used by NewClient:
//
//	http.DefaultTransport
//	  └─ RateLimitTransport            (retries 403/429 rate-limit signals)
//	       └─ TransientRetryTransport  (retries 5xx / network errors) ← outermost
//
// Security note: non-idempotent HTTP methods (POST, PUT, PATCH, DELETE) are
// never retried automatically because blind retries on those methods can
// produce duplicate side-effects (e.g. creating the same issue twice).
// Callers that know a non-idempotent endpoint is safe to retry may opt in by
// setting the request header "X-Retry-Non-Idempotent: true".

import (
	cryptorand "crypto/rand"
	"context"
	"errors"
	"math/big"
	"net"
	"net/http"
	"time"
)

// Transient-retry defaults — tuned independently from the rate-limit constants.
const (
	// TransientMaxRetries is the default cap on retry attempts per request.
	TransientMaxRetries = 3
	// TransientMaxElapsed is the default wall-clock budget for all retries of a
	// single request. Once this elapses the last response/error is returned.
	TransientMaxElapsed = 30 * time.Second
	// TransientBaseDelay is the initial back-off window before jitter is applied.
	TransientBaseDelay = 250 * time.Millisecond
	// TransientMaxDelay caps any individual sleep regardless of attempt number.
	TransientMaxDelay = 5 * time.Second
)

// idempotentMethods lists the HTTP methods that are safe to retry without
// explicit opt-in.
var idempotentMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodOptions: true,
}

// TransientRetryTransport is an http.RoundTripper that transparently retries
// requests that receive a 5xx server error or a transient network-level error.
//
// Zero-value fields fall back to the package-level defaults defined above.
type TransientRetryTransport struct {
	// Base is the underlying transport. Defaults to http.DefaultTransport.
	Base http.RoundTripper
	// MaxRetries is the maximum number of retry attempts (0 → TransientMaxRetries).
	MaxRetries int
	// MaxElapsed is the total wall-clock budget for retrying a single request
	// (0 → TransientMaxElapsed).
	MaxElapsed time.Duration
	// BaseDelay is the starting back-off window (0 → TransientBaseDelay).
	BaseDelay time.Duration
	// MaxDelay caps any single sleep (0 → TransientMaxDelay).
	MaxDelay time.Duration
}

// NewTransientRetryTransport returns a TransientRetryTransport wrapping base
// with sensible defaults. Pass nil to use http.DefaultTransport as the base.
func NewTransientRetryTransport(base http.RoundTripper) *TransientRetryTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &TransientRetryTransport{
		Base:       base,
		MaxRetries: TransientMaxRetries,
		MaxElapsed: TransientMaxElapsed,
		BaseDelay:  TransientBaseDelay,
		MaxDelay:   TransientMaxDelay,
	}
}

// RoundTrip executes the HTTP request and retries on transient failures.
//
// Retried conditions:
//   - HTTP 500, 502, 503, 504 response status codes.
//   - Network-level errors (connection refused/reset, i/o timeout, etc.) that
//     implement net.Error or *net.OpError.
//
// Never retried:
//   - Non-idempotent methods (POST, PUT, PATCH, DELETE) unless the request
//     carries the header "X-Retry-Non-Idempotent: true".
//   - Context cancellation or deadline-exceeded errors.
//   - Any other 4xx or non-transient response.
func (t *TransientRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	maxRetries := t.MaxRetries
	if maxRetries <= 0 {
		maxRetries = TransientMaxRetries
	}
	maxElapsed := t.MaxElapsed
	if maxElapsed <= 0 {
		maxElapsed = TransientMaxElapsed
	}
	baseDelay := t.BaseDelay
	if baseDelay <= 0 {
		baseDelay = TransientBaseDelay
	}
	maxDelay := t.MaxDelay
	if maxDelay <= 0 {
		maxDelay = TransientMaxDelay
	}

	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}

	// Determine once whether this request may be retried.
	retryAllowed := idempotentMethods[req.Method] ||
		req.Header.Get("X-Retry-Non-Idempotent") == "true"

	start := time.Now()

	var (
		resp *http.Response
		err  error
	)

	for attempt := 0; attempt <= maxRetries; attempt++ {
		// Clone so each attempt gets its own request (safe for nil bodies on GET).
		clone := req.Clone(req.Context())
		resp, err = base.RoundTrip(clone)

		transientNet := err != nil && isTransientNetworkError(err)
		transient5xx := err == nil && isTransientStatus(resp.StatusCode)

		// Not a transient failure — return immediately.
		if !transientNet && !transient5xx {
			return resp, err
		}

		// Non-idempotent and no explicit opt-in — surface the error as-is.
		if !retryAllowed {
			return resp, err
		}

		// Retry budget exhausted — return whatever we have.
		if attempt == maxRetries || time.Since(start) >= maxElapsed {
			break
		}

		// Drain body before sleeping so the connection returns to the pool.
		if resp != nil {
			resp.Body.Close()
			resp = nil
		}

		sleep := transientBackoff(attempt, baseDelay, maxDelay)

		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(sleep):
		}
	}

	return resp, err
}

// isTransientStatus reports whether the HTTP status code warrants a retry.
// 501 Not Implemented is intentionally excluded — it is a permanent signal.
func isTransientStatus(code int) bool {
	switch code {
	case http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// isTransientNetworkError reports whether err is a retriable network-level
// failure. Context cancellation and deadline-exceeded are explicitly excluded
// because they represent caller intent and must not be swallowed.
func isTransientNetworkError(err error) bool {
	if err == nil {
		return false
	}
	// Never retry context errors — they are caller-initiated.
	if isContextErr(err) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return false
}

// isContextErr returns true for context.Canceled and context.DeadlineExceeded,
// including OS-level wrappings (e.g. "operation was canceled" on Linux).
func isContextErr(err error) bool {
	if err == nil {
		return false
	}
	// Unwrap through net.OpError / url.Error layers to reach the sentinel.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// Some OS-level errors describe cancellation with a different string.
	s := err.Error()
	return s == "context canceled" || s == "context deadline exceeded" ||
		s == "operation was canceled"
}

// transientBackoff returns a full-jitter sleep for the given attempt number.
//
// The back-off window doubles each attempt (exponential back-off) up to
// maxDelay, then the actual sleep is drawn uniformly from [0, window] to
// prevent synchronized retry storms when many goroutines hit the same failure.
func transientBackoff(attempt int, baseDelay, maxDelay time.Duration) time.Duration {
	if baseDelay <= 0 {
		baseDelay = TransientBaseDelay
	}
	if maxDelay <= 0 {
		maxDelay = TransientMaxDelay
	}

	// Saturating shift: window = baseDelay * 2^attempt, capped at maxDelay.
	shift := attempt
	if shift > 30 {
		shift = 30 // guard against int64 overflow
	}
	window := baseDelay * (1 << uint(shift))
	if window <= 0 || window > maxDelay {
		window = maxDelay
	}

	// Full jitter: pick uniformly in [0, window].
	n, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(window)+1))
	if err != nil {
		// Crypto source failure is vanishingly rare; fall back to deterministic cap.
		return window
	}
	return time.Duration(n.Int64())
}
