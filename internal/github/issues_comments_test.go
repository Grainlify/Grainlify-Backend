package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// helpers shared across comment tests
// ---------------------------------------------------------------------------

// commentClient builds a *Client whose HTTP transport is a RateLimitTransport
// pointed at the given test server.  MaxRetries and MaxWait are configurable
// so tests run in milliseconds.
func commentClient(srv *httptest.Server, maxRetries int, maxWait time.Duration) *Client {
	rt := NewRateLimitTransport(nil)
	rt.Base = http.DefaultTransport
	rt.MaxRetries = maxRetries
	rt.MaxWait = maxWait
	return &Client{
		HTTP:      &http.Client{Transport: rt},
		UserAgent: "test-agent",
	}
}

// validCommentResponse writes a successful GitHub comment creation response.
func validCommentResponse(id int64, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		now := time.Now().UTC().Format(time.RFC3339)
		fmt.Fprintf(w, `{"id":%d,"body":%s,"user":{"login":"bot"},"html_url":"https://github.com","created_at":%q,"updated_at":%q}`,
			id, mustMarshal(body), now, now)
	}
}

func mustMarshal(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// secondaryLimitHandler returns a 403 with Retry-After set to retryAfterSecs.
func secondaryLimitHandler(retryAfterSecs int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSecs))
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintln(w, `{"message":"You have exceeded a secondary rate limit.","documentation_url":"https://docs.github.com/rest/overview/resources-in-the-rest-api#secondary-rate-limits"}`)
	}
}

// primaryLimitHandler returns a 403 with X-RateLimit-Remaining: 0 (no Retry-After).
func primaryLimitHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(-1*time.Second).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintln(w, `{"message":"API rate limit exceeded"}`)
	}
}

// serverSequence builds an httptest.Server that serves the given handlers in
// order; once the list is exhausted it always serves the last handler.
func serverSequence(handlers ...http.HandlerFunc) *httptest.Server {
	var n int64
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := int(atomic.AddInt64(&n, 1)) - 1
		if idx >= len(handlers) {
			idx = len(handlers) - 1
		}
		handlers[idx](w, r)
	}))
}

// ---------------------------------------------------------------------------
// CreateIssueComment — input validation
// ---------------------------------------------------------------------------

func TestCreateIssueComment_MissingToken(t *testing.T) {
	c := NewClient()
	_, err := c.CreateIssueComment(context.Background(), "", "owner/repo", 1, "hello")
	if err == nil || err.Error() != "missing github access token" {
		t.Fatalf("want missing-token error, got %v", err)
	}
}

func TestCreateIssueComment_InvalidIssueNumber(t *testing.T) {
	c := NewClient()
	_, err := c.CreateIssueComment(context.Background(), "tok", "owner/repo", 0, "hello")
	if err == nil {
		t.Fatal("want error for issue number 0, got nil")
	}
}

func TestCreateIssueComment_NegativeIssueNumber(t *testing.T) {
	c := NewClient()
	_, err := c.CreateIssueComment(context.Background(), "tok", "owner/repo", -1, "hello")
	if err == nil {
		t.Fatal("want error for negative issue number, got nil")
	}
}

func TestCreateIssueComment_EmptyBody(t *testing.T) {
	c := NewClient()
	_, err := c.CreateIssueComment(context.Background(), "tok", "owner/repo", 1, "")
	if err == nil || err.Error() != "comment body is required" {
		t.Fatalf("want comment-body error, got %v", err)
	}
}

func TestCreateIssueComment_WhitespaceBody(t *testing.T) {
	c := NewClient()
	_, err := c.CreateIssueComment(context.Background(), "tok", "owner/repo", 1, "   ")
	if err == nil || err.Error() != "comment body is required" {
		t.Fatalf("want comment-body error for whitespace, got %v", err)
	}
}

