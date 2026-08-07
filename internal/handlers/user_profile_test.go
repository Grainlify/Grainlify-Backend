package handlers_test

// Scope note: covers handlers.UserProfileHandler (internal/handlers/user_profile.go)
// - Profile, PublicProfile, ContributionCalendar, ContributionActivity,
// ProjectsContributed, ProjectsLed, UpdateProfile, UpdateAvatar.
//
// Every identifier defined in this file is prefixed with "userProfileSuite"
// so it cannot collide with fixtures other concurrently-developed *_test.go
// files in this package define for their own domains. Where an existing,
// already-stable fixture from projects_public_test.go (projectsFxUser,
// projectsFxNextGHUserID, projectsFxEcosystem, projectsFxInsertProject,
// projectsFxProjectSpec, projectsFxDoJSON) already does exactly what's
// needed, it's reused rather than duplicated.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
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

// userProfileSuiteJWTSecret mints/validates tokens for every test in this file.
const userProfileSuiteJWTSecret = "user-profile-suite-test-jwt-secret"

// userProfileSuiteApp wires a fiber app exposing exactly the /profile*
// routes internal/api/api.go registers against handlers.UserProfileHandler,
// including the same auth.RequireAuth middleware production uses (and its
// absence on the one genuinely public route, /profile/public).
func userProfileSuiteApp(cfg config.Config, d *db.DB) *fiber.App {
	h := handlers.NewUserProfileHandler(cfg, d)
	app := fiber.New()
	app.Get("/profile", auth.RequireAuth(cfg.JWTSecret), h.Profile())
	app.Get("/profile/public", h.PublicProfile())
	app.Get("/profile/calendar", auth.RequireAuth(cfg.JWTSecret), h.ContributionCalendar())
	app.Get("/profile/activity", auth.RequireAuth(cfg.JWTSecret), h.ContributionActivity())
	app.Get("/profile/projects", auth.RequireAuth(cfg.JWTSecret), h.ProjectsContributed())
	app.Get("/profile/projects-led", auth.RequireAuth(cfg.JWTSecret), h.ProjectsLed())
	app.Put("/profile/update", auth.RequireAuth(cfg.JWTSecret), h.UpdateProfile())
	app.Put("/profile/avatar", auth.RequireAuth(cfg.JWTSecret), h.UpdateAvatar())
	return app
}

// userProfileSuiteJWT issues a real HS256 JWT via internal/auth's own issuing
// function, matching exactly what auth.RequireAuth expects.
func userProfileSuiteJWT(t *testing.T, userID uuid.UUID, role string) string {
	t.Helper()
	tok, err := auth.IssueJWT(userProfileSuiteJWTSecret, userID, role, "", "", time.Hour)
	if err != nil {
		t.Fatalf("userProfileSuiteJWT: issue jwt: %v", err)
	}
	return tok
}

// userProfileSuiteLinkGitHub inserts a github_accounts row linking userID to
// login. The access_token value is never decrypted successfully in these
// tests (config.Config.TokenEncKeyB64 is left unset), which is fine: every
// handler under test tolerates a failed token decrypt/lookup gracefully
// (falls back to an unauthenticated GitHub API call for avatar enrichment).
func userProfileSuiteLinkGitHub(t *testing.T, pool db.DBPool, userID uuid.UUID, login string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO github_accounts (user_id, github_user_id, login, access_token, token_type, scope)
VALUES ($1, $2, $3, $4, 'bearer', 'repo')
`, userID, projectsFxNextGHUserID(), login, []byte("dummy-token-for-tests"))
	if err != nil {
		t.Fatalf("userProfileSuiteLinkGitHub: %v", err)
	}
}

// userProfileSuiteContribSpec configures userProfileSuiteIssue/PR.
type userProfileSuiteContribSpec struct {
	ProjectID   uuid.UUID
	AuthorLogin string
	Number      int
	Title       string
	State       string    // defaults to "open"
	CreatedAt   time.Time // defaults to time.Now().UTC()
}

// userProfileSuiteIssue inserts a github_issues row per the schema in
// migrations/000003_github_issue_pr_sync.up.sql.
func userProfileSuiteIssue(t *testing.T, pool db.DBPool, spec userProfileSuiteContribSpec) uuid.UUID {
	t.Helper()
	if spec.State == "" {
		spec.State = "open"
	}
	if spec.CreatedAt.IsZero() {
		spec.CreatedAt = time.Now().UTC()
	}
	if spec.Title == "" {
		spec.Title = "test issue " + uuid.NewString()
	}
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO github_issues (project_id, github_issue_id, number, state, title, author_login, url, created_at_github)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id
`, spec.ProjectID, projectsFxNextGHUserID(), spec.Number, spec.State, spec.Title, spec.AuthorLogin,
		fmt.Sprintf("https://github.com/test-owner/test-repo/issues/%d", spec.Number), spec.CreatedAt).Scan(&id)
	if err != nil {
		t.Fatalf("userProfileSuiteIssue: insert: %v", err)
	}
	return id
}

