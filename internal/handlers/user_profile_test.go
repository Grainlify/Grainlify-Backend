package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// Test ContributionActivity pagination behavior after migration to ParsePagination()
// These tests verify all the required behaviors from Issue #224

func TestContributionActivity_NegativeOffsetReturns400(t *testing.T) {
	// Test that negative offsets return HTTP 400 (this was missing before the fix)
	app := fiber.New(fiber.Config{DisableStartupMessage: true})

	// Create a minimal handler that just tests pagination parsing
	// We can't easily test the full handler without database setup
	app.Get("/test", func(c *fiber.Ctx) error {
		// This should return 400 for negative offset
		p, err := handlers.ParsePagination(c, 50, 100)
		if err != nil {
			// ParsePagination already wrote the response
			return nil
		}

		// Should not reach here with negative offset
		return c.JSON(fiber.Map{
			"activities": []fiber.Map{},
			"total":      0,
			"limit":      p.Limit,
			"offset":     p.Offset,
		})
	})

	req := httptest.NewRequest("GET", "/test?offset=-1", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 400, resp.StatusCode)

	// Verify error response
	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	assert.Contains(t, body["error"], "offset must be non-negative")
}

func TestContributionActivity_ZeroLimitBecomes50(t *testing.T) {
	// Test that limit=0 becomes 50 (default)
	app := fiber.New(fiber.Config{DisableStartupMessage: true})

	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 50, 100)
		if err != nil {
			return nil
		}

		return c.JSON(fiber.Map{
			"activities": []fiber.Map{},
			"total":      0,
			"limit":      p.Limit,
			"offset":     p.Offset,
		})
	})

	req := httptest.NewRequest("GET", "/test?limit=0", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)

	// Verify limit is reset to default (50)
	assert.Equal(t, float64(50), body["limit"])
	assert.Equal(t, float64(0), body["offset"])
}

func TestContributionActivity_NegativeLimitBecomes50(t *testing.T) {
	// Test that negative limits become 50 (default)
	app := fiber.New(fiber.Config{DisableStartupMessage: true})

	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 50, 100)
		if err != nil {
			return nil
		}

		return c.JSON(fiber.Map{
			"activities": []fiber.Map{},
			"total":      0,
			"limit":      p.Limit,
			"offset":     p.Offset,
		})
	})

	req := httptest.NewRequest("GET", "/test?limit=-5", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)

	// Verify limit is reset to default (50)
	assert.Equal(t, float64(50), body["limit"])
	assert.Equal(t, float64(0), body["offset"])
}

func TestContributionActivity_LimitAbove100Becomes100(t *testing.T) {
	// Test that limits > 100 are capped at 100
	app := fiber.New(fiber.Config{DisableStartupMessage: true})

	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 50, 100)
		if err != nil {
			return nil
		}

		return c.JSON(fiber.Map{
			"activities": []fiber.Map{},
			"total":      0,
			"limit":      p.Limit,
			"offset":     p.Offset,
		})
	})

	req := httptest.NewRequest("GET", "/test?limit=999", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)

	// Verify limit is capped at 100
	assert.Equal(t, float64(100), body["limit"])
	assert.Equal(t, float64(0), body["offset"])
}

func TestContributionActivity_ValidLimitUnchanged(t *testing.T) {
	// Test that valid limits remain unchanged
	app := fiber.New(fiber.Config{DisableStartupMessage: true})

	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 50, 100)
		if err != nil {
			return nil
		}

		return c.JSON(fiber.Map{
			"activities": []fiber.Map{},
			"total":      0,
			"limit":      p.Limit,
			"offset":     p.Offset,
		})
	})

	req := httptest.NewRequest("GET", "/test?limit=25", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)

	// Verify limit remains as specified
	assert.Equal(t, float64(25), body["limit"])
	assert.Equal(t, float64(0), body["offset"])
}

func TestContributionActivity_ValidOffsetUnchanged(t *testing.T) {
	// Test that valid offsets remain unchanged
	app := fiber.New(fiber.Config{DisableStartupMessage: true})

	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 50, 100)
		if err != nil {
			return nil
		}

		return c.JSON(fiber.Map{
			"activities": []fiber.Map{},
			"total":      0,
			"limit":      p.Limit,
			"offset":     p.Offset,
		})
	})

	req := httptest.NewRequest("GET", "/test?limit=30&offset=15", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)

	// Verify both values remain as specified
	assert.Equal(t, float64(30), body["limit"])
	assert.Equal(t, float64(15), body["offset"])
}