func TestCreateIssueComment_InvalidFullName(t *testing.T) {
	c := NewClient()
	_, err := c.CreateIssueComment(context.Background(), "tok", "noslash", 1, "hello")
	if err == nil {
		t.Fatal("want error for invalid full name, got nil")
	}
}

// ---------------------------------------------------------------------------
// CreateIssueComment — successful path
// ---------------------------------------------------------------------------

func TestCreateIssueComment_Success(t *testing.T) {
	srv := serverSequence(validCommentResponse(42, "hello world"))
	defer srv.Close()

	c := commentClient(srv, 3, 50*time.Millisecond)
	// Rewrite the URL: the client builds github.com URLs; we swap to our test server.
	// We do this by replacing the transport base with a server-redirecting transport.
	c.HTTP.Transport = &urlRewriteTransport{
		base:    c.HTTP.Transport,
		replace: "https://api.github.com",
		with:    srv.URL,
	}

	got, err := c.CreateIssueComment(context.Background(), "token123", "owner/repo", 1, "hello world")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != 42 {
		t.Fatalf("want ID 42, got %d", got.ID)
	}
	if got.Body != "hello world" {
		t.Fatalf("want body 'hello world', got %q", got.Body)
	}
	if got.User.Login != "bot" {
		t.Fatalf("want login 'bot', got %q", got.User.Login)
	}
}

// ---------------------------------------------------------------------------
// CreateIssueComment — secondary rate limit (core requirement)
// ---------------------------------------------------------------------------

// TestCreateIssueComment_SecondaryRateLimit_Exhausted verifies that when
// RateLimitTransport exhausts all retries on a secondary rate limit, the error
// wraps ErrSecondaryRateLimit and IsSecondaryRateLimited returns true.
func TestCreateIssueComment_SecondaryRateLimit_Exhausted(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		secondaryLimitHandler(0)(w, r)
	}))
	defer srv.Close()

	c := commentClient(srv, 2, 50*time.Millisecond)
	c.HTTP.Transport = &urlRewriteTransport{
		base:    c.HTTP.Transport,
		replace: "https://api.github.com",
		with:    srv.URL,
	}

	_, err := c.CreateIssueComment(context.Background(), "tok", "owner/repo", 1, "body")
	if err == nil {
		t.Fatal("want error, got nil")
	}

	// Must be detectable via the sentinel helper.
	if !IsSecondaryRateLimited(err) {
		t.Fatalf("IsSecondaryRateLimited = false, want true; err = %v", err)
	}
	if !errors.Is(err, ErrSecondaryRateLimit) {
		t.Fatalf("errors.Is(err, ErrSecondaryRateLimit) = false; err = %v", err)
	}

	// Transport made initial + 2 retry attempts = 3 total.
	if atomic.LoadInt64(&calls) != 3 {
		t.Fatalf("want 3 calls, got %d", calls)
	}
}

// TestCreateIssueComment_SecondaryRateLimit_RetryAfterHonored verifies that
// when Retry-After is present, the transport sleeps accordingly before
// retrying, and a subsequent success is returned normally.
func TestCreateIssueComment_SecondaryRateLimit_RetryAfterHonored(t *testing.T) {
	var calls int64
	srv := serverSequence(
		// First call: secondary rate limit with Retry-After: 0 (instant retry in tests).
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&calls, 1)
			secondaryLimitHandler(0)(w, r)
		},
		// Second call: success.
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&calls, 1)
			validCommentResponse(99, "ok")(w, r)
		},
	)
	defer srv.Close()

	c := commentClient(srv, 3, 50*time.Millisecond)
	c.HTTP.Transport = &urlRewriteTransport{
		base:    c.HTTP.Transport,
		replace: "https://api.github.com",
		with:    srv.URL,
	}

	got, err := c.CreateIssueComment(context.Background(), "tok", "owner/repo", 1, "body")
	if err != nil {
		t.Fatalf("want success after retry, got %v", err)
	}
	if got.ID != 99 {
		t.Fatalf("want ID 99, got %d", got.ID)
	}
	if atomic.LoadInt64(&calls) != 2 {
		t.Fatalf("want 2 calls (1 rate-limit + 1 success), got %d", calls)
	}
}

