package handlers_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// newGitHubAppTestApp wires a fresh fiber app exposing the same routes (and
// auth requirements) that internal/api/api.go registers for the GitHub App
// handler.
func newGitHubAppTestApp(cfg config.Config, h *handlers.GitHubAppHandler) *fiber.App {
	app := fiber.New()
	app.Post("/auth/github/app/install/start", auth.RequireAuth(cfg.JWTSecret), h.StartInstallation())
	app.Get("/auth/github/app/install/callback", h.HandleInstallationCallback())
	return app
}

// insertGitHubAppOAuthState inserts an oauth_states row directly via SQL so
// callback tests can exercise expired/wrong-kind state without going
// through StartInstallation. Cascade-deleted when the owning user row is
// cleaned up (see seedGitHubHandlersTestUser).
func insertGitHubAppOAuthState(t *testing.T, d *db.DB, state string, userID uuid.UUID, kind string, expiresAt time.Time) {
	t.Helper()
	_, err := d.Pool.Exec(context.Background(), `
INSERT INTO oauth_states (state, user_id, kind, expires_at)
VALUES ($1, $2, $3, $4)
`, state, userID, kind, expiresAt)
	if err != nil {
		t.Fatalf("insertGitHubAppOAuthState: %v", err)
	}
}

func TestGitHubAppStartInstallation_Success(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{
		JWTSecret:     "test-jwt-secret-github-app-start",
		GitHubAppID:   "123456",
		GitHubAppSlug: "grainlify",
		PublicBaseURL: "http://localhost:8080",
	}
	h := handlers.NewGitHubAppHandler(cfg, d)
	app := newGitHubAppTestApp(cfg, h)

	userID := seedGitHubHandlersTestUser(t, d)
	token, err := auth.IssueJWT(cfg.JWTSecret, userID, "contributor", "", "", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}

	req := httptest.NewRequest("POST", "/auth/github/app/install/start", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusOK)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	installURL, _ := body["install_url"].(string)
	if installURL == "" {
		t.Fatal("install_url is empty")
	}
	respState, _ := body["state"].(string)
	if respState == "" {
		t.Fatal("state is empty")
	}

	u, err := url.Parse(installURL)
	if err != nil {
		t.Fatalf("install_url %q doesn't parse as URL: %v", installURL, err)
	}
	if u.Scheme != "https" || u.Host != "github.com" {
		t.Errorf("install_url scheme/host = %s://%s, want https://github.com", u.Scheme, u.Host)
	}
	if u.Path != "/apps/grainlify/installations/new" {
		t.Errorf("install_url path = %q, want /apps/grainlify/installations/new", u.Path)
	}

	q := u.Query()
	if q.Get("state") != respState {
		t.Errorf("install_url state param = %q, want %q (match top-level state field)", q.Get("state"), respState)
	}
	redirectURL := q.Get("redirect_url")
	if redirectURL == "" {
		t.Fatal("install_url redirect_url param is empty, want the callback URL embedded")
	}
	if !strings.Contains(redirectURL, "/auth/github/app/install/callback") {
		t.Errorf("redirect_url = %q, want it to contain the install callback path", redirectURL)
	}
	if !strings.Contains(redirectURL, "state=") {
		t.Errorf("redirect_url = %q, want it to have the state embedded", redirectURL)
	}
}

func TestGitHubAppStartInstallation_MalformedAppID(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{
		JWTSecret:     "test-jwt-secret-github-app-start",
		GitHubAppID:   "123#comment", // regression case: unstripped inline .env comment
		GitHubAppSlug: "grainlify",
	}
	h := handlers.NewGitHubAppHandler(cfg, d)
	app := newGitHubAppTestApp(cfg, h)

	userID := seedGitHubHandlersTestUser(t, d)
	token, err := auth.IssueJWT(cfg.JWTSecret, userID, "contributor", "", "", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}

	req := httptest.NewRequest("POST", "/auth/github/app/install/start", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (not a redirect/200)", resp.StatusCode, fiber.StatusServiceUnavailable)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["error"] != "github_app_not_configured" {
		t.Errorf("error = %v, want github_app_not_configured", body["error"])
	}
}

func TestGitHubAppStartInstallation_MalformedAppSlug(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{
		JWTSecret:     "test-jwt-secret-github-app-start",
		GitHubAppID:   "123456",
		GitHubAppSlug: "grain lify", // space is not allowed by githubAppSlugPattern
	}
	h := handlers.NewGitHubAppHandler(cfg, d)
	app := newGitHubAppTestApp(cfg, h)

	userID := seedGitHubHandlersTestUser(t, d)
	token, err := auth.IssueJWT(cfg.JWTSecret, userID, "contributor", "", "", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}

	req := httptest.NewRequest("POST", "/auth/github/app/install/start", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (not a redirect/200)", resp.StatusCode, fiber.StatusServiceUnavailable)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["error"] != "github_app_not_configured" {
		t.Errorf("error = %v, want github_app_not_configured", body["error"])
	}
}

