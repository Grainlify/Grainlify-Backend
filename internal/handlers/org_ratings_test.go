package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

const orgRatingsSuiteJWTSecret = "org-ratings-suite-test-secret"

// orgRatingsSuiteToken issues a token signed with this suite's own JWT
// secret (orgRatingsSuiteApp's auth.RequireAuth middleware verifies against
// that secret specifically, not whatever another suite in this package
// happens to use).
func orgRatingsSuiteToken(t *testing.T, userID uuid.UUID, role string) string {
	t.Helper()
	tok, err := auth.IssueJWT(orgRatingsSuiteJWTSecret, userID, role, "", "", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}
	return tok
}

// orgRatingsSuiteApp wires a fiber app exposing exactly the routes
// internal/api/api.go registers against handlers.OrgRatingsHandler.
func orgRatingsSuiteApp(cfg config.Config, d *db.DB) *fiber.App {
	h := handlers.NewOrgRatingsHandler(cfg, d)
	app := fiber.New()
	app.Get("/orgs/:login", h.Summary())
	app.Get("/orgs/:login/activity", h.Activity())
	app.Get("/orgs/:login/calendar", h.Calendar())
	app.Get("/orgs/:login/ratings", h.List())
	app.Get("/orgs/:login/ratings/me", auth.RequireAuth(cfg.JWTSecret), h.MyStatus())
	app.Post("/orgs/:login/ratings", auth.RequireAuth(cfg.JWTSecret), h.Submit())
	return app
}

// orgRatingsFxMergedPR inserts a github_pull_requests row with merged as an
// explicit parameter - projectsFxInsertProject's sibling PR helpers in this
// package all hardcode merged=false, unusable for eligibility tests.
// projectsFxNextGHUserID is reused only as a convenient, already-unique
// int64 source for github_pr_id, not for any user-semantic meaning.
func orgRatingsFxMergedPR(t *testing.T, pool db.DBPool, projectID uuid.UUID, number int, authorLogin string, merged bool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO github_pull_requests (project_id, github_pr_id, number, state, title, author_login, url, merged)
VALUES ($1, $2, $3, 'closed', $4, $5, $6, $7)
`, projectID, projectsFxNextGHUserID(), number, "pr-"+uuid.New().String()[:8], authorLogin,
		fmt.Sprintf("https://github.com/test/test/pull/%d", number), merged)
	if err != nil {
		t.Fatalf("orgRatingsFxMergedPR: insert: %v", err)
	}
}

func TestOrgRatingsHandler_Summary_ReturnsAggregateStats(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: orgRatingsSuiteJWTSecret}
	app := orgRatingsSuiteApp(cfg, d)

	orgLogin := "summary-org-" + uuid.New().String()[:8]
	ownerID := projectsFxUser(t, d.Pool)
	stars := 42
	projectID := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
		OwnerUserID:    ownerID,
		GitHubFullName: orgLogin + "/repo-" + uuid.New().String()[:8],
		StarsCount:     &stars,
	})
	orgRatingsFxMergedPR(t, d.Pool, projectID, 1, "contrib-"+uuid.New().String()[:8], true)

	resp, body := notifSuiteDo(t, app, "GET", "/orgs/"+orgLogin, "", nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	var got struct {
		Login          string `json:"login"`
		RepoCount      int    `json:"repo_count"`
		StarsCount     int    `json:"stars_count"`
		MergedPRsCount int    `json:"merged_prs_count"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, body)
	}
	if got.RepoCount != 1 {
		t.Errorf("repo_count = %d, want 1", got.RepoCount)
	}
	if got.StarsCount != 42 {
		t.Errorf("stars_count = %d, want 42", got.StarsCount)
	}
	if got.MergedPRsCount != 1 {
		t.Errorf("merged_prs_count = %d, want 1", got.MergedPRsCount)
	}
}

func TestOrgRatingsHandler_Summary_404sForUnknownOrg(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: orgRatingsSuiteJWTSecret}
	app := orgRatingsSuiteApp(cfg, d)

	resp, body := notifSuiteDo(t, app, "GET", "/orgs/no-such-org-"+uuid.New().String(), "", nil)
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", resp.StatusCode, body)
	}
}