// TestCreateIssueComment_SecondaryRateLimit_RetryAfterInError verifies that
// the Retry-After value appears in the error message when retries are exhausted.
func TestCreateIssueComment_SecondaryRateLimit_RetryAfterInError(t *testing.T) {
	srv := httptest.NewServer(secondaryLimitHandler(42))
	defer srv.Close()

	c := commentClient(srv, 0, 50*time.Millisecond) // 0 → DefaultMaxRetries=3
	c.HTTP.Transport = &urlRewriteTransport{
		base:    c.HTTP.Transport,
		replace: "https://api.github.com",
		with:    srv.URL,
	}

	_, err := c.CreateIssueComment(context.Background(), "tok", "owner/repo", 1, "body")
	if !IsSecondaryRateLimited(err) {
		t.Fatalf("want secondary rate limit error, got %v", err)
	}
	if err.Error() != "github secondary rate limit exhausted (Retry-After: 42)" {
		t.Fatalf("unexpected error message: %q", err.Error())
	}
}

// TestCreateIssueComment_BurstSimulation simulates a burst of 5 concurrent
// comment posts where the first two hit a secondary rate limit before
// succeeding.  Verifies none of the callers corrupts the shared rate limiter.
func TestCreateIssueComment_BurstSimulation(t *testing.T) {
	var (
		calls   int64
		limited int64
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		// First two distinct calls get rate-limited; rest succeed.
		if n <= 2 {
			atomic.AddInt64(&limited, 1)
			secondaryLimitHandler(0)(w, r)
			return
		}
		validCommentResponse(n, "burst")(w, r)
	}))
	defer srv.Close()

	results := make(chan error, 5)
	for i := 0; i < 5; i++ {
		go func(n int) {
			rt := NewRateLimitTransport(nil)
			rt.MaxRetries = 3
			rt.MaxWait = 50 * time.Millisecond
			// Each goroutine gets its own urlRewriteTransport to avoid a data race
			// on the shared rewrite.base field.
			cl := &Client{
				HTTP: &http.Client{Transport: &urlRewriteTransport{
					base:    rt,
					replace: "https://api.github.com",
					with:    srv.URL,
				}},
				UserAgent: "test",
			}
			_, err := cl.CreateIssueComment(context.Background(), "tok", "owner/repo", n+1, "burst")
			results <- err
		}(i)
	}

	var errs int
	for i := 0; i < 5; i++ {
		if err := <-results; err != nil {
			errs++
		}
	}

	// With 5 goroutines and only 2 rate-limited slots (before counter moves past 2),
	// all goroutines should eventually succeed.
	if errs > 0 {
		t.Fatalf("want 0 errors in burst, got %d", errs)
	}
}

// ---------------------------------------------------------------------------
// CreateIssueComment — 5xx (GitHub outage)
// ---------------------------------------------------------------------------

// TestCreateIssueComment_ServerError verifies that a 5xx is not retried by
// RateLimitTransport (it only retries rate-limit codes) and is returned as a
// non-rate-limit error.
func TestCreateIssueComment_ServerError(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, `{"message":"Service Unavailable"}`)
	}))
	defer srv.Close()

	c := commentClient(srv, 3, 50*time.Millisecond)
	c.HTTP.Transport = &urlRewriteTransport{
		base:    c.HTTP.Transport,
		replace: "https://api.github.com",
		with:    srv.URL,
	}

	_, err := c.CreateIssueComment(context.Background(), "tok", "owner/repo", 1, "body")
	if err == nil {
		t.Fatal("want error on 503, got nil")
	}
	if IsSecondaryRateLimited(err) {
		t.Fatal("5xx must NOT be classified as secondary rate limit")
	}
	// 503 is not a rate-limit code so transport never retries — exactly 1 call.
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("want 1 call (no retry on 5xx), got %d", calls)
	}
}