// userProfileSuitePR inserts a github_pull_requests row per the schema in
// migrations/000003_github_issue_pr_sync.up.sql.
func userProfileSuitePR(t *testing.T, pool db.DBPool, spec userProfileSuiteContribSpec) uuid.UUID {
	t.Helper()
	if spec.State == "" {
		spec.State = "open"
	}
	if spec.CreatedAt.IsZero() {
		spec.CreatedAt = time.Now().UTC()
	}
	if spec.Title == "" {
		spec.Title = "test pr " + uuid.NewString()
	}
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO github_pull_requests (project_id, github_pr_id, number, state, title, author_login, url, merged, created_at_github)
VALUES ($1, $2, $3, $4, $5, $6, $7, false, $8)
RETURNING id
`, spec.ProjectID, projectsFxNextGHUserID(), spec.Number, spec.State, spec.Title, spec.AuthorLogin,
		fmt.Sprintf("https://github.com/test-owner/test-repo/pull/%d", spec.Number), spec.CreatedAt).Scan(&id)
	if err != nil {
		t.Fatalf("userProfileSuitePR: insert: %v", err)
	}
	return id
}

// userProfileSuiteDoRaw issues an HTTP request with a literal raw body
// string (used only where projectsFxDoJSON's "marshal a Go value" contract
// can't produce what's needed, e.g. deliberately malformed JSON).
func userProfileSuiteDoRaw(t *testing.T, app *fiber.App, method, path, bearer, rawBody string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(rawBody))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := app.Test(req, 20000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("userProfileSuiteDoRaw: read body: %v", err)
	}
	return resp.StatusCode, b
}

// userProfileSuiteRank mirrors the "rank" object nested in Profile()'s and
// PublicProfile()'s JSON responses.
type userProfileSuiteRank struct {
	Position  *int   `json:"position"`
	Tier      string `json:"tier"`
	TierName  string `json:"tier_name"`
	TierColor string `json:"tier_color"`
}

// userProfileSuiteProfileResp mirrors Profile()'s JSON response shape.
// Languages/Ecosystems are typed as slices (not map[string]any) specifically
// so that both a JSON `null` and a JSON `[]` decode uniformly to a
// zero-length slice - Profile() emits null for the "linked GitHub, zero
// contributions" case but [] for the "no GitHub account" case (see
// user_profile.go:53-57 vs the bare `var languages []fiber.Map` at :108).
type userProfileSuiteProfileResp struct {
	ContributionsCount         int                   `json:"contributions_count"`
	ProjectsContributedToCount int                   `json:"projects_contributed_to_count"`
	ProjectsLedCount           int                   `json:"projects_led_count"`
	RewardsCount               int                   `json:"rewards_count"`
	Languages                  []map[string]any      `json:"languages"`
	Ecosystems                 []map[string]any      `json:"ecosystems"`
	KYCVerified                bool                  `json:"kyc_verified"`
	Bio                        *string               `json:"bio"`
	Website                    *string               `json:"website"`
	Rank                       *userProfileSuiteRank `json:"rank"`
}

// userProfileSuiteFindByField returns the first item in items whose string
// field matches value, or nil.
func userProfileSuiteFindByField(items []map[string]any, field, value string) map[string]any {
	for _, it := range items {
		if s, ok := it[field].(string); ok && s == value {
			return it
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Profile()
// ---------------------------------------------------------------------------

func TestUserProfileHandler_Profile(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: userProfileSuiteJWTSecret}
	app := userProfileSuiteApp(cfg, d)

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "GET", "/profile", "", nil)
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("user with no linked github account gets a minimal zeroed response", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		token := userProfileSuiteJWT(t, owner, "contributor")

		status, body := projectsFxDoJSON(t, app, "GET", "/profile", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if cc, _ := raw["contributions_count"].(float64); cc != 0 {
			t.Errorf("contributions_count = %v, want 0", raw["contributions_count"])
		}
		if langs, _ := raw["languages"].([]any); len(langs) != 0 {
			t.Errorf("languages = %v, want empty", raw["languages"])
		}
		if _, hasRank := raw["rank"]; hasRank {
			t.Errorf("expected no \"rank\" key at all for a user with no linked GitHub account (early-return path only sets contributions_count/languages/ecosystems), got keys=%v", raw)
		}
		if _, hasProjectsLed := raw["projects_led_count"]; hasProjectsLed {
			t.Errorf("expected no \"projects_led_count\" key for a user with no linked GitHub account, got keys=%v", raw)
		}
	})

	t.Run("user with linked github but zero contributions gets full zeroed response, rank defaults to bronze", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		login := "profile-empty-" + uuid.NewString()[:8]
		userProfileSuiteLinkGitHub(t, d.Pool, owner, login)
		token := userProfileSuiteJWT(t, owner, "contributor")

		status, body := projectsFxDoJSON(t, app, "GET", "/profile", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp userProfileSuiteProfileResp
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, body)
		}
		if resp.ContributionsCount != 0 {
			t.Errorf("contributions_count = %d, want 0", resp.ContributionsCount)
		}
		if resp.ProjectsContributedToCount != 0 {
			t.Errorf("projects_contributed_to_count = %d, want 0", resp.ProjectsContributedToCount)
		}
		if resp.ProjectsLedCount != 0 {
			t.Errorf("projects_led_count = %d, want 0", resp.ProjectsLedCount)
		}
		if len(resp.Languages) != 0 {
			t.Errorf("languages = %v, want empty", resp.Languages)
		}
		if len(resp.Ecosystems) != 0 {
			t.Errorf("ecosystems = %v, want empty", resp.Ecosystems)
		}
		if resp.KYCVerified {
			t.Errorf("kyc_verified = true, want false")
		}
		if resp.Rank == nil {
			t.Fatal("expected a \"rank\" object even with zero contributions")
		}
		if resp.Rank.Position != nil {
			t.Errorf("rank.position = %v, want nil for a user with no contributions", *resp.Rank.Position)
		}
		// Surprise worth flagging: unlike PublicProfile() (which defaults an
		// unranked user's tier to "unranked"), Profile()'s no-rank fallback
		// is "bronze" - see user_profile.go:211-220 vs :1073-1080.
		if resp.Rank.Tier != "bronze" {
			t.Errorf("rank.tier = %q, want \"bronze\" (Profile()'s no-contributions default)", resp.Rank.Tier)
		}
	})

	t.Run("user with contributions returns aggregated stats", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		login := "profile-active-" + uuid.NewString()[:8]
		userProfileSuiteLinkGitHub(t, d.Pool, owner, login)
		ecoID, ecoName := projectsFxEcosystem(t, d.Pool)
		lang := "Go"

		contribProject := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
			OwnerUserID: owner, Status: "verified", NeedsMetadata: false,
			EcosystemID: &ecoID, Language: &lang,
		})
		userProfileSuiteIssue(t, d.Pool, userProfileSuiteContribSpec{ProjectID: contribProject, AuthorLogin: login, Number: 1})
		userProfileSuitePR(t, d.Pool, userProfileSuiteContribSpec{ProjectID: contribProject, AuthorLogin: login, Number: 1})

		// A project whose github_full_name happens to start with "<login>/"
		// counts toward projects_led_count here - Profile() computes "led"
		// via SPLIT_PART(github_full_name, '/', 1) matching the caller's
		// GitHub login (user_profile.go:251-257), NOT via the owner_user_id
		// column. Contrast with the dedicated ProjectsLed() endpoint, which
		// uses owner_user_id (:795-801) - the two can disagree.
		projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
			OwnerUserID: owner, Status: "verified", NeedsMetadata: false,
			GitHubFullName: login + "/led-repo-" + uuid.NewString()[:8],
		})

		if _, err := d.Pool.Exec(context.Background(), `UPDATE users SET bio = $1 WHERE id = $2`, "hello from profile suite", owner); err != nil {
			t.Fatalf("seed bio: %v", err)
		}

		token := userProfileSuiteJWT(t, owner, "contributor")
		status, body := projectsFxDoJSON(t, app, "GET", "/profile", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp userProfileSuiteProfileResp
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, body)
		}

		if resp.ContributionsCount != 2 {
			t.Errorf("contributions_count = %d, want 2", resp.ContributionsCount)
		}
		if resp.ProjectsContributedToCount != 1 {
			t.Errorf("projects_contributed_to_count = %d, want 1", resp.ProjectsContributedToCount)
		}
		if resp.ProjectsLedCount != 1 {
			t.Errorf("projects_led_count = %d, want 1 (via github_full_name prefix match)", resp.ProjectsLedCount)
		}
		if got := resp.Bio; got == nil || *got != "hello from profile suite" {
			t.Errorf("bio = %v, want \"hello from profile suite\"", got)
		}
		if resp.Website != nil {
			t.Errorf("website = %v, want nil/omitted (never set)", *resp.Website)
		}

		langEntry := userProfileSuiteFindByField(resp.Languages, "language", "Go")
		if langEntry == nil {
			t.Fatalf("expected a \"Go\" entry in languages, got %v", resp.Languages)
		}
		if cnt, _ := langEntry["contribution_count"].(float64); int(cnt) != 2 {
			t.Errorf("languages[Go].contribution_count = %v, want 2", langEntry["contribution_count"])
		}

		ecoEntry := userProfileSuiteFindByField(resp.Ecosystems, "ecosystem_name", ecoName)
		if ecoEntry == nil {
			t.Fatalf("expected a %q entry in ecosystems, got %v", ecoName, resp.Ecosystems)
		}
		if cnt, _ := ecoEntry["contribution_count"].(float64); int(cnt) != 2 {
			t.Errorf("ecosystems[%s].contribution_count = %v, want 2", ecoName, ecoEntry["contribution_count"])
		}

		if resp.Rank == nil || resp.Rank.Position == nil || *resp.Rank.Position < 1 {
			t.Errorf("rank.position = %v, want a positive int (this login now has >0 contributions)", resp.Rank)
		}
		if resp.Rank != nil && resp.Rank.Tier == "" {
			t.Errorf("rank.tier is empty, want a non-empty tier name")
		}

		// kyc_verified flips to true once kyc_status = 'verified'.
		if _, err := d.Pool.Exec(context.Background(), `UPDATE users SET kyc_status = 'verified' WHERE id = $1`, owner); err != nil {
			t.Fatalf("set kyc_status: %v", err)
		}
		status, body = projectsFxDoJSON(t, app, "GET", "/profile", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp2 userProfileSuiteProfileResp
		if err := json.Unmarshal(body, &resp2); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !resp2.KYCVerified {
			t.Errorf("kyc_verified = false after setting kyc_status='verified', want true")
		}
	})
}

// ---------------------------------------------------------------------------
// PublicProfile()
// ---------------------------------------------------------------------------

// userProfileSuitePublicResp mirrors PublicProfile()'s JSON response shape.
type userProfileSuitePublicResp struct {
	Login                      string                `json:"login"`
	UserID                     string                `json:"user_id"`
	AvatarURL                  string                `json:"avatar_url"`
	ContributionsCount         int                   `json:"contributions_count"`
	ProjectsContributedToCount int                   `json:"projects_contributed_to_count"`
	ProjectsLedCount           int                   `json:"projects_led_count"`
	Languages                  []map[string]any      `json:"languages"`
	Ecosystems                 []map[string]any      `json:"ecosystems"`
	KYCVerified                bool                  `json:"kyc_verified"`
	Rank                       *userProfileSuiteRank `json:"rank"`
}

func TestUserProfileHandler_PublicProfile(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: userProfileSuiteJWTSecret}
	app := userProfileSuiteApp(cfg, d)

	t.Run("missing identifier returns 400", func(t *testing.T) {
		status, body := projectsFxDoJSON(t, app, "GET", "/profile/public", "", nil)
		if status != fiber.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body=%s", status, body)
		}
		var resp map[string]any
		json.Unmarshal(body, &resp)
		if resp["error"] != "missing_identifier" {
			t.Errorf("error = %v, want missing_identifier", resp["error"])
		}
	})

	t.Run("invalid user_id format returns 400", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "GET", "/profile/public?user_id=not-a-uuid", "", nil)
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("no auth required - a request with no bearer token still succeeds", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		login := "public-noauth-" + uuid.NewString()[:8]
		userProfileSuiteLinkGitHub(t, d.Pool, owner, login)
		status, body := projectsFxDoJSON(t, app, "GET", "/profile/public?user_id="+owner.String(), "", nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200 with no Authorization header at all, body=%s", status, body)
		}
	})

	t.Run("unknown user_id returns 404", func(t *testing.T) {
		status, body := projectsFxDoJSON(t, app, "GET", "/profile/public?user_id="+uuid.NewString(), "", nil)
		if status != fiber.StatusNotFound {
			t.Fatalf("status = %d, want 404, body=%s", status, body)
		}
		var resp map[string]any
		json.Unmarshal(body, &resp)
		if resp["error"] != "user_not_found" {
			t.Errorf("error = %v, want user_not_found", resp["error"])
		}
	})

	// Surprise worth flagging: looking up by a *login* that doesn't exist in
	// github_accounts behaves completely differently from an unknown
	// user_id - it's a 200 with a synthetic "unranked" placeholder profile,
	// not a 404 (user_profile.go:897-915).
	t.Run("unknown login returns 200 with a synthetic unranked placeholder, NOT 404", func(t *testing.T) {
		unknownLogin := "no-such-login-" + uuid.NewString()
		status, body := projectsFxDoJSON(t, app, "GET", "/profile/public?login="+unknownLogin, "", nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200 (unknown login is NOT a 404 here), body=%s", status, body)
		}
		var resp userProfileSuitePublicResp
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Login != unknownLogin {
			t.Errorf("login = %q, want %q", resp.Login, unknownLogin)
		}
		if resp.UserID != "" {
			t.Errorf("user_id = %q, want empty string for an unknown/unregistered login", resp.UserID)
		}
		if resp.Rank == nil || resp.Rank.Tier != "unranked" {
			t.Errorf("rank.tier = %v, want \"unranked\"", resp.Rank)
		}
	})

	t.Run("existing user by user_id succeeds", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		login := "public-byid-" + uuid.NewString()[:8]
		userProfileSuiteLinkGitHub(t, d.Pool, owner, login)
		ecoID, ecoName := projectsFxEcosystem(t, d.Pool)
		lang := "Rust"
		proj := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
			OwnerUserID: owner, Status: "verified", NeedsMetadata: false,
			EcosystemID: &ecoID, Language: &lang,
		})
		userProfileSuiteIssue(t, d.Pool, userProfileSuiteContribSpec{ProjectID: proj, AuthorLogin: login, Number: 1})

		status, body := projectsFxDoJSON(t, app, "GET", "/profile/public?user_id="+owner.String(), "", nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp userProfileSuitePublicResp
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, body)
		}
		if resp.Login != login {
			t.Errorf("login = %q, want %q", resp.Login, login)
		}
		if resp.UserID != owner.String() {
			t.Errorf("user_id = %q, want %q", resp.UserID, owner.String())
		}
		if resp.ContributionsCount != 1 {
			t.Errorf("contributions_count = %d, want 1", resp.ContributionsCount)
		}
		if resp.AvatarURL == "" {
			t.Errorf("avatar_url = %q, want a non-empty fallback GitHub avatar URL", resp.AvatarURL)
		}
		ecoEntry := userProfileSuiteFindByField(resp.Ecosystems, "ecosystem_name", ecoName)
		if ecoEntry == nil {
			t.Errorf("expected ecosystem %q present, got %v", ecoName, resp.Ecosystems)
		}
	})

	t.Run("existing user by login succeeds, case-insensitively, and echoes the queried casing", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		storedLogin := "PublicByLogin-" + uuid.NewString()[:8]
		userProfileSuiteLinkGitHub(t, d.Pool, owner, storedLogin)

		queriedLogin := strings.ToUpper(storedLogin) // different casing than stored
		status, body := projectsFxDoJSON(t, app, "GET", "/profile/public?login="+queriedLogin, "", nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp userProfileSuitePublicResp
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.UserID != owner.String() {
			t.Errorf("user_id = %q, want %q (case-insensitive login lookup should have matched)", resp.UserID, owner.String())
		}
		// Surprise: the response echoes back the *queried* casing, not the
		// casing actually stored in github_accounts.login
		// (user_profile.go:917: `githubLogin = &loginParam`).
		if resp.Login != queriedLogin {
			t.Errorf("login = %q, want %q (echoes query param casing verbatim, not stored casing %q)", resp.Login, queriedLogin, storedLogin)
		}
	})
}

// ---------------------------------------------------------------------------
// ContributionCalendar()
// ---------------------------------------------------------------------------

type userProfileSuiteCalendarResp struct {
	Calendar []map[string]any `json:"calendar"`
	Total    int              `json:"total"`
}

func TestUserProfileHandler_ContributionCalendar(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: userProfileSuiteJWTSecret}
	app := userProfileSuiteApp(cfg, d)

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "GET", "/profile/calendar", "", nil)
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("invalid user_id query param returns 400", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		token := userProfileSuiteJWT(t, owner, "contributor")
		status, _ := projectsFxDoJSON(t, app, "GET", "/profile/calendar?user_id=not-a-uuid", token, nil)
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("no linked github account returns an empty calendar, not 365 zeroed days", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		token := userProfileSuiteJWT(t, owner, "contributor")
		status, body := projectsFxDoJSON(t, app, "GET", "/profile/calendar", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp userProfileSuiteCalendarResp
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Calendar) != 0 {
			t.Errorf("len(calendar) = %d, want 0 for a user with no linked GitHub account", len(resp.Calendar))
		}
		if resp.Total != 0 {
			t.Errorf("total = %d, want 0", resp.Total)
		}
	})

	t.Run("linked github, zero contributions still returns a full 365-day zeroed calendar", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		login := "cal-empty-" + uuid.NewString()[:8]
		userProfileSuiteLinkGitHub(t, d.Pool, owner, login)
		token := userProfileSuiteJWT(t, owner, "contributor")

		status, body := projectsFxDoJSON(t, app, "GET", "/profile/calendar", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp userProfileSuiteCalendarResp
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Calendar) != 365 {
			t.Errorf("len(calendar) = %d, want 365 for a linked account with zero contributions", len(resp.Calendar))
		}
		if resp.Total != 0 {
			t.Errorf("total = %d, want 0", resp.Total)
		}
	})

	// Real bug found while writing this test: user_profile.go:363-364 sets
	// now := time.Now().UTC() / startDate := now.AddDate(0,0,-365), then the
	// generation loop at :427-442 runs `for currentDate.Before(now) ||
	// currentDate.Equal(now.Truncate(24*time.Hour))`. Because currentDate
	// preserves now's original time-of-day at every step, the 365th and
	// final loop increment lands currentDate exactly on `now` (not before
	// it), and it only equals midnight-truncated `now` if the request
	// happens to run at exactly 00:00:00 UTC. So the loop stops one
	// iteration early: today's calendar-date entry is never generated at
	// all, even though the SQL aggregate (`created_at_github <= now`, feeding
	// `total`) DOES include a contribution made today. Net effect: a
	// contribution made "today" bumps `total` but is invisible in the
	// day-by-day `calendar` array - there is no cell for it to land in.
	t.Run("BUG: a contribution made today counts toward total but has no entry in the calendar array", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		login := "cal-todaybug-" + uuid.NewString()[:8]
		userProfileSuiteLinkGitHub(t, d.Pool, owner, login)
		ecoID, _ := projectsFxEcosystem(t, d.Pool)
		proj := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: owner, Status: "verified", NeedsMetadata: false, EcosystemID: &ecoID})
		userProfileSuiteIssue(t, d.Pool, userProfileSuiteContribSpec{ProjectID: proj, AuthorLogin: login, Number: 1, CreatedAt: time.Now().UTC()})
		token := userProfileSuiteJWT(t, owner, "contributor")

		status, body := projectsFxDoJSON(t, app, "GET", "/profile/calendar", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp userProfileSuiteCalendarResp
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Total != 1 {
			t.Fatalf("total = %d, want 1 (today's contribution IS counted in the aggregate)", resp.Total)
		}
		todayStr := time.Now().UTC().Format("2006-01-02")
		if entry := userProfileSuiteFindByField(resp.Calendar, "date", todayStr); entry != nil {
			t.Errorf("found a calendar entry for today (%s) with count=%v - if this now passes, the off-by-one in the day-generation loop (user_profile.go:427-442) has been fixed; please update this test's expectations rather than deleting it", todayStr, entry["count"])
		}
	})

	t.Run("returns aggregated total for own contributions", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		login := "cal-active-" + uuid.NewString()[:8]
		userProfileSuiteLinkGitHub(t, d.Pool, owner, login)
		ecoID, _ := projectsFxEcosystem(t, d.Pool)
		proj := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: owner, Status: "verified", NeedsMetadata: false, EcosystemID: &ecoID})
		yesterday := time.Now().UTC().AddDate(0, 0, -1)
		userProfileSuiteIssue(t, d.Pool, userProfileSuiteContribSpec{ProjectID: proj, AuthorLogin: login, Number: 1, CreatedAt: yesterday})
		userProfileSuitePR(t, d.Pool, userProfileSuiteContribSpec{ProjectID: proj, AuthorLogin: login, Number: 1, CreatedAt: yesterday})
		token := userProfileSuiteJWT(t, owner, "contributor")

		status, body := projectsFxDoJSON(t, app, "GET", "/profile/calendar", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp userProfileSuiteCalendarResp
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Total != 2 {
			t.Errorf("total = %d, want 2", resp.Total)
		}
		yStr := yesterday.Format("2006-01-02")
		entry := userProfileSuiteFindByField(resp.Calendar, "date", yStr)
		if entry == nil {
			t.Fatalf("expected a calendar entry for %s, got none", yStr)
		}
		if cnt, _ := entry["count"].(float64); int(cnt) != 2 {
			t.Errorf("calendar[%s].count = %v, want 2", yStr, entry["count"])
		}
		if lvl, _ := entry["level"].(float64); lvl < 1 {
			t.Errorf("calendar[%s].level = %v, want >=1 for a non-zero-count day", yStr, entry["level"])
		}
	})

	t.Run("viewing another user's calendar via login query param", func(t *testing.T) {
		subject := projectsFxUser(t, d.Pool)
		subjectLogin := "cal-subject-" + uuid.NewString()[:8]
		userProfileSuiteLinkGitHub(t, d.Pool, subject, subjectLogin)
		ecoID, _ := projectsFxEcosystem(t, d.Pool)
		proj := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: subject, Status: "verified", NeedsMetadata: false, EcosystemID: &ecoID})
		yesterday := time.Now().UTC().AddDate(0, 0, -1)
		userProfileSuiteIssue(t, d.Pool, userProfileSuiteContribSpec{ProjectID: proj, AuthorLogin: subjectLogin, Number: 1, CreatedAt: yesterday})

		viewer := projectsFxUser(t, d.Pool)
		viewerToken := userProfileSuiteJWT(t, viewer, "contributor")

		status, body := projectsFxDoJSON(t, app, "GET", "/profile/calendar?login="+subjectLogin, viewerToken, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp userProfileSuiteCalendarResp
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Total != 1 {
			t.Errorf("total = %d, want 1 (the subject's contribution, viewed via a different caller's token)", resp.Total)
		}
	})
}

