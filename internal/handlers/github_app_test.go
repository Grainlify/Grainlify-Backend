// github_app_test.go covers GitHubAppHandler's installation flow:
// StartInstallation, HandleInstallationCallback, and
// syncInstallationRepositories. Previously GitHubAppHandler was only
// referenced in projects_public_cache_test.go as an empty &GitHubAppHandler{}
// to satisfy cache-invalidation wiring, with none of its actual logic
// exercised. Tests here require a real PostgreSQL database and are skipped
// automatically when TEST_DB_URL is not set (same convention as
// internal/ingest/github_webhook_test.go). This file is intentionally
// self-contained (its own DB-pool/mock-transport/seed helpers, distinctly
// named) rather than reusing similarly-shaped helpers that may exist in
// other new test files from other in-flight, independent PRs in this same
// campaign -- avoiding a same-package function redeclaration if both merge.
// package handlers (not handlers_test) so syncInstallationRepositories can
// be called directly and synchronously, rather than waiting on the
// fire-and-forget goroutine HandleInstallationCallback starts.
package handlers

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/migrate"
)

// openGitHubAppTestPool connects to TEST_DB_URL and applies migrations,
// skipping the test if TEST_DB_URL is not set.
func openGitHubAppTestPool(t *testing.T) *pgxpool.Pool {
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

// githubAppMockRoundTripper implements http.RoundTripper for mocking
// GitHub App API calls in tests.
type githubAppMockRoundTripper struct {
	fn func(req *http.Request) (*http.Response, error)
}

func (m *githubAppMockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.fn(req)
}

func githubAppHTTPBody(s string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(s))
}

// seedGitHubAppProject inserts a verified, no-installation project (with its
// own owner user) and registers cleanup. Returns the project's ID.
func seedGitHubAppProject(t *testing.T, pool *pgxpool.Pool, fullName string) string {
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

// testAppPrivateKeyPEM returns a freshly generated RSA private key PEM-encoded
// the way GitHubAppPrivateKey is expected to be configured, so
// github.NewGitHubAppClient can parse it without needing a real GitHub App.
func testAppPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	return string(pem.EncodeToMemory(block))
}

func newTestGitHubAppHandler(pool *pgxpool.Pool, cfg config.Config) *GitHubAppHandler {
	return NewGitHubAppHandler(cfg, &db.DB{Pool: pool})
}

func newInstallationTestApp(h *GitHubAppHandler) *fiber.App {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/auth/github/app/install/start", func(c *fiber.Ctx) error {
		c.Locals(auth.LocalUserID, c.Query("_test_user_id"))
		return h.StartInstallation()(c)
	})
	app.Get("/auth/github/app/install/callback", h.HandleInstallationCallback())
	return app
}

// ---------------------------------------------------------------------------
// StartInstallation
// ---------------------------------------------------------------------------