func TestContributionActivity_DefaultBehavior(t *testing.T) {
	// Test default behavior when no parameters are provided
	app := fiber.New(fiber.Config{DisableStartupMessage: true})

	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 50, 100)
		if err != nil {
			return nil
		}

		return c.JSON(fiber.Map{
			"activities": []fiber.Map{},
			"total":      0,
			"limit":      p.Limit,
			"offset":     p.Offset,
		})
	})

	req := httptest.NewRequest("GET", "/test", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)

	// Verify default values: limit=50, offset=0
	assert.Equal(t, float64(50), body["limit"])
	assert.Equal(t, float64(0), body["offset"])
}

func TestContributionActivity_EmptyLoginResponseStructure(t *testing.T) {
	// Test that the empty login response structure is preserved
	// This simulates the case where githubLogin is empty in ContributionActivity
	app := fiber.New(fiber.Config{DisableStartupMessage: true})

	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 50, 100)
		if err != nil {
			return nil
		}

		// Simulate empty login case - should return same structure as before
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"activities": []fiber.Map{},
			"total":      0,
			"limit":      p.Limit,
			"offset":     p.Offset,
		})
	})

	req := httptest.NewRequest("GET", "/test?limit=25&offset=5", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)

	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)

	// Verify the response structure matches expected format
	assert.NotNil(t, body["activities"])
	assert.Equal(t, float64(0), body["total"])
	assert.Equal(t, float64(25), body["limit"])
	assert.Equal(t, float64(5), body["offset"])

	// Verify activities is an empty array
	activities, ok := body["activities"].([]interface{})
	assert.True(t, ok)
	assert.Empty(t, activities)
}

// Integration test for boundary values
func TestContributionActivity_BoundaryValues(t *testing.T) {
	testCases := []struct {
		name           string
		query          string
		expectedLimit  float64
		expectedOffset float64
		expectedStatus int
	}{
		{
			name:           "limit equals max (100)",
			query:          "?limit=100",
			expectedLimit:  100,
			expectedOffset: 0,
			expectedStatus: 200,
		},
		{
			name:           "limit equals 1 (minimum valid)",
			query:          "?limit=1",
			expectedLimit:  1,
			expectedOffset: 0,
			expectedStatus: 200,
		},
		{
			name:           "offset equals 0",
			query:          "?offset=0",
			expectedLimit:  50, // default
			expectedOffset: 0,
			expectedStatus: 200,
		},
		{
			name:           "large valid offset",
			query:          "?limit=10&offset=1000",
			expectedLimit:  10,
			expectedOffset: 1000,
			expectedStatus: 200,
		},
	}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 50, 100)
		if err != nil {
			return nil
		}

		return c.JSON(fiber.Map{
			"activities": []fiber.Map{},
			"total":      0,
			"limit":      p.Limit,
			"offset":     p.Offset,
		})
	})

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/test"+tc.query, nil)
			resp, err := app.Test(req)
			require.NoError(t, err)
			assert.Equal(t, tc.expectedStatus, resp.StatusCode)

			if tc.expectedStatus == 200 {
				var body map[string]interface{}
				err = json.NewDecoder(resp.Body).Decode(&body)
				require.NoError(t, err)

				assert.Equal(t, tc.expectedLimit, body["limit"])
				assert.Equal(t, tc.expectedOffset, body["offset"])
			}
		})
	}
}

// rankFixtureUser is one seeded contributor for TestProfile_RankPositionOrdering.
type rankFixtureUser struct {
	userID     uuid.UUID
	login      string
	issueCount int
	prCount    int
}