// ---------------------------------------------------------------------------
// ContributionActivity()
// ---------------------------------------------------------------------------

type userProfileSuiteActivityResp struct {
	Activities []map[string]any `json:"activities"`
	Total      int              `json:"total"`
	Limit      int              `json:"limit"`
	Offset     int              `json:"offset"`
}

func TestUserProfileHandler_ContributionActivity(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: userProfileSuiteJWTSecret}
	app := userProfileSuiteApp(cfg, d)

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "GET", "/profile/activity", "", nil)
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("zero contributions returns an empty activities list, not an error", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		login := "activity-empty-" + uuid.NewString()[:8]
		userProfileSuiteLinkGitHub(t, d.Pool, owner, login)
		token := userProfileSuiteJWT(t, owner, "contributor")

		status, body := projectsFxDoJSON(t, app, "GET", "/profile/activity", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp userProfileSuiteActivityResp
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Activities) != 0 {
			t.Errorf("len(activities) = %d, want 0", len(resp.Activities))
		}
		if resp.Total != 0 {
			t.Errorf("total = %d, want 0", resp.Total)
		}
		if resp.Limit != 50 {
			t.Errorf("limit = %d, want default 50", resp.Limit)
		}
	})

	t.Run("returns issue and pull_request contributions with expected fields", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		login := "activity-active-" + uuid.NewString()[:8]
		userProfileSuiteLinkGitHub(t, d.Pool, owner, login)
		ecoID, _ := projectsFxEcosystem(t, d.Pool)
		proj := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: owner, Status: "verified", NeedsMetadata: false, EcosystemID: &ecoID})
		issueTitle := "activity issue " + uuid.NewString()
		prTitle := "activity pr " + uuid.NewString()
		userProfileSuiteIssue(t, d.Pool, userProfileSuiteContribSpec{ProjectID: proj, AuthorLogin: login, Number: 1, Title: issueTitle})
		userProfileSuitePR(t, d.Pool, userProfileSuiteContribSpec{ProjectID: proj, AuthorLogin: login, Number: 1, Title: prTitle})
		token := userProfileSuiteJWT(t, owner, "contributor")

		status, body := projectsFxDoJSON(t, app, "GET", "/profile/activity", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp userProfileSuiteActivityResp
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Total != 2 {
			t.Errorf("total = %d, want 2", resp.Total)
		}
		issueEntry := userProfileSuiteFindByField(resp.Activities, "title", issueTitle)
		if issueEntry == nil {
			t.Fatalf("expected an activity entry titled %q, got %d activities", issueTitle, len(resp.Activities))
		}
		if issueEntry["type"] != "issue" {
			t.Errorf("issue entry type = %v, want \"issue\"", issueEntry["type"])
		}
		prEntry := userProfileSuiteFindByField(resp.Activities, "title", prTitle)
		if prEntry == nil {
			t.Fatalf("expected an activity entry titled %q, got %d activities", prTitle, len(resp.Activities))
		}
		if prEntry["type"] != "pull_request" {
			t.Errorf("pr entry type = %v, want \"pull_request\"", prEntry["type"])
		}
		if prEntry["project_name"] == "" || prEntry["project_name"] == nil {
			t.Errorf("project_name is empty, want the project's github_full_name")
		}
	})

	t.Run("limit/offset pagination orders newest first", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		login := "activity-page-" + uuid.NewString()[:8]
		userProfileSuiteLinkGitHub(t, d.Pool, owner, login)
		ecoID, _ := projectsFxEcosystem(t, d.Pool)
		proj := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: owner, Status: "verified", NeedsMetadata: false, EcosystemID: &ecoID})

		now := time.Now().UTC()
		titleA := "page-A-" + uuid.NewString() // newest
		titleB := "page-B-" + uuid.NewString()
		titleC := "page-C-" + uuid.NewString() // oldest
		userProfileSuiteIssue(t, d.Pool, userProfileSuiteContribSpec{ProjectID: proj, AuthorLogin: login, Number: 1, Title: titleA, CreatedAt: now})
		userProfileSuiteIssue(t, d.Pool, userProfileSuiteContribSpec{ProjectID: proj, AuthorLogin: login, Number: 2, Title: titleB, CreatedAt: now.Add(-1 * time.Minute)})
		userProfileSuiteIssue(t, d.Pool, userProfileSuiteContribSpec{ProjectID: proj, AuthorLogin: login, Number: 3, Title: titleC, CreatedAt: now.Add(-2 * time.Minute)})
		token := userProfileSuiteJWT(t, owner, "contributor")

		status, body := projectsFxDoJSON(t, app, "GET", "/profile/activity?limit=2&offset=0", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var page1 userProfileSuiteActivityResp
		if err := json.Unmarshal(body, &page1); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if page1.Total != 3 {
			t.Errorf("total = %d, want 3", page1.Total)
		}
		if len(page1.Activities) != 2 {
			t.Fatalf("len(activities) = %d, want 2", len(page1.Activities))
		}
		if page1.Activities[0]["title"] != titleA || page1.Activities[1]["title"] != titleB {
			t.Errorf("page1 order = [%v, %v], want [%s, %s] (newest first)", page1.Activities[0]["title"], page1.Activities[1]["title"], titleA, titleB)
		}

		status, body = projectsFxDoJSON(t, app, "GET", "/profile/activity?limit=2&offset=2", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var page2 userProfileSuiteActivityResp
		if err := json.Unmarshal(body, &page2); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(page2.Activities) != 1 {
			t.Fatalf("len(activities) = %d, want 1", len(page2.Activities))
		}
		if page2.Activities[0]["title"] != titleC {
			t.Errorf("page2[0].title = %v, want %s", page2.Activities[0]["title"], titleC)
		}
	})

	t.Run("limit is capped at 100", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		token := userProfileSuiteJWT(t, owner, "contributor")
		status, body := projectsFxDoJSON(t, app, "GET", "/profile/activity?limit=500", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp userProfileSuiteActivityResp
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Limit != 100 {
			t.Errorf("limit = %d, want capped to 100", resp.Limit)
		}
	})
}

