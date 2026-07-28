// projects_public_test.go exercises the query construction and response
// shaping in listFetch/recommendedFetch/filterOptionsFetch/Get() directly,
// as opposed to projects_public_cache_test.go which only exercises cache
// hit/miss/invalidation behavior via pre-seeded cache entries. Tests here
// require a real PostgreSQL database and are skipped automatically when
// TEST_DB_URL is not set (same convention as
// internal/ingest/github_webhook_test.go and internal/handlers/admin_test.go).
// package handlers (not handlers_test) so tests can use the unexported
// newProjectsPublicHandler constructor to inject a nil cache, matching
// projects_public_cache_test.go's existing convention.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/migrate"
)

func openPublicTestPool(t *testing.T) *pgxpool.Pool {
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

// seededProject is one fixture project's identifying inputs, used both to
// seed the row and to assert on the response.
type seededProject struct {
	id        string
	fullName  string
	ecosystem string // ecosystem name (may be "")
	language  string
	category  string
	tags      []string
	ownerID   string
}

// seedFilterFixture inserts an ecosystem plus a handful of verified projects
// with distinct/overlapping language, category, ecosystem, and tag
// combinations, and registers cleanup. Every project belongs to a unique
// owner user so the FK is satisfied without collisions.
func seedFilterFixture(t *testing.T, pool *pgxpool.Pool) []seededProject {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().Format("20060102150405.000000000")

	var ecosystemID string
	ecosystemName := "Starknet-" + suffix
	if err := pool.QueryRow(ctx,
		`INSERT INTO ecosystems (slug, name) VALUES ($1, $2) RETURNING id`,
		"starknet-"+suffix, ecosystemName,
	).Scan(&ecosystemID); err != nil {
		t.Fatalf("seed ecosystem: %v", err)
	}

	specs := []seededProject{
		{fullName: "acme/go-widget-" + suffix, ecosystem: ecosystemName, language: "Go", category: "Tooling", tags: []string{"cli", "backend"}},
		{fullName: "acme/rust-widget-" + suffix, ecosystem: ecosystemName, language: "Rust", category: "Tooling", tags: []string{"zeta-tag", "alpha-tag"}},
		{fullName: "other/py-widget-" + suffix, ecosystem: "", language: "Python", category: "DeFi", tags: []string{"alpha-tag", "beta-tag"}},
	}

	for i := range specs {
		var ownerID string
		if err := pool.QueryRow(ctx, `INSERT INTO users (role) VALUES ('maintainer') RETURNING id`).Scan(&ownerID); err != nil {
			t.Fatalf("seed owner: %v", err)
		}
		specs[i].ownerID = ownerID

		tagsJSON, _ := json.Marshal(specs[i].tags)
		var ecoID interface{}
		if specs[i].ecosystem != "" {
			ecoID = ecosystemID
		}

		if err := pool.QueryRow(ctx, `
			INSERT INTO projects (owner_user_id, github_full_name, status, language, category, tags, ecosystem_id, needs_metadata)
			VALUES ($1, $2, 'verified', $3, $4, $5::jsonb, $6, false)
			RETURNING id
		`, ownerID, specs[i].fullName, specs[i].language, specs[i].category, string(tagsJSON), ecoID).Scan(&specs[i].id); err != nil {
			t.Fatalf("seed project %q: %v", specs[i].fullName, err)
		}
	}

	t.Cleanup(func() {
		bg := context.Background()
		for _, s := range specs {
			_, _ = pool.Exec(bg, `DELETE FROM projects WHERE id = $1`, s.id)
			_, _ = pool.Exec(bg, `DELETE FROM users WHERE id = $1`, s.ownerID)
		}
		_, _ = pool.Exec(bg, `DELETE FROM ecosystems WHERE id = $1`, ecosystemID)
	})

	return specs
}

func newPublicTestApp(pool *pgxpool.Pool) (*fiber.App, *ProjectsPublicHandler) {
	h := newProjectsPublicHandler(config.Config{}, &db.DB{Pool: pool}, nil)
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/projects", h.List())
	app.Get("/projects/recommended", h.Recommended())
	app.Get("/projects/filters", h.FilterOptions())
	app.Get("/projects/:id", h.Get())
	return app, h
}

func decodeProjectsList(t *testing.T, resp *http.Response) []map[string]interface{} {
	t.Helper()
	var body struct {
		Projects []map[string]interface{} `json:"projects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body.Projects
}

func fullNames(rows []map[string]interface{}) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i], _ = r["github_full_name"].(string)
	}
	return out
}

// ---------------------------------------------------------------------------
// listFetch filter-combination query construction
// ---------------------------------------------------------------------------

func TestListFetch_NoFilters_ReturnsAllVerifiedFixtureProjects(t *testing.T) {
	pool := openPublicTestPool(t)
	fixture := seedFilterFixture(t, pool)
	app, _ := newPublicTestApp(pool)

	resp, err := app.Test(httptest.NewRequest("GET", "/projects?limit=200", nil), -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	got := fullNames(decodeProjectsList(t, resp))
	for _, s := range fixture {
		if !contains(got, s.fullName) {
			t.Errorf("expected %q in unfiltered list, got %v", s.fullName, got)
		}
	}
}

func TestListFetch_SingleFilter_Ecosystem(t *testing.T) {
	pool := openPublicTestPool(t)
	fixture := seedFilterFixture(t, pool)
	app, _ := newPublicTestApp(pool)

	url := fmt.Sprintf("/projects?limit=200&ecosystem=%s", fixture[0].ecosystem)
	resp, err := app.Test(httptest.NewRequest("GET", url, nil), -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	got := fullNames(decodeProjectsList(t, resp))
	// Both fixture[0] and fixture[1] share the same ecosystem.
	if !contains(got, fixture[0].fullName) || !contains(got, fixture[1].fullName) {
		t.Errorf("expected both same-ecosystem projects in results, got %v", got)
	}
	if contains(got, fixture[2].fullName) {
		t.Errorf("expected the different-ecosystem project to be excluded, got %v", got)
	}
}

// TestListFetch_CombinedFilters_ArgPositionsStayAligned is the key regression
// guard for issue #311: combining ecosystem+language+tags shifts $N
// placeholder positions for every condition appended after it. If argPos
// drifted (an off-by-one, or a filter silently not applied), this query
// would either error or match the wrong rows. Only fixture[0] satisfies all
// three conditions simultaneously.
func TestListFetch_CombinedFilters_ArgPositionsStayAligned(t *testing.T) {
	pool := openPublicTestPool(t)
	fixture := seedFilterFixture(t, pool)
	app, _ := newPublicTestApp(pool)

	url := fmt.Sprintf("/projects?limit=200&ecosystem=%s&language=%s&tags=%s",
		fixture[0].ecosystem, fixture[0].language, fixture[0].tags[0])
	resp, err := app.Test(httptest.NewRequest("GET", url, nil), -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	got := fullNames(decodeProjectsList(t, resp))
	if len(got) != 1 || got[0] != fixture[0].fullName {
		t.Fatalf("combined ecosystem+language+tags filters = %v, want exactly [%q]", got, fixture[0].fullName)
	}
}

func TestListFetch_TagsFilter_OnlyMatchesProjectsContainingAllTags(t *testing.T) {
	pool := openPublicTestPool(t)
	fixture := seedFilterFixture(t, pool)
	app, _ := newPublicTestApp(pool)

	// fixture[1] and fixture[2] both have "alpha-tag", but only fixture[2]
	// additionally has "beta-tag".
	resp, err := app.Test(httptest.NewRequest("GET", "/projects?limit=200&tags=alpha-tag,beta-tag", nil), -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	got := fullNames(decodeProjectsList(t, resp))
	if !contains(got, fixture[2].fullName) {
		t.Errorf("expected project with both tags in results, got %v", got)
	}
	if contains(got, fixture[1].fullName) {
		t.Errorf("expected project missing beta-tag to be excluded, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// recommendedFetch
// ---------------------------------------------------------------------------

func TestRecommendedFetch_ReturnsFixtureProjects(t *testing.T) {
	pool := openPublicTestPool(t)
	fixture := seedFilterFixture(t, pool)
	app, _ := newPublicTestApp(pool)

	resp, err := app.Test(httptest.NewRequest("GET", "/projects/recommended?limit=20", nil), -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	got := fullNames(decodeProjectsList(t, resp))
	for _, s := range fixture {
		if !contains(got, s.fullName) {
			t.Errorf("expected %q in recommended list, got %v", s.fullName, got)
		}
	}
}

// ---------------------------------------------------------------------------
// filterOptionsFetch tag de-duplication/sorting
// ---------------------------------------------------------------------------

func TestFilterOptionsFetch_TagsDedupedAndSorted(t *testing.T) {
	pool := openPublicTestPool(t)
	seedFilterFixture(t, pool) // tags across fixture: cli, backend, zeta-tag, alpha-tag (x2), beta-tag
	app, _ := newPublicTestApp(pool)

	resp, err := app.Test(httptest.NewRequest("GET", "/projects/filters", nil), -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	seen := map[string]int{}
	for _, tag := range body.Tags {
		seen[tag]++
	}
	if seen["alpha-tag"] > 1 {
		t.Errorf("expected alpha-tag deduplicated across projects, got count %d in %v", seen["alpha-tag"], body.Tags)
	}
	for i := 1; i < len(body.Tags); i++ {
		if body.Tags[i-1] > body.Tags[i] {
			t.Fatalf("tags not sorted: %v", body.Tags)
		}
	}
	for _, want := range []string{"alpha-tag", "beta-tag", "zeta-tag", "cli", "backend"} {
		if seen[want] == 0 {
			t.Errorf("expected tag %q present, got %v", want, body.Tags)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Get() GitHub-enrichment branching
// ---------------------------------------------------------------------------

// getMockRoundTripper implements http.RoundTripper. Get()'s getFetch always
// constructs its own github.NewClient() internally (no injectable seam), and
// that client's default transport falls back to http.DefaultTransport when
// unset (github.NewRateLimitTransport), so swapping http.DefaultTransport
// for the duration of a test is the only way to control its GitHub calls.
// Restored via t.Cleanup; safe because this package's tests run serially
// (none of them call t.Parallel()).
type getMockRoundTripper struct {
	fn func(req *http.Request) (*http.Response, error)
}

func (m *getMockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.fn(req)
}

func withMockGitHubTransport(t *testing.T, fn func(req *http.Request) (*http.Response, error)) {
	t.Helper()
	orig := http.DefaultTransport
	http.DefaultTransport = &getMockRoundTripper{fn: fn}
	t.Cleanup(func() { http.DefaultTransport = orig })
}

// seedSingleProject inserts one verified, no-installation project and
// registers cleanup. Returns its ID and full name.
func seedSingleProject(t *testing.T, pool *pgxpool.Pool, fullName string) string {
	t.Helper()
	ctx := context.Background()
	var ownerID string
	if err := pool.QueryRow(ctx, `INSERT INTO users (role) VALUES ('maintainer') RETURNING id`).Scan(&ownerID); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	var projectID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO projects (owner_user_id, github_full_name, status, needs_metadata)
		VALUES ($1, $2, 'verified', false) RETURNING id
	`, ownerID, fullName).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM projects WHERE id = $1`, projectID)
		_, _ = pool.Exec(bg, `DELETE FROM users WHERE id = $1`, ownerID)
	})
	return projectID
}

func TestGet_VerifiedPublicRepo_FullEnrichment(t *testing.T) {
	pool := openPublicTestPool(t)
	suffix := time.Now().Format("20060102150405.000000000")
	fullName := "acme/enriched-" + suffix
	projectID := seedSingleProject(t, pool, fullName)

	withMockGitHubTransport(t, func(req *http.Request) (*http.Response, error) {
		switch {
		case req.URL.Path == "/repos/acme/enriched-"+suffix:
			body := fmt.Sprintf(`{"id":1,"full_name":%q,"html_url":"https://github.com/%s","private":false,"stargazers_count":5}`, fullName, fullName)
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: httpBody(body)}, nil
		case req.URL.Path == "/repos/acme/enriched-"+suffix+"/languages":
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: httpBody(`{"Go":100}`)}, nil
		case req.URL.Path == "/repos/acme/enriched-"+suffix+"/readme":
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: httpBody(`{"content":"SGVsbG8=","encoding":"base64"}`)}, nil
		default:
			t.Fatalf("unexpected GitHub request: %s", req.URL.Path)
			return nil, nil
		}
	})

	app, _ := newPublicTestApp(pool)
	resp, err := app.Test(httptest.NewRequest("GET", "/projects/"+projectID, nil), -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["repo"] == nil {
		t.Error("expected repo field to be populated for a successful GetRepo fetch")
	}
	if body["readme"] != "Hello" {
		t.Errorf("readme = %v, want %q", body["readme"], "Hello")
	}
}

func TestGet_PrivateRepo_NotAccessible(t *testing.T) {
	pool := openPublicTestPool(t)
	suffix := time.Now().Format("20060102150405.000000000")
	fullName := "acme/private-" + suffix
	projectID := seedSingleProject(t, pool, fullName)

	withMockGitHubTransport(t, func(req *http.Request) (*http.Response, error) {
		body := fmt.Sprintf(`{"id":1,"full_name":%q,"private":true}`, fullName)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: httpBody(body)}, nil
	})

	app, _ := newPublicTestApp(pool)
	resp, err := app.Test(httptest.NewRequest("GET", "/projects/"+projectID, nil), -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var body map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["error"] != "project_not_accessible" {
		t.Errorf("error = %v, want project_not_accessible", body["error"])
	}
}

func TestGet_RepoFetch404_ClassifiedAsNotAccessible(t *testing.T) {
	pool := openPublicTestPool(t)
	suffix := time.Now().Format("20060102150405.000000000")
	fullName := "acme/gone-" + suffix
	projectID := seedSingleProject(t, pool, fullName)

	withMockGitHubTransport(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 404, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: httpBody(`{"message":"Not Found"}`)}, nil
	})

	app, _ := newPublicTestApp(pool)
	resp, err := app.Test(httptest.NewRequest("GET", "/projects/"+projectID, nil), -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestGet_RepoFetchNonClassifiedError_BestEffortContinues covers a GetRepo
// failure that is neither 404 nor 403 (a 500 here): per current documented
// behavior, Get() logs a warning and continues with DB-only fields rather
// than failing the whole request.
func TestGet_RepoFetchNonClassifiedError_BestEffortContinues(t *testing.T) {
	pool := openPublicTestPool(t)
	suffix := time.Now().Format("20060102150405.000000000")
	fullName := "acme/flaky-" + suffix
	projectID := seedSingleProject(t, pool, fullName)

	withMockGitHubTransport(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: httpBody(`{"message":"Internal Server Error"}`)}, nil
	})

	app, _ := newPublicTestApp(pool)
	resp, err := app.Test(httptest.NewRequest("GET", "/projects/"+projectID, nil), -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200 (best-effort continue on a non-404/403 GitHub error)", resp.StatusCode)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["repo"] != nil {
		t.Errorf("expected no repo field when GetRepo failed, got %v", body["repo"])
	}
	if body["github_full_name"] != fullName {
		t.Errorf("github_full_name = %v, want %q (DB-only fields still present)", body["github_full_name"], fullName)
	}
}

// TestGet_LanguagesAndReadmeFailure_DoesNotFailRequest covers GetRepo
// succeeding while GetRepoLanguages/GetReadme both fail: the request must
// still succeed with those fields empty, since they're documented as
// best-effort.
func TestGet_LanguagesAndReadmeFailure_DoesNotFailRequest(t *testing.T) {
	pool := openPublicTestPool(t)
	suffix := time.Now().Format("20060102150405.000000000")
	fullName := "acme/partial-" + suffix
	projectID := seedSingleProject(t, pool, fullName)

	withMockGitHubTransport(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/repos/acme/partial-"+suffix {
			body := fmt.Sprintf(`{"id":1,"full_name":%q,"private":false}`, fullName)
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: httpBody(body)}, nil
		}
		// languages and readme both 404.
		return &http.Response{StatusCode: 404, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: httpBody(`{"message":"Not Found"}`)}, nil
	})

	app, _ := newPublicTestApp(pool)
	resp, err := app.Test(httptest.NewRequest("GET", "/projects/"+projectID, nil), -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200 (languages/readme failures must not fail the request)", resp.StatusCode)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["repo"] == nil {
		t.Error("expected repo field populated since GetRepo succeeded")
	}
	if readme, _ := body["readme"].(string); readme != "" {
		t.Errorf("readme = %q, want empty string on a failed README fetch", readme)
	}
}

func httpBody(s string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(s))
}
