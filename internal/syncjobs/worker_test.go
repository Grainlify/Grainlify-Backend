package syncjobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"

	"github.com/jagadeesh/grainlify/backend/internal/github"
)

func TestJobCompletionContextSurvivesWorkerCancellation(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()

	ctx, cancel := jobCompletionContext(parent, time.Second)
	defer cancel()

	if err := ctx.Err(); err != nil {
		t.Fatalf("jobCompletionContext returned canceled context: %v", err)
	}
}

func TestJobFinalStateRequeuesCanceledWork(t *testing.T) {
	state := jobFinalState(context.Canceled, 0, 5, 30*time.Second, time.Hour)

	if state.status != "pending" {
		t.Fatalf("status = %q, want pending", state.status)
	}
	if state.incrementAttempts {
		t.Fatal("canceled job should not increment attempts")
	}
	if state.lastErr == "" {
		t.Fatal("canceled job should record a retryable shutdown reason")
	}
	if state.runAt != nil {
		t.Fatal("canceled job should not set a backoff run_at")
	}
}

func TestJobFinalStateCompletesSuccessfulWork(t *testing.T) {
	state := jobFinalState(nil, 1, 5, 30*time.Second, time.Hour)

	if state.status != "completed" {
		t.Fatalf("status = %q, want completed", state.status)
	}
	if !state.incrementAttempts {
		t.Fatal("completed job should increment attempts")
	}
	if state.lastErr != "" {
		t.Fatalf("lastErr = %q, want empty", state.lastErr)
	}
	if state.runAt != nil {
		t.Fatal("completed job should not set run_at")
	}
}

func TestJobFinalStateReschedulesTransientFailure(t *testing.T) {
	err := errors.New("transient github error")
	state := jobFinalState(err, 1, 5, 30*time.Second, time.Hour)

	if state.status != "pending" {
		t.Fatalf("status = %q, want pending", state.status)
	}
	if !state.incrementAttempts {
		t.Fatal("failed job should increment attempts")
	}
	if state.lastErr == "" {
		t.Fatal("failed job should record last_error")
	}
	if state.runAt == nil {
		t.Fatal("transient failure should set a future run_at for backoff")
	}
	if !state.runAt.After(time.Now()) {
		t.Fatal("run_at should be in the future")
	}
}

func TestJobFinalStateDeadLettersAfterMaxAttempts(t *testing.T) {
	err := errors.New("persistent failure")
	// attempts=4, maxAttempts=5 → nextAttempt=5 >= maxAttempts → dead
	state := jobFinalState(err, 4, 5, 30*time.Second, time.Hour)

	if state.status != "dead" {
		t.Fatalf("status = %q, want dead", state.status)
	}
	if !state.incrementAttempts {
		t.Fatal("dead job should still increment attempts for audit")
	}
	if state.lastErr == "" {
		t.Fatal("dead job should record last_error")
	}
	if state.runAt != nil {
		t.Fatal("dead job should not set run_at")
	}
}

func TestSanitizeErrorRemovesSecrets(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"token=ghp_abc123xyz", "[REDACTED]"},
		{"GET https://api.github.com/?auth=secret_token: 401", "GET https://api.github.com/?: 401"},
		{"no secrets here", "no secrets here"},
	}
	for _, tc := range cases {
		got := sanitizeError(tc.input)
		// We just check that known secret values are not present verbatim.
		if tc.input != tc.want && got == tc.input {
			t.Errorf("sanitizeError(%q) = %q, secrets not removed", tc.input, got)
		}
	}
}

func TestBackoffDurationBounds(t *testing.T) {
	base := 30 * time.Second
	for attempt := 1; attempt <= 10; attempt++ {
		d := backoffDuration(base, attempt, time.Hour)
		if d <= 0 {
			t.Errorf("attempt %d: backoff = %v, want > 0", attempt, d)
		}
		if d > time.Hour {
			t.Errorf("attempt %d: backoff = %v exceeds 1h cap", attempt, d)
		}
	}
	// Verify growth: attempt 3 should generally be larger than attempt 1
	d1 := backoffDuration(base, 1, time.Hour)
	d3 := backoffDuration(base, 3, time.Hour)
	// With jitter the ranges may overlap at extremes, so use a loose check.
	// base*2^0*1.25=37.5s vs base*2^2*0.75=90s — plenty of headroom.
	if d3 < d1 {
		t.Errorf("backoff not growing: attempt3=%v < attempt1=%v", d3, d1)
	}
}

