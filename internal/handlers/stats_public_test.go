package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// ---------------------------------------------------------------------------
// Fixtures for LandingStatsHandler (internal/handlers/stats_public.go).
//
// Every identifier here is prefixed with "statsSuite" so it can't collide
// with fixtures other concurrently-developed *_test.go files in this
// package define for their own domains.
//
// Get() has no scoping query param - active_projects/contributors are
// global counts over the whole shared test database, which every other
// concurrently-running test suite (plus prior runs of these very tests -
// nothing here truncates rows) may also be adding verified projects and
// contributors to at any moment. Exact-equality assertions against these
// counts are therefore unsafe. Instead these tests snapshot the endpoint
// immediately before and after seeding known-new rows and assert the count
// grew by AT LEAST the amount this test itself contributed (a "lower-bound
// delta"): concurrent writers can only push the delta up, never down,
// since nothing here (or, per the task's own ground rules, anywhere else)
// deletes rows this test just inserted.
// ---------------------------------------------------------------------------

// statsSuiteNextGHUserID returns a fresh value for a BIGINT UNIQUE
// github_user_id column (users.github_user_id). It's randomized rather
// than derived from a per-file "base := time.Now().UnixNano()" plus an
// incrementing counter: this test binary links many other *_test.go files
// in this package that each define their own analogous base var, and since
// all package-level var initializers run within microseconds of each other
// at process startup, those bases were observed in practice to land on the
// identical nanosecond - producing systematically colliding github_user_id
// values across files/tests ("duplicate key value violates unique
// constraint users_github_user_id_key"). rand's top-level functions are
// auto-seeded and safe for concurrent use, and the ~63-bit random space
// makes collisions with anything else negligible.
func statsSuiteNextGHUserID() int64 {
	return rand.Int63()
}

// statsSuiteItemSeq mints unique github_issues/github_pull_requests
// (github_issue_id/github_pr_id, number) values; uniqueness is only
// required per-project, but a single global counter trivially satisfies that.
var statsSuiteItemSeq int64

// statsSuiteUser inserts a minimal row into users and returns its id.
func statsSuiteUser(t *testing.T, pool db.DBPool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO users (role, display_name, github_user_id)
VALUES ('contributor', $1, $2)
RETURNING id
`, "statssuite-user-"+uuid.New().String(), statsSuiteNextGHUserID()).Scan(&id)
	if err != nil {
		t.Fatalf("statsSuiteUser: insert user: %v", err)
	}
	return id
}

// statsSuiteProjectSpec configures statsSuiteProject.
type statsSuiteProjectSpec struct {
	OwnerUserID uuid.UUID
	Status      string // defaults to "verified"
	Deleted     bool
}

// statsSuiteProject inserts a projects row and returns its id.
func statsSuiteProject(t *testing.T, pool db.DBPool, spec statsSuiteProjectSpec) uuid.UUID {
	t.Helper()
	if spec.Status == "" {
		spec.Status = "verified"
	}
	fullName := fmt.Sprintf("statssuite-owner-%s/statssuite-repo-%s", uuid.New().String()[:8], uuid.New().String()[:8])
	var deletedAt *time.Time
	if spec.Deleted {
		now := time.Now()
		deletedAt = &now
	}
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO projects (owner_user_id, github_full_name, status, deleted_at)
VALUES ($1, $2, $3, $4)
RETURNING id
`, spec.OwnerUserID, fullName, spec.Status, deletedAt).Scan(&id)
	if err != nil {
		t.Fatalf("statsSuiteProject: insert project: %v", err)
	}
	return id
}

// statsSuiteIssue inserts an open github_issues row authored by authorLogin
// against projectID.
func statsSuiteIssue(t *testing.T, pool db.DBPool, projectID uuid.UUID, authorLogin string) {
	t.Helper()
	n := atomic.AddInt64(&statsSuiteItemSeq, 1)
	_, err := pool.Exec(context.Background(), `
INSERT INTO github_issues (project_id, github_issue_id, number, state, author_login)
VALUES ($1, $2, $3, 'open', $4)
`, projectID, n, n, authorLogin)
	if err != nil {
		t.Fatalf("statsSuiteIssue: insert issue: %v", err)
	}
}

// statsSuitePR inserts an open github_pull_requests row authored by
// authorLogin against projectID.
func statsSuitePR(t *testing.T, pool db.DBPool, projectID uuid.UUID, authorLogin string) {
	t.Helper()
	n := atomic.AddInt64(&statsSuiteItemSeq, 1)
	_, err := pool.Exec(context.Background(), `
INSERT INTO github_pull_requests (project_id, github_pr_id, number, state, author_login)
VALUES ($1, $2, $3, 'open', $4)
`, projectID, n, n, authorLogin)
	if err != nil {
		t.Fatalf("statsSuitePR: insert PR: %v", err)
	}
}

