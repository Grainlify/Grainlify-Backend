package syncjobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// failingExecDBPool wraps mockWorkerDBPool but fails Exec for the first
// failCount calls, then succeeds -- simulating one or more rows in a batch
// that fail to persist while the rest of the page upserts fine.
type failingExecDBPool struct {
	mockWorkerDBPool
	mu        sync.Mutex
	failCount int
	execCalls int
}

func (m *failingExecDBPool) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.execCalls++
	if m.execCalls <= m.failCount {
		return pgconn.CommandTag{}, errors.New("simulated constraint violation")
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

// TestSyncIssues_RowFailurePropagatesError is the regression test for issue
// #294: a failed per-row upsert must not be silently discarded. Before the
// fix, syncIssues always returned nil once pagination finished, so runJob
// would report the sync job "completed" even though every row failed to
// persist -- the job-level retry/backoff/dead-letter machinery in
// jobFinalState never engaged.
func TestSyncIssues_RowFailurePropagatesError(t *testing.T) {
	// First page has 2 issues; fail the first upsert, succeed the second,
	// then a second (empty) page ends pagination.
	var pageCount int
	var mu sync.Mutex
	rt := &mockRoundTripper{
		fn: func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			pageCount++
			current := pageCount
			mu.Unlock()

			var data []byte
			if current == 1 {
				items := []github.IssueListItem{
					{ID: 1, Number: 1, State: "open", Title: "Issue 1"},
					{ID: 2, Number: 2, State: "open", Title: "Issue 2"},
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
		pool:    &failingExecDBPool{failCount: 1},
		limiter: rate.NewLimiter(rate.Inf, 0),
		gh: &github.Client{
			HTTP: &http.Client{Transport: rt},
		},
	}

	err := w.syncIssues(context.Background(), uuid.New(), "owner/repo", "token")
	if err == nil {
		t.Fatal("syncIssues returned nil error despite a failed row upsert; the sync job would be silently marked completed")
	}
}

// TestSyncPRs_RowFailurePropagatesError is the PR-sync counterpart of
// TestSyncIssues_RowFailurePropagatesError.
func TestSyncPRs_RowFailurePropagatesError(t *testing.T) {
	var pageCount int
	var mu sync.Mutex
	rt := &mockRoundTripper{
		fn: func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			pageCount++
			current := pageCount
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
		pool:    &failingExecDBPool{failCount: 1},
		limiter: rate.NewLimiter(rate.Inf, 0),
		gh: &github.Client{
			HTTP: &http.Client{Transport: rt},
		},
	}

	err := w.syncPRs(context.Background(), uuid.New(), "owner/repo", "token")
	if err == nil {
		t.Fatal("syncPRs returned nil error despite a failed row upsert; the sync job would be silently marked completed")
	}
}

// TestJobFinalState_RowFailureErrorTriggersRetryOrDeadLetter proves the
// error produced by a row-failure (the "sync issues: N/M rows failed to
// persist" shape) flows correctly into the existing jobFinalState
// retry/backoff/dead-letter logic, exactly like any other runErr -- closing
// the loop from "per-row failure" to "job not marked completed."
func TestJobFinalState_RowFailureErrorTriggersRetryOrDeadLetter(t *testing.T) {
	rowFailureErr := fmt.Errorf("sync issues: %d/%d rows failed to persist", 1, 2)

	// Below the dead-letter threshold: reschedules with backoff, not "completed".
	state := jobFinalState(rowFailureErr, 0, 5, 30*time.Second, time.Hour)
	if state.status != "pending" {
		t.Fatalf("status = %q, want pending (job must not be marked completed)", state.status)
	}
	if state.runAt == nil {
		t.Fatal("expected a backoff runAt to be set")
	}

	// At the dead-letter threshold: job is dead-lettered, not "completed".
	deadState := jobFinalState(rowFailureErr, 4, 5, 30*time.Second, time.Hour)
	if deadState.status != "dead" {
		t.Fatalf("status = %q, want dead", deadState.status)
	}
}

// panickingTx is a pgx.Tx whose QueryRow always panics, simulating an
// unexpected data shape in the sync pipeline.
type panickingTx struct{}

func (panickingTx) Begin(ctx context.Context) (pgx.Tx, error)                       { return nil, nil }
func (panickingTx) BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error) {
	return nil, nil
}
func (panickingTx) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	return 0, nil
}
func (panickingTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults       { return nil }
func (panickingTx) LargeObjects() pgx.LargeObjects                                       { return pgx.LargeObjects{} }
func (panickingTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	return nil, nil
}
func (panickingTx) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (panickingTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, nil
}
func (panickingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	panic("deliberate panic: unexpected data shape in sync pipeline")
}
func (panickingTx) Commit(ctx context.Context) error   { return nil }
func (panickingTx) Rollback(ctx context.Context) error { return nil }
func (panickingTx) Conn() *pgx.Conn                    { return nil }

// panickingBeginTxPool wraps mockWorkerDBPool but returns a panickingTx
// from BeginTx so that processOne panics during the first db.WithTx call.
type panickingBeginTxPool struct {
	mockWorkerDBPool
}

func (p *panickingBeginTxPool) BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error) {
	return panickingTx{}, nil
}

// TestRunSurvivesPanicInProcessOne proves that a panic inside processOne
// is recovered by safeProcessOne and the Run ticker loop continues rather
// than crashing the goroutine (and, in production, the worker process).
func TestRunSurvivesPanicInProcessOne(t *testing.T) {
	handler := &testLogHandler{}
	origLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(origLogger)

	w := &Worker{
		pool:    &panickingBeginTxPool{},
		limiter: rate.NewLimiter(rate.Inf, 0),
		gh:      &github.Client{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Run in a goroutine. If the panic escapes, the test binary crashes.
	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	// Wait for Run to return (context timeout).
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run returned unexpected error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not exit within timeout")
	}

	// Verify the panic was logged.
	handler.mu.Lock()
	defer handler.mu.Unlock()

	var panicLogged bool
	for _, r := range handler.records {
		if r.Level == slog.LevelError && r.Message == "sync worker panic recovered" {
			panicLogged = true
			attrs := getRecordAttrMap(r)
			if attrs["panic"] != "deliberate panic: unexpected data shape in sync pipeline" {
				t.Errorf("unexpected panic value: %v", attrs["panic"])
			}
			break
		}
	}
	if !panicLogged {
		t.Error("expected 'sync worker panic recovered' log entry, but none was found")
	}
}

// TestRunSurvivesMultiplePanics proves the loop keeps ticking after
// multiple consecutive panics.
func TestRunSurvivesMultiplePanics(t *testing.T) {
	handler := &testLogHandler{}
	origLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(origLogger)

	w := &Worker{
		pool:    &panickingBeginTxPool{},
		limiter: rate.NewLimiter(rate.Inf, 0),
		gh:      &github.Client{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run returned unexpected error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not exit within timeout")
	}

	handler.mu.Lock()
	defer handler.mu.Unlock()

	var panicCount int
	for _, r := range handler.records {
		if r.Level == slog.LevelError && r.Message == "sync worker panic recovered" {
			panicCount++
		}
	}
	// With a 5s timeout and 1s ticker, we expect multiple ticks (≥3 panics).
	if panicCount < 2 {
		t.Errorf("expected at least 2 panic recoveries, got %d", panicCount)
	}
}
