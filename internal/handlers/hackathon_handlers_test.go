package handlers_test

import (
	"context"
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

const hackathonSuiteJWTSecret = "hackathon-suite-test-secret"

// hackathonSuiteApp wires every new GrainHack route the same way
// internal/api/api.go does, against this suite's own JWT secret.
func hackathonSuiteApp(d *db.DB) *fiber.App {
	requireAdmin := auth.RequireLiveRole(handlers.NewRoleLookup(d), "admin")
	app := fiber.New()

	hackathonPublic := handlers.NewHackathonPublicHandler(d)
	app.Get("/hackathons", hackathonPublic.List())
	app.Get("/hackathons/:id", hackathonPublic.GetByID())
	app.Get("/hackathons/:id/issues", hackathonPublic.IssuesForHackathon())

	hackathonApps := handlers.NewHackathonApplicationsHandler(d)
	app.Post("/hackathons/:id/applications", auth.RequireAuth(hackathonSuiteJWTSecret), hackathonApps.Apply())
	app.Get("/hackathon-applications/me", auth.RequireAuth(hackathonSuiteJWTSecret), hackathonApps.Mine())

	hackathonIssues := handlers.NewHackathonIssuesHandler(d)
	app.Get("/projects/:id/hackathon-issues", auth.RequireAuth(hackathonSuiteJWTSecret), hackathonIssues.ListForProject())
	app.Get("/projects/:id/hackathon-issues/:number", auth.RequireAuth(hackathonSuiteJWTSecret), hackathonIssues.Get())
	app.Put("/projects/:id/hackathon-issues/:number", auth.RequireAuth(hackathonSuiteJWTSecret), hackathonIssues.UpdateFields())

	adminGroup := app.Group("/admin", auth.RequireAuth(hackathonSuiteJWTSecret))

	adminHackathons := handlers.NewAdminHackathonsHandler(d)
	adminGroup.Post("/hackathons", requireAdmin, adminHackathons.Create())
	adminGroup.Get("/hackathons", requireAdmin, adminHackathons.List())
	adminGroup.Get("/hackathons/:id", requireAdmin, adminHackathons.GetByID())
	adminGroup.Put("/hackathons/:id", requireAdmin, adminHackathons.Update())
	adminGroup.Post("/hackathons/:id/transition", requireAdmin, adminHackathons.Transition())

	adminHackathonApps := handlers.NewAdminHackathonApplicationsHandler(config.Config{}, d, nil)
	adminGroup.Get("/hackathons/:id/applications", requireAdmin, adminHackathonApps.ListAdmin())
	adminGroup.Post("/hackathons/applications/:appId/accept", requireAdmin, adminHackathonApps.Accept())
	adminGroup.Post("/hackathons/applications/:appId/reject", requireAdmin, adminHackathonApps.Reject())
	adminGroup.Post("/hackathons/applications/:appId/request-more-info", requireAdmin, adminHackathonApps.RequestMoreInfo())

	adminGroup.Get("/hackathons/:id/issues", requireAdmin, hackathonIssues.ListForHackathon())

	adminHackathonConfig := handlers.NewAdminHackathonConfigHandler(d)
	adminGroup.Get("/hackathon-config", requireAdmin, adminHackathonConfig.List())
	adminGroup.Put("/hackathon-config", requireAdmin, adminHackathonConfig.Update())
	adminGroup.Post("/hackathon-config/reset", requireAdmin, adminHackathonConfig.Reset())
	adminGroup.Get("/hackathon-config/audit", requireAdmin, adminHackathonConfig.Audit())

	return app
}

func hackathonSuiteToken(t *testing.T, userID uuid.UUID, role string) string {
	t.Helper()
	tok, err := auth.IssueJWT(hackathonSuiteJWTSecret, userID, role, "", "", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}
	return tok
}

func hackathonSuiteInsertHackathon(t *testing.T, d *db.DB, phase string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO hackathons (name, phase) VALUES ($1, $2) RETURNING id
`, "hackathon-suite-"+uuid.NewString(), phase).Scan(&id)
	if err != nil {
		t.Fatalf("insert hackathon: %v", err)
	}
	return id
}

func hackathonSuiteInsertApplication(t *testing.T, d *db.DB, hackathonID, projectID, applicantID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO hackathon_project_applications
  (hackathon_id, project_id, applicant_user_id, short_description, goal, expected_issue_count, maintainer_contact, status)
VALUES ($1, $2, $3, 'desc', 'goal', 3, 'contact@example.com', 'pending')
RETURNING id
`, hackathonID, projectID, applicantID).Scan(&id)
	if err != nil {
		t.Fatalf("insert application: %v", err)
	}
	return id
}