func TestOrgRatingsHandler_List_ReturnsReviewsWithReviewerIdentity(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: orgRatingsSuiteJWTSecret}
	app := orgRatingsSuiteApp(cfg, d)

	orgLogin := "list-org-" + uuid.New().String()[:8]
	reviewerID := projectsFxUser(t, d.Pool)
	reviewerLogin := "reviewer-" + uuid.New().String()[:8]
	issueAppsFxLinkedAccount(t, d.Pool, reviewerID, reviewerLogin, issueAppsFxEncKey())

	if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO org_ratings (user_id, org_login, rating, comment) VALUES ($1, $2, 4, 'solid project')
`, reviewerID, orgLogin); err != nil {
		t.Fatalf("seed org_ratings: %v", err)
	}

	resp, body := notifSuiteDo(t, app, "GET", "/orgs/"+orgLogin+"/ratings", "", nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	var got struct {
		Ratings []struct {
			Rating      int    `json:"rating"`
			Comment     string `json:"comment"`
			GithubLogin string `json:"github_login"`
		} `json:"ratings"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, body)
	}
	if got.Total < 1 || len(got.Ratings) < 1 {
		t.Fatalf("expected at least 1 rating, got total=%d len=%d", got.Total, len(got.Ratings))
	}
	found := false
	for _, r := range got.Ratings {
		if r.GithubLogin == reviewerLogin {
			found = true
			if r.Rating != 4 || r.Comment != "solid project" {
				t.Errorf("rating/comment = %d/%q, want 4/%q", r.Rating, r.Comment, "solid project")
			}
		}
	}
	if !found {
		t.Errorf("expected a review from %q in the list, got %+v", reviewerLogin, got.Ratings)
	}
}

func TestOrgRatingsHandler_MyStatus_EligibleFalseForUnlinkedAccount(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: orgRatingsSuiteJWTSecret}
	app := orgRatingsSuiteApp(cfg, d)

	userID := projectsFxUser(t, d.Pool)
	token := orgRatingsSuiteToken(t, userID, "contributor")

	resp, body := notifSuiteDo(t, app, "GET", "/orgs/some-org-"+uuid.New().String()[:8]+"/ratings/me", token, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200 (an unlinked GitHub account is not an error), body = %s", resp.StatusCode, body)
	}
	var got struct {
		Eligible bool `json:"eligible"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, body)
	}
	if got.Eligible {
		t.Error("eligible = true, want false for a user with no linked GitHub account")
	}
}

func TestOrgRatingsHandler_MyStatus_EligibleTrueAfterMergedPR(t *testing.T) {
	d := testDB(t)
	keyB64 := issueAppsFxEncKey()
	cfg := config.Config{JWTSecret: orgRatingsSuiteJWTSecret, TokenEncKeyB64: keyB64}
	app := orgRatingsSuiteApp(cfg, d)

	userID := projectsFxUser(t, d.Pool)
	login := "org-ratings-user-" + uuid.New().String()[:8]
	issueAppsFxLinkedAccount(t, d.Pool, userID, login, keyB64)

	orgLogin := "org-ratings-org-" + uuid.New().String()[:8]
	ownerID := projectsFxUser(t, d.Pool)
	projectID := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
		OwnerUserID:    ownerID,
		GitHubFullName: orgLogin + "/repo-" + uuid.New().String()[:8],
	})
	orgRatingsFxMergedPR(t, d.Pool, projectID, 1, login, true)

	token := orgRatingsSuiteToken(t, userID, "contributor")
	resp, body := notifSuiteDo(t, app, "GET", "/orgs/"+orgLogin+"/ratings/me", token, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	var got struct {
		Eligible bool `json:"eligible"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, body)
	}
	if !got.Eligible {
		t.Errorf("eligible = false, want true after a merged PR in this org")
	}
}

func TestOrgRatingsHandler_Submit_401sUnauthenticated(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: orgRatingsSuiteJWTSecret}
	app := orgRatingsSuiteApp(cfg, d)

	resp, body := notifSuiteDo(t, app, "POST", "/orgs/some-org/ratings", "", []byte(`{"rating":5}`))
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body = %s", resp.StatusCode, body)
	}
}