func TestStartInstallation_NotConfigured(t *testing.T) {
	pool := openGitHubAppTestPool(t)
	h := newTestGitHubAppHandler(pool, config.Config{}) // GitHubAppID unset

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/start", func(c *fiber.Ctx) error {
		c.Locals(auth.LocalUserID, uuid.New().String())
		return h.StartInstallation()(c)
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/start", nil), -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestStartInstallation_BuildsURLAndInsertsState(t *testing.T) {
	pool := openGitHubAppTestPool(t)
	cfg := config.Config{
		GitHubAppID:   "12345",
		GitHubAppSlug: "grainlify-test-app",
		PublicBaseURL: "https://api.example.com",
	}
	h := newTestGitHubAppHandler(pool, cfg)
	app := newInstallationTestApp(h)

	// oauth_states.user_id has a FK to users(id), so a real seeded user is
	// required here (unlike syncInstallationRepositories's userID, which is
	// only ever used to populate projects.owner_user_id).
	userID := seedOwnerUser(t, pool)
	resp, err := app.Test(httptest.NewRequest("GET", "/auth/github/app/install/start?_test_user_id="+userID.String(), nil), -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		InstallURL string `json:"install_url"`
		State      string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM oauth_states WHERE state = $1`, body.State)
	})

	if !strings.Contains(body.InstallURL, "https://github.com/apps/grainlify-test-app/installations/new") {
		t.Errorf("install_url = %q, want it to start with the app install URL", body.InstallURL)
	}
	if !strings.Contains(body.InstallURL, "state="+body.State) {
		t.Errorf("install_url = %q, want it to include state=%s", body.InstallURL, body.State)
	}
	if !strings.Contains(body.InstallURL, "redirect_url=") {
		t.Errorf("install_url = %q, want it to embed a redirect_url callback param", body.InstallURL)
	}
	// The embedded redirect_url must itself carry the callback path + state,
	// per the workaround documented in StartInstallation for GitHub's
	// unreliable callback state param.
	if !strings.Contains(body.InstallURL, "auth%2Fgithub%2Fapp%2Finstall%2Fcallback") {
		t.Errorf("install_url = %q, want the embedded redirect_url to point at our install callback", body.InstallURL)
	}

	var storedUserID uuid.UUID
	var kind string
	if err := pool.QueryRow(context.Background(),
		`SELECT user_id, kind FROM oauth_states WHERE state = $1`, body.State,
	).Scan(&storedUserID, &kind); err != nil {
		t.Fatalf("expected a stored oauth_states row: %v", err)
	}
	if storedUserID != userID {
		t.Errorf("stored user_id = %v, want %v", storedUserID, userID)
	}
	if kind != "github_app_install" {
		t.Errorf("stored kind = %q, want github_app_install", kind)
	}
}

// ---------------------------------------------------------------------------
// HandleInstallationCallback
// ---------------------------------------------------------------------------

func TestHandleInstallationCallback_MissingInstallationID_RedirectsCancelled(t *testing.T) {
	pool := openGitHubAppTestPool(t)
	h := newTestGitHubAppHandler(pool, config.Config{FrontendBaseURL: "https://app.example.com"})
	app := newInstallationTestApp(h)

	req := httptest.NewRequest("GET", "/auth/github/app/install/callback", nil)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "github_app_install=cancelled") {
		t.Errorf("redirect location = %q, want github_app_install=cancelled", loc)
	}
}

func TestHandleInstallationCallback_InvalidState_Returns400(t *testing.T) {
	pool := openGitHubAppTestPool(t)
	h := newTestGitHubAppHandler(pool, config.Config{FrontendBaseURL: "https://app.example.com"})
	app := newInstallationTestApp(h)

	req := httptest.NewRequest("GET", "/auth/github/app/install/callback?installation_id=999&state=does-not-exist", nil)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["error"] != "invalid_or_expired_state" {
		t.Errorf("error = %q, want invalid_or_expired_state", body["error"])
	}
}

func TestHandleInstallationCallback_ValidStateNoUserID_SkipsSyncButRedirects(t *testing.T) {
	pool := openGitHubAppTestPool(t)
	h := newTestGitHubAppHandler(pool, config.Config{FrontendBaseURL: "https://app.example.com"})
	app := newInstallationTestApp(h)

	state := "no-user-state-" + time.Now().Format("20060102150405.000000000")
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO oauth_states (state, user_id, kind, expires_at)
		VALUES ($1, NULL, 'github_app_install', now() + interval '10 minutes')
	`, state); err != nil {
		t.Fatalf("seed oauth_states: %v", err)
	}

	req := httptest.NewRequest("GET", "/auth/github/app/install/callback?installation_id=999&state="+state, nil)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "github_app_installed=true") {
		t.Errorf("redirect location = %q, want github_app_installed=true", loc)
	}

	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM oauth_states WHERE state = $1`, state,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Error("expected the consumed state row to be deleted")
	}
}

func TestHandleInstallationCallback_ValidStateWithUserID_DeletesStateAndRedirects(t *testing.T) {
	pool := openGitHubAppTestPool(t)
	// No GitHubAppID/PrivateKey configured, so the fire-and-forget
	// syncInstallationRepositories goroutine returns almost immediately
	// after logging an error (its own logic is covered directly and
	// synchronously by the TestSyncInstallationRepositories_* tests below).
	h := newTestGitHubAppHandler(pool, config.Config{FrontendBaseURL: "https://app.example.com"})
	app := newInstallationTestApp(h)

	var ownerID string
	if err := pool.QueryRow(context.Background(), `INSERT INTO users (role) VALUES ('maintainer') RETURNING id`).Scan(&ownerID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, ownerID) })

	state := "with-user-state-" + time.Now().Format("20060102150405.000000000")
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO oauth_states (state, user_id, kind, expires_at)
		VALUES ($1, $2, 'github_app_install', now() + interval '10 minutes')
	`, state, ownerID); err != nil {
		t.Fatalf("seed oauth_states: %v", err)
	}

	req := httptest.NewRequest("GET", "/auth/github/app/install/callback?installation_id=999&state="+state+"&setup_action=install", nil)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "github_app_installed=true") || !strings.Contains(loc, "installation_id=999") {
		t.Errorf("redirect location = %q, want github_app_installed=true and installation_id=999", loc)
	}

	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM oauth_states WHERE state = $1`, state,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Error("expected the consumed state row to be deleted")
	}
}

// ---------------------------------------------------------------------------
// syncInstallationRepositories -- called directly and synchronously, per the
// issue's suggested execution, rather than through the fire-and-forget
// goroutine HandleInstallationCallback starts.
// ---------------------------------------------------------------------------

// withMockGitHubAppTransport swaps http.DefaultTransport so
// GitHubAppClient's requests (which default to it when HTTP.Transport is
// unset) are intercepted, mirroring projects_public_test.go's
// withMockGitHubTransport for the same reason: syncInstallationRepositories
// constructs its github.NewGitHubAppClient internally with no injectable
// seam.
func withMockGitHubAppTransport(t *testing.T, repos []map[string]interface{}) {
	t.Helper()
	orig := http.DefaultTransport
	http.DefaultTransport = &githubAppMockRoundTripper{fn: func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Path, "/access_tokens"):
			return &http.Response{StatusCode: 201, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: githubAppHTTPBody(`{"token":"test-installation-token","expires_at":"2099-01-01T00:00:00Z"}`)}, nil
		case req.URL.Path == "/installation/repositories":
			payload, _ := json.Marshal(map[string]interface{}{
				"total_count":  len(repos),
				"repositories": repos,
			})
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: githubAppHTTPBody(string(payload))}, nil
		default:
			t.Fatalf("unexpected GitHub App request: %s %s", req.Method, req.URL.Path)
			return nil, nil
		}
	}}
	t.Cleanup(func() { http.DefaultTransport = orig })
}

func testAppConfig(t *testing.T) config.Config {
	return config.Config{
		GitHubAppID:         "12345",
		GitHubAppPrivateKey: testAppPrivateKeyPEM(t),
	}
}

func seedActiveEcosystem(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	suffix := time.Now().Format("20060102150405.000000000")
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO ecosystems (slug, name, status) VALUES ($1, $2, 'active') RETURNING id`,
		"sync-fixture-"+suffix, "Sync Fixture "+suffix,
	).Scan(&id); err != nil {
		t.Fatalf("seed ecosystem: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM ecosystems WHERE id = $1`, id) })
}

func TestSyncInstallationRepositories_PrivateRepoAlreadyPresent_SoftDeleted(t *testing.T) {
	pool := openGitHubAppTestPool(t)
	seedActiveEcosystem(t, pool)
	suffix := time.Now().Format("20060102150405.000000000")
	fullName := "acme/private-repo-" + suffix
	projectID := seedGitHubAppProject(t, pool, fullName)

	withMockGitHubAppTransport(t, []map[string]interface{}{
		{"id": 1, "full_name": fullName, "private": true},
	})

	h := newTestGitHubAppHandler(pool, testAppConfig(t))
	h.syncInstallationRepositories(context.Background(), uuid.New(), "999")

	var deletedAt *time.Time
	if err := pool.QueryRow(context.Background(), `SELECT deleted_at FROM projects WHERE id = $1`, projectID).Scan(&deletedAt); err != nil {
		t.Fatalf("select project: %v", err)
	}
	if deletedAt == nil {
		t.Error("expected the private repo's project to be soft-deleted, deleted_at is still NULL")
	}
}

func TestSyncInstallationRepositories_NewPublicRepo_CreatedAndAutoVerified(t *testing.T) {
	pool := openGitHubAppTestPool(t)
	seedActiveEcosystem(t, pool)
	suffix := time.Now().Format("20060102150405.000000000")
	fullName := "acme/new-public-repo-" + suffix

	withMockGitHubAppTransport(t, []map[string]interface{}{
		{"id": 42, "full_name": fullName, "private": false, "language": "Go", "topics": []string{"cli"}},
	})

	userID := seedOwnerUser(t, pool)
	h := newTestGitHubAppHandler(pool, testAppConfig(t))
	h.syncInstallationRepositories(context.Background(), userID, "999")

	var status string
	var githubRepoID int64
	var projectID string
	if err := pool.QueryRow(context.Background(),
		`SELECT id, status, github_repo_id FROM projects WHERE github_full_name = $1`, fullName,
	).Scan(&projectID, &status, &githubRepoID); err != nil {
		t.Fatalf("expected a created project row: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, projectID) })

	if status != "verified" {
		t.Errorf("status = %q, want verified (auto-verified immediately after creation)", status)
	}
	if githubRepoID != 42 {
		t.Errorf("github_repo_id = %d, want 42", githubRepoID)
	}

	var syncJobCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM sync_jobs WHERE project_id = $1`, projectID,
	).Scan(&syncJobCount); err != nil {
		t.Fatalf("count sync_jobs: %v", err)
	}
	if syncJobCount != 2 {
		t.Errorf("sync_jobs count = %d, want 2 (sync_issues + sync_prs)", syncJobCount)
	}
}

func TestSyncInstallationRepositories_ExistingPublicRepo_UpdatedNotDuplicated(t *testing.T) {
	pool := openGitHubAppTestPool(t)
	seedActiveEcosystem(t, pool)
	suffix := time.Now().Format("20060102150405.000000000")
	fullName := "acme/existing-public-repo-" + suffix
	projectID := seedGitHubAppProject(t, pool, fullName)

	// Simulate a previously-deleted/unverified project to prove the sync
	// restores and (re-)verifies it rather than skipping or duplicating.
	if _, err := pool.Exec(context.Background(),
		`UPDATE projects SET status = 'rejected', deleted_at = now() WHERE id = $1`, projectID,
	); err != nil {
		t.Fatalf("seed rejected state: %v", err)
	}

	withMockGitHubAppTransport(t, []map[string]interface{}{
		{"id": 77, "full_name": fullName, "private": false},
	})

	h := newTestGitHubAppHandler(pool, testAppConfig(t))
	h.syncInstallationRepositories(context.Background(), uuid.New(), "999")

	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM projects WHERE github_full_name = $1`, fullName,
	).Scan(&count); err != nil {
		t.Fatalf("count projects: %v", err)
	}
	if count != 1 {
		t.Fatalf("project count for %q = %d, want exactly 1 (updated in place, not duplicated)", fullName, count)
	}

	var status string
	var deletedAt *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT status, deleted_at FROM projects WHERE id = $1`, projectID,
	).Scan(&status, &deletedAt); err != nil {
		t.Fatalf("select project: %v", err)
	}
	if status != "verified" {
		t.Errorf("status = %q, want verified", status)
	}
	if deletedAt != nil {
		t.Error("expected deleted_at to be cleared on re-verification")
	}

	var syncJobCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM sync_jobs WHERE project_id = $1`, projectID,
	).Scan(&syncJobCount); err != nil {
		t.Fatalf("count sync_jobs: %v", err)
	}
	if syncJobCount != 2 {
		t.Errorf("sync_jobs count = %d, want 2 (re-enqueued, not duplicated beyond one sync_issues + one sync_prs)", syncJobCount)
	}
}

// seedOwnerUser inserts a standalone user row (for tests that need a userID
// but no accompanying project) and registers cleanup.
func seedOwnerUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `INSERT INTO users (role) VALUES ('maintainer') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("seed owner user: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id) })
	return id
}