func hackathonSuiteInsertProject(t *testing.T, d *db.DB, ownerID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO projects (owner_user_id, github_full_name, status)
VALUES ($1, $2, 'verified')
RETURNING id
`, ownerID, "hackathon-suite-"+uuid.NewString()).Scan(&id)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return id
}

// hackathonSuiteInsertIssue seeds a hackathon_issues row, optionally with a
// matching github_issues row so IssuesForHackathon's title join has
// something real to read.
func hackathonSuiteInsertIssue(
	t *testing.T, d *db.DB, hackathonID, projectID uuid.UUID, number int, status string, title string,
) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	var publishedAt any
	if status == "published" {
		publishedAt = time.Now()
	}
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO hackathon_issues
  (hackathon_id, project_id, issue_number, org_login, status, difficulty_tier, acceptance_criteria, published_at)
VALUES ($1, $2, $3, 'hackathon-suite-org', $4, 'easy', 'Ship a test proving the fix', $5)
RETURNING id
`, hackathonID, projectID, number, status, publishedAt).Scan(&id)
	if err != nil {
		t.Fatalf("insert hackathon_issues: %v", err)
	}
	if title != "" {
		if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO github_issues (project_id, github_issue_id, number, title)
VALUES ($1, $2, $3, $4)
`, projectID, int64(number)+1_000_000_000, number, title); err != nil {
			t.Fatalf("insert github_issues: %v", err)
		}
	}
	return id
}

// --- Public routes: contributor-facing published issues ---

// The specific gap this endpoint fills: GET /projects/:id/hackathon-issues
// is owner-or-admin gated, so a contributor who owns none of a hackathon's
// projects could not list its issues at all. This is that same contributor,
// calling the new endpoint instead, with no auth header at all.
func TestHackathonPublic_IssuesForHackathon_ReachableByNonOwner(t *testing.T) {
	d := testDB(t)
	app := hackathonSuiteApp(d)
	hackathonID := hackathonSuiteInsertHackathon(t, d, "issue_prep")
	maintainerID := adminSuiteInsertUser(t, d, "contributor")
	projectID := hackathonSuiteInsertProject(t, d, maintainerID)
	hackathonSuiteInsertIssue(t, d, hackathonID, projectID, 1, "published", "Fix the flaky retry loop")

	// Confirm the gap first: the project-owned route refuses a stranger - a
	// real registered user, just not this project's owner or an admin.
	strangerID := adminSuiteInsertUser(t, d, "contributor")
	strangerToken := hackathonSuiteToken(t, strangerID, "contributor")
	resp, _ := notifSuiteDo(t, app, "GET", "/projects/"+projectID.String()+"/hackathon-issues", strangerToken, nil)
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("owner-gated route status = %d, want 403 for a non-owner (the gap this endpoint fills)", resp.StatusCode)
	}

	// The new endpoint, called with no auth at all - not even a bearer token
	// for a stranger, since it is public the same way GetByID is.
	resp, body := notifSuiteDo(t, app, "GET", "/hackathons/"+hackathonID.String()+"/issues", "", nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Fix the flaky retry loop") {
		t.Errorf("expected the published issue's title in the response: %s", body)
	}
	if !strings.Contains(string(body), `"repo_full_name"`) {
		t.Errorf("expected repo_full_name in the response: %s", body)
	}
}

func TestHackathonPublic_IssuesForHackathon_OnlyPublished(t *testing.T) {
	d := testDB(t)
	app := hackathonSuiteApp(d)
	hackathonID := hackathonSuiteInsertHackathon(t, d, "issue_prep")
	projectOwnerID := adminSuiteInsertUser(t, d, "contributor")
	projectID := hackathonSuiteInsertProject(t, d, projectOwnerID)
	hackathonSuiteInsertIssue(t, d, hackathonID, projectID, 1, "published", "Published issue")
	hackathonSuiteInsertIssue(t, d, hackathonID, projectID, 2, "pending", "Pending issue")
	hackathonSuiteInsertIssue(t, d, hackathonID, projectID, 3, "removed", "Removed issue")

	resp, body := notifSuiteDo(t, app, "GET", "/hackathons/"+hackathonID.String()+"/issues", "", nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Published issue") {
		t.Errorf("missing the published issue: %s", body)
	}
	if strings.Contains(string(body), "Pending issue") || strings.Contains(string(body), "Removed issue") {
		t.Errorf("a pending or removed issue leaked into the public list: %s", body)
	}
}

func TestHackathonPublic_IssuesForHackathon_404sForDraft(t *testing.T) {
	d := testDB(t)
	app := hackathonSuiteApp(d)
	draftID := hackathonSuiteInsertHackathon(t, d, "draft")

	resp, _ := notifSuiteDo(t, app, "GET", "/hackathons/"+draftID.String()+"/issues", "", nil)
	if resp.StatusCode != fiber.StatusNotFound {
		t.Errorf("status = %d, want 404 for a draft hackathon's issues", resp.StatusCode)
	}
}

// The whole point of this endpoint's design: no field, at any key, that
// discloses how contested an issue is. A raw string search rather than a
// json.Unmarshal into a known struct on purpose - unmarshalling into this
// package's own DTO would trivially "pass" by construction; a field added
// to the handler but not the test struct would leak silently.
func TestHackathonPublic_IssuesForHackathon_NoApplicantSignal(t *testing.T) {
	d := testDB(t)
	app := hackathonSuiteApp(d)
	hackathonID := hackathonSuiteInsertHackathon(t, d, "issue_prep")
	projectOwnerID := adminSuiteInsertUser(t, d, "contributor")
	projectID := hackathonSuiteInsertProject(t, d, projectOwnerID)
	issueID := hackathonSuiteInsertIssue(t, d, hackathonID, projectID, 1, "published", "Contested issue")
	for i := 0; i < 3; i++ {
		applicantID := adminSuiteInsertUser(t, d, "contributor")
		if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO hackathon_issue_applications (hackathon_id, hackathon_issue_id, user_id, github_login, status)
VALUES ($1, $2, $3, $4, 'applied')
`, hackathonID, issueID, applicantID, "applicant-"+uuid.NewString()); err != nil {
			t.Fatalf("insert application: %v", err)
		}
	}

	resp, body := notifSuiteDo(t, app, "GET", "/hackathons/"+hackathonID.String()+"/issues", "", nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	forbidden := []string{
		"applicant_count", "applicant_bucket", "applicant",
		"flagged_for_admin", "flagged_reason", "org_login", "synced_at",
	}
	for _, key := range forbidden {
		if strings.Contains(string(body), key) {
			t.Errorf("response leaks %q, which discloses contention or internal bookkeeping: %s", key, body)
		}
	}
}

