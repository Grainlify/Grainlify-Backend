package github

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// transientErrorResponse writes a transient 5xx response once, useful for
// forcing a single retry.
func transientErrorResponse(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}
}

// TestTransientRetryTransport_PreservesBodyOnRetry is a regression test for
// the bug where a retried non-idempotent request (opted in via
// X-Retry-Non-Idempotent: true) sent an empty body on attempts after the
// first, because req.Clone copies the Body reader by reference instead of
// re-reading it from GetBody.
func TestTransientRetryTransport_PreservesBodyOnRetry(t *testing.T) {
	const wantBody = `{"title":"original payload"}`

	var (
		receivedBodies [][]byte
		callCount      int64
	)

	srv := httptest.NewServer(serve([]http.HandlerFunc{
		transientErrorResponse(http.StatusServiceUnavailable), // attempt 1: transient failure
		okResponse, // attempt 2: succeeds
	}))
	defer srv.Close()

	// Wrap the base transport so we can capture the body actually sent on
	// each attempt, exactly as it goes out over the wire.
	recordingBase := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt64(&callCount, 1)
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
			// Restore the body so the real transport can still send it —
			// io.ReadAll above drained it.
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		receivedBodies = append(receivedBodies, body)
		return http.DefaultTransport.RoundTrip(r)
	})

	tr := &TransientRetryTransport{
		Base:       recordingBase,
		MaxRetries: 2,
		MaxElapsed: 2 * time.Second,
		BaseDelay:  1 * time.Millisecond,
		MaxDelay:   5 * time.Millisecond,
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL, bytes.NewBufferString(wantBody))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("X-Retry-Non-Idempotent", "true")
	req.Header.Set("Content-Type", "application/json")

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected final status 200, got %d", resp.StatusCode)
	}

	if len(receivedBodies) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(receivedBodies))
	}

	for i, b := range receivedBodies {
		if string(b) != wantBody {
			t.Errorf("attempt %d: body = %q, want %q", i+1, string(b), wantBody)
		}
	}
}

// TestTransientRetryTransport_IdempotentGetUnaffected is a guard against
// regressions in the existing idempotent-method retry path: GET requests
// (which typically have a nil body) must continue to retry normally.
func TestTransientRetryTransport_IdempotentGetUnaffected(t *testing.T) {
	srv := httptest.NewServer(serve([]http.HandlerFunc{
		transientErrorResponse(http.StatusBadGateway),
		okResponse,
	}))
	defer srv.Close()

	tr := &TransientRetryTransport{
		Base:       http.DefaultTransport,
		MaxRetries: 2,
		MaxElapsed: 2 * time.Second,
		BaseDelay:  1 * time.Millisecond,
		MaxDelay:   5 * time.Millisecond,
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected final status 200 after retry, got %d", resp.StatusCode)
	}
}