func TestOrgRatingsHandler_Submit_RejectsIneligible(t *testing.T) {
	d := testDB(t)
	keyB64 := issueAppsFxEncKey()
	cfg := config.Config{JWTSecret: orgRatingsSuiteJWTSecret, TokenEncKeyB64: keyB64}
	app := orgRatingsSuiteApp(cfg, d)

	userID := projectsFxUser(t, d.Pool)
	login := "no-pr-user-" + uuid.New().String()[:8]
	issueAppsFxLinkedAccount(t, d.Pool, userID, login, keyB64)

	orgLogin := "ineligible-org-" + uuid.New().String()[:8]
	ownerID := projectsFxUser(t, d.Pool)
	projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
		OwnerUserID:    ownerID,
		GitHubFullName: orgLogin + "/repo-" + uuid.New().String()[:8],
	})
	// No merged PR seeded for `login` under orgLogin.

	token := orgRatingsSuiteToken(t, userID, "contributor")
	resp, body := notifSuiteDo(t, app, "POST", "/orgs/"+orgLogin+"/ratings", token, []byte(`{"rating":5}`))
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", resp.StatusCode, body)
	}
}

func TestOrgRatingsHandler_Submit_RejectsSelfRating(t *testing.T) {
	d := testDB(t)
	keyB64 := issueAppsFxEncKey()
	cfg := config.Config{JWTSecret: orgRatingsSuiteJWTSecret, TokenEncKeyB64: keyB64}
	app := orgRatingsSuiteApp(cfg, d)

	ownerID := projectsFxUser(t, d.Pool)
	ownerLogin := "org-owner-" + uuid.New().String()[:8]
	issueAppsFxLinkedAccount(t, d.Pool, ownerID, ownerLogin, keyB64)

	orgLogin := "self-rate-org-" + uuid.New().String()[:8]
	projectID := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
		OwnerUserID:    ownerID,
		GitHubFullName: orgLogin + "/repo-" + uuid.New().String()[:8],
	})
	orgRatingsFxMergedPR(t, d.Pool, projectID, 1, ownerLogin, true)

	token := orgRatingsSuiteToken(t, ownerID, "maintainer")
	resp, body := notifSuiteDo(t, app, "POST", "/orgs/"+orgLogin+"/ratings", token, []byte(`{"rating":5,"comment":"great"}`))
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", resp.StatusCode, body)
	}
}

func TestOrgRatingsHandler_Submit_UpsertsNotDuplicate(t *testing.T) {
	d := testDB(t)
	keyB64 := issueAppsFxEncKey()
	cfg := config.Config{JWTSecret: orgRatingsSuiteJWTSecret, TokenEncKeyB64: keyB64}
	app := orgRatingsSuiteApp(cfg, d)

	userID := projectsFxUser(t, d.Pool)
	login := "upsert-user-" + uuid.New().String()[:8]
	issueAppsFxLinkedAccount(t, d.Pool, userID, login, keyB64)

	orgLogin := "upsert-org-" + uuid.New().String()[:8]
	ownerID := projectsFxUser(t, d.Pool)
	projectID := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
		OwnerUserID:    ownerID,
		GitHubFullName: orgLogin + "/repo-" + uuid.New().String()[:8],
	})
	orgRatingsFxMergedPR(t, d.Pool, projectID, 1, login, true)

	token := orgRatingsSuiteToken(t, userID, "contributor")

	resp1, body1 := notifSuiteDo(t, app, "POST", "/orgs/"+orgLogin+"/ratings", token, []byte(`{"rating":3,"comment":"ok"}`))
	if resp1.StatusCode != fiber.StatusOK {
		t.Fatalf("first submit status = %d, want 200, body = %s", resp1.StatusCode, body1)
	}
	resp2, body2 := notifSuiteDo(t, app, "POST", "/orgs/"+orgLogin+"/ratings", token, []byte(`{"rating":5,"comment":"actually great"}`))
	if resp2.StatusCode != fiber.StatusOK {
		t.Fatalf("second submit status = %d, want 200, body = %s", resp2.StatusCode, body2)
	}

	var count int
	if err := d.Pool.QueryRow(context.Background(), `
SELECT COUNT(*) FROM org_ratings WHERE user_id = $1 AND LOWER(org_login) = LOWER($2)
`, userID, orgLogin).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Fatalf("org_ratings row count = %d, want 1 (upsert, not duplicate)", count)
	}

	var rating int
	var comment string
	if err := d.Pool.QueryRow(context.Background(), `
SELECT rating, comment FROM org_ratings WHERE user_id = $1 AND LOWER(org_login) = LOWER($2)
`, userID, orgLogin).Scan(&rating, &comment); err != nil {
		t.Fatalf("row query: %v", err)
	}
	if rating != 5 || comment != "actually great" {
		t.Errorf("rating/comment = %d/%q, want 5/%q (should reflect the second submit)", rating, comment, "actually great")
	}
}

