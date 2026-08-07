package handlers_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// seedGitHubHandlersTestUser inserts a minimal user row for the GitHub OAuth
// and GitHub App handler tests (github_oauth_test.go, github_app_test.go)
// and returns its id. The row (and anything that FK-cascades from it, e.g.
// oauth_states/github_accounts rows) is deleted when the test completes.
func seedGitHubHandlersTestUser(t *testing.T, d *db.DB) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO users (role) VALUES ('contributor') RETURNING id
`).Scan(&id)
	if err != nil {
		t.Fatalf("seedGitHubHandlersTestUser: insert user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func TestGitHubOAuthLoginStart_Redirects(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{
		GitHubOAuthClientID:    "test-login-client-id",
		GitHubOAuthRedirectURL: "http://localhost:8080/auth/github/login/callback",
	}
	h := handlers.NewGitHubOAuthHandler(cfg, d)

	app := fiber.New()
	app.Get("/auth/github/login/start", h.LoginStart())

	resp, err := app.Test(httptest.NewRequest("GET", "/auth/github/login/start", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusFound)
	}

	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatal("Location header is empty")
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location header %q doesn't parse as URL: %v", loc, err)
	}
	q := u.Query()
	if got := q.Get("client_id"); got != cfg.GitHubOAuthClientID {
		t.Errorf("client_id = %q, want %q", got, cfg.GitHubOAuthClientID)
	}
	if got := q.Get("redirect_uri"); got != cfg.GitHubOAuthRedirectURL {
		t.Errorf("redirect_uri = %q, want %q", got, cfg.GitHubOAuthRedirectURL)
	}
	if q.Get("state") == "" {
		t.Error("state query param is empty, want a non-empty state")
	}
}

func TestGitHubOAuthStatus_NoAuthorizationHeader(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: "test-jwt-secret-github-oauth-status"}
	h := handlers.NewGitHubOAuthHandler(cfg, d)

	app := fiber.New()
	app.Get("/auth/github/status", auth.RequireAuth(cfg.JWTSecret), h.Status())

	resp, err := app.Test(httptest.NewRequest("GET", "/auth/github/status", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
	}
}

func TestGitHubOAuthStatus_ValidJWT_NotLinked(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: "test-jwt-secret-github-oauth-status"}
	h := handlers.NewGitHubOAuthHandler(cfg, d)

	app := fiber.New()
	app.Get("/auth/github/status", auth.RequireAuth(cfg.JWTSecret), h.Status())

	userID := seedGitHubHandlersTestUser(t, d)
	token, err := auth.IssueJWT(cfg.JWTSecret, userID, "contributor", "", "", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}

	req := httptest.NewRequest("GET", "/auth/github/status", nil)
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
	if linked, ok := body["linked"].(bool); !ok || linked {
		t.Errorf("linked = %v, want false", body["linked"])
	}
}

func TestGitHubOAuthCallbackUnified_MissingCode(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{
		GitHubOAuthClientID:     "test-callback-client-id",
		GitHubOAuthClientSecret: "test-callback-client-secret",
		GitHubOAuthRedirectURL:  "http://localhost:8080/auth/github/login/callback",
		JWTSecret:               "test-jwt-secret-github-oauth-callback",
	}
	h := handlers.NewGitHubOAuthHandler(cfg, d)

	app := fiber.New()
	app.Get("/auth/github/callback", h.CallbackUnified())

	resp, err := app.Test(httptest.NewRequest("GET", "/auth/github/callback?state=some-state", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusBadRequest)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["error"] != "missing_code_or_state" {
		t.Errorf("error = %v, want missing_code_or_state", body["error"])
	}
}

func TestGitHubOAuthCallbackUnified_MissingState(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{
		GitHubOAuthClientID:     "test-callback-client-id",
		GitHubOAuthClientSecret: "test-callback-client-secret",
		GitHubOAuthRedirectURL:  "http://localhost:8080/auth/github/login/callback",
		JWTSecret:               "test-jwt-secret-github-oauth-callback",
	}
	h := handlers.NewGitHubOAuthHandler(cfg, d)

	app := fiber.New()
	app.Get("/auth/github/callback", h.CallbackUnified())

	resp, err := app.Test(httptest.NewRequest("GET", "/auth/github/callback?code=some-code", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusBadRequest)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["error"] != "missing_code_or_state" {
		t.Errorf("error = %v, want missing_code_or_state", body["error"])
	}
}

func TestGitHubOAuthCallbackUnified_GarbageState(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{
		GitHubOAuthClientID:     "test-callback-client-id",
		GitHubOAuthClientSecret: "test-callback-client-secret",
		GitHubOAuthRedirectURL:  "http://localhost:8080/auth/github/login/callback",
		JWTSecret:               "test-jwt-secret-github-oauth-callback",
	}
	h := handlers.NewGitHubOAuthHandler(cfg, d)

	app := fiber.New()
	app.Get("/auth/github/callback", h.CallbackUnified())

	resp, err := app.Test(httptest.NewRequest("GET", "/auth/github/callback?code=some-code&state=totally-garbage-state-not-in-db", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusBadRequest)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["error"] != "invalid_or_expired_state" {
		t.Errorf("error = %v, want invalid_or_expired_state", body["error"])
	}
}