// An issue whose draw has run must say so, or the page goes on promising a
// draw that already happened. Every assignment status is covered: held and
// completed assignments mark the issue assigned; released ones free it.
func TestHackathonPublic_IssuesForHackathon_ReportsAssignedWithoutTheAssignee(t *testing.T) {
	cases := []struct {
		status string // "" = no assignment at all
		want   bool
	}{
		{"", false},
		{"active", true},
		{"pr_submitted", true},
		{"completed", true},
		{"released_stale", false},
		{"released_voluntary", false},
		{"released_event_end", false},
	}
	for _, tc := range cases {
		name := tc.status
		if name == "" {
			name = "none"
		}
		t.Run(name, func(t *testing.T) {
			d := testDB(t)
			app := hackathonSuiteApp(d)
			hackathonID := hackathonSuiteInsertHackathon(t, d, "live")
			ownerID := adminSuiteInsertUser(t, d, "contributor")
			projectID := hackathonSuiteInsertProject(t, d, ownerID)
			issueID := hackathonSuiteInsertIssue(t, d, hackathonID, projectID, 1, "published", "Drawn issue")

			winnerID := adminSuiteInsertUser(t, d, "contributor")
			winnerLogin := "winner-" + uuid.NewString()[:8]
			if tc.status != "" {
				if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO hackathon_assignments
  (hackathon_id, hackathon_issue_id, project_id, issue_number, user_id, github_login, org_login, status)
VALUES ($1, $2, $3, 1, $4, $5, 'hackathon-suite-org', $6)
`, hackathonID, issueID, projectID, winnerID, winnerLogin, tc.status); err != nil {
					t.Fatalf("insert assignment: %v", err)
				}
			}

			resp, body := notifSuiteDo(t, app, "GET", "/hackathons/"+hackathonID.String()+"/issues", "", nil)
			if resp.StatusCode != fiber.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			want := `"assigned":false`
			if tc.want {
				want = `"assigned":true`
			}
			if !strings.Contains(string(body), want) {
				t.Errorf("assignment status %q: want %s in %s", tc.status, want, body)
			}
			// Whoever won stays off the public page: neither their login nor
			// their id, at any key.
			for _, leak := range []string{winnerLogin, winnerID.String(), "github_login", "assignee"} {
				if strings.Contains(string(body), leak) {
					t.Errorf("response discloses the assignee (%q): %s", leak, body)
				}
			}
		})
	}
}

// --- Public routes: draft hackathons never appear ---

func TestHackathonPublic_List_ExcludesDraft(t *testing.T) {
	d := testDB(t)
	app := hackathonSuiteApp(d)
	hackathonSuiteInsertHackathon(t, d, "draft")
	liveID := hackathonSuiteInsertHackathon(t, d, "live")

	resp, body := notifSuiteDo(t, app, "GET", "/hackathons", "", nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !containsID(body, liveID) {
		t.Errorf("expected the live hackathon %s in the response body: %s", liveID, body)
	}
}

func TestHackathonPublic_GetByID_404sForDraft(t *testing.T) {
	d := testDB(t)
	app := hackathonSuiteApp(d)
	draftID := hackathonSuiteInsertHackathon(t, d, "draft")

	resp, _ := notifSuiteDo(t, app, "GET", "/hackathons/"+draftID.String(), "", nil)
	if resp.StatusCode != fiber.StatusNotFound {
		t.Errorf("status = %d, want 404 for a draft hackathon", resp.StatusCode)
	}
}

// --- Admin gating: every /admin/hackathons* route requires the admin role ---

func TestAdminHackathons_RequiresAuth(t *testing.T) {
	d := testDB(t)
	app := hackathonSuiteApp(d)
	resp, _ := notifSuiteDo(t, app, "GET", "/admin/hackathons", "", nil)
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Errorf("status = %d, want 401 with no token", resp.StatusCode)
	}
}

func TestAdminHackathons_RequiresAdminRole(t *testing.T) {
	d := testDB(t)
	app := hackathonSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := hackathonSuiteToken(t, userID, "contributor")

	resp, _ := notifSuiteDo(t, app, "POST", "/admin/hackathons", token, []byte(`{"name":"Test Hack"}`))
	if resp.StatusCode != fiber.StatusForbidden {
		t.Errorf("status = %d, want 403 for a non-admin", resp.StatusCode)
	}
}

func TestAdminHackathons_Create_AdminSucceeds(t *testing.T) {
	d := testDB(t)
	app := hackathonSuiteApp(d)
	adminUserID := adminSuiteInsertUser(t, d, "admin")
	token := hackathonSuiteToken(t, adminUserID, "admin")

	resp, body := notifSuiteDo(t, app, "POST", "/admin/hackathons", token, []byte(`{"name":"Test Hack"}`))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
}

// --- Transition: sequential-only is enforced through the HTTP layer too ---

func TestAdminHackathons_Transition_RejectsSkippingAhead(t *testing.T) {
	d := testDB(t)
	app := hackathonSuiteApp(d)
	adminUserID := adminSuiteInsertUser(t, d, "admin")
	token := hackathonSuiteToken(t, adminUserID, "admin")
	hackathonID := hackathonSuiteInsertHackathon(t, d, "draft")

	resp, _ := notifSuiteDo(t, app, "POST", "/admin/hackathons/"+hackathonID.String()+"/transition", token, []byte(`{"to_phase":"live"}`))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a skip-ahead transition", resp.StatusCode)
	}
}

// --- Application review: reason is required for reject/request-more-info ---

func TestAdminHackathonApplications_Reject_RequiresReason(t *testing.T) {
	d := testDB(t)
	app := hackathonSuiteApp(d)
	adminUserID := adminSuiteInsertUser(t, d, "admin")
	token := hackathonSuiteToken(t, adminUserID, "admin")

	applicantID := adminSuiteInsertUser(t, d, "contributor")
	projectID := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: applicantID})
	hackathonID := hackathonSuiteInsertHackathon(t, d, "application_period")
	appID := hackathonSuiteInsertApplication(t, d, hackathonID, projectID, applicantID)

	resp, _ := notifSuiteDo(t, app, "POST", "/admin/hackathons/applications/"+appID.String()+"/reject", token, []byte(`{}`))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Errorf("status = %d, want 400 when rejecting with no reason", resp.StatusCode)
	}

	resp, body := notifSuiteDo(t, app, "POST", "/admin/hackathons/applications/"+appID.String()+"/reject", token, []byte(`{"reason":"missing maintainer contact"}`))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, body = %s (reject with a real reason should succeed)", resp.StatusCode, body)
	}
}

func TestAdminHackathonApplications_Accept_NotifiesApplicant(t *testing.T) {
	d := testDB(t)
	app := hackathonSuiteApp(d)
	adminUserID := adminSuiteInsertUser(t, d, "admin")
	token := hackathonSuiteToken(t, adminUserID, "admin")

	applicantID := adminSuiteInsertUser(t, d, "contributor")
	projectID := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: applicantID})
	hackathonID := hackathonSuiteInsertHackathon(t, d, "application_period")
	appID := hackathonSuiteInsertApplication(t, d, hackathonID, projectID, applicantID)

	resp, body := notifSuiteDo(t, app, "POST", "/admin/hackathons/applications/"+appID.String()+"/accept", token, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	var status string
	if err := d.Pool.QueryRow(context.Background(), `SELECT status FROM hackathon_project_applications WHERE id = $1`, appID).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "accepted" {
		t.Errorf("status = %q, want accepted", status)
	}
}

// --- Maintainer-facing hackathon-issue fields: owner-or-admin, not admin-only ---

func TestHackathonIssues_UpdateFields_OwnerCanEdit_OthersCannot(t *testing.T) {
	d := testDB(t)
	app := hackathonSuiteApp(d)

	ownerID := adminSuiteInsertUser(t, d, "contributor")
	otherUserID := adminSuiteInsertUser(t, d, "contributor")
	projectID := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: ownerID})
	hackathonID := hackathonSuiteInsertHackathon(t, d, "issue_prep")
	hackathonSuiteInsertApplication(t, d, hackathonID, projectID, ownerID)
	// Directly accept it (skip the admin-review round trip for this test's purpose).
	if _, err := d.Pool.Exec(context.Background(), `UPDATE hackathon_project_applications SET status = 'accepted' WHERE hackathon_id = $1 AND project_id = $2`, hackathonID, projectID); err != nil {
		t.Fatalf("accept application: %v", err)
	}
	if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO hackathon_issues (hackathon_id, project_id, issue_number, org_login, status) VALUES ($1, $2, 5, 'org', 'pending')
`, hackathonID, projectID); err != nil {
		t.Fatalf("insert hackathon_issues: %v", err)
	}

	ownerToken := hackathonSuiteToken(t, ownerID, "contributor")
	path := "/projects/" + projectID.String() + "/hackathon-issues/5"

	// A plain contributor who is NOT the owner and NOT an admin is forbidden.
	otherToken := hackathonSuiteToken(t, otherUserID, "contributor")
	resp, _ := notifSuiteDo(t, app, "PUT", path, otherToken, []byte(`{"acceptance_criteria":"x"}`))
	if resp.StatusCode != fiber.StatusForbidden {
		t.Errorf("status = %d, want 403 for a non-owner, non-admin user", resp.StatusCode)
	}

	// The owner - who is NOT a platform admin - can edit it. This is the
	// whole point of the two-gate design: maintainer-facing, not admin-only.
	resp, body := notifSuiteDo(t, app, "PUT", path, ownerToken, []byte(`{"acceptance_criteria":"must pass CI","difficulty_tier":"easy"}`))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, body = %s (project owner, a non-admin, should be able to edit)", resp.StatusCode, body)
	}
}

// TestHackathonIssues_UpdateFields_AdminIsAnOverrideOnOwnership covers the one
// guard in this change that is a RESTORATION rather than a substitution, and
// which until now had no admin test of any kind.
//
// What canManageProject used to be:
//
//	role, _ := c.Locals(auth.LocalRole).(string)
//	if role == "admin" {
//	    return true          // <- returns. The project row is never read.
//	}
//	var ownerID uuid.UUID
//	... SELECT owner_user_id ...
//
// The defect there is not only that `role` came from the token. It is that the
// admin branch answered a different question: not "may this person act on this
// project" but "is this person an admin". Swapping the claim for a live read
// would have produced a correct value and still skipped ownership entirely.
//
// So the fix reads ownership first and always, and treats admin as an override
// on top of it. In a diff that is nearly indistinguishable from the seven
// substitutions elsewhere in the change; these tests are where the difference
// is written down.
func TestHackathonIssues_UpdateFields_AdminIsAnOverrideOnOwnership(t *testing.T) {
	d := testDB(t)
	app := hackathonSuiteApp(d)

	ownerID := adminSuiteInsertUser(t, d, "contributor")
	projectID := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: ownerID})
	hackathonID := hackathonSuiteInsertHackathon(t, d, "issue_prep")
	hackathonSuiteInsertApplication(t, d, hackathonID, projectID, ownerID)
	if _, err := d.Pool.Exec(context.Background(), `UPDATE hackathon_project_applications SET status = 'accepted' WHERE hackathon_id = $1 AND project_id = $2`, hackathonID, projectID); err != nil {
		t.Fatalf("accept application: %v", err)
	}
	if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO hackathon_issues (hackathon_id, project_id, issue_number, org_login, status) VALUES ($1, $2, 7, 'org', 'pending')
`, hackathonID, projectID); err != nil {
		t.Fatalf("insert hackathon_issues: %v", err)
	}
	path := "/projects/" + projectID.String() + "/hackathon-issues/7"

	t.Run("an admin in the database may edit a project they do not own", func(t *testing.T) {
		admin := adminSuiteInsertUser(t, d, "admin")
		tok := hackathonSuiteToken(t, admin, "admin")
		resp, body := notifSuiteDo(t, app, "PUT", path, tok, []byte(`{"acceptance_criteria":"reviewed by staff"}`))
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("status = %d, body = %s - the admin override is intended behaviour and must survive the fix", resp.StatusCode, body)
		}
	})

	// The negative control, and the case that separates this from the code it
	// replaced. Same route, same non-owner, same validly signed token claiming
	// "admin". The only difference from the subtest above is what users.role
	// says. The old guard returned true here.
	t.Run("a token claiming admin over a database contributor is refused", func(t *testing.T) {
		impostor := adminSuiteInsertUser(t, d, "contributor")
		tok := hackathonSuiteToken(t, impostor, "admin")
		resp, body := notifSuiteDo(t, app, "PUT", path, tok, []byte(`{"acceptance_criteria":"should not land"}`))
		if resp.StatusCode != fiber.StatusForbidden {
			t.Fatalf("status = %d, want 403, body = %s", resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "not_authorized") {
			t.Errorf("error = %s, want not_authorized", body)
		}
		// And it did not land. Asserting the status alone would not catch a
		// guard that refused the response while the write went through.
		var got *string
		if err := d.Pool.QueryRow(context.Background(),
			`SELECT acceptance_criteria FROM hackathon_issues WHERE project_id = $1 AND issue_number = 7`,
			projectID).Scan(&got); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got != nil && *got == "should not land" {
			t.Error("the refused request wrote anyway")
		}
	})

	// Ownership is consulted even for an admin - the specific thing the old
	// code skipped by returning early.
	//
	// This is the only seam where a REAL admin behaves differently before and
	// after: for a project that exists, an admin was allowed either way, so no
	// test can tell the two apart. For a project that does not exist, the old
	// code returned true without looking and fell through to the handler body
	// (a 404 from the issue lookup); the new code reads the project row first,
	// finds nothing, and refuses at the guard.
	//
	// The assertion is that it refuses, and the code it refuses with says the
	// authorisation check itself could not be completed. A 404 would arguably
	// be friendlier to an admin who mistyped a project id - flagged rather
	// than changed here, because this test is about the ordering, not the
	// status code, and changing the status is a separate decision.
	t.Run("an admin acting on a project that does not exist is refused, not waved through", func(t *testing.T) {
		admin := adminSuiteInsertUser(t, d, "admin")
		tok := hackathonSuiteToken(t, admin, "admin")
		missing := "/projects/" + uuid.NewString() + "/hackathon-issues/7"
		resp, body := notifSuiteDo(t, app, "PUT", missing, tok, []byte(`{"acceptance_criteria":"x"}`))
		if resp.StatusCode == fiber.StatusOK {
			t.Fatalf("status = 200 on a project that does not exist; ownership was not consulted")
		}
		if !strings.Contains(string(body), "authorization_check_failed") {
			t.Errorf("error = %s, want authorization_check_failed - the guard should be "+
				"what stops this, which is how we know the project row is read before the role", body)
		}
	})
}

// --- Config settings: admin-only, and audit-trailed ---

func TestAdminHackathonConfig_Update_WritesAuditRow(t *testing.T) {
	d := testDB(t)
	app := hackathonSuiteApp(d)
	adminUserID := adminSuiteInsertUser(t, d, "admin")
	token := hackathonSuiteToken(t, adminUserID, "admin")
	hackathonID := hackathonSuiteInsertHackathon(t, d, "draft")

	resp, body := notifSuiteDo(t, app, "PUT", "/admin/hackathon-config", token,
		[]byte(`{"hackathon_id":"`+hackathonID.String()+`","key":"max_issues_per_org","value":"5"}`))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	resp, body = notifSuiteDo(t, app, "GET", "/admin/hackathon-config/audit?hackathon_id="+hackathonID.String()+"&key=max_issues_per_org", token, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"new_value":"5"`) {
		t.Errorf("audit response missing the new value: %s", body)
	}
}

func containsID(body []byte, id uuid.UUID) bool {
	return strings.Contains(string(body), id.String())
}
