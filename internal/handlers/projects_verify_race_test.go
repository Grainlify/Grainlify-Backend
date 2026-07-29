// projects_verify_race_test.go exercises the SELECT ... FOR UPDATE re-check
// added to verifyAndWebhook (Issue #359). package handlers (not
// handlers_test) so verifyAndWebhook can be called directly and
// concurrently, bypassing Verify()'s in-process verifyInFlight guard
// entirely — that guard is a same-process fast path, not the correctness
// mechanism, so a meaningful test of the fix has to race two calls to
// verifyAndWebhook itself, each starting from the same stale
// existingWebhookID snapshot the original bug describes.
//
// This file is intentionally self-contained (its own DB-pool/mock-transport/
// seed helpers, distinctly named) rather than reusing similarly-shaped
// helpers from other test files, in case another in-flight PR in this same
// campaign adds same-named helpers of its own.
package handlers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/github"
	"github.com/jagadeesh/grainlify/backend/internal/migrate"
)

const verifyRaceTokenEncKeyB64 = "MDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDA=" // 32 raw bytes, base64-encoded

// openVerifyRaceTestPool connects to TEST_DB_URL and applies migrations,
// skipping the test if TEST_DB_URL is not set.
func openVerifyRaceTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DB_URL")
	if dsn == "" {
		t.Skip("TEST_DB_URL not set – skipping DB integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	if err := migrate.Up(ctx, pool, true); err != nil {
		pool.Close()
		t.Fatalf("migrate.Up: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// verifyRaceMockTransport answers GitHub's "get repo" and "create webhook"
// endpoints, counting webhook-creation calls and sleeping briefly on each one
// to reliably widen the window in which two concurrent verifyAndWebhook
// calls both reach the create-webhook decision point.
type verifyRaceMockTransport struct {
	webhookCreateCalls int64
}

func (m *verifyRaceMockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/repos/"):
		body := `{"id":1,"full_name":"acme/verify-race","private":false,"permissions":{"admin":true,"push":true,"pull":true}}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/hooks"):
		n := atomic.AddInt64(&m.webhookCreateCalls, 1)
		// Widen the overlap window between the two concurrent calls.
		time.Sleep(150 * time.Millisecond)
		body := fmt.Sprintf(`{"id":%d}`, 1000+n)
		return &http.Response{
			StatusCode: http.StatusCreated,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	default:
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	}
}

// seedVerifyRaceProject creates an owner user, a linked GitHub account for
// them, and a project row with webhook_id = NULL — the exact starting state
// two racing verifyAndWebhook calls would both observe.
func seedVerifyRaceProject(t *testing.T, pool *pgxpool.Pool) (projectID uuid.UUID, ownerID uuid.UUID, fullName string) {
	t.Helper()
	ctx := context.Background()

	if err := pool.QueryRow(ctx, `INSERT INTO users (role) VALUES ('maintainer') RETURNING id`).Scan(&ownerID); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, ownerID) })

	if err := github.StoreLinkedAccount(ctx, pool, ownerID, 42, "acme-owner", "", "fake-access-token", "bearer", "repo", verifyRaceTokenEncKeyB64); err != nil {
		t.Fatalf("store linked account: %v", err)
	}

	fullName = "acme/verify-race-" + uuid.NewString()
	if err := pool.QueryRow(ctx, `
INSERT INTO projects (owner_user_id, github_full_name, status)
VALUES ($1, $2, 'pending_verification') RETURNING id
`, ownerID, fullName).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, projectID) })

	return projectID, ownerID, fullName
}

// TestVerifyAndWebhook_ConcurrentCallsCreateExactlyOneWebhook races two
// verifyAndWebhook calls that both start from the same stale
// existingWebhookID = nil snapshot — exactly what happens today when two
// POST /projects/:id/verify requests for the same project both read
// webhook_id = NULL before either's background goroutine commits (Issue
// #359). Before the fix, both would call CreateWebhook independently; the
// SELECT ... FOR UPDATE re-check must ensure only one actually does.
func TestVerifyAndWebhook_ConcurrentCallsCreateExactlyOneWebhook(t *testing.T) {
	pool := openVerifyRaceTestPool(t)
	projectID, ownerID, fullName := seedVerifyRaceProject(t, pool)

	mock := &verifyRaceMockTransport{}
	origTransport := http.DefaultTransport
	http.DefaultTransport = mock
	t.Cleanup(func() { http.DefaultTransport = origTransport })

	h := NewProjectsHandler(config.Config{
		TokenEncKeyB64:      verifyRaceTokenEncKeyB64,
		PublicBaseURL:       "https://grainlify.example",
		GitHubWebhookSecret: "test-webhook-secret",
	}, &db.DB{Pool: pool})

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			// existingWebhookID = nil on both calls, mirroring the stale
			// snapshot each Verify() invocation captured before either
			// goroutine's transaction commits.
			h.verifyAndWebhook(context.Background(), projectID, ownerID, fullName, nil)
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&mock.webhookCreateCalls); got != 1 {
		t.Fatalf("CreateWebhook called %d times for two concurrent verifyAndWebhook calls on the same project, want exactly 1", got)
	}

	var status string
	var webhookID *int64
	if err := pool.QueryRow(context.Background(), `
SELECT status, webhook_id FROM projects WHERE id = $1
`, projectID).Scan(&status, &webhookID); err != nil {
		t.Fatalf("query final project state: %v", err)
	}
	if status != "verified" {
		t.Errorf("status = %q, want %q", status, "verified")
	}
	if webhookID == nil || *webhookID == 0 {
		t.Errorf("webhook_id not recorded on the project row")
	}
}