// TestCreateIssueComment_RecoverAfter5xx verifies that a transient 5xx followed
// by a success is not retried by the transport (5xx is not a retry trigger).
// The caller is responsible for higher-level retry on outage errors.
func TestCreateIssueComment_RecoverAfter5xx(t *testing.T) {
	var calls int64
	srv := serverSequence(
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&calls, 1)
			w.WriteHeader(http.StatusInternalServerError)
		},
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&calls, 1)
			validCommentResponse(7, "body")(w, r)
		},
	)
	defer srv.Close()

	c := commentClient(srv, 3, 50*time.Millisecond)
	c.HTTP.Transport = &urlRewriteTransport{
		base:    c.HTTP.Transport,
		replace: "https://api.github.com",
		with:    srv.URL,
	}

	// First call hits 500 — transport returns it unchanged (not a rate-limit).
	_, err := c.CreateIssueComment(context.Background(), "tok", "owner/repo", 1, "body")
	if err == nil {
		t.Fatal("want error on first 500 call")
	}

	// Second call (caller retries) hits success.
	got, err := c.CreateIssueComment(context.Background(), "tok", "owner/repo", 1, "body")
	if err != nil {
		t.Fatalf("want success on second call, got %v", err)
	}
	if got.ID != 7 {
		t.Fatalf("want ID 7, got %d", got.ID)
	}
}

// ---------------------------------------------------------------------------
// CreateIssueComment — 403 without Retry-After (auth failure, not rate limit)
// ---------------------------------------------------------------------------

// TestCreateIssueComment_AuthFailureNotRateLimit verifies that a bare 403
// (no Retry-After, no X-RateLimit-Remaining:0) is NOT classified as a
// secondary rate limit — it's an auth/permission failure.
func TestCreateIssueComment_AuthFailureNotRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Plain 403: no rate-limit headers.
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintln(w, `{"message":"Must have push access to create a comment"}`)
	}))
	defer srv.Close()

	c := commentClient(srv, 3, 50*time.Millisecond)
	c.HTTP.Transport = &urlRewriteTransport{
		base:    c.HTTP.Transport,
		replace: "https://api.github.com",
		with:    srv.URL,
	}

	_, err := c.CreateIssueComment(context.Background(), "tok", "owner/repo", 1, "body")
	if err == nil {
		t.Fatal("want error on 403, got nil")
	}
	if IsSecondaryRateLimited(err) {
		t.Fatalf("bare 403 must NOT be secondary rate limit; got %v", err)
	}
}

// ---------------------------------------------------------------------------
// CreateIssueComment — missing Retry-After fallback
// ---------------------------------------------------------------------------

// TestCreateIssueComment_PrimaryRateLimit_NoRetryAfter verifies that a primary
// rate limit (X-RateLimit-Remaining:0, no Retry-After) is retried and, if
// exhausted, does NOT set ErrSecondaryRateLimit.
func TestCreateIssueComment_PrimaryRateLimit_NoRetryAfter(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		primaryLimitHandler()(w, r)
	}))
	defer srv.Close()

	c := commentClient(srv, 2, 50*time.Millisecond)
	c.HTTP.Transport = &urlRewriteTransport{
		base:    c.HTTP.Transport,
		replace: "https://api.github.com",
		with:    srv.URL,
	}

	_, err := c.CreateIssueComment(context.Background(), "tok", "owner/repo", 1, "body")
	if err == nil {
		t.Fatal("want error after exhausted primary rate limit")
	}
	// Primary limit (no Retry-After) must NOT be classified as secondary.
	if IsSecondaryRateLimited(err) {
		t.Fatal("primary rate limit must not satisfy IsSecondaryRateLimited")
	}
	// 1 initial + 2 retries = 3 total.
	if atomic.LoadInt64(&calls) != 3 {
		t.Fatalf("want 3 calls, got %d", calls)
	}
}

