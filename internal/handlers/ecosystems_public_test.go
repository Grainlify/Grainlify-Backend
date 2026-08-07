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
// Fixtures for EcosystemsPublicHandler (internal/handlers/ecosystems_public.go).
//
// Every identifier here is prefixed with "ecosystemsPublicSuite" so it can't
// collide with fixtures other concurrently-developed *_test.go files in this
// package define for their own domains.
// ---------------------------------------------------------------------------

// ecosystemsPublicSuiteNextGHUserID returns a fresh value for a BIGINT
// UNIQUE github_user_id column (users.github_user_id). It's randomized
// rather than derived from a per-file "base := time.Now().UnixNano()" plus
// an incrementing counter: this test binary links many other *_test.go
// files in this package that each define their own analogous base var, and
// since all package-level var initializers run within microseconds of each
// other at process startup, those bases were observed in practice to land
// on the identical nanosecond - producing systematically colliding
// github_user_id values across files/tests ("duplicate key value violates
// unique constraint users_github_user_id_key"). rand's top-level functions
// are auto-seeded and safe for concurrent use, and the ~63-bit random space
// makes collisions with anything else negligible.
func ecosystemsPublicSuiteNextGHUserID() int64 {
	return rand.Int63()
}

// ecosystemsPublicSuiteItemSeq is a process-wide monotonic counter used to
// mint unique github_issues/github_pull_requests (github_issue_id/github_pr_id,
// number) values. Uniqueness is only required per-project, but a single
// globally-increasing counter trivially satisfies that.
var ecosystemsPublicSuiteItemSeq int64

// ecosystemsPublicSuiteUser inserts a minimal row into users and returns its id.
func ecosystemsPublicSuiteUser(t *testing.T, pool db.DBPool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO users (role, display_name, github_user_id)
VALUES ('contributor', $1, $2)
RETURNING id
`, "ecopub-user-"+uuid.New().String(), ecosystemsPublicSuiteNextGHUserID()).Scan(&id)
	if err != nil {
		t.Fatalf("ecosystemsPublicSuiteUser: insert user: %v", err)
	}
	return id
}

// ecosystemsPublicSuiteEcoSpec configures ecosystemsPublicSuiteEcosystem.
// Zero values pick sensible defaults.
type ecosystemsPublicSuiteEcoSpec struct {
	Status       string // defaults to "active"
	About        *string
	Links        []string
	KeyAreas     []string
	Technologies []string
}

// ecosystemsPublicSuiteEcosystem inserts a uniquely-named/slugged ecosystem
// and returns (id, name).
func ecosystemsPublicSuiteEcosystem(t *testing.T, pool db.DBPool, spec ecosystemsPublicSuiteEcoSpec) (uuid.UUID, string) {
	t.Helper()
	if spec.Status == "" {
		spec.Status = "active"
	}
	suffix := uuid.New().String()
	name := "EcoPub Ecosystem " + suffix
	slug := "ecopub-ecosystem-" + suffix

	marshalOrEmptyArray := func(v []string) []byte {
		if len(v) == 0 {
			return []byte("[]")
		}
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("ecosystemsPublicSuiteEcosystem: marshal: %v", err)
		}
		return b
	}

	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO ecosystems (slug, name, status, about, links, key_areas, technologies)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id
`, slug, name, spec.Status, spec.About,
		marshalOrEmptyArray(spec.Links), marshalOrEmptyArray(spec.KeyAreas), marshalOrEmptyArray(spec.Technologies)).Scan(&id)
	if err != nil {
		t.Fatalf("ecosystemsPublicSuiteEcosystem: insert ecosystem: %v", err)
	}
	return id, name
}

// ecosystemsPublicSuiteProjectSpec configures ecosystemsPublicSuiteProject.
type ecosystemsPublicSuiteProjectSpec struct {
	OwnerUserID   uuid.UUID
	EcosystemID   uuid.UUID
	Status        string // defaults to "verified"
	NeedsMetadata bool
	Deleted       bool
}

