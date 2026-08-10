package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// assignmentSuiteApp wires the §4 assignment routes the same way
// internal/api/api.go does.
func assignmentSuiteApp(d *db.DB) *fiber.App {
	app := fiber.New()

	issueApps := handlers.NewHackathonIssueApplicationsHandler(config.Config{TokenEncKeyB64: asmtFxEncKey()}, d)
	app.Get("/projects/:id/grainhack/:number", auth.RequireAuth(hackathonSuiteJWTSecret), issueApps.GetForContributor())
	app.Post("/hackathon-issues/:id/apply", auth.RequireAuth(hackathonSuiteJWTSecret), issueApps.Apply())
	app.Get("/hackathon-issue-applications/me", auth.RequireAuth(hackathonSuiteJWTSecret), issueApps.Mine())
	app.Get("/hackathon-assignments/me", auth.RequireAuth(hackathonSuiteJWTSecret), issueApps.MyAssignments())
	app.Post("/hackathon-assignments/:id/release", auth.RequireAuth(hackathonSuiteJWTSecret), issueApps.Release())

	adminGroup := app.Group("/admin", auth.RequireAuth(hackathonSuiteJWTSecret))
	draws := handlers.NewAdminHackathonDrawsHandler(d)
	adminGroup.Post("/hackathon-issues/:id/simulate-draw", auth.RequireRole("admin"), draws.Simulate())
	adminGroup.Get("/hackathons/:id/draws", auth.RequireRole("admin"), draws.ListDraws())
	adminGroup.Get("/hackathons/:id/assignments", auth.RequireRole("admin"), draws.ListAssignments())
	return app
}