// ---------------------------------------------------------------------------
// ProjectsContributed()
// ---------------------------------------------------------------------------

func TestUserProfileHandler_ProjectsContributed(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: userProfileSuiteJWTSecret}
	app := userProfileSuiteApp(cfg, d)

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "GET", "/profile/projects", "", nil)
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("zero contributions returns an empty array, not an error", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		login := "projcontrib-empty-" + uuid.NewString()[:8]
		userProfileSuiteLinkGitHub(t, d.Pool, owner, login)
		token := userProfileSuiteJWT(t, owner, "contributor")

		status, body := projectsFxDoJSON(t, app, "GET", "/profile/projects", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var items []map[string]any
		if err := json.Unmarshal(body, &items); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, body)
		}
		if len(items) != 0 {
			t.Errorf("len(items) = %d, want 0", len(items))
		}
	})

	t.Run("returns distinct verified projects contributed to", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		login := "projcontrib-active-" + uuid.NewString()[:8]
		userProfileSuiteLinkGitHub(t, d.Pool, owner, login)
		ecoID, ecoName := projectsFxEcosystem(t, d.Pool)
		lang := "TypeScript"
		proj := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
			OwnerUserID: owner, Status: "verified", NeedsMetadata: false,
			EcosystemID: &ecoID, Language: &lang,
		})
		// Two contributions (issue + PR) to the SAME project must collapse
		// to one entry (DISTINCT project).
		userProfileSuiteIssue(t, d.Pool, userProfileSuiteContribSpec{ProjectID: proj, AuthorLogin: login, Number: 1})
		userProfileSuitePR(t, d.Pool, userProfileSuiteContribSpec{ProjectID: proj, AuthorLogin: login, Number: 1})
		token := userProfileSuiteJWT(t, owner, "contributor")

		status, body := projectsFxDoJSON(t, app, "GET", "/profile/projects", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var items []map[string]any
		if err := json.Unmarshal(body, &items); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, body)
		}
		if len(items) != 1 {
			t.Fatalf("len(items) = %d, want 1 (distinct project despite 2 contributions), body=%s", len(items), body)
		}
		if items[0]["id"] != proj.String() {
			t.Errorf("id = %v, want %v", items[0]["id"], proj.String())
		}
		if items[0]["ecosystem_name"] != ecoName {
			t.Errorf("ecosystem_name = %v, want %v", items[0]["ecosystem_name"], ecoName)
		}
		if items[0]["language"] != lang {
			t.Errorf("language = %v, want %v", items[0]["language"], lang)
		}
	})

	t.Run("viewing another user's contributed projects via user_id query param", func(t *testing.T) {
		subject := projectsFxUser(t, d.Pool)
		subjectLogin := "projcontrib-subject-" + uuid.NewString()[:8]
		userProfileSuiteLinkGitHub(t, d.Pool, subject, subjectLogin)
		ecoID, _ := projectsFxEcosystem(t, d.Pool)
		proj := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: subject, Status: "verified", NeedsMetadata: false, EcosystemID: &ecoID})
		userProfileSuiteIssue(t, d.Pool, userProfileSuiteContribSpec{ProjectID: proj, AuthorLogin: subjectLogin, Number: 1})

		viewer := projectsFxUser(t, d.Pool)
		viewerToken := userProfileSuiteJWT(t, viewer, "contributor")

		status, body := projectsFxDoJSON(t, app, "GET", "/profile/projects?user_id="+subject.String(), viewerToken, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var items []map[string]any
		if err := json.Unmarshal(body, &items); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(items) != 1 || items[0]["id"] != proj.String() {
			t.Errorf("items = %v, want exactly [%s] (subject's project, viewed via viewer's token)", items, proj)
		}
	})
}