func TestGitHubAppStartInstallation_MalformedAppSlugWithHash(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{
		JWTSecret:     "test-jwt-secret-github-app-start",
		GitHubAppID:   "123456",
		GitHubAppSlug: "grain#lify", // '#' is not allowed by githubAppSlugPattern
	}
	h := handlers.NewGitHubAppHandler(cfg, d)
	app := newGitHubAppTestApp(cfg, h)

	userID := seedGitHubHandlersTestUser(t, d)
	token, err := auth.IssueJWT(cfg.JWTSecret, userID, "contributor", "", "", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}

	req := httptest.NewRequest("POST", "/auth/github/app/install/start", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (not a redirect/200)", resp.StatusCode, fiber.StatusServiceUnavailable)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["error"] != "github_app_not_configured" {
		t.Errorf("error = %v, want github_app_not_configured", body["error"])
	}
}

// TestGitHubAppStartInstallation_EmptySlugFallsBackToAppID documents actual
// behavior: when GitHubAppSlug is empty but GitHubAppID is a valid numeric
// ID, the handler falls back to using the ID itself as the install URL
// slug (digits satisfy githubAppSlugPattern too).
func TestGitHubAppStartInstallation_EmptySlugFallsBackToAppID(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{
		JWTSecret:   "test-jwt-secret-github-app-start",
		GitHubAppID: "123456",
		// GitHubAppSlug intentionally left empty.
	}
	h := handlers.NewGitHubAppHandler(cfg, d)
	app := newGitHubAppTestApp(cfg, h)

	userID := seedGitHubHandlersTestUser(t, d)
	token, err := auth.IssueJWT(cfg.JWTSecret, userID, "contributor", "", "", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}

	req := httptest.NewRequest("POST", "/auth/github/app/install/start", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusOK)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	installURL, _ := body["install_url"].(string)
	if !strings.HasPrefix(installURL, "https://github.com/apps/123456/installations/new") {
		t.Errorf("install_url = %q, want it to use the app ID as the slug (https://github.com/apps/123456/installations/new...)", installURL)
	}
}

func TestGitHubAppStartInstallation_NilDB(t *testing.T) {
	cfg := config.Config{
		JWTSecret:     "test-jwt-secret-github-app-start",
		GitHubAppID:   "123456",
		GitHubAppSlug: "grainlify",
	}
	// h.db is nil: this test intentionally does not use testDB(t), since the
	// nil-db branch returns before any DB access happens.
	h := handlers.NewGitHubAppHandler(cfg, nil)
	app := newGitHubAppTestApp(cfg, h)

	token, err := auth.IssueJWT(cfg.JWTSecret, uuid.New(), "contributor", "", "", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}

	req := httptest.NewRequest("POST", "/auth/github/app/install/start", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusServiceUnavailable)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["error"] != "db_not_configured" {
		t.Errorf("error = %v, want db_not_configured", body["error"])
	}
}

func TestGitHubAppStartInstallation_MissingOrInvalidAuth(t *testing.T) {
	cfg := config.Config{JWTSecret: "test-jwt-secret-github-app-start"}
	h := handlers.NewGitHubAppHandler(cfg, nil)
	app := newGitHubAppTestApp(cfg, h)

	t.Run("missing authorization header", func(t *testing.T) {
		resp, err := app.Test(httptest.NewRequest("POST", "/auth/github/app/install/start", nil))
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/auth/github/app/install/start", nil)
		req.Header.Set("Authorization", "Bearer not-a-real-jwt")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
		}
	})
}

func TestGitHubAppHandleInstallationCallback_MissingInstallationID(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{FrontendBaseURL: "https://app.example.com"}
	h := handlers.NewGitHubAppHandler(cfg, d)
	app := newGitHubAppTestApp(cfg, h)

	resp, err := app.Test(httptest.NewRequest("GET", "/auth/github/app/install/callback", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusFound)
	}
	loc := resp.Header.Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location header %q doesn't parse as URL: %v", loc, err)
	}
	if u.Query().Get("github_app_install") != "cancelled" {
		t.Errorf("Location = %q, want github_app_install=cancelled", loc)
	}
}