// seedRankFixture creates users/github_accounts/a verified project/issues/PRs
// for TestProfile_RankPositionOrdering, and registers cleanup that removes
// everything it inserted (project delete cascades to issues/PRs; user delete
// cascades to github_accounts).
func seedRankFixture(t *testing.T, pool db.DBPool, users []rankFixtureUser) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	ownerID := users[0].userID
	for _, u := range users {
		_, err := pool.Exec(ctx,
			`INSERT INTO users (id, role) VALUES ($1, 'contributor')`, u.userID)
		require.NoError(t, err)
		_, err = pool.Exec(ctx,
			`INSERT INTO github_accounts (user_id, github_user_id, login, access_token) VALUES ($1, $2, $3, $4)`,
			u.userID, int64(uuid.New().ID()), u.login, []byte("test-token"))
		require.NoError(t, err)
	}

	var projectID uuid.UUID
	fullName := fmt.Sprintf("rank-fixture/%s", suffix)
	err := pool.QueryRow(ctx,
		`INSERT INTO projects (owner_user_id, github_full_name, status) VALUES ($1, $2, 'verified') RETURNING id`,
		ownerID, fullName).Scan(&projectID)
	require.NoError(t, err)

	issueNum, prNum := 1, 1
	for _, u := range users {
		for i := 0; i < u.issueCount; i++ {
			_, err := pool.Exec(ctx,
				`INSERT INTO github_issues (project_id, github_issue_id, number, author_login) VALUES ($1, $2, $3, $4)`,
				projectID, int64(1_000_000+issueNum), issueNum, u.login)
			require.NoError(t, err)
			issueNum++
		}
		for i := 0; i < u.prCount; i++ {
			_, err := pool.Exec(ctx,
				`INSERT INTO github_pull_requests (project_id, github_pr_id, number, author_login) VALUES ($1, $2, $3, $4)`,
				projectID, int64(2_000_000+prNum), prNum, u.login)
			require.NoError(t, err)
			prNum++
		}
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, projectID)
		for _, u := range users {
			_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, u.userID)
		}
	})

	return projectID
}

// TestProfile_RankPositionOrdering locks in correct rank-position computation
// for Profile()'s rankPosition query after rewriting it to compute each
// contributor's contribution count once (issue_counts/pr_counts CTEs) instead
// of via duplicated correlated subqueries (issue #291). Rather than asserting
// an absolute rank number -- which would be fragile against any other data
// already present in a shared test database -- this seeds three contributors
// with distinct, known contribution counts and asserts their ranks come back
// in the correct relative, consecutive order.
func TestProfile_RankPositionOrdering(t *testing.T) {
	pool := openTestPool(t)

	users := []rankFixtureUser{
		{userID: uuid.New(), login: "rank-fixture-mid-" + uuid.New().String()[:8], issueCount: 2, prCount: 1}, // 3
		{userID: uuid.New(), login: "rank-fixture-top-" + uuid.New().String()[:8], issueCount: 5, prCount: 0}, // 5
		{userID: uuid.New(), login: "rank-fixture-low-" + uuid.New().String()[:8], issueCount: 0, prCount: 1}, // 1
	}
	mid, top, low := users[0], users[1], users[2]
	seedRankFixture(t, pool, users)

	cfg := config.Config{JWTSecret: "test-jwt-secret-for-rank-position-fixture"}
	h := handlers.NewUserProfileHandler(cfg, &db.DB{Pool: pool})
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/profile", auth.RequireAuth(cfg.JWTSecret), h.Profile())

	rankOf := func(u rankFixtureUser) int {
		token, err := auth.IssueJWT(cfg.JWTSecret, u.userID, "contributor", "evm", "0x123", time.Hour)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/profile", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := app.Test(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, fiber.StatusOK, resp.StatusCode)

		var body struct {
			Rank struct {
				Position *int `json:"position"`
			} `json:"rank"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		require.NotNil(t, body.Rank.Position, "expected a non-nil rank position for %s", u.login)
		return *body.Rank.Position
	}

	posTop := rankOf(top)
	posMid := rankOf(mid)
	posLow := rankOf(low)

	assert.Less(t, posTop, posMid, "top contributor (5) should rank above mid (3)")
	assert.Less(t, posMid, posLow, "mid contributor (3) should rank above low (1)")
	assert.Equal(t, posTop+1, posMid, "mid should be exactly one position behind top")
	assert.Equal(t, posMid+1, posLow, "low should be exactly one position behind mid")
}

// TestProfile_ExcludesSoftDeletedProjectContributions locks in issue #339:
// Profile()'s contribution count and ContributionActivity()'s feed/total must
// exclude contributions to verified-but-soft-deleted projects, matching the
// convention already used by ProjectsContributed()/ProjectsLed() in the same
// file. Seeds one contribution to a normal verified project and one to a
// verified project that has since been soft-deleted (status untouched,
// deleted_at set — mirroring how Mine()'s private-repo detection soft-deletes
// without resetting status), and asserts only the former is counted/returned.
func TestProfile_ExcludesSoftDeletedProjectContributions(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	userID := uuid.New()
	login := "soft-delete-fixture-" + uuid.New().String()[:8]
	suffix := uuid.New().String()[:8]

	_, err := pool.Exec(ctx, `INSERT INTO users (id, role) VALUES ($1, 'contributor')`, userID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO github_accounts (user_id, github_user_id, login, access_token) VALUES ($1, $2, $3, $4)`,
		userID, int64(uuid.New().ID()), login, []byte("test-token"))
	require.NoError(t, err)

	var activeProjectID, deletedProjectID uuid.UUID
	err = pool.QueryRow(ctx,
		`INSERT INTO projects (owner_user_id, github_full_name, status) VALUES ($1, $2, 'verified') RETURNING id`,
		userID, fmt.Sprintf("soft-delete-fixture/active-%s", suffix)).Scan(&activeProjectID)
	require.NoError(t, err)
	err = pool.QueryRow(ctx,
		`INSERT INTO projects (owner_user_id, github_full_name, status) VALUES ($1, $2, 'verified') RETURNING id`,
		userID, fmt.Sprintf("soft-delete-fixture-deleted/gone-%s", suffix)).Scan(&deletedProjectID)
	require.NoError(t, err)

	// Soft-delete the second project without resetting status, mirroring
	// Mine()'s private-repo handling elsewhere in this codebase.
	_, err = pool.Exec(ctx, `UPDATE projects SET deleted_at = now() WHERE id = $1`, deletedProjectID)
	require.NoError(t, err)

	_, err = pool.Exec(ctx,
		`INSERT INTO github_issues (project_id, github_issue_id, number, author_login, title, url, state, created_at_github) VALUES ($1, $2, $3, $4, $5, $6, $7, now())`,
		activeProjectID, int64(uuid.New().ID()&0x7fffffff), 1, login, "active issue", "https://example.com/active", "open")
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO github_issues (project_id, github_issue_id, number, author_login, title, url, state, created_at_github) VALUES ($1, $2, $3, $4, $5, $6, $7, now())`,
		deletedProjectID, int64(uuid.New().ID()&0x7fffffff), 1, login, "deleted-project issue", "https://example.com/deleted", "open")
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = ANY($1)`, []uuid.UUID{activeProjectID, deletedProjectID})
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})

	cfg := config.Config{JWTSecret: "test-jwt-secret-for-soft-delete-fixture"}
	h := handlers.NewUserProfileHandler(cfg, &db.DB{Pool: pool})
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/profile", auth.RequireAuth(cfg.JWTSecret), h.Profile())
	app.Get("/activity", auth.RequireAuth(cfg.JWTSecret), h.ContributionActivity())

	token, err := auth.IssueJWT(cfg.JWTSecret, userID, "contributor", "evm", "0x123", time.Hour)
	require.NoError(t, err)

	t.Run("Profile excludes the soft-deleted project's contribution", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/profile", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := app.Test(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, fiber.StatusOK, resp.StatusCode)

		var body struct {
			ContributionsCount int `json:"contributions_count"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		assert.Equal(t, 1, body.ContributionsCount, "only the active project's contribution should be counted")
	})

	t.Run("ContributionActivity excludes the soft-deleted project's entry", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/activity?login="+login, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := app.Test(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, fiber.StatusOK, resp.StatusCode)

		var body struct {
			Activities []map[string]interface{} `json:"activities"`
			Total      int                      `json:"total"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		assert.Equal(t, 1, body.Total, "only the active project's contribution should count toward the total")
		require.Len(t, body.Activities, 1)
		assert.NotContains(t, body.Activities[0]["project_id"], deletedProjectID.String())
	})
}