// ---------------------------------------------------------------------------
// ProjectsLed()
// ---------------------------------------------------------------------------

func TestUserProfileHandler_ProjectsLed(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: userProfileSuiteJWTSecret}
	app := userProfileSuiteApp(cfg, d)

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "GET", "/profile/projects-led", "", nil)
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("zero led projects returns an empty array, not an error", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		token := userProfileSuiteJWT(t, owner, "contributor")
		status, body := projectsFxDoJSON(t, app, "GET", "/profile/projects-led", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var items []map[string]any
		if err := json.Unmarshal(body, &items); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, body)
		}
		if len(items) != 0 {
			t.Errorf("len(items) = %d, want 0", len(items))
		}
	})

	t.Run("returns verified projects owned by the caller (own token, no query params)", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		ecoID, ecoName := projectsFxEcosystem(t, d.Pool)
		led := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: owner, Status: "verified", NeedsMetadata: false, EcosystemID: &ecoID})
		// A pending_verification project owned by the same user must NOT appear.
		notVerified := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: owner, Status: "pending_verification", NeedsMetadata: false})
		token := userProfileSuiteJWT(t, owner, "contributor")

		status, body := projectsFxDoJSON(t, app, "GET", "/profile/projects-led", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var items []map[string]any
		if err := json.Unmarshal(body, &items); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, body)
		}
		ids := projectsFxIDSet(items)
		if !ids[led.String()] {
			t.Errorf("expected owned+verified project %s present, got %v", led, ids)
		}
		if ids[notVerified.String()] {
			t.Errorf("pending_verification project %s should not appear in ProjectsLed()", notVerified)
		}
		found := userProfileSuiteFindByField(items, "id", led.String())
		if found == nil || found["ecosystem_name"] != ecoName {
			t.Errorf("expected led project's ecosystem_name = %q, got %v", ecoName, found)
		}
	})

	t.Run("owner_user_id drives ProjectsLed(), unlike Profile()'s github_full_name-prefix heuristic", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		login := "led-vs-owner-" + uuid.NewString()[:8]
		userProfileSuiteLinkGitHub(t, d.Pool, owner, login)

		// Named as if "led" by this login, but owner_user_id points elsewhere.
		otherOwner := projectsFxUser(t, d.Pool)
		namedLikeLed := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
			OwnerUserID: otherOwner, Status: "verified", NeedsMetadata: false,
			GitHubFullName: login + "/not-actually-owned-" + uuid.NewString()[:8],
		})
		token := userProfileSuiteJWT(t, owner, "contributor")

		status, body := projectsFxDoJSON(t, app, "GET", "/profile/projects-led", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var items []map[string]any
		if err := json.Unmarshal(body, &items); err != nil {
			t.Fatalf("decode: %v", err)
		}
		ids := projectsFxIDSet(items)
		if ids[namedLikeLed.String()] {
			t.Errorf("project %s named like it's owned by %q but with a different owner_user_id should NOT appear in ProjectsLed() (owner_user_id-driven, not name-driven)", namedLikeLed, login)
		}
	})

	t.Run("unknown login query param returns an empty array, not 404", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		token := userProfileSuiteJWT(t, owner, "contributor")
		status, body := projectsFxDoJSON(t, app, "GET", "/profile/projects-led?login=no-such-login-"+uuid.NewString(), token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200 (unknown login is not a 404 here), body=%s", status, body)
		}
		var items []map[string]any
		if err := json.Unmarshal(body, &items); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(items) != 0 {
			t.Errorf("len(items) = %d, want 0", len(items))
		}
	})
}