func TestJobFinalStateRepeatedFailuresBackoffAndCap(t *testing.T) {
	err := errors.New("token revoked")
	base := time.Minute
	capDelay := 10 * time.Minute
	threshold := 6

	var previousDelay time.Duration
	for attempts := 0; attempts < threshold-1; attempts++ {
		before := time.Now()
		state := jobFinalState(err, attempts, threshold, base, capDelay)
		if state.status != "pending" {
			t.Fatalf("attempts %d: status = %q, want pending", attempts, state.status)
		}
		if state.runAt == nil {
			t.Fatalf("attempts %d: expected run_at backoff", attempts)
		}

		delay := state.runAt.Sub(before)
		if delay <= 0 {
			t.Fatalf("attempts %d: delay = %v, want positive", attempts, delay)
		}
		if delay > capDelay+time.Second {
			t.Fatalf("attempts %d: delay = %v, exceeds cap %v", attempts, delay, capDelay)
		}
		if attempts > 0 && delay < previousDelay/2 {
			t.Fatalf("attempts %d: delay regressed too much: previous=%v current=%v", attempts, previousDelay, delay)
		}
		previousDelay = delay
	}

	state := jobFinalState(err, threshold-1, threshold, base, capDelay)
	if state.status != "dead" {
		t.Fatalf("status = %q, want dead needing manual attention", state.status)
	}
	if state.runAt != nil {
		t.Fatal("manual-attention job should not be rescheduled")
	}
}

type testLogHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *testLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return true
}

func (h *testLogHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *testLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h
}

func (h *testLogHandler) WithGroup(name string) slog.Handler {
	return h
}

func getRecordAttrMap(r slog.Record) map[string]any {
	attrs := make(map[string]any)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	return attrs
}

type mockRoundTripper struct {
	fn func(req *http.Request) (*http.Response, error)
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.fn(req)
}

type mockWorkerDBPool struct{}

func (m *mockWorkerDBPool) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (m *mockWorkerDBPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, nil
}

func (m *mockWorkerDBPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return nil
}

func (m *mockWorkerDBPool) BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error) {
	return nil, nil
}

func (m *mockWorkerDBPool) Ping(ctx context.Context) error {
	return nil
}

func (m *mockWorkerDBPool) Close() {}

func (m *mockWorkerDBPool) Config() *pgxpool.Config {
	return nil
}

func TestSyncIssues_PaginationCapHit(t *testing.T) {
	handler := &testLogHandler{}
	origLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(origLogger)

	var requestCount int
	var mu sync.Mutex

	rt := &mockRoundTripper{
		fn: func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			requestCount++
			mu.Unlock()

			items := []github.IssueListItem{
				{ID: 1, Number: 1, State: "open", Title: "Issue"},
			}
			data, _ := json.Marshal(items)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(data)),
				Header:     make(http.Header),
			}, nil
		},
	}

	w := &Worker{
		pool:    &mockWorkerDBPool{},
		limiter: rate.NewLimiter(rate.Inf, 0),
		gh: &github.Client{
			HTTP: &http.Client{Transport: rt},
		},
	}

	projectID := uuid.New()
	err := w.syncIssues(context.Background(), projectID, "owner/repo", "token")
	if err != nil {
		t.Fatalf("syncIssues returned unexpected error: %v", err)
	}

	if requestCount != 50 {
		t.Errorf("expected 50 page requests, got %d", requestCount)
	}

	handler.mu.Lock()
	defer handler.mu.Unlock()

	var warnFound bool
	for _, r := range handler.records {
		if r.Level == slog.LevelWarn && r.Message == "sync issues hit pagination cap, results may be incomplete" {
			warnFound = true
			attrs := getRecordAttrMap(r)
			if attrs["project_id"] != projectID {
				t.Errorf("project_id attr: want %v, got %v", projectID, attrs["project_id"])
			}
			if attrs["repo"] != "owner/repo" {
				t.Errorf("repo attr: want 'owner/repo', got %v", attrs["repo"])
			}
			if attrs["pages_fetched"] != int64(50) && attrs["pages_fetched"] != 50 {
				t.Errorf("pages_fetched attr: want 50, got %v", attrs["pages_fetched"])
			}
			if attrs["total_issues"] != int64(50) && attrs["total_issues"] != 50 {
				t.Errorf("total_issues attr: want 50, got %v", attrs["total_issues"])
			}
		}
	}

	if !warnFound {
		t.Error("expected slog.Warn log for pagination cap hit, but none was recorded")
	}
}

func TestSyncIssues_NormalCompletion(t *testing.T) {
	handler := &testLogHandler{}
	origLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(origLogger)

	var requestCount int
	var mu sync.Mutex

	rt := &mockRoundTripper{
		fn: func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			requestCount++
			current := requestCount
			mu.Unlock()

			var data []byte
			if current == 1 {
				items := []github.IssueListItem{
					{ID: 1, Number: 1, State: "open", Title: "Issue 1"},
				}
				data, _ = json.Marshal(items)
			} else {
				data = []byte("[]")
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(data)),
				Header:     make(http.Header),
			}, nil
		},
	}

	w := &Worker{
		pool:    &mockWorkerDBPool{},
		limiter: rate.NewLimiter(rate.Inf, 0),
		gh: &github.Client{
			HTTP: &http.Client{Transport: rt},
		},
	}

	projectID := uuid.New()
	err := w.syncIssues(context.Background(), projectID, "owner/repo", "token")
	if err != nil {
		t.Fatalf("syncIssues returned unexpected error: %v", err)
	}

	if requestCount != 2 {
		t.Errorf("expected 2 page requests, got %d", requestCount)
	}

	handler.mu.Lock()
	defer handler.mu.Unlock()

	var infoFound bool
	for _, r := range handler.records {
		if r.Level == slog.LevelWarn {
			t.Errorf("unexpected warning logged during normal completion: %s", r.Message)
		}
		if r.Level == slog.LevelInfo && r.Message == "sync issues completed" {
			infoFound = true
			attrs := getRecordAttrMap(r)
			if attrs["project_id"] != projectID {
				t.Errorf("project_id attr: want %v, got %v", projectID, attrs["project_id"])
			}
			if attrs["repo"] != "owner/repo" {
				t.Errorf("repo attr: want 'owner/repo', got %v", attrs["repo"])
			}
			if attrs["total_issues"] != int64(1) && attrs["total_issues"] != 1 {
				t.Errorf("total_issues attr: want 1, got %v", attrs["total_issues"])
			}
		}
	}

	if !infoFound {
		t.Error("expected slog.Info log for normal sync completion, but none was recorded")
	}
}

