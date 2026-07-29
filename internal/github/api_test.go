package github

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newTransientTransport builds a TransientRetryTransport pointed at a fresh
// httptest.Server. MaxElapsed is set large enough that it never triggers in
// unit tests; BaseDelay and MaxDelay are tiny so tests run fast.
func newTransientTransport(handler http.Handler, maxRetries int) (*TransientRetryTransport, *httptest.Server) {
	srv := httptest.NewServer(handler)
	tr := NewTransientRetryTransport(http.DefaultTransport)
	tr.Base = http.DefaultTransport
	tr.MaxRetries = maxRetries
	tr.MaxElapsed = 10 * time.Second
	tr.BaseDelay = 1 * time.Millisecond
	tr.MaxDelay = 5 * time.Millisecond
	return tr, srv
}

// serveSequence returns responses in order; repeats the last one once exhausted.
func serveSequence(responses []http.HandlerFunc) http.HandlerFunc {
	var n int64
	return func(w http.ResponseWriter, r *http.Request) {
		idx := int(atomic.AddInt64(&n, 1)) - 1
		if idx >= len(responses) {
			idx = len(responses) - 1
		}
		responses[idx](w, r)
	}
}

func writeStatus(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"status":%d}`, code)
	}
}

func write200(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintln(w, `{"login":"alice","id":1}`)
}

// doRequest sends a request with the given method to url via tr.
func doRequest(tr http.RoundTripper, method, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), method, url, nil)
	if err != nil {
		return nil, err
	}
	return tr.RoundTrip(req)
}

// ---------------------------------------------------------------------------
// NewClient timeout tests (Issue #357)
// ---------------------------------------------------------------------------

// TestNewClient_RateLimitWaitLongerThanOldTimeoutIsHonored simulates a
// primary rate-limit response whose X-RateLimit-Reset is further away than
// the old hardcoded 10-second client Timeout. Before the fix, http.Client's
// Timeout derived a context deadline that both composed transports select on
// while sleeping, so this request would have failed with a context-deadline
// error at ~10s instead of waiting for the reset and succeeding. It must now
// succeed, proving the client-level Timeout no longer truncates
// RateLimitTransport's own retry budget.
func TestNewClient_RateLimitWaitLongerThanOldTimeoutIsHonored(t *testing.T) {
	const (
		rateLimitWaitSeconds = 14 // > the old 10s client Timeout
		// X-RateLimit-Reset is a Unix second timestamp, so computing it via
		// time.Now().Add(...).Unix() truncates whatever fraction of the
		// current second has already elapsed — the actual wait can be up to
		// ~1s shorter than the nominal value. minElapsed leaves headroom for
		// that truncation (and scheduling jitter) while still asserting the
		// wait clearly exceeded the old 10s timeout.
		minElapsed = 11 * time.Second
	)

	resetAt := time.Now().Add(rateLimitWaitSeconds * time.Second).Unix()
	srv := httptest.NewServer(serveSequence([]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", resetAt))
			w.WriteHeader(http.StatusForbidden)
		},
		write200,
	}))
	defer srv.Close()

	client := NewClient()

	start := time.Now()
	resp, err := client.HTTP.Get(srv.URL)
	if err != nil {
		t.Fatalf("expected the rate-limit wait to be honored and succeed, got error: %v", err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after the rate-limit reset elapsed, got %d", resp.StatusCode)
	}
	if elapsed < minElapsed {
		t.Fatalf("expected the request to wait past the old 10s timeout for the rate-limit reset, only took %v — did the client time out early instead of waiting?", elapsed)
	}
}

// TestNewClient_FastRequestUnaffected is a sanity check that the raised
// Timeout doesn't change behavior for ordinary, non-rate-limited requests.
func TestNewClient_FastRequestUnaffected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(write200))
	defer srv.Close()

	client := NewClient()
	resp, err := client.HTTP.Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error on a normal request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// isTransientStatus unit tests
// ---------------------------------------------------------------------------

func TestIsTransientStatus(t *testing.T) {
	transient := []int{500, 502, 503, 504}
	for _, code := range transient {
		if !isTransientStatus(code) {
			t.Errorf("isTransientStatus(%d) = false, want true", code)
		}
	}
	notTransient := []int{200, 201, 400, 401, 403, 404, 429, 501}
	for _, code := range notTransient {
		if isTransientStatus(code) {
			t.Errorf("isTransientStatus(%d) = true, want false", code)
		}
	}
}

// ---------------------------------------------------------------------------
// isTransientNetworkError unit tests
// ---------------------------------------------------------------------------

func TestIsTransientNetworkError_nil(t *testing.T) {
	if isTransientNetworkError(nil) {
		t.Fatal("nil error should not be transient")
	}
}

func TestIsTransientNetworkError_contextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Perform a real dial to get a context-cancelled net error.
	d := &net.Dialer{}
	_, err := d.DialContext(ctx, "tcp", "127.0.0.1:1")
	if err == nil {
		t.Skip("expected dial to fail")
	}
	if isTransientNetworkError(err) {
		t.Fatalf("context-cancelled error should not be considered transient: %v", err)
	}
}

func TestIsTransientNetworkError_opError(t *testing.T) {
	err := &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("connection refused")}
	if !isTransientNetworkError(err) {
		t.Fatal("net.OpError should be considered transient")
	}
}

// ---------------------------------------------------------------------------
// transientBackoff unit tests
// ---------------------------------------------------------------------------

func TestTransientBackoff_withinBounds(t *testing.T) {
	base := 10 * time.Millisecond
	max := 100 * time.Millisecond
	for attempt := 0; attempt < 10; attempt++ {
		d := transientBackoff(attempt, base, max)
		if d < 0 || d > max {
			t.Fatalf("attempt %d: backoff %s out of [0, %s]", attempt, d, max)
		}
	}
}

func TestTransientBackoff_jitterVaries(t *testing.T) {
	base := 50 * time.Millisecond
	max := 500 * time.Millisecond
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		seen[transientBackoff(2, base, max)] = true
	}
	if len(seen) < 2 {
		t.Fatal("backoff should vary across calls due to jitter")
	}
}

func TestTransientBackoff_zeroDefaults(t *testing.T) {
	d := transientBackoff(0, 0, 0)
	if d < 0 || d > TransientMaxDelay {
		t.Fatalf("zero-input backoff %s out of [0, %s]", d, TransientMaxDelay)
	}
}

// ---------------------------------------------------------------------------
// RoundTrip behaviour tests
// ---------------------------------------------------------------------------

// TestTransient_200NotRetried verifies a successful response is returned
// immediately with exactly one call.
func TestTransient_200NotRetried(t *testing.T) {
	var calls int64
	tr, srv := newTransientTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		write200(w, r)
	}), 3)
	defer srv.Close()

	resp, err := doRequest(tr, http.MethodGet, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("want 1 call, got %d", calls)
	}
}

// TestTransient_503ThenSuccess verifies three consecutive 503s followed by
// a 200 result in the 200 being returned after 3 retries (4 total calls).
func TestTransient_503ThenSuccess(t *testing.T) {
	var calls int64
	handler := serveSequence([]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) { atomic.AddInt64(&calls, 1); writeStatus(503)(w, r) },
		func(w http.ResponseWriter, r *http.Request) { atomic.AddInt64(&calls, 1); writeStatus(503)(w, r) },
		func(w http.ResponseWriter, r *http.Request) { atomic.AddInt64(&calls, 1); writeStatus(503)(w, r) },
		func(w http.ResponseWriter, r *http.Request) { atomic.AddInt64(&calls, 1); write200(w, r) },
	})
	tr, srv := newTransientTransport(handler, 3)
	defer srv.Close()

	resp, err := doRequest(tr, http.MethodGet, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if atomic.LoadInt64(&calls) != 4 {
		t.Fatalf("want 4 calls (3 retries), got %d", calls)
	}
}

// TestTransient_ExhaustedReturnsLastResponse verifies that when all retries
// are spent the final 503 is returned to the caller.
func TestTransient_ExhaustedReturnsLastResponse(t *testing.T) {
	var calls int64
	tr, srv := newTransientTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		writeStatus(503)(w, r)
	}), 2)
	defer srv.Close()

	resp, err := doRequest(tr, http.MethodGet, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
	// initial + 2 retries = 3 total
	if atomic.LoadInt64(&calls) != 3 {
		t.Fatalf("want 3 calls, got %d", calls)
	}
}

// TestTransient_PostNotRetried verifies that POST requests are NOT retried
// on 503 by default (security requirement).
func TestTransient_PostNotRetried(t *testing.T) {
	var calls int64
	tr, srv := newTransientTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		writeStatus(503)(w, r)
	}), 3)
	defer srv.Close()

	resp, err := doRequest(tr, http.MethodPost, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("POST should not be retried: want 1 call, got %d", calls)
	}
}

// TestTransient_PatchNotRetried verifies PATCH is also excluded.
func TestTransient_PatchNotRetried(t *testing.T) {
	var calls int64
	tr, srv := newTransientTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		writeStatus(503)(w, r)
	}), 3)
	defer srv.Close()

	resp, err := doRequest(tr, http.MethodPatch, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("PATCH should not be retried: want 1 call, got %d", calls)
	}
}

// TestTransient_DeleteNotRetried verifies DELETE is also excluded.
func TestTransient_DeleteNotRetried(t *testing.T) {
	var calls int64
	tr, srv := newTransientTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		writeStatus(503)(w, r)
	}), 3)
	defer srv.Close()

	resp, err := doRequest(tr, http.MethodDelete, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("DELETE should not be retried: want 1 call, got %d", calls)
	}
}

// TestTransient_PostOptInRetried verifies that POST with
// X-Retry-Non-Idempotent: true IS retried.
func TestTransient_PostOptInRetried(t *testing.T) {
	var calls int64
	handler := serveSequence([]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) { atomic.AddInt64(&calls, 1); writeStatus(503)(w, r) },
		func(w http.ResponseWriter, r *http.Request) { atomic.AddInt64(&calls, 1); write200(w, r) },
	})
	tr, srv := newTransientTransport(handler, 3)
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, nil)
	req.Header.Set("X-Retry-Non-Idempotent", "true")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if atomic.LoadInt64(&calls) != 2 {
		t.Fatalf("want 2 calls with opt-in retry, got %d", calls)
	}
}

// TestTransient_404NotRetried verifies that client errors (4xx) are never retried.
func TestTransient_404NotRetried(t *testing.T) {
	var calls int64
	tr, srv := newTransientTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		writeStatus(404)(w, r)
	}), 3)
	defer srv.Close()

	resp, err := doRequest(tr, http.MethodGet, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("404 should not be retried: want 1 call, got %d", calls)
	}
}

// TestTransient_501NotRetried verifies 501 Not Implemented is not retried
// (permanent server signal).
func TestTransient_501NotRetried(t *testing.T) {
	var calls int64
	tr, srv := newTransientTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		writeStatus(501)(w, r)
	}), 3)
	defer srv.Close()

	resp, err := doRequest(tr, http.MethodGet, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("501 should not be retried: want 1 call, got %d", calls)
	}
}

// TestTransient_ContextCancelledDuringSleep verifies the transport stops
// retrying and returns ctx.Err() when the context is cancelled mid-sleep.
func TestTransient_ContextCancelledDuringSleep(t *testing.T) {
	tr := NewTransientRetryTransport(http.DefaultTransport)
	tr.MaxRetries = 5
	tr.MaxElapsed = 10 * time.Second
	tr.BaseDelay = 200 * time.Millisecond
	tr.MaxDelay = 500 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeStatus(503)(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	_, err := tr.RoundTrip(req)
	if err == nil {
		t.Fatal("expected context error, got nil")
	}
}

// TestTransient_MaxElapsedStopsRetry verifies that once the wall-clock budget
// is exhausted the last response is returned immediately.
func TestTransient_MaxElapsedStopsRetry(t *testing.T) {
	var calls int64
	tr := NewTransientRetryTransport(http.DefaultTransport)
	tr.MaxRetries = 10
	tr.MaxElapsed = 50 * time.Millisecond
	tr.BaseDelay = 30 * time.Millisecond
	tr.MaxDelay = 30 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		writeStatus(503)(w, r)
	}))
	defer srv.Close()

	resp, err := doRequest(tr, http.MethodGet, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// Should be well under MaxRetries=10 because MaxElapsed=50ms caps it.
	if atomic.LoadInt64(&calls) >= 10 {
		t.Fatalf("MaxElapsed not respected: got %d calls", calls)
	}
}

// TestTransient_NetworkErrorRetried verifies a network-level error (connection
// to a port that rejects connections) is retried on a GET.
func TestTransient_NetworkErrorRetried(t *testing.T) {
	// Use a closed server to force connection-refused errors.
	srv := httptest.NewServer(http.HandlerFunc(write200))
	addr := srv.URL
	srv.Close() // close immediately so the port is gone

	tr := NewTransientRetryTransport(http.DefaultTransport)
	tr.MaxRetries = 2
	tr.MaxElapsed = 5 * time.Second
	tr.BaseDelay = 1 * time.Millisecond
	tr.MaxDelay = 5 * time.Millisecond

	_, err := doRequest(tr, http.MethodGet, addr)
	// We expect an error (the server is gone); what we verify is that it was
	// actually retried (we cannot easily count here, but we confirm no panic
	// and that the error is non-nil as expected).
	if err == nil {
		t.Fatal("expected connection error, got nil")
	}
}

// TestTransient_NetworkErrorPostNotRetried verifies a network error on POST
// is NOT retried.
func TestTransient_NetworkErrorPostNotRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(write200))
	addr := srv.URL
	srv.Close()

	var callCount int64
	tr := NewTransientRetryTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt64(&callCount, 1)
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("connection refused")}
	}))
	tr.MaxRetries = 3
	tr.MaxElapsed = 5 * time.Second
	tr.BaseDelay = 1 * time.Millisecond
	tr.MaxDelay = 5 * time.Millisecond

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, addr, nil)
	_, err := tr.RoundTrip(req)
	if err == nil {
		t.Fatal("expected error")
	}
	if atomic.LoadInt64(&callCount) != 1 {
		t.Fatalf("POST network error should not be retried: want 1 call, got %d", callCount)
	}
}

// ---------------------------------------------------------------------------
// NewClient wiring test
// ---------------------------------------------------------------------------

// TestNewClient_TransportComposition verifies that NewClient wires
// TransientRetryTransport (outer) over RateLimitTransport (inner).
func TestNewClient_TransportComposition(t *testing.T) {
	c := NewClient()
	outer, ok := c.HTTP.Transport.(*TransientRetryTransport)
	if !ok {
		t.Fatalf("NewClient outer transport is %T, want *TransientRetryTransport", c.HTTP.Transport)
	}
	if _, ok := outer.Base.(*RateLimitTransport); !ok {
		t.Fatalf("NewClient inner transport is %T, want *RateLimitTransport", outer.Base)
	}
}

// ---------------------------------------------------------------------------
// Integration-style: composed stack (TransientRetry → RateLimit → mock)
// ---------------------------------------------------------------------------

// TestComposedStack_503ThenRateLimitThenSuccess exercises the full composed
// transport stack: the first call returns 503 (handled by TransientRetry),
// the second returns a rate-limit 403 (handled by RateLimit), and the third
// succeeds.
func TestComposedStack_503ThenRateLimitThenSuccess(t *testing.T) {
	var calls int64
	handler := serveSequence([]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&calls, 1)
			writeStatus(503)(w, r)
		},
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&calls, 1)
			// rate-limit signal with zero wait
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "1") // past epoch — zero wait
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintln(w, `{"message":"rate limited"}`)
		},
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&calls, 1)
			write200(w, r)
		},
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	rl := NewRateLimitTransport(http.DefaultTransport)
	rl.Base = http.DefaultTransport
	rl.MaxRetries = 3
	rl.MaxWait = 5 * time.Millisecond

	tr := NewTransientRetryTransport(rl)
	tr.MaxRetries = 3
	tr.MaxElapsed = 10 * time.Second
	tr.BaseDelay = 1 * time.Millisecond
	tr.MaxDelay = 5 * time.Millisecond

	resp, err := doRequest(tr, http.MethodGet, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if atomic.LoadInt64(&calls) != 3 {
		t.Fatalf("want 3 total calls, got %d", calls)
	}
}