// asmtFxLiveIssue sets up a live hackathon with an accepted project and one
// published, open-for-applications issue.
func asmtFxLiveIssue(t *testing.T, d *db.DB) (hackathonID, projectID, issueID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	owner := adminSuiteInsertUser(t, d, "contributor")
	projectID = projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: owner, Status: "verified"})
	hackathonID = hackathonSuiteInsertHackathon(t, d, "live")

	if _, err := d.Pool.Exec(ctx, `
UPDATE hackathons SET announced_at = now() - interval '180 days',
  starts_at = now() - interval '1 day', ends_at = now() + interval '30 days'
WHERE id = $1`, hackathonID); err != nil {
		t.Fatalf("set hackathon dates: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `
INSERT INTO hackathon_project_applications
  (hackathon_id, project_id, applicant_user_id, short_description, goal, expected_issue_count, maintainer_contact, status)
VALUES ($1,$2,$3,'d','g',3,'c@example.com','accepted')`, hackathonID, projectID, owner); err != nil {
		t.Fatalf("accept project: %v", err)
	}

	var orgLogin string
	if err := d.Pool.QueryRow(ctx,
		`SELECT SPLIT_PART(github_full_name, '/', 1) FROM projects WHERE id = $1`, projectID).Scan(&orgLogin); err != nil {
		t.Fatalf("org login: %v", err)
	}
	if err := d.Pool.QueryRow(ctx, `
INSERT INTO hackathon_issues
  (hackathon_id, project_id, issue_number, org_login, status, acceptance_criteria, difficulty_tier,
   reserved, application_window_opens_at, application_window_closes_at, published_at)
VALUES ($1,$2,901,$3,'published','c','standard',false, now() - interval '1 hour', now() + interval '1 hour', now())
RETURNING id`, hackathonID, projectID, orgLogin).Scan(&issueID); err != nil {
		t.Fatalf("insert issue: %v", err)
	}
	return hackathonID, projectID, issueID
}

// asmtFxEncKey is the token-encryption key this suite's app is built with;
// the apply handler decrypts the applicant's own OAuth token to run the
// pre-event-activity gate, so the fixture has to store a real encrypted one.
func asmtFxEncKey() string { return issueAppsFxEncKey() }

func asmtFxContributor(t *testing.T, d *db.DB, login string) uuid.UUID {
	t.Helper()
	userID := adminSuiteInsertUser(t, d, "contributor")
	issueAppsFxLinkedAccount(t, d.Pool, userID, login, asmtFxEncKey())
	return userID
}

func TestHackathonApply_RequiresAuth(t *testing.T) {
	d := testDB(t)
	app := assignmentSuiteApp(d)
	_, _, issueID := asmtFxLiveIssue(t, d)

	resp, _ := notifSuiteDo(t, app, "POST", "/hackathon-issues/"+issueID.String()+"/apply", "", nil)
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestHackathonApply_SucceedsAndIsIdempotentlyRejectedWhileOpen(t *testing.T) {
	d := testDB(t)
	app := assignmentSuiteApp(d)
	_, _, issueID := asmtFxLiveIssue(t, d)
	userID := asmtFxContributor(t, d, "applicant-one")
	tok := hackathonSuiteToken(t, userID, "contributor")

	resp, body := notifSuiteDo(t, app, "POST", "/hackathon-issues/"+issueID.String()+"/apply", tok, []byte(`{"application_text":"I'd like to work on this."}`))
	if resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("first apply status = %d, want 201; body=%s", resp.StatusCode, body)
	}

	// A second application while the first is still open is a conflict -
	// otherwise one account could stack duplicate tickets in one pool.
	resp, _ = notifSuiteDo(t, app, "POST", "/hackathon-issues/"+issueID.String()+"/apply", tok, nil)
	if resp.StatusCode != fiber.StatusConflict {
		t.Errorf("duplicate apply status = %d, want 409", resp.StatusCode)
	}

	// The application is assessed even with the AI flag off, so the draw
	// has a fit to weight.
	var fit string
	if err := d.Pool.QueryRow(context.Background(), `
SELECT COALESCE(fit,'') FROM hackathon_issue_applications WHERE hackathon_issue_id = $1 AND user_id = $2
`, issueID, userID).Scan(&fit); err != nil {
		t.Fatalf("read fit: %v", err)
	}
	if fit != "plausible" {
		t.Errorf("fit = %q, want plausible (AI assessment is off by default)", fit)
	}
}

// A gate failure must return the specific reason (§4.1) and still leave a
// persisted record of why.
func TestHackathonApply_GateFailureReturnsSpecificReasonAndPersists(t *testing.T) {
	d := testDB(t)
	app := assignmentSuiteApp(d)
	hackathonID, _, issueID := asmtFxLiveIssue(t, d)
	if _, err := d.Pool.Exec(context.Background(), `
UPDATE hackathon_issues SET application_window_closes_at = now() - interval '1 minute' WHERE id = $1
`, issueID); err != nil {
		t.Fatalf("close window: %v", err)
	}
	userID := asmtFxContributor(t, d, "too-late")
	tok := hackathonSuiteToken(t, userID, "contributor")

	resp, respBody := notifSuiteDo(t, app, "POST", "/hackathon-issues/"+issueID.String()+"/apply", tok, nil)
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	var body struct {
		Error  string `json:"error"`
		Gate   string `json:"gate"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(respBody, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Gate != "application_window" {
		t.Errorf("gate = %q, want application_window", body.Gate)
	}
	if body.Reason == "" {
		t.Error("no reason returned - §4.1 requires the specific reason be shown to the applicant")
	}

	var status, reason string
	if err := d.Pool.QueryRow(context.Background(), `
SELECT status, COALESCE(gate_failure_reason,'') FROM hackathon_issue_applications
WHERE hackathon_id = $1 AND user_id = $2
`, hackathonID, userID).Scan(&status, &reason); err != nil {
		t.Fatalf("read persisted rejection: %v", err)
	}
	if status != "rejected_gate" || reason == "" {
		t.Errorf("persisted = (%s, %q), want (rejected_gate, non-empty)", status, reason)
	}
}

func TestHackathonSimulateDraw_RequiresAdmin(t *testing.T) {
	d := testDB(t)
	app := assignmentSuiteApp(d)
	_, _, issueID := asmtFxLiveIssue(t, d)
	contributor := asmtFxContributor(t, d, "not-an-admin")

	resp, _ := notifSuiteDo(t, app, "POST", "/admin/hackathon-issues/"+issueID.String()+"/simulate-draw",
		hackathonSuiteToken(t, contributor, "contributor"), []byte("{}"))
	if resp.StatusCode != fiber.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// The admin simulate action must return the full ticket breakdown and write
// no assignment - that's what makes it usable before a real event.
func TestHackathonSimulateDraw_ReturnsPoolAndWritesNoAssignment(t *testing.T) {
	d := testDB(t)
	app := assignmentSuiteApp(d)
	hackathonID, _, issueID := asmtFxLiveIssue(t, d)

	applicant := asmtFxContributor(t, d, "sim-candidate")
	if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO hackathon_issue_applications
  (hackathon_id, hackathon_issue_id, user_id, github_login, status, fit, difficulty_match, fit_assessed_at)
VALUES ($1,$2,$3,'sim-candidate','applied','plausible','matched',now())
`, hackathonID, issueID, applicant); err != nil {
		t.Fatalf("insert application: %v", err)
	}

	adminID := adminSuiteInsertUser(t, d, "admin")
	resp, respBody := notifSuiteDo(t, app, "POST", "/admin/hackathon-issues/"+issueID.String()+"/simulate-draw",
		hackathonSuiteToken(t, adminID, "admin"), []byte(`{"seed":4242}`))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Seed         int64 `json:"seed"`
		IsSimulation bool  `json:"is_simulation"`
		Pool         []struct {
			GitHubLogin string             `json:"github_login"`
			Tickets     float64            `json:"tickets"`
			Weights     map[string]float64 `json:"weights"`
		} `json:"pool"`
		WinnerUserID *string `json:"winner_user_id"`
	}
	if err := json.Unmarshal(respBody, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Seed != 4242 {
		t.Errorf("seed = %d, want the requested 4242 so the draw is replayable", body.Seed)
	}
	if !body.IsSimulation {
		t.Error("is_simulation = false on the simulate endpoint")
	}
	if len(body.Pool) != 1 || body.Pool[0].GitHubLogin != "sim-candidate" {
		t.Fatalf("pool = %+v, want the single applicant", body.Pool)
	}
	if body.Pool[0].Tickets <= 0 || len(body.Pool[0].Weights) == 0 {
		t.Error("pool entry is missing the ticket breakdown an admin needs to sanity-check weights")
	}
	if body.WinnerUserID == nil {
		t.Error("simulation returned no would-be winner")
	}

	var assignments int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM hackathon_assignments WHERE hackathon_issue_id = $1`, issueID).Scan(&assignments); err != nil {
		t.Fatalf("count assignments: %v", err)
	}
	if assignments != 0 {
		t.Errorf("simulation wrote %d assignment(s), want 0", assignments)
	}
}

func TestHackathonRelease_OnlyTheAssigneeCanRelease(t *testing.T) {
	d := testDB(t)
	app := assignmentSuiteApp(d)
	hackathonID, projectID, issueID := asmtFxLiveIssue(t, d)

	owner := asmtFxContributor(t, d, "the-assignee")
	var orgLogin string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT SPLIT_PART(github_full_name,'/',1) FROM projects WHERE id = $1`, projectID).Scan(&orgLogin); err != nil {
		t.Fatalf("org: %v", err)
	}
	var assignmentID uuid.UUID
	if err := d.Pool.QueryRow(context.Background(), `
INSERT INTO hackathon_assignments
  (hackathon_id, hackathon_issue_id, project_id, issue_number, user_id, github_login, org_login, status, holds_slot)
VALUES ($1,$2,$3,901,$4,'the-assignee',$5,'active',true) RETURNING id
`, hackathonID, issueID, projectID, owner, orgLogin).Scan(&assignmentID); err != nil {
		t.Fatalf("insert assignment: %v", err)
	}

	stranger := asmtFxContributor(t, d, "a-stranger")
	resp, _ := notifSuiteDo(t, app, "POST", "/hackathon-assignments/"+assignmentID.String()+"/release",
		hackathonSuiteToken(t, stranger, "contributor"), nil)
	if resp.StatusCode != fiber.StatusNotFound {
		t.Errorf("stranger release status = %d, want 404", resp.StatusCode)
	}

	resp, respBody := notifSuiteDo(t, app, "POST", "/hackathon-assignments/"+assignmentID.String()+"/release",
		hackathonSuiteToken(t, owner, "contributor"), nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("assignee release status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		AbandonRecorded bool `json:"abandon_recorded"`
	}
	if err := json.Unmarshal(respBody, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.AbandonRecorded {
		t.Error("release immediately after assignment recorded an abandon despite the grace window")
	}
}

// TestContributorIssue_ApplicantVisibility covers the bucketing policy: an
// exact live count across a long window rewards applying late, because the
// last applicant sees the whole field and picks the least contested issue.
func TestContributorIssue_ApplicantVisibility(t *testing.T) {
	d := testDB(t)
	app := assignmentSuiteApp(d)
	hackathonID, projectID, issueID := asmtFxLiveIssue(t, d)

	// Five applicants puts the pool in the "many" band.
	for i := 0; i < 5; i++ {
		u := asmtFxContributor(t, d, fmt.Sprintf("pool-%d-%s", i, uuid.NewString()[:6]))
		if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO hackathon_issue_applications
  (hackathon_id, hackathon_issue_id, user_id, github_login, status, fit, difficulty_match, fit_assessed_at)
VALUES ($1,$2,$3,$4,'applied','plausible','matched',now())`, hackathonID, issueID, u, "pool"); err != nil {
			t.Fatalf("insert application: %v", err)
		}
	}

	viewer := asmtFxContributor(t, d, "viewer-"+uuid.NewString()[:6])
	tok := hackathonSuiteToken(t, viewer, "contributor")
	path := "/projects/" + projectID.String() + "/grainhack/901"

	read := func() (visibility string, count *int, bucket string) {
		resp, body := notifSuiteDo(t, app, "GET", path, tok, nil)
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, body)
		}
		var out struct {
			ApplicantVisibility string `json:"applicant_visibility"`
			ApplicantCount      *int   `json:"applicant_count"`
			ApplicantBucket     string `json:"applicant_bucket"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out.ApplicantVisibility, out.ApplicantCount, out.ApplicantBucket
	}

	setVisibility := func(v string) {
		if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO hackathon_config_settings (hackathon_id, key, value) VALUES ($1,'applicant_count_visibility',$2)
ON CONFLICT (hackathon_id, key) WHERE hackathon_id IS NOT NULL
DO UPDATE SET value = EXCLUDED.value`, hackathonID, v); err != nil {
			t.Fatalf("set visibility: %v", err)
		}
	}

	t.Run("bucketed hides the exact number", func(t *testing.T) {
		setVisibility("bucketed")
		vis, count, bucket := read()
		if vis != "bucketed" {
			t.Errorf("visibility = %q, want bucketed", vis)
		}
		if count != nil {
			t.Errorf("exact count leaked as %d while bucketing", *count)
		}
		if bucket != "many" {
			t.Errorf("bucket = %q, want many for a 5-applicant pool", bucket)
		}
	})

	t.Run("hidden reveals neither", func(t *testing.T) {
		setVisibility("hidden")
		_, count, bucket := read()
		if count != nil || bucket != "" {
			t.Errorf("hidden still exposed count=%v bucket=%q", count, bucket)
		}
	})

	t.Run("exact reveals the number", func(t *testing.T) {
		setVisibility("exact")
		_, count, _ := read()
		if count == nil || *count != 5 {
			t.Errorf("count = %v, want 5", count)
		}
	})

	t.Run("a closed window reveals the exact number regardless", func(t *testing.T) {
		setVisibility("bucketed")
		if _, err := d.Pool.Exec(context.Background(),
			`UPDATE hackathon_issues SET application_window_closes_at = now() - interval '1 minute' WHERE id = $1`, issueID); err != nil {
			t.Fatalf("close window: %v", err)
		}
		// Once the pool is settled, precision can no longer steer anyone's
		// choice of where to apply.
		_, count, _ := read()
		if count == nil || *count != 5 {
			t.Errorf("after close, count = %v, want the exact 5", count)
		}
	})
}