// ---------------------------------------------------------------------------
// UpdateProfile()
// ---------------------------------------------------------------------------

// userProfileSuiteReadUserFields is the subset of a users row
// UpdateProfile() can mutate, read back directly via SQL for round-trip
// assertions.
type userProfileSuiteReadUserFields struct {
	FirstName, LastName, Location, Website, Bio    *string
	Telegram, LinkedIn, WhatsApp, Twitter, Discord *string
}

func userProfileSuiteReadUser(t *testing.T, pool db.DBPool, userID uuid.UUID) userProfileSuiteReadUserFields {
	t.Helper()
	var f userProfileSuiteReadUserFields
	err := pool.QueryRow(context.Background(), `
SELECT first_name, last_name, location, website, bio, telegram, linkedin, whatsapp, twitter, discord
FROM users WHERE id = $1
`, userID).Scan(&f.FirstName, &f.LastName, &f.Location, &f.Website, &f.Bio, &f.Telegram, &f.LinkedIn, &f.WhatsApp, &f.Twitter, &f.Discord)
	if err != nil {
		t.Fatalf("userProfileSuiteReadUser: %v", err)
	}
	return f
}

func userProfileSuiteStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestUserProfileHandler_UpdateProfile(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: userProfileSuiteJWTSecret}
	app := userProfileSuiteApp(cfg, d)

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "PUT", "/profile/update", "", map[string]any{"bio": "x"})
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("malformed JSON body returns 400 invalid_json", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		token := userProfileSuiteJWT(t, owner, "contributor")
		status, body := userProfileSuiteDoRaw(t, app, "PUT", "/profile/update", token, `{not-valid-json`)
		if status != fiber.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body=%s", status, body)
		}
		var resp map[string]any
		json.Unmarshal(body, &resp)
		if resp["error"] != "invalid_json" {
			t.Errorf("error = %v, want invalid_json", resp["error"])
		}
	})

	t.Run("empty body (no recognized fields) returns 400 no_fields_to_update", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		token := userProfileSuiteJWT(t, owner, "contributor")
		status, body := projectsFxDoJSON(t, app, "PUT", "/profile/update", token, map[string]any{})
		if status != fiber.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body=%s", status, body)
		}
		var resp map[string]any
		json.Unmarshal(body, &resp)
		if resp["error"] != "no_fields_to_update" {
			t.Errorf("error = %v, want no_fields_to_update", resp["error"])
		}
	})

	t.Run("updates all accepted fields and they persist, trimmed of whitespace", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		// Linked so the Profile() cross-check below takes its full-response
		// path rather than the no-GitHub-account early return (which omits
		// bio/website/etc entirely - see the dedicated Profile() tests).
		userProfileSuiteLinkGitHub(t, d.Pool, owner, "update-profile-"+uuid.NewString()[:8])
		token := userProfileSuiteJWT(t, owner, "contributor")
		suffix := uuid.NewString()[:8]

		payload := map[string]any{
			"first_name": "  John-" + suffix + "  ",
			"last_name":  "  Doe-" + suffix + "  ",
			"location":   "  Remote  ",
			"website":    "  https://example.com/" + suffix + "  ",
			"bio":        "  building things  ",
			"telegram":   "  @johndoe" + suffix + "  ",
			"linkedin":   "  linkedin.com/in/johndoe" + suffix + "  ",
			"whatsapp":   "  +10000000000  ",
			"twitter":    "  @johndoe_tw" + suffix + "  ",
			"discord":    "  johndoe#0001  ",
		}
		status, body := projectsFxDoJSON(t, app, "PUT", "/profile/update", token, payload)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp map[string]any
		json.Unmarshal(body, &resp)
		if resp["message"] != "profile_updated" {
			t.Errorf("message = %v, want profile_updated", resp["message"])
		}

		got := userProfileSuiteReadUser(t, d.Pool, owner)
		checks := []struct {
			name string
			got  *string
			want string
		}{
			{"first_name", got.FirstName, "John-" + suffix},
			{"last_name", got.LastName, "Doe-" + suffix},
			{"location", got.Location, "Remote"},
			{"website", got.Website, "https://example.com/" + suffix},
			{"bio", got.Bio, "building things"},
			{"telegram", got.Telegram, "@johndoe" + suffix},
			{"linkedin", got.LinkedIn, "linkedin.com/in/johndoe" + suffix},
			{"whatsapp", got.WhatsApp, "+10000000000"},
			{"twitter", got.Twitter, "@johndoe_tw" + suffix},
			{"discord", got.Discord, "johndoe#0001"},
		}
		for _, c := range checks {
			if userProfileSuiteStr(c.got) != c.want {
				t.Errorf("%s = %q, want %q (whitespace-trimmed)", c.name, userProfileSuiteStr(c.got), c.want)
			}
		}

		// Cross-check via Profile() itself: bio should now be visible there too.
		status, body = projectsFxDoJSON(t, app, "GET", "/profile", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("GET /profile status = %d, want 200, body=%s", status, body)
		}
		var profResp userProfileSuiteProfileResp
		json.Unmarshal(body, &profResp)
		if userProfileSuiteStr(profResp.Bio) != "building things" {
			t.Errorf("Profile().bio = %v, want \"building things\" (should reflect UpdateProfile()'s write)", profResp.Bio)
		}
	})

	t.Run("partial update leaves previously-set fields untouched", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		token := userProfileSuiteJWT(t, owner, "contributor")

		status, body := projectsFxDoJSON(t, app, "PUT", "/profile/update", token, map[string]any{"first_name": "Persisted"})
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		status, body = projectsFxDoJSON(t, app, "PUT", "/profile/update", token, map[string]any{"bio": "second update"})
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}

		got := userProfileSuiteReadUser(t, d.Pool, owner)
		if userProfileSuiteStr(got.FirstName) != "Persisted" {
			t.Errorf("first_name = %q, want \"Persisted\" to survive an unrelated later PUT that didn't mention it", userProfileSuiteStr(got.FirstName))
		}
		if userProfileSuiteStr(got.Bio) != "second update" {
			t.Errorf("bio = %q, want \"second update\"", userProfileSuiteStr(got.Bio))
		}
	})
}