func TestGitHubAppHandleInstallationCallback_InvalidState(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{FrontendBaseURL: "https://app.example.com"}
	h := handlers.NewGitHubAppHandler(cfg, d)
	app := newGitHubAppTestApp(cfg, h)

	resp, err := app.Test(httptest.NewRequest("GET", "/auth/github/app/install/callback?installation_id=99999&state=totally-garbage-state-not-in-db", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusBadRequest)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["error"] != "invalid_or_expired_state" {
		t.Errorf("error = %v, want invalid_or_expired_state", body["error"])
	}
}

func TestGitHubAppHandleInstallationCallback_ExpiredState(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{FrontendBaseURL: "https://app.example.com"}
	h := handlers.NewGitHubAppHandler(cfg, d)
	app := newGitHubAppTestApp(cfg, h)

	userID := seedGitHubHandlersTestUser(t, d)
	state := "expired-" + uuid.New().String()
	insertGitHubAppOAuthState(t, d, state, userID, "github_app_install", time.Now().UTC().Add(-1*time.Hour))

	resp, err := app.Test(httptest.NewRequest("GET", "/auth/github/app/install/callback?installation_id=99999&state="+state, nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusBadRequest)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["error"] != "invalid_or_expired_state" {
		t.Errorf("error = %v, want invalid_or_expired_state", body["error"])
	}
}

func TestGitHubAppHandleInstallationCallback_WrongKindState(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{FrontendBaseURL: "https://app.example.com"}
	h := handlers.NewGitHubAppHandler(cfg, d)
	app := newGitHubAppTestApp(cfg, h)

	userID := seedGitHubHandlersTestUser(t, d)
	state := "wrong-kind-" + uuid.New().String()
	// A state row that exists and is unexpired, but has a different kind
	// (e.g. left over from a "link GitHub account" flow) must not be
	// accepted by the install callback, which filters on kind =
	// 'github_app_install'.
	insertGitHubAppOAuthState(t, d, state, userID, "github_link", time.Now().UTC().Add(10*time.Minute))

	resp, err := app.Test(httptest.NewRequest("GET", "/auth/github/app/install/callback?installation_id=99999&state="+state, nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusBadRequest)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["error"] != "invalid_or_expired_state" {
		t.Errorf("error = %v, want invalid_or_expired_state", body["error"])
	}
}

// TestGitHubAppHandleInstallationCallback_MissingStateStillRedirectsSuccess
// documents actual behavior: an empty `state` query param is NOT treated as
// an error. The handler only validates state "if state != \"\""; when it's
// absent entirely, it skips validation, skips the repository sync, and
// still redirects to the frontend as if installation succeeded.
func TestGitHubAppHandleInstallationCallback_MissingStateStillRedirectsSuccess(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{FrontendBaseURL: "https://app.example.com"}
	h := handlers.NewGitHubAppHandler(cfg, d)
	app := newGitHubAppTestApp(cfg, h)

	resp, err := app.Test(httptest.NewRequest("GET", "/auth/github/app/install/callback?installation_id=99999", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusFound)
	}
	loc := resp.Header.Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location header %q doesn't parse as URL: %v", loc, err)
	}
	q := u.Query()
	if q.Get("github_app_installed") != "true" {
		t.Errorf("Location = %q, want github_app_installed=true", loc)
	}
	if q.Get("installation_id") != "99999" {
		t.Errorf("Location = %q, want installation_id=99999", loc)
	}
}

func TestGitHubAppHandleInstallationCallback_Success(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{FrontendBaseURL: "https://app.example.com"}
	h := handlers.NewGitHubAppHandler(cfg, d)
	app := newGitHubAppTestApp(cfg, h)

	userID := seedGitHubHandlersTestUser(t, d)
	state := "success-" + uuid.New().String()
	insertGitHubAppOAuthState(t, d, state, userID, "github_app_install", time.Now().UTC().Add(10*time.Minute))

	resp, err := app.Test(httptest.NewRequest("GET", "/auth/github/app/install/callback?installation_id=99999&state="+state+"&setup_action=install", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusFound)
	}
	loc := resp.Header.Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location header %q doesn't parse as URL: %v", loc, err)
	}
	if u.Scheme != "https" || u.Host != "app.example.com" || u.Path != "/dashboard" {
		t.Errorf("Location = %q, want https://app.example.com/dashboard...", loc)
	}
	q := u.Query()
	if q.Get("github_app_installed") != "true" {
		t.Errorf("Location = %q, want github_app_installed=true", loc)
	}
	if q.Get("installation_id") != "99999" {
		t.Errorf("Location = %q, want installation_id=99999", loc)
	}
	if q.Get("setup_action") != "install" {
		t.Errorf("Location = %q, want setup_action=install", loc)
	}

	// The consumed state should have been deleted (single use / anti-replay).
	var count int
	if err := d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM oauth_states WHERE state = $1`, state).Scan(&count); err != nil {
		t.Fatalf("query oauth_states: %v", err)
	}
	if count != 0 {
		t.Errorf("oauth_states row for consumed state still exists, want it deleted after use")
	}
}