// ---------------------------------------------------------------------------
// CreateIssueComment — 429 (Too Many Requests) variants
// ---------------------------------------------------------------------------

// TestCreateIssueComment_429WithRetryAfter verifies that a 429 response with
// Retry-After is treated as a secondary rate limit (same as 403+Retry-After).
// GitHub may use either status code for secondary limits.
func TestCreateIssueComment_429WithRetryAfter(t *testing.T) {
	var calls int64
	srv := serverSequence(
		// First call: 429 with Retry-After (secondary limit via status 429).
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&calls, 1)
			w.Header().Set("Retry-After", "0") // 0s for test speed
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintln(w, `{"message":"secondary rate limit"}`)
		},
		// Second call: success.
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&calls, 1)
			validCommentResponse(55, "ok")(w, r)
		},
	)
	defer srv.Close()

	c := commentClient(srv, 3, 50*time.Millisecond)
	c.HTTP.Transport = &urlRewriteTransport{
		base:    c.HTTP.Transport,
		replace: "https://api.github.com",
		with:    srv.URL,
	}

	got, err := c.CreateIssueComment(context.Background(), "tok", "owner/repo", 1, "body")
	if err != nil {
		t.Fatalf("want success after 429 retry, got %v", err)
	}
	if got.ID != 55 {
		t.Fatalf("want ID 55, got %d", got.ID)
	}
	if atomic.LoadInt64(&calls) != 2 {
		t.Fatalf("want 2 calls, got %d", calls)
	}
}

// TestCreateIssueComment_429Exhausted verifies that a persistent 429+Retry-After
// response returns ErrSecondaryRateLimit once all retries are exhausted.
func TestCreateIssueComment_429Exhausted(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintln(w, `{"message":"secondary rate limit"}`)
	}))
	defer srv.Close()

	c := commentClient(srv, 2, 50*time.Millisecond) // 2 retries → 3 total calls
	c.HTTP.Transport = &urlRewriteTransport{
		base:    c.HTTP.Transport,
		replace: "https://api.github.com",
		with:    srv.URL,
	}

	_, err := c.CreateIssueComment(context.Background(), "tok", "owner/repo", 1, "body")
	if !IsSecondaryRateLimited(err) {
		t.Fatalf("want ErrSecondaryRateLimit for exhausted 429, got %v", err)
	}
	if atomic.LoadInt64(&calls) != 3 {
		t.Fatalf("want 3 calls, got %d", calls)
	}
}

// TestCreateIssueComment_429WithoutRetryAfter verifies that a bare 429 (no
// Retry-After, no X-RateLimit-Remaining:0) is not retried by the transport and
// is NOT classified as a secondary rate limit — it is an unrecognised 4xx.
func TestCreateIssueComment_429WithoutRetryAfter(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		// Bare 429 — no rate-limit indicator headers.
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintln(w, `{"message":"too many requests"}`)
	}))
	defer srv.Close()

	c := commentClient(srv, 3, 50*time.Millisecond)
	c.HTTP.Transport = &urlRewriteTransport{
		base:    c.HTTP.Transport,
		replace: "https://api.github.com",
		with:    srv.URL,
	}

	_, err := c.CreateIssueComment(context.Background(), "tok", "owner/repo", 1, "body")
	if err == nil {
		t.Fatal("want error on bare 429, got nil")
	}
	if IsSecondaryRateLimited(err) {
		t.Fatal("bare 429 without Retry-After must NOT be secondary rate limit")
	}
	// Transport does not retry bare 429 (no rate-limit headers) → exactly 1 call.
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("want 1 call (no retry on bare 429), got %d", calls)
	}
}

// ---------------------------------------------------------------------------
// CreateIssueComment — HTMLURL is populated on success
// ---------------------------------------------------------------------------