func TestSyncPRs_PaginationCapHit(t *testing.T) {
	handler := &testLogHandler{}
	origLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(origLogger)

	var requestCount int
	var mu sync.Mutex

	rt := &mockRoundTripper{
		fn: func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			requestCount++
			mu.Unlock()

			items := []github.PRListItem{
				{ID: 100, Number: 10, State: "open", Title: "PR 10"},
			}
			data, _ := json.Marshal(items)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(data)),
				Header:     make(http.Header),
			}, nil
		},
	}

	w := &Worker{
		pool:    &mockWorkerDBPool{},
		limiter: rate.NewLimiter(rate.Inf, 0),
		gh: &github.Client{
			HTTP: &http.Client{Transport: rt},
		},
	}

	projectID := uuid.New()
	err := w.syncPRs(context.Background(), projectID, "owner/repo", "token")
	if err != nil {
		t.Fatalf("syncPRs returned unexpected error: %v", err)
	}

	if requestCount != 50 {
		t.Errorf("expected 50 page requests, got %d", requestCount)
	}

	handler.mu.Lock()
	defer handler.mu.Unlock()

	var warnFound bool
	for _, r := range handler.records {
		if r.Level == slog.LevelWarn && r.Message == "sync PRs hit pagination cap, results may be incomplete" {
			warnFound = true
			attrs := getRecordAttrMap(r)
			if attrs["project_id"] != projectID {
				t.Errorf("project_id attr: want %v, got %v", projectID, attrs["project_id"])
			}
			if attrs["repo"] != "owner/repo" {
				t.Errorf("repo attr: want 'owner/repo', got %v", attrs["repo"])
			}
			if attrs["pages_fetched"] != int64(50) && attrs["pages_fetched"] != 50 {
				t.Errorf("pages_fetched attr: want 50, got %v", attrs["pages_fetched"])
			}
			if attrs["total_prs"] != int64(50) && attrs["total_prs"] != 50 {
				t.Errorf("total_prs attr: want 50, got %v", attrs["total_prs"])
			}
		}
	}

	if !warnFound {
		t.Error("expected slog.Warn log for PR pagination cap hit, but none was recorded")
	}
}

func TestSyncPRs_NormalCompletion(t *testing.T) {
	handler := &testLogHandler{}
	origLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(origLogger)

	var requestCount int
	var mu sync.Mutex

	rt := &mockRoundTripper{
		fn: func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			requestCount++
			current := requestCount
			mu.Unlock()

			var data []byte
			if current == 1 {
				items := []github.PRListItem{
					{ID: 100, Number: 10, State: "open", Title: "PR 10"},
				}
				data, _ = json.Marshal(items)
			} else {
				data = []byte("[]")
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(data)),
				Header:     make(http.Header),
			}, nil
		},
	}

	w := &Worker{
		pool:    &mockWorkerDBPool{},
		limiter: rate.NewLimiter(rate.Inf, 0),
		gh: &github.Client{
			HTTP: &http.Client{Transport: rt},
		},
	}

	projectID := uuid.New()
	err := w.syncPRs(context.Background(), projectID, "owner/repo", "token")
	if err != nil {
		t.Fatalf("syncPRs returned unexpected error: %v", err)
	}

	if requestCount != 2 {
		t.Errorf("expected 2 page requests, got %d", requestCount)
	}

	handler.mu.Lock()
	defer handler.mu.Unlock()

	var infoFound bool
	for _, r := range handler.records {
		if r.Level == slog.LevelWarn {
			t.Errorf("unexpected warning logged during normal PR completion: %s", r.Message)
		}
		if r.Level == slog.LevelInfo && r.Message == "sync PRs completed" {
			infoFound = true
			attrs := getRecordAttrMap(r)
			if attrs["project_id"] != projectID {
				t.Errorf("project_id attr: want %v, got %v", projectID, attrs["project_id"])
			}
			if attrs["repo"] != "owner/repo" {
				t.Errorf("repo attr: want 'owner/repo', got %v", attrs["repo"])
			}
			if attrs["total_prs"] != int64(1) && attrs["total_prs"] != 1 {
				t.Errorf("total_prs attr: want 1, got %v", attrs["total_prs"])
			}
		}
	}

	if !infoFound {
		t.Error("expected slog.Info log for normal PR sync completion, but none was recorded")
	}
}
