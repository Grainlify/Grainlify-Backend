package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// roundTripFunc lets a plain func satisfy http.RoundTripper in tests.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newTestTransport returns a RateLimitTransport backed by a recording server,
// with MaxWait and sleep replaced so tests run in milliseconds.
func newTestTransport(handler http.Handler, maxRetries int, maxWait time.Duration) (*RateLimitTransport, *httptest.Server) {
	srv := httptest.NewServer(handler)
	tr := NewRateLimitTransport(nil)
	tr.Base = http.DefaultTransport
	tr.MaxRetries = maxRetries
	tr.MaxWait = maxWait
	return tr, srv
}

// serve returns an http.HandlerFunc that serves responses in order.
// If called more times than len(responses), it always returns the last response.
func serve(responses []http.HandlerFunc) http.HandlerFunc {
	var n int64
	return func(w http.ResponseWriter, r *http.Request) {
		idx := int(atomic.AddInt64(&n, 1)) - 1
		if idx >= len(responses) {
			idx = len(responses) - 1
		}
		responses[idx](w, r)
	}
}

// rateLimitResponse writes a primary-rate-limit response (403, X-RateLimit-Remaining: 0).
func rateLimitResponse(resetInSecs int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resetUnix := time.Now().Add(time.Duration(resetInSecs) * time.Second).Unix()
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetUnix, 10))
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintln(w, `{"message":"API rate limit exceeded"}`)
	}
}

// retryAfterResponse writes a secondary-rate-limit response (403, Retry-After).
func retryAfterResponse(secs int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintln(w, `{"message":"secondary rate limit"}`)
	}
}

// okResponse writes a 200 JSON body.
func okResponse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintln(w, `{"ok":true}`)
}

// --- Tests ---

// TestNoRetryOnSuccess verifies a 200 is returned without any retry.
func TestNoRetryOnSuccess(t *testing.T) {
	var calls int64
	tr, srv := newTestTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		okResponse(w, r)
	}), 3, 10*time.Millisecond)
	defer srv.Close()

	resp, err := doGet(tr, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("want 1 call, got %d", calls)
	}
}

// TestPrimaryRateLimitThenSuccess verifies retry after X-RateLimit-Remaining:0.
func TestPrimaryRateLimitThenSuccess(t *testing.T) {
	var calls int64
	handler := serve([]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&calls, 1)
			// Reset already in the past so wait is ≤1s (capped further by MaxWait).
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(-1*time.Second).Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
		},
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&calls, 1)
			okResponse(w, r)
		},
	})
	tr, srv := newTestTransport(handler, 3, 50*time.Millisecond)
	defer srv.Close()

	resp, err := doGet(tr, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if atomic.LoadInt64(&calls) != 2 {
		t.Fatalf("want 2 calls, got %d", calls)
	}
}

// TestRetryAfterThenSuccess verifies retry after Retry-After header (secondary limit).
func TestRetryAfterThenSuccess(t *testing.T) {
	var calls int64
	handler := serve([]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&calls, 1)
			w.Header().Set("Retry-After", "0") // 0s so test is instant
			w.WriteHeader(http.StatusTooManyRequests)
		},
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&calls, 1)
			okResponse(w, r)
		},
	})
	tr, srv := newTestTransport(handler, 3, 50*time.Millisecond)
	defer srv.Close()

	resp, err := doGet(tr, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if atomic.LoadInt64(&calls) != 2 {
		t.Fatalf("want 2 calls, got %d", calls)
	}
}

// TestExhaustedRetriesReturnsRateLimitResponse verifies that when all retries
// are spent, the final rate-limit response is returned to the caller.
func TestExhaustedRetriesReturnsRateLimitResponse(t *testing.T) {
	var calls int64
	tr, srv := newTestTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		rateLimitResponse(0)(w, r)
	}), 2, 50*time.Millisecond)
	defer srv.Close()

	resp, err := doGet(tr, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d", resp.StatusCode)
	}
	// initial attempt + 2 retries = 3 total
	if atomic.LoadInt64(&calls) != 3 {
		t.Fatalf("want 3 calls, got %d", calls)
	}
}

// TestMaxWaitCapIsRespected ensures the sleep never exceeds MaxWait.
func TestMaxWaitCapIsRespected(t *testing.T) {
	handler := serve([]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) {
			// Ask for 1 hour wait — should be capped.
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusForbidden)
		},
		okResponse,
	})
	tr, srv := newTestTransport(handler, 1, 50*time.Millisecond)
	defer srv.Close()

	start := time.Now()
	resp, err := doGet(tr, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	elapsed := time.Since(start)
	// Should have slept ≤ MaxWait (50ms) + some overhead, not 3600s.
	if elapsed > 2*time.Second {
		t.Fatalf("MaxWait not respected: elapsed %s", elapsed)
	}
}

// TestContextCancellationDuringSleep verifies the transport returns on ctx cancel.
func TestContextCancellationDuringSleep(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ask for a long wait so the sleep is active when ctx is cancelled.
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusForbidden)
	})
	tr, srv := newTestTransport(handler, 3, 10*time.Second)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	client := &http.Client{Transport: tr}
	_, err := client.Do(req)
	if err == nil {
		t.Fatal("expected context error, got nil")
	}
}

// TestNon403NotRetried verifies that a plain 500 is not retried.
func TestNon403NotRetried(t *testing.T) {
	var calls int64
	tr, srv := newTestTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}), 3, 50*time.Millisecond)
	defer srv.Close()

	resp, err := doGet(tr, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("want 1 call (no retry on 500), got %d", calls)
	}
}