// TestCreateIssueComment_HTMLURLPopulated verifies that the HTMLURL field
// returned by GitHub is propagated into the IssueComment result.
func TestCreateIssueComment_HTMLURLPopulated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		now := time.Now().UTC().Format(time.RFC3339)
		fmt.Fprintf(w,
			`{"id":100,"body":"hi","user":{"login":"bot"},"html_url":"https://github.com/owner/repo/issues/1#issuecomment-100","created_at":%q,"updated_at":%q}`,
			now, now)
	}))
	defer srv.Close()

	c := commentClient(srv, 1, 50*time.Millisecond)
	c.HTTP.Transport = &urlRewriteTransport{
		base:    c.HTTP.Transport,
		replace: "https://api.github.com",
		with:    srv.URL,
	}

	got, err := c.CreateIssueComment(context.Background(), "tok", "owner/repo", 1, "hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "https://github.com/owner/repo/issues/1#issuecomment-100"
	if got.HTMLURL != want {
		t.Fatalf("HTMLURL = %q, want %q", got.HTMLURL, want)
	}
}

// ---------------------------------------------------------------------------
// DeleteIssueComment — additional validation
// ---------------------------------------------------------------------------

// TestDeleteIssueComment_InvalidFullName verifies that an unparseable full name
// returns an error for delete, same as create.
func TestDeleteIssueComment_InvalidFullName(t *testing.T) {
	c := NewClient()
	err := c.DeleteIssueComment(context.Background(), "tok", "noslash", 1)
	if err == nil {
		t.Fatal("want error for invalid full name in delete, got nil")
	}
}