// newStatsSuiteApp wires a fiber app exposing exactly the route
// internal/api/api.go registers against handlers.LandingStatsHandler
// (unauthenticated, matching production - GET /stats/landing is registered
// outside any auth-guarded group in api.go).
func newStatsSuiteApp(d *db.DB) *fiber.App {
	h := handlers.NewLandingStatsHandler(d)
	app := fiber.New()
	app.Get("/stats/landing", h.Get())
	return app
}

// statsSuiteDoJSON issues a GET request against app and returns the status
// code and raw response body. A generous 20s timeout (matching the
// convention already used in projects_public_test.go) avoids flaking under
// load from concurrently-running test suites sharing this database.
func statsSuiteDoJSON(t *testing.T, app *fiber.App, path string) (int, []byte) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", path, nil), 20000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, body
}

// statsSuiteResponse mirrors handlers.LandingStatsResponse's JSON shape.
type statsSuiteResponse struct {
	ActiveProjects       int64 `json:"active_projects"`
	Contributors         int64 `json:"contributors"`
	GrantsDistributedUSD int64 `json:"grants_distributed_usd"`
}

// statsSuiteGet performs GET /stats/landing and decodes the response,
// failing the test on a non-200 status or decode error.
func statsSuiteGet(t *testing.T, app *fiber.App) statsSuiteResponse {
	t.Helper()
	status, body := statsSuiteDoJSON(t, app, "/stats/landing")
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", status, body)
	}
	var resp statsSuiteResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

// ---------------------------------------------------------------------------
// Get() - GET /stats/landing
// ---------------------------------------------------------------------------

func TestStatsPublicSuite_Get_ResponseHasExactExpectedFieldNames(t *testing.T) {
	d := testDB(t)
	app := newStatsSuiteApp(d)

	status, body := statsSuiteDoJSON(t, app, "/stats/landing")
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", status, body)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	for _, key := range []string{"active_projects", "contributors", "grants_distributed_usd"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("response missing expected field %q; got keys %v", key, raw)
		}
	}
	if len(raw) != 3 {
		t.Errorf("response has %d top-level fields %v, want exactly active_projects/contributors/grants_distributed_usd", len(raw), raw)
	}
}

func TestStatsPublicSuite_Get_ReflectsSeededProjectsAndContributors(t *testing.T) {
	d := testDB(t)
	app := newStatsSuiteApp(d)

	before := statsSuiteGet(t, app)

	owner1 := statsSuiteUser(t, d.Pool)
	owner2 := statsSuiteUser(t, d.Pool)
	p1 := statsSuiteProject(t, d.Pool, statsSuiteProjectSpec{OwnerUserID: owner1, Status: "verified"})
	p2 := statsSuiteProject(t, d.Pool, statsSuiteProjectSpec{OwnerUserID: owner2, Status: "verified"})

	aliceLogin := "statssuite-alice-" + uuid.New().String()[:8]
	bobLogin := "statssuite-bob-" + uuid.New().String()[:8]
	carolLogin := "statssuite-carol-" + uuid.New().String()[:8]
	statsSuiteIssue(t, d.Pool, p1, aliceLogin)
	statsSuitePR(t, d.Pool, p1, bobLogin)
	statsSuiteIssue(t, d.Pool, p2, carolLogin)

	after := statsSuiteGet(t, app)

	if got := after.ActiveProjects - before.ActiveProjects; got < 2 {
		t.Errorf("active_projects grew by %d, want >= 2 (this test added 2 new verified, non-deleted projects)", got)
	}
	if got := after.Contributors - before.Contributors; got < 3 {
		t.Errorf("contributors grew by %d, want >= 3 (this test added 3 new distinct contributor logins: %s, %s, %s)", got, aliceLogin, bobLogin, carolLogin)
	}
}

// TestStatsPublicSuite_Get_GrantsDistributedUSDIsHardcodedZero documents a
// known, intentional gap: stats_public.go's Get() doc comment states
// "Grants distributed is currently 0 (no payouts table implemented yet)",
// and the handler sets resp.GrantsDistributedUSD = 0 unconditionally
// (internal/handlers/stats_public.go:65), never deriving it from seeded
// data. This asserts the CURRENT actual value rather than assuming it
// should reflect any real grants data.
func TestStatsPublicSuite_Get_GrantsDistributedUSDIsHardcodedZero(t *testing.T) {
	d := testDB(t)
	app := newStatsSuiteApp(d)

	resp := statsSuiteGet(t, app)
	if resp.GrantsDistributedUSD != 0 {
		t.Errorf("grants_distributed_usd = %d, want 0 (hardcoded per stats_public.go Get(); no payouts table exists yet)", resp.GrantsDistributedUSD)
	}
}

// ---------------------------------------------------------------------------
// db_not_configured guard (needs no live DB).
// ---------------------------------------------------------------------------

func TestStatsPublicSuite_NilDBPool_ReturnsServiceUnavailable(t *testing.T) {
	h := handlers.NewLandingStatsHandler(&db.DB{Pool: nil})
	app := fiber.New()
	app.Get("/stats/landing", h.Get())

	status, body := statsSuiteDoJSON(t, app, "/stats/landing")
	if status != fiber.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503, body=%s", status, body)
	}
}