// TestForbiddenWithoutRateLimitHeadersNotRetried verifies a 403 without
// rate-limit indicators is not retried (e.g. auth failure).
func TestForbiddenWithoutRateLimitHeadersNotRetried(t *testing.T) {
	var calls int64
	tr, srv := newTestTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusForbidden) // no rate-limit headers
	}), 3, 50*time.Millisecond)
	defer srv.Close()

	resp, err := doGet(tr, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("want 1 call, got %d", calls)
	}
}

// TestMultipleRateLimitsBeforeSuccess verifies retrying twice then succeeding.
func TestMultipleRateLimitsBeforeSuccess(t *testing.T) {
	var calls int64
	handler := serve([]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&calls, 1)
			retryAfterResponse(0)(w, r)
		},
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&calls, 1)
			retryAfterResponse(0)(w, r)
		},
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&calls, 1)
			okResponse(w, r)
		},
	})
	tr, srv := newTestTransport(handler, 3, 50*time.Millisecond)
	defer srv.Close()

	resp, err := doGet(tr, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if atomic.LoadInt64(&calls) != 3 {
		t.Fatalf("want 3 calls, got %d", calls)
	}
}

// TestNewClientUsesRateLimitTransport ensures NewClient wires the transport.
func TestNewClientUsesRateLimitTransport(t *testing.T) {
	c := NewClient()
	if _, ok := c.HTTP.Transport.(*RateLimitTransport); !ok {
		t.Fatalf("NewClient transport is %T, want *RateLimitTransport", c.HTTP.Transport)
	}
}

// doGet is a helper that sends a GET with a fresh background context.
func doGet(tr http.RoundTripper, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return tr.RoundTrip(req)
}

// TestZeroMaxRetriesFallsBackToDefault verifies that MaxRetries=0 uses DefaultMaxRetries.
func TestZeroMaxRetriesFallsBackToDefault(t *testing.T) {
	var calls int64
	handler := serve([]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) { atomic.AddInt64(&calls, 1); retryAfterResponse(0)(w, r) },
		func(w http.ResponseWriter, r *http.Request) { atomic.AddInt64(&calls, 1); retryAfterResponse(0)(w, r) },
		func(w http.ResponseWriter, r *http.Request) { atomic.AddInt64(&calls, 1); retryAfterResponse(0)(w, r) },
		func(w http.ResponseWriter, r *http.Request) { atomic.AddInt64(&calls, 1); okResponse(w, r) },
	})
	tr, srv := newTestTransport(handler, 0, 50*time.Millisecond) // 0 → DefaultMaxRetries=3
	defer srv.Close()

	resp, err := doGet(tr, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if atomic.LoadInt64(&calls) != 4 {
		t.Fatalf("want 4 calls (default 3 retries), got %d", calls)
	}
}

// TestZeroMaxWaitFallsBackToDefault verifies that MaxWait=0 uses DefaultMaxWait as cap.
func TestZeroMaxWaitFallsBackToDefault(t *testing.T) {
	tr := NewRateLimitTransport(nil)
	tr.MaxWait = 0 // triggers the fallback inside RoundTrip
	// DefaultMaxWait (60s) >> 1s Retry-After, so the cap doesn't shorten it.
	// Just verify we can still complete a successful request without panicking.
	handler := http.HandlerFunc(okResponse)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	tr.Base = http.DefaultTransport

	resp, err := doGet(tr, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
}

// TestPrimaryRateLimitResetCapIsRespected ensures X-RateLimit-Reset far in the future
// is capped to MaxWait.
func TestPrimaryRateLimitResetCapIsRespected(t *testing.T) {
	handler := serve([]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) {
			// Reset 1 hour from now — should be capped.
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
		},
		okResponse,
	})
	tr, srv := newTestTransport(handler, 1, 50*time.Millisecond)
	defer srv.Close()

	start := time.Now()
	resp, err := doGet(tr, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if time.Since(start) > 2*time.Second {
		t.Fatalf("MaxWait not respected for X-RateLimit-Reset: elapsed %s", time.Since(start))
	}
}

// TestFallbackBackoffUsesBoundedJitter verifies unheadered rate-limit waits are
// randomized and never exceed the configured MaxWait cap.
func TestFallbackBackoffUsesBoundedJitter(t *testing.T) {
	resp := &http.Response{Header: make(http.Header)}
	maxWait := 20 * time.Millisecond

	seen := map[time.Duration]bool{}
	for i := 0; i < 100; i++ {
		d := rateLimitWait(resp, maxWait)
		if d < 0 || d > maxWait {
			t.Fatalf("jittered wait = %s, want within [0, %s]", d, maxWait)
		}
		seen[d] = true
	}

	if len(seen) < 2 {
		t.Fatalf("jittered wait did not vary across repeated calls; saw %d unique value(s)", len(seen))
	}
}

// TestHeaderDrivenWaitsTakePrecedenceOverJitter verifies explicit GitHub wait
// headers remain deterministic and are not replaced by fallback jitter.
func TestHeaderDrivenWaitsTakePrecedenceOverJitter(t *testing.T) {
	retryAfter := &http.Response{Header: make(http.Header)}
	retryAfter.Header.Set("Retry-After", "2")
	if got := rateLimitWait(retryAfter, 5*time.Second); got != 2*time.Second {
		t.Fatalf("Retry-After wait = %s, want 2s", got)
	}

	reset := &http.Response{Header: make(http.Header)}
	reset.Header.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(2*time.Second).Unix(), 10))
	got := rateLimitWait(reset, 5*time.Second)
	if got < time.Second || got > 2*time.Second {
		t.Fatalf("X-RateLimit-Reset wait = %s, want within [1s, 2s]", got)
	}
}