// orgRatingsFxMergedPRAt inserts a merged github_pull_requests row with an
// explicit merged_at_github timestamp - orgRatingsFxMergedPR above doesn't
// set merged_at_github at all, which Activity()'s query buckets by, so it's
// unusable for these tests.
func orgRatingsFxMergedPRAt(t *testing.T, pool db.DBPool, projectID uuid.UUID, mergedAt time.Time) {
	t.Helper()
	number := int(projectsFxNextGHUserID() % 100000)
	_, err := pool.Exec(context.Background(), `
INSERT INTO github_pull_requests (project_id, github_pr_id, number, state, title, author_login, url, merged, merged_at_github, created_at_github)
VALUES ($1, $2, $3, 'closed', $4, $5, $6, true, $7, $7)
`, projectID, projectsFxNextGHUserID(), number, "pr-"+uuid.New().String()[:8], "contrib-"+uuid.New().String()[:8],
		fmt.Sprintf("https://github.com/test/test/pull/%d", number), mergedAt)
	if err != nil {
		t.Fatalf("orgRatingsFxMergedPRAt: insert: %v", err)
	}
}

func TestOrgRatingsHandler_Activity_WeeklyBucketsWithZeroFill(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: orgRatingsSuiteJWTSecret}
	app := orgRatingsSuiteApp(cfg, d)

	orgLogin := "activity-org-" + uuid.New().String()[:8]
	ownerID := projectsFxUser(t, d.Pool)
	projectID := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
		OwnerUserID:    ownerID,
		GitHubFullName: orgLogin + "/repo-" + uuid.New().String()[:8],
	})

	now := time.Now().UTC()
	// One issue "now" and one from ~8 weeks back - both inside the 12-week
	// window, comfortably far apart to land in different weekly buckets
	// regardless of exactly where the current week boundary falls.
	userProfileSuiteIssue(t, d.Pool, userProfileSuiteContribSpec{ProjectID: projectID, Number: 1, CreatedAt: now})
	userProfileSuiteIssue(t, d.Pool, userProfileSuiteContribSpec{ProjectID: projectID, Number: 2, CreatedAt: now.AddDate(0, 0, -56)})
	orgRatingsFxMergedPRAt(t, d.Pool, projectID, now)

	resp, body := notifSuiteDo(t, app, "GET", "/orgs/"+orgLogin+"/activity", "", nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", resp.StatusCode, body)
	}

	var got struct {
		Weeks []struct {
			WeekStart    string `json:"week_start"`
			IssuesOpened int    `json:"issues_opened"`
			PRsMerged    int    `json:"prs_merged"`
		} `json:"weeks"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, body)
	}

	if len(got.Weeks) != 12 {
		t.Fatalf("len(weeks) = %d, want 12 (zero-filled range, not just matched rows)", len(got.Weeks))
	}
	for i := 1; i < len(got.Weeks); i++ {
		if got.Weeks[i].WeekStart <= got.Weeks[i-1].WeekStart {
			t.Fatalf("weeks not in strictly ascending order at index %d: %q then %q", i, got.Weeks[i-1].WeekStart, got.Weeks[i].WeekStart)
		}
	}

	var totalIssues, totalMergedPRs, zeroWeeks int
	for _, w := range got.Weeks {
		totalIssues += w.IssuesOpened
		totalMergedPRs += w.PRsMerged
		if w.IssuesOpened == 0 && w.PRsMerged == 0 {
			zeroWeeks++
		}
	}
	if totalIssues != 2 {
		t.Errorf("total issues_opened across all weeks = %d, want 2", totalIssues)
	}
	if totalMergedPRs != 1 {
		t.Errorf("total prs_merged across all weeks = %d, want 1", totalMergedPRs)
	}
	// Only 2 of the 12 weeks have any activity at all - most of the
	// remaining 10 must show real zeroes (proving generate_series zero-fill
	// actually ran), not just be absent from the result.
	if zeroWeeks < 8 {
		t.Errorf("weeks with zero activity = %d, want at least 8 (most of a 12-week window with only 2 contributions in it)", zeroWeeks)
	}
}

func TestOrgRatingsHandler_Activity_404sForUnknownOrg(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: orgRatingsSuiteJWTSecret}
	app := orgRatingsSuiteApp(cfg, d)

	resp, _ := notifSuiteDo(t, app, "GET", "/orgs/no-such-activity-org-"+uuid.New().String()+"/activity", "", nil)
	if resp.StatusCode != fiber.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestOrgRatingsHandler_Calendar_ZeroFilledWithCorrectTotal(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: orgRatingsSuiteJWTSecret}
	app := orgRatingsSuiteApp(cfg, d)

	orgLogin := "calendar-org-" + uuid.New().String()[:8]
	ownerID := projectsFxUser(t, d.Pool)
	projectID := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
		OwnerUserID:    ownerID,
		GitHubFullName: orgLogin + "/repo-" + uuid.New().String()[:8],
	})

	now := time.Now().UTC()
	userProfileSuiteIssue(t, d.Pool, userProfileSuiteContribSpec{ProjectID: projectID, Number: 1, CreatedAt: now})
	userProfileSuiteIssue(t, d.Pool, userProfileSuiteContribSpec{ProjectID: projectID, Number: 2, CreatedAt: now})
	userProfileSuitePR(t, d.Pool, userProfileSuiteContribSpec{ProjectID: projectID, Number: 1, CreatedAt: now.AddDate(0, 0, -100)})

	resp, body := notifSuiteDo(t, app, "GET", "/orgs/"+orgLogin+"/calendar", "", nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", resp.StatusCode, body)
	}

	var got struct {
		Calendar []struct {
			Date  string `json:"date"`
			Count int    `json:"count"`
			Level int    `json:"level"`
		} `json:"calendar"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, body)
	}

	if len(got.Calendar) != 365 {
		t.Fatalf("len(calendar) = %d, want 365", len(got.Calendar))
	}
	if got.Total != 3 {
		t.Errorf("total = %d, want 3", got.Total)
	}

	todayStr := now.Format("2006-01-02")
	var todayEntry, zeroEntries int
	for _, day := range got.Calendar {
		if day.Date == todayStr {
			todayEntry++
			if day.Count != 2 {
				t.Errorf("today's count = %d, want 2", day.Count)
			}
			if day.Level != 4 {
				t.Errorf("today's level = %d, want 4 (max day, 2 out of a maxCount of 2)", day.Level)
			}
		}
		if day.Count == 0 {
			zeroEntries++
		}
	}
	if todayEntry != 1 {
		t.Fatalf("expected exactly one calendar entry for today (%s), found %d", todayStr, todayEntry)
	}
	if zeroEntries < 300 {
		t.Errorf("zero-count days = %d, want most of a 365-day window with only 2 contributing days in it", zeroEntries)
	}
}

func TestOrgRatingsHandler_Calendar_404sForUnknownOrg(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: orgRatingsSuiteJWTSecret}
	app := orgRatingsSuiteApp(cfg, d)

	resp, _ := notifSuiteDo(t, app, "GET", "/orgs/no-such-calendar-org-"+uuid.New().String()+"/calendar", "", nil)
	if resp.StatusCode != fiber.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