// ---------------------------------------------------------------------------
// UpdateAvatar()
// ---------------------------------------------------------------------------

func TestUserProfileHandler_UpdateAvatar(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: userProfileSuiteJWTSecret}
	app := userProfileSuiteApp(cfg, d)

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "PUT", "/profile/avatar", "", map[string]any{"avatar_url": "https://example.com/a.png"})
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("empty avatar_url returns 400", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		token := userProfileSuiteJWT(t, owner, "contributor")
		status, body := projectsFxDoJSON(t, app, "PUT", "/profile/avatar", token, map[string]any{"avatar_url": ""})
		if status != fiber.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body=%s", status, body)
		}
		var resp map[string]any
		json.Unmarshal(body, &resp)
		if resp["error"] != "avatar_url_required" {
			t.Errorf("error = %v, want avatar_url_required", resp["error"])
		}
	})

	t.Run("invalid format returns 400", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		token := userProfileSuiteJWT(t, owner, "contributor")
		status, body := projectsFxDoJSON(t, app, "PUT", "/profile/avatar", token, map[string]any{"avatar_url": "not-a-url"})
		if status != fiber.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body=%s", status, body)
		}
		var resp map[string]any
		json.Unmarshal(body, &resp)
		if resp["error"] != "invalid_avatar_url_format" {
			t.Errorf("error = %v, want invalid_avatar_url_format", resp["error"])
		}
	})

	t.Run("valid https URL persists", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		token := userProfileSuiteJWT(t, owner, "contributor")
		url := "https://example.com/avatar-" + uuid.NewString() + ".png"
		status, body := projectsFxDoJSON(t, app, "PUT", "/profile/avatar", token, map[string]any{"avatar_url": url})
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp map[string]any
		json.Unmarshal(body, &resp)
		if resp["avatar_url"] != url {
			t.Errorf("avatar_url in response = %v, want %v", resp["avatar_url"], url)
		}

		var stored *string
		if err := d.Pool.QueryRow(context.Background(), `SELECT avatar_url FROM users WHERE id = $1`, owner).Scan(&stored); err != nil {
			t.Fatalf("read back avatar_url: %v", err)
		}
		if userProfileSuiteStr(stored) != url {
			t.Errorf("stored avatar_url = %q, want %q", userProfileSuiteStr(stored), url)
		}
	})

	t.Run("valid data:image/ URL is also accepted", func(t *testing.T) {
		owner := projectsFxUser(t, d.Pool)
		token := userProfileSuiteJWT(t, owner, "contributor")
		dataURL := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAUA"
		status, body := projectsFxDoJSON(t, app, "PUT", "/profile/avatar", token, map[string]any{"avatar_url": dataURL})
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var stored *string
		if err := d.Pool.QueryRow(context.Background(), `SELECT avatar_url FROM users WHERE id = $1`, owner).Scan(&stored); err != nil {
			t.Fatalf("read back avatar_url: %v", err)
		}
		if userProfileSuiteStr(stored) != dataURL {
			t.Errorf("stored avatar_url = %q, want %q", userProfileSuiteStr(stored), dataURL)
		}
	})
}