// ecosystemsPublicSuiteProject inserts a projects row scoped to a given
// ecosystem and returns its id.
func ecosystemsPublicSuiteProject(t *testing.T, pool db.DBPool, spec ecosystemsPublicSuiteProjectSpec) uuid.UUID {
	t.Helper()
	if spec.Status == "" {
		spec.Status = "verified"
	}
	fullName := fmt.Sprintf("ecopub-owner-%s/ecopub-repo-%s", uuid.New().String()[:8], uuid.New().String()[:8])
	var deletedAt *time.Time
	if spec.Deleted {
		now := time.Now()
		deletedAt = &now
	}

	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO projects (owner_user_id, github_full_name, ecosystem_id, status, needs_metadata, deleted_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id
`, spec.OwnerUserID, fullName, spec.EcosystemID, spec.Status, spec.NeedsMetadata, deletedAt).Scan(&id)
	if err != nil {
		t.Fatalf("ecosystemsPublicSuiteProject: insert project: %v", err)
	}
	return id
}

// ecosystemsPublicSuiteIssue inserts a github_issues row for projectID.
func ecosystemsPublicSuiteIssue(t *testing.T, pool db.DBPool, projectID uuid.UUID, authorLogin, state string) {
	t.Helper()
	n := atomic.AddInt64(&ecosystemsPublicSuiteItemSeq, 1)
	_, err := pool.Exec(context.Background(), `
INSERT INTO github_issues (project_id, github_issue_id, number, state, author_login)
VALUES ($1, $2, $3, $4, $5)
`, projectID, n, n, state, authorLogin)
	if err != nil {
		t.Fatalf("ecosystemsPublicSuiteIssue: insert issue: %v", err)
	}
}

// ecosystemsPublicSuitePR inserts a github_pull_requests row for projectID.
func ecosystemsPublicSuitePR(t *testing.T, pool db.DBPool, projectID uuid.UUID, authorLogin, state string) {
	t.Helper()
	n := atomic.AddInt64(&ecosystemsPublicSuiteItemSeq, 1)
	_, err := pool.Exec(context.Background(), `
INSERT INTO github_pull_requests (project_id, github_pr_id, number, state, author_login)
VALUES ($1, $2, $3, $4, $5)
`, projectID, n, n, state, authorLogin)
	if err != nil {
		t.Fatalf("ecosystemsPublicSuitePR: insert PR: %v", err)
	}
}

// newEcosystemsPublicSuiteApp wires a fiber app exposing exactly the routes
// internal/api/api.go registers against handlers.EcosystemsPublicHandler
// (both unauthenticated, matching production - see api.go's registration of
// GET /ecosystems and GET /ecosystems/:id outside any auth-guarded group).
func newEcosystemsPublicSuiteApp(d *db.DB) *fiber.App {
	h := handlers.NewEcosystemsPublicHandler(d)
	app := fiber.New()
	app.Get("/ecosystems", h.ListActive())
	app.Get("/ecosystems/:id", h.GetByID())
	return app
}

// ecosystemsPublicSuiteDoJSON issues a GET request against app and returns
// the status code and raw response body. A generous 20s timeout (matching
// the convention already used in projects_public_test.go) avoids flaking
// under load from concurrently-running test suites sharing this database.
func ecosystemsPublicSuiteDoJSON(t *testing.T, app *fiber.App, path string) (int, []byte) {
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

// ---------------------------------------------------------------------------
// ListActive() - GET /ecosystems
// ---------------------------------------------------------------------------

func TestEcosystemsPublicSuite_ListActive_OnlyActiveEcosystemsReturned(t *testing.T) {
	d := testDB(t)
	app := newEcosystemsPublicSuiteApp(d)

	activeID, _ := ecosystemsPublicSuiteEcosystem(t, d.Pool, ecosystemsPublicSuiteEcoSpec{Status: "active"})
	_, inactiveName := ecosystemsPublicSuiteEcosystem(t, d.Pool, ecosystemsPublicSuiteEcoSpec{Status: "inactive"})

	status, body := ecosystemsPublicSuiteDoJSON(t, app, "/ecosystems")
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", status, body)
	}

	var resp struct {
		Ecosystems []map[string]any `json:"ecosystems"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	var foundActive, foundInactive bool
	for _, e := range resp.Ecosystems {
		if e["id"] == activeID.String() {
			foundActive = true
		}
		if e["name"] == inactiveName {
			foundInactive = true
		}
	}
	if !foundActive {
		t.Errorf("active ecosystem %s not found in ListActive() response", activeID)
	}
	if foundInactive {
		t.Errorf("inactive ecosystem %q leaked into ListActive() response; want only status='active' ecosystems", inactiveName)
	}
}

func TestEcosystemsPublicSuite_ListActive_ComputesProjectAndUserCounts(t *testing.T) {
	d := testDB(t)
	app := newEcosystemsPublicSuiteApp(d)

	ecoID, _ := ecosystemsPublicSuiteEcosystem(t, d.Pool, ecosystemsPublicSuiteEcoSpec{Status: "active"})
	owner1 := ecosystemsPublicSuiteUser(t, d.Pool)
	owner2 := ecosystemsPublicSuiteUser(t, d.Pool)
	owner3 := ecosystemsPublicSuiteUser(t, d.Pool)

	// owner1: 2 non-deleted projects (verified + pending_verification) - the
	// ListActive() query counts projects regardless of status, only
	// excluding soft-deleted rows, so both should count.
	ecosystemsPublicSuiteProject(t, d.Pool, ecosystemsPublicSuiteProjectSpec{OwnerUserID: owner1, EcosystemID: ecoID, Status: "verified"})
	ecosystemsPublicSuiteProject(t, d.Pool, ecosystemsPublicSuiteProjectSpec{OwnerUserID: owner1, EcosystemID: ecoID, Status: "pending_verification"})
	// owner2: 1 verified project.
	ecosystemsPublicSuiteProject(t, d.Pool, ecosystemsPublicSuiteProjectSpec{OwnerUserID: owner2, EcosystemID: ecoID, Status: "verified"})
	// owner3: 1 soft-deleted project - excluded entirely by the LEFT JOIN's
	// "p.deleted_at IS NULL" condition, so neither project_count nor
	// user_count should reflect it.
	ecosystemsPublicSuiteProject(t, d.Pool, ecosystemsPublicSuiteProjectSpec{OwnerUserID: owner3, EcosystemID: ecoID, Status: "verified", Deleted: true})

	// This ecosystem's project rows are only ever joined via its own unique
	// ecosystem_id, so concurrently-running test suites inserting unrelated
	// projects elsewhere cannot affect these counts - exact equality is safe.
	status, body := ecosystemsPublicSuiteDoJSON(t, app, "/ecosystems")
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", status, body)
	}
	var resp struct {
		Ecosystems []map[string]any `json:"ecosystems"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	var found map[string]any
	for _, e := range resp.Ecosystems {
		if e["id"] == ecoID.String() {
			found = e
			break
		}
	}
	if found == nil {
		t.Fatalf("ecosystem %s not found in ListActive() response (ecosystems: %d)", ecoID, len(resp.Ecosystems))
	}
	if pc, ok := found["project_count"].(float64); !ok || pc != 3 {
		t.Errorf("project_count = %v, want 3 (owner1's 2 non-deleted projects + owner2's 1; owner3's deleted project excluded)", found["project_count"])
	}
	if uc, ok := found["user_count"].(float64); !ok || uc != 2 {
		t.Errorf("user_count = %v, want 2 distinct owners (owner1, owner2; owner3 excluded because its only project is deleted)", found["user_count"])
	}
}

// ---------------------------------------------------------------------------
// GetByID() - GET /ecosystems/:id
// ---------------------------------------------------------------------------

// TestEcosystemsPublicSuite_GetByID_ReturnsDetailAndComputedStats documents a
// real, currently-shipping bug in GetByID() (internal/handlers/ecosystems_public.go:66-79):
// the second query's SQL text references only the "$1" placeholder (it never
// uses $2/$3/$4), but the Go call passes four positional arguments
// (`ecoID, ecoID, ecoID, ecoID`). pgx rejects that mismatch outright
// ("expected 1 arguments, got 4" - confirmed by running the exact query
// standalone against this test's Postgres instance), and because the
// handler discards the Scan error via "_ = ...QueryRow(...).Scan(...)", the
// failure is silent: project_count/contributors_count/open_issues_count/
// open_prs_count are left at their Go zero-value (0) and shipped to the
// client with a 200 OK, no matter what's actually seeded. This test asserts
// that CURRENT (buggy) behavior rather than the evidently-intended one, per
// this suite's mandate to test actual behavior, not assumed/intended
// behavior. The first query (name/about/links/... detail fields, lines
// 37-48) has no such bug and does reflect seeded data correctly.
func TestEcosystemsPublicSuite_GetByID_ReturnsDetailAndComputedStats(t *testing.T) {
	d := testDB(t)
	app := newEcosystemsPublicSuiteApp(d)

	about := "About this ecosystem"
	ecoID, ecoName := ecosystemsPublicSuiteEcosystem(t, d.Pool, ecosystemsPublicSuiteEcoSpec{
		Status:       "active",
		About:        &about,
		Links:        []string{"https://example.com"},
		KeyAreas:     []string{"DeFi"},
		Technologies: []string{"Rust"},
	})
	owner := ecosystemsPublicSuiteUser(t, d.Pool)

	// Counted (if the stats query worked): verified AND needs_metadata=false.
	countedProject := ecosystemsPublicSuiteProject(t, d.Pool, ecosystemsPublicSuiteProjectSpec{
		OwnerUserID: owner, EcosystemID: ecoID, Status: "verified", NeedsMetadata: false,
	})
	// Not counted: verified but needs_metadata=true.
	ecosystemsPublicSuiteProject(t, d.Pool, ecosystemsPublicSuiteProjectSpec{
		OwnerUserID: owner, EcosystemID: ecoID, Status: "verified", NeedsMetadata: true,
	})
	// Not counted: not yet verified.
	ecosystemsPublicSuiteProject(t, d.Pool, ecosystemsPublicSuiteProjectSpec{
		OwnerUserID: owner, EcosystemID: ecoID, Status: "pending_verification",
	})

	ecosystemsPublicSuiteIssue(t, d.Pool, countedProject, "ecopub-alice", "open")
	ecosystemsPublicSuiteIssue(t, d.Pool, countedProject, "ecopub-bob", "closed")
	ecosystemsPublicSuitePR(t, d.Pool, countedProject, "ecopub-carol", "open")

	status, body := ecosystemsPublicSuiteDoJSON(t, app, "/ecosystems/"+ecoID.String())
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// Detail fields come from the first query (no bug) and correctly
	// reflect seeded data.
	if resp["name"] != ecoName {
		t.Errorf("name = %v, want %q", resp["name"], ecoName)
	}
	if resp["about"] != about {
		t.Errorf("about = %v, want %q", resp["about"], about)
	}
	links, ok := resp["links"].([]any)
	if !ok || len(links) != 1 || links[0] != "https://example.com" {
		t.Errorf("links = %v, want [\"https://example.com\"]", resp["links"])
	}

	// Stats fields come from the second query, which used to pass 4
	// positional args (ecoID x4) for a query that only ever references
	// $1 - pgx rejected that outright ("expected 1 arguments, got 4"), and
	// since the error was discarded, these were always silently 0 in
	// production. Fixed by passing ecoID once (Postgres reuses $1 across
	// all 4 subqueries); these now assert the real computed counts.
	if pc, _ := resp["project_count"].(float64); pc != 1 {
		t.Errorf("project_count = %v, want 1", resp["project_count"])
	}
	if cc, _ := resp["contributors_count"].(float64); cc != 3 {
		t.Errorf("contributors_count = %v, want 3", resp["contributors_count"])
	}
	if oi, _ := resp["open_issues_count"].(float64); oi != 1 {
		t.Errorf("open_issues_count = %v, want 1", resp["open_issues_count"])
	}
	if op, _ := resp["open_prs_count"].(float64); op != 1 {
		t.Errorf("open_prs_count = %v, want 1", resp["open_prs_count"])
	}
}

func TestEcosystemsPublicSuite_GetByID_NotFoundForNonexistentUUID(t *testing.T) {
	d := testDB(t)
	app := newEcosystemsPublicSuiteApp(d)

	status, body := ecosystemsPublicSuiteDoJSON(t, app, "/ecosystems/"+uuid.New().String())
	if status != fiber.StatusNotFound {
		t.Errorf("status = %d, want 404, body=%s", status, body)
	}
}

func TestEcosystemsPublicSuite_GetByID_NotFoundForInactiveEcosystem(t *testing.T) {
	d := testDB(t)
	app := newEcosystemsPublicSuiteApp(d)

	ecoID, _ := ecosystemsPublicSuiteEcosystem(t, d.Pool, ecosystemsPublicSuiteEcoSpec{Status: "inactive"})

	status, body := ecosystemsPublicSuiteDoJSON(t, app, "/ecosystems/"+ecoID.String())
	if status != fiber.StatusNotFound {
		t.Errorf("status = %d, want 404 for an inactive ecosystem, body=%s", status, body)
	}
}

func TestEcosystemsPublicSuite_GetByID_BadRequestForMalformedID(t *testing.T) {
	d := testDB(t)
	app := newEcosystemsPublicSuiteApp(d)

	status, body := ecosystemsPublicSuiteDoJSON(t, app, "/ecosystems/not-a-uuid")
	if status != fiber.StatusBadRequest {
		t.Errorf("status = %d, want 400, body=%s", status, body)
	}
}

// ---------------------------------------------------------------------------
// db_not_configured guard (shared by both handlers, needs no live DB).
// ---------------------------------------------------------------------------

func TestEcosystemsPublicSuite_NilDBPool_ReturnsServiceUnavailable(t *testing.T) {
	h := handlers.NewEcosystemsPublicHandler(&db.DB{Pool: nil})
	app := fiber.New()
	app.Get("/ecosystems", h.ListActive())
	app.Get("/ecosystems/:id", h.GetByID())

	status, body := ecosystemsPublicSuiteDoJSON(t, app, "/ecosystems")
	if status != fiber.StatusServiceUnavailable {
		t.Errorf("ListActive: status = %d, want 503, body=%s", status, body)
	}

	status, body = ecosystemsPublicSuiteDoJSON(t, app, "/ecosystems/"+uuid.New().String())
	if status != fiber.StatusServiceUnavailable {
		t.Errorf("GetByID: status = %d, want 503, body=%s", status, body)
	}
}