// TestDeleteIssueComment_MissingToken verifies that an empty access token
// returns an error, consistent with CreateIssueComment.
func TestDeleteIssueComment_MissingToken(t *testing.T) {
	c := NewClient()
	err := c.DeleteIssueComment(context.Background(), "", "owner/repo", 1)
	if err == nil || err.Error() != "missing github access token" {
		t.Fatalf("want missing-token error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// CreateIssueComment — context cancellation
// ---------------------------------------------------------------------------

// TestCreateIssueComment_ContextCancel verifies that cancelling the context
// during a Retry-After sleep returns a context error promptly.
func TestCreateIssueComment_ContextCancel(t *testing.T) {
	srv := httptest.NewServer(secondaryLimitHandler(60)) // asks for 60s wait
	defer srv.Close()

	c := commentClient(srv, 3, 10*time.Second)
	c.HTTP.Transport = &urlRewriteTransport{
		base:    c.HTTP.Transport,
		replace: "https://api.github.com",
		with:    srv.URL,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.CreateIssueComment(ctx, "tok", "owner/repo", 1, "body")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want context error, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("context cancellation not respected: elapsed %s", elapsed)
	}
}

// ---------------------------------------------------------------------------
// DeleteIssueComment — validation and success
// ---------------------------------------------------------------------------

func TestDeleteIssueComment_InvalidID(t *testing.T) {
	c := NewClient()
	err := c.DeleteIssueComment(context.Background(), "tok", "owner/repo", 0)
	if err == nil {
		t.Fatal("want error for commentID=0")
	}
}

func TestDeleteIssueComment_NegativeID(t *testing.T) {
	c := NewClient()
	err := c.DeleteIssueComment(context.Background(), "tok", "owner/repo", -5)
	if err == nil {
		t.Fatal("want error for negative commentID")
	}
}

func TestDeleteIssueComment_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("want DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := commentClient(srv, 3, 50*time.Millisecond)
	c.HTTP.Transport = &urlRewriteTransport{
		base:    c.HTTP.Transport,
		replace: "https://api.github.com",
		with:    srv.URL,
	}

	if err := c.DeleteIssueComment(context.Background(), "tok", "owner/repo", 123); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDeleteIssueComment_SecondaryRateLimit verifies ErrSecondaryRateLimit is
// returned for delete operations too.
func TestDeleteIssueComment_SecondaryRateLimit(t *testing.T) {
	srv := httptest.NewServer(secondaryLimitHandler(0))
	defer srv.Close()

	c := commentClient(srv, 2, 50*time.Millisecond)
	c.HTTP.Transport = &urlRewriteTransport{
		base:    c.HTTP.Transport,
		replace: "https://api.github.com",
		with:    srv.URL,
	}

	err := c.DeleteIssueComment(context.Background(), "tok", "owner/repo", 1)
	if !IsSecondaryRateLimited(err) {
		t.Fatalf("want ErrSecondaryRateLimit, got %v", err)
	}
}

// TestDeleteIssueComment_ContextCancel verifies context cancellation during
// a rate-limit sleep on delete.
func TestDeleteIssueComment_ContextCancel(t *testing.T) {
	srv := httptest.NewServer(secondaryLimitHandler(60))
	defer srv.Close()

	c := commentClient(srv, 3, 10*time.Second)
	c.HTTP.Transport = &urlRewriteTransport{
		base:    c.HTTP.Transport,
		replace: "https://api.github.com",
		with:    srv.URL,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := c.DeleteIssueComment(ctx, "tok", "owner/repo", 1)
	if err == nil {
		t.Fatal("want context error, got nil")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("context cancellation not respected on delete")
	}
}

// ---------------------------------------------------------------------------
// ErrSecondaryRateLimit sentinel
// ---------------------------------------------------------------------------

func TestIsSecondaryRateLimited_Nil(t *testing.T) {
	if IsSecondaryRateLimited(nil) {
		t.Fatal("IsSecondaryRateLimited(nil) must be false")
	}
}

func TestIsSecondaryRateLimited_OtherError(t *testing.T) {
	if IsSecondaryRateLimited(errors.New("something else")) {
		t.Fatal("IsSecondaryRateLimited(other) must be false")
	}
}

func TestIsSecondaryRateLimited_Sentinel(t *testing.T) {
	if !IsSecondaryRateLimited(ErrSecondaryRateLimit) {
		t.Fatal("IsSecondaryRateLimited(ErrSecondaryRateLimit) must be true")
	}
}

func TestIsSecondaryRateLimited_WrappedSentinel(t *testing.T) {
	wrapped := fmt.Errorf("outer: %w", ErrSecondaryRateLimit)
	if !IsSecondaryRateLimited(wrapped) {
		t.Fatal("IsSecondaryRateLimited(wrapped sentinel) must be true")
	}
}

func TestSecondaryRateLimitError_MessageWithRetryAfter(t *testing.T) {
	err := &secondaryRateLimitError{retryAfter: "30"}
	want := "github secondary rate limit exhausted (Retry-After: 30)"
	if err.Error() != want {
		t.Fatalf("got %q, want %q", err.Error(), want)
	}
}

func TestSecondaryRateLimitError_MessageWithoutRetryAfter(t *testing.T) {
	err := &secondaryRateLimitError{}
	want := "github secondary rate limit exhausted"
	if err.Error() != want {
		t.Fatalf("got %q, want %q", err.Error(), want)
	}
}

// ---------------------------------------------------------------------------
// urlRewriteTransport — test helper that redirects api.github.com → test server
// ---------------------------------------------------------------------------

// urlRewriteTransport rewrites request URLs so calls to the real GitHub API
// are redirected to a test server, allowing full integration-style tests
// without any live network access.
type urlRewriteTransport struct {
	base    http.RoundTripper
	replace string // prefix to replace (e.g. "https://api.github.com")
	with    string // replacement (e.g. "http://127.0.0.1:PORT")
}

func (t *urlRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone to avoid mutating the caller's request.
	clone := req.Clone(req.Context())
	original := clone.URL.String()
	rewritten := original
	if len(t.replace) > 0 && len(t.with) > 0 {
		rewritten = t.with + original[len(t.replace):]
	}
	newURL, err := clone.URL.Parse(rewritten)
	if err != nil {
		return nil, err
	}
	clone.URL = newURL
	clone.Host = newURL.Host
	return t.base.RoundTrip(clone)
}