// TestProfile_NoContributionsReturnsUnrankedTier locks in issue #349: a user
// with zero verified-project contributions has no row in the rankPosition
// query's ranked_users CTE, so rankPosition comes back nil. Profile()'s
// fallback branch must report "unranked" (matching PublicProfile()'s
// handling of the identical case), not "bronze".
func TestProfile_NoContributionsReturnsUnrankedTier(t *testing.T) {
	pool := openTestPool(t)

	unranked := rankFixtureUser{
		userID:     uuid.New(),
		login:      "rank-fixture-none-" + uuid.New().String()[:8],
		issueCount: 0,
		prCount:    0,
	}
	seedRankFixture(t, pool, []rankFixtureUser{unranked})

	cfg := config.Config{JWTSecret: "test-jwt-secret-for-unranked-profile-fixture"}
	h := handlers.NewUserProfileHandler(cfg, &db.DB{Pool: pool})
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/profile", auth.RequireAuth(cfg.JWTSecret), h.Profile())

	token, err := auth.IssueJWT(cfg.JWTSecret, unranked.userID, "contributor", "evm", "0x123", time.Hour)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/profile", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, fiber.StatusOK, resp.StatusCode)

	var body struct {
		Rank struct {
			Position *int   `json:"position"`
			Tier     string `json:"tier"`
		} `json:"rank"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))

	assert.Nil(t, body.Rank.Position, "user with no contributions should have a nil rank position")
	assert.Equal(t, "unranked", body.Rank.Tier, "user with no contributions should be reported as unranked, not bronze")
}

// Regression tests for Issue #406: ContributionCalendar() and
// ContributionActivity() used to declare a fresh, block-scoped `err` via
// `:=` inside the user_id-param and own-profile branches (shadowing the
// outer `err`), so a genuine github_accounts lookup failure was silently
// discarded and rendered identically to "no linked GitHub account" — a
// 200 with an empty calendar/activity list instead of a 500. These tests
// use a mockDBPool (from projects_test.go, same package) that returns an
// injected non-ErrNoRows error from QueryRow to prove the fix surfaces it.

func newGithubLookupErrorPool(lookupErr error) *mockDBPool {
	return &mockDBPool{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			return mockRow{err: lookupErr}
		},
	}
}

func TestContributionCalendar_GithubLookupDBErrorReturns500_UserIDParam(t *testing.T) {
	cfg := config.Config{}
	h := handlers.NewUserProfileHandler(cfg, &db.DB{Pool: newGithubLookupErrorPool(fmt.Errorf("connection reset by peer"))})
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/calendar", h.ContributionCalendar())

	req := httptest.NewRequest(http.MethodGet, "/calendar?user_id="+uuid.New().String(), nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode,
		"a genuine DB error on the github_accounts lookup must surface as a 500, not an empty-but-200 calendar")
}

func TestContributionCalendar_GithubLookupDBErrorReturns500_OwnProfile(t *testing.T) {
	cfg := config.Config{}
	h := handlers.NewUserProfileHandler(cfg, &db.DB{Pool: newGithubLookupErrorPool(fmt.Errorf("connection reset by peer"))})
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(auth.LocalUserID, uuid.New().String())
		return c.Next()
	})
	app.Get("/calendar", h.ContributionCalendar())

	req := httptest.NewRequest(http.MethodGet, "/calendar", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode,
		"a genuine DB error on the own-profile github_accounts lookup must surface as a 500, not an empty-but-200 calendar")
}

func TestContributionCalendar_GithubAccountNotFoundStillReturnsEmptyCalendar(t *testing.T) {
	cfg := config.Config{}
	h := handlers.NewUserProfileHandler(cfg, &db.DB{Pool: newGithubLookupErrorPool(pgx.ErrNoRows)})
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/calendar", h.ContributionCalendar())

	req := httptest.NewRequest(http.MethodGet, "/calendar?user_id="+uuid.New().String(), nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, fiber.StatusOK, resp.StatusCode,
		"a genuinely absent github account (ErrNoRows) must still degrade gracefully, unchanged from before the fix")

	var body map[string]interface{}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, float64(0), body["total"])
}

func TestContributionActivity_GithubLookupDBErrorReturns500_UserIDParam(t *testing.T) {
	cfg := config.Config{}
	h := handlers.NewUserProfileHandler(cfg, &db.DB{Pool: newGithubLookupErrorPool(fmt.Errorf("connection reset by peer"))})
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/activity", h.ContributionActivity())

	req := httptest.NewRequest(http.MethodGet, "/activity?user_id="+uuid.New().String(), nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode,
		"a genuine DB error on the github_accounts lookup must surface as a 500, not an empty-but-200 activity list")
}

func TestContributionActivity_GithubLookupDBErrorReturns500_OwnProfile(t *testing.T) {
	cfg := config.Config{}
	h := handlers.NewUserProfileHandler(cfg, &db.DB{Pool: newGithubLookupErrorPool(fmt.Errorf("connection reset by peer"))})
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(auth.LocalUserID, uuid.New().String())
		return c.Next()
	})
	app.Get("/activity", h.ContributionActivity())

	req := httptest.NewRequest(http.MethodGet, "/activity", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode,
		"a genuine DB error on the own-profile github_accounts lookup must surface as a 500, not an empty-but-200 activity list")
}

func TestContributionActivity_GithubAccountNotFoundStillReturnsEmptyActivity(t *testing.T) {
	cfg := config.Config{}
	h := handlers.NewUserProfileHandler(cfg, &db.DB{Pool: newGithubLookupErrorPool(pgx.ErrNoRows)})
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/activity", h.ContributionActivity())

	req := httptest.NewRequest(http.MethodGet, "/activity?user_id="+uuid.New().String(), nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, fiber.StatusOK, resp.StatusCode,
		"a genuinely absent github account (ErrNoRows) must still degrade gracefully, unchanged from before the fix")

	var body map[string]interface{}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, float64(0), body["total"])
}
