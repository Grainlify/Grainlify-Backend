package handlers_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

func appealSuiteApp(d *db.DB) *fiber.App {
	requireAdmin := auth.RequireLiveRole(handlers.NewRoleLookup(d), "admin")
	app := fiber.New()
	h := handlers.NewHackathonAppealsHandler(d)
	app.Get("/grainhack/my-verdicts", auth.RequireAuth(hackathonSuiteJWTSecret), h.MyVerdicts())
	app.Post("/grainhack/verdicts/:id/appeal", auth.RequireAuth(hackathonSuiteJWTSecret), h.Appeal())
	adminGroup := app.Group("/admin", auth.RequireAuth(hackathonSuiteJWTSecret))
	adminGroup.Get("/hackathons/:id/appeals", requireAdmin, h.AdminList())
	adminGroup.Post("/hackathon-appeals/:id/decide", requireAdmin, h.AdminDecide())
	return app
}

func appealFxOwnedVerdict(t *testing.T, d *db.DB, hackathonID, projectID, userID uuid.UUID, pr int) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO hackathon_verdicts
  (hackathon_id, project_id, pr_number, user_id, github_login, prefilter_status, final_bucket, final_source)
VALUES ($1,$2,$3,$4,'octocat','passed','accepted','auto_confirmed')
RETURNING id`, hackathonID, projectID, pr, userID).Scan(&id)
	if err != nil {
		t.Fatalf("appealFxOwnedVerdict: %v", err)
	}
	return id
}

func appealFxPublish(t *testing.T, d *db.DB, hackathonID uuid.UUID) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(), `
UPDATE hackathons SET phase = 'results_published', results_published_at = now() WHERE id = $1
`, hackathonID); err != nil {
		t.Fatalf("appealFxPublish: %v", err)
	}
}

// A verdict must not be visible to its own author until results are
// published. Judging runs during Phase 4, and a bucket read off a
// half-finished queue is one the contributor would reasonably act on.
func TestAppeals_MyVerdictsHidesEverythingBeforeResultsArePublished(t *testing.T) {
	d := testDB(t)
	app := appealSuiteApp(d)
	hackathonID, projectID, _ := asmtFxLiveIssue(t, d)
	contributor := adminSuiteInsertUser(t, d, "contributor")
	appealFxOwnedVerdict(t, d, hackathonID, projectID, contributor, 7100)

	token := hackathonSuiteToken(t, contributor, "contributor")

	resp, body := notifSuiteDo(t, app, "GET", "/grainhack/my-verdicts", token, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", resp.StatusCode, body)
	}
	var before struct {
		Verdicts []map[string]any `json:"verdicts"`
	}
	if err := json.Unmarshal(body, &before); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(before.Verdicts) != 0 {
		t.Fatalf("got %d verdicts before results were published, want 0", len(before.Verdicts))
	}

	appealFxPublish(t, d, hackathonID)

	resp, body = notifSuiteDo(t, app, "GET", "/grainhack/my-verdicts", token, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", resp.StatusCode, body)
	}
	var after struct {
		Verdicts []map[string]any `json:"verdicts"`
	}
	if err := json.Unmarshal(body, &after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(after.Verdicts) != 1 {
		t.Fatalf("got %d verdicts after publication, want 1", len(after.Verdicts))
	}
}

// Someone else's verdict is not appealable, and an appeal with no stated
// grounds is refused before it reaches a reviewer.
func TestAppeals_SubmitRejectsBlankReasonAndOtherPeoplesVerdicts(t *testing.T) {
	d := testDB(t)
	app := appealSuiteApp(d)
	hackathonID, projectID, _ := asmtFxLiveIssue(t, d)
	owner := adminSuiteInsertUser(t, d, "contributor")
	stranger := adminSuiteInsertUser(t, d, "contributor")
	verdictID := appealFxOwnedVerdict(t, d, hackathonID, projectID, owner, 7101)
	appealFxPublish(t, d, hackathonID)

	path := "/grainhack/verdicts/" + verdictID.String() + "/appeal"

	resp, body := notifSuiteDo(t, app, "POST", path,
		hackathonSuiteToken(t, owner, "contributor"), []byte(`{"reason":"   "}`))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Errorf("blank reason: status = %d, want 400, body=%s", resp.StatusCode, body)
	}

	resp, body = notifSuiteDo(t, app, "POST", path,
		hackathonSuiteToken(t, stranger, "contributor"), []byte(`{"reason":"this is mine"}`))
	if resp.StatusCode != fiber.StatusForbidden {
		t.Errorf("another user's verdict: status = %d, want 403, body=%s", resp.StatusCode, body)
	}

	resp, body = notifSuiteDo(t, app, "POST", path,
		hackathonSuiteToken(t, owner, "contributor"), []byte(`{"reason":"the added tests were not counted"}`))
	if resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("valid appeal: status = %d, want 201, body=%s", resp.StatusCode, body)
	}

	// One appeal per verdict.
	resp, _ = notifSuiteDo(t, app, "POST", path,
		hackathonSuiteToken(t, owner, "contributor"), []byte(`{"reason":"again"}`))
	if resp.StatusCode != fiber.StatusConflict {
		t.Errorf("duplicate appeal: status = %d, want 409", resp.StatusCode)
	}
}

func TestAppeals_AdminDecideRequiresReasonAndIsAdminOnly(t *testing.T) {
	d := testDB(t)
	app := appealSuiteApp(d)
	hackathonID, projectID, _ := asmtFxLiveIssue(t, d)
	owner := adminSuiteInsertUser(t, d, "contributor")
	admin := adminSuiteInsertUser(t, d, "admin")
	verdictID := appealFxOwnedVerdict(t, d, hackathonID, projectID, owner, 7102)
	appealFxPublish(t, d, hackathonID)

	resp, body := notifSuiteDo(t, app, "POST", "/grainhack/verdicts/"+verdictID.String()+"/appeal",
		hackathonSuiteToken(t, owner, "contributor"), []byte(`{"reason":"please re-check"}`))
	if resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("appeal: status = %d, body=%s", resp.StatusCode, body)
	}
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	decidePath := "/admin/hackathon-appeals/" + created.ID.String() + "/decide"

	// A contributor cannot decide their own appeal.
	resp, _ = notifSuiteDo(t, app, "POST", decidePath,
		hackathonSuiteToken(t, owner, "contributor"), []byte(`{"upheld":true,"bucket":"exceptional","reason":"me"}`))
	if resp.StatusCode != fiber.StatusForbidden {
		t.Errorf("contributor deciding: status = %d, want 403", resp.StatusCode)
	}

	resp, body = notifSuiteDo(t, app, "POST", decidePath,
		hackathonSuiteToken(t, admin, "admin"), []byte(`{"upheld":true,"bucket":"substantial","reason":""}`))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Errorf("decision with no reason: status = %d, want 400, body=%s", resp.StatusCode, body)
	}

	resp, body = notifSuiteDo(t, app, "POST", decidePath,
		hackathonSuiteToken(t, admin, "admin"),
		[]byte(`{"upheld":true,"bucket":"substantial","reason":"reworked the retry path"}`))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("valid decision: status = %d, want 200, body=%s", resp.StatusCode, body)
	}

	var bucket, source string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT final_bucket, final_source FROM hackathon_verdicts WHERE id = $1`, verdictID).Scan(&bucket, &source); err != nil {
		t.Fatalf("read verdict: %v", err)
	}
	if bucket != "substantial" || source != "human_override" {
		t.Errorf("verdict = (%q, %q), want (substantial, human_override)", bucket, source)
	}
}
