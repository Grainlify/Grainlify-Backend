package handlers_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

const socialFollowSuiteJWTSecret = "social-follow-suite-test-secret"

const pngDataURL = "data:image/png;base64,iVBORw0KGgo="

func socialFollowSuiteToken(t *testing.T, userID uuid.UUID, role string) string {
	t.Helper()
	tok, err := auth.IssueJWT(socialFollowSuiteJWTSecret, userID, role, "", "", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}
	return tok
}

func socialFollowSuiteApp(d *db.DB) *fiber.App {
	app := fiber.New()
	h := handlers.NewSocialFollowHandler(d, nil)
	app.Post("/social-follow/submit", auth.RequireAuth(socialFollowSuiteJWTSecret), h.SubmitAll())
	app.Get("/social-follow/me", auth.RequireAuth(socialFollowSuiteJWTSecret), h.Me())

	admin := app.Group("/admin", auth.RequireAuth(socialFollowSuiteJWTSecret))
	admin.Get("/social-follow/submissions", auth.RequireRole("admin"), h.ListSubmissions())
	admin.Post("/social-follow/submissions/:id/approve", auth.RequireRole("admin"), h.Approve())
	admin.Post("/social-follow/submissions/:id/reject", auth.RequireRole("admin"), h.Reject())
	admin.Post("/social-follow/submissions/:id/revoke", auth.RequireRole("admin"), h.Revoke())
	return app
}

// submitBoth posts a complete submission and returns its id.
func submitBoth(t *testing.T, app *fiber.App, token string) string {
	t.Helper()
	body := `{"linkedin_screenshot":"` + pngDataURL + `","x_screenshot":"` + pngDataURL + `"}`
	resp, respBody := notifSuiteDo(t, app, "POST", "/social-follow/submit", token, []byte(body))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("submit status = %d, body = %s", resp.StatusCode, respBody)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		t.Fatalf("unmarshal submit: %v", err)
	}
	return out.ID
}

func socialFollowMe(t *testing.T, app *fiber.App, token string) map[string]any {
	t.Helper()
	_, body := notifSuiteDo(t, app, "GET", "/social-follow/me", token, nil)
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal me: %v, body = %s", err, body)
	}
	return out
}

// TestSocialFollow_SubmissionIsAllOrNothing is the property the atomic model
// exists for. A submission carrying one platform's proof must not be
// creatable at all - not accepted-and-flagged, not stored as partial. The old
// per-platform endpoint made a half-approved state representable, and
// anything representable eventually happens.
func TestSocialFollow_SubmissionIsAllOrNothing(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := socialFollowSuiteToken(t, userID, "contributor")

	for _, tc := range []struct {
		name string
		body string
	}{
		{"linkedin only", `{"linkedin_screenshot":"` + pngDataURL + `"}`},
		{"x only", `{"x_screenshot":"` + pngDataURL + `"}`},
		{"neither", `{}`},
		{"one is blank", `{"linkedin_screenshot":"` + pngDataURL + `","x_screenshot":"  "}`},
	} {
		resp, _ := notifSuiteDo(t, app, "POST", "/social-follow/submit", token, []byte(tc.body))
		if resp.StatusCode != fiber.StatusBadRequest {
			t.Errorf("%s: status = %d, want %d", tc.name, resp.StatusCode, fiber.StatusBadRequest)
		}
	}

	// Nothing partial may have been written by any of those attempts.
	var rows int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM social_follow_submissions WHERE user_id = $1`, userID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d submission row(s) written by rejected partial submissions, want 0", rows)
	}

	me := socialFollowMe(t, app, token)
	if me["submitted"] != false || me["eligible"] != false {
		t.Errorf("me = %+v, want submitted and eligible both false", me)
	}
}

// TestSocialFollow_ApproveMakesEligibleAndRevokeTakesItBack walks the whole
// lifecycle, because the states only mean anything in sequence.
func TestSocialFollow_ApproveMakesEligibleAndRevokeTakesItBack(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := socialFollowSuiteToken(t, userID, "contributor")
	adminID := adminSuiteInsertUser(t, d, "admin")
	adminToken := socialFollowSuiteToken(t, adminID, "admin")

	id := submitBoth(t, app, token)
	if me := socialFollowMe(t, app, token); me["status"] != "pending" || me["eligible"] != false {
		t.Fatalf("after submit me = %+v, want pending and not eligible", me)
	}

	resp, body := notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/"+id+"/approve", adminToken, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("approve status = %d, body = %s", resp.StatusCode, body)
	}
	if me := socialFollowMe(t, app, token); me["status"] != "approved" || me["eligible"] != true {
		t.Fatalf("after approve me = %+v, want approved and eligible", me)
	}

	resp, body = notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/"+id+"/revoke", adminToken,
		[]byte(`{"reason":"unfollowed on LinkedIn"}`))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("revoke status = %d, body = %s", resp.StatusCode, body)
	}

	me := socialFollowMe(t, app, token)
	if me["status"] != "revoked" || me["eligible"] != true && me["eligible"] != false {
		t.Fatalf("after revoke me = %+v", me)
	}
	if me["eligible"] != false {
		t.Error("still eligible after revocation")
	}
	// The contributor must be able to see WHY. A withdrawal with no stated
	// reason reads as arbitrary.
	if me["decision_reason"] != "unfollowed on LinkedIn" {
		t.Errorf("decision_reason = %v, want the revocation reason shown to the contributor", me["decision_reason"])
	}

	// Revoking is a status change, not a deletion: the submission and both
	// screenshots survive, because they are what a dispute turns on.
	var linkedIn, x string
	if err := d.Pool.QueryRow(context.Background(), `
SELECT linkedin_screenshot, x_screenshot FROM social_follow_submissions WHERE id = $1
`, id).Scan(&linkedIn, &x); err != nil {
		t.Fatalf("submission missing after revocation: %v", err)
	}
	if linkedIn == "" || x == "" {
		t.Error("screenshots were cleared by revocation; the audit trail is the point")
	}
}

// TestSocialFollow_RejectionAndRevocationRequireAReason. Both are shown to the
// contributor, so a decision with nothing to show is not a usable decision.
func TestSocialFollow_RejectionAndRevocationRequireAReason(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := socialFollowSuiteToken(t, userID, "contributor")
	adminID := adminSuiteInsertUser(t, d, "admin")
	adminToken := socialFollowSuiteToken(t, adminID, "admin")

	id := submitBoth(t, app, token)

	resp, _ := notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/"+id+"/reject", adminToken, []byte(`{}`))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Errorf("reject without a reason: status = %d, want %d", resp.StatusCode, fiber.StatusBadRequest)
	}

	// Approve first so revoke has something legitimate to act on.
	notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/"+id+"/approve", adminToken, nil)
	resp, _ = notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/"+id+"/revoke", adminToken, []byte(`{"reason":"   "}`))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Errorf("revoke with a blank reason: status = %d, want %d", resp.StatusCode, fiber.StatusBadRequest)
	}
}

// TestSocialFollow_RevokeOnlyAppliesToAnApproval. Revoking something pending
// or rejected is meaningless and is almost certainly the wrong row.
func TestSocialFollow_RevokeOnlyAppliesToAnApproval(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := socialFollowSuiteToken(t, userID, "contributor")
	adminID := adminSuiteInsertUser(t, d, "admin")
	adminToken := socialFollowSuiteToken(t, adminID, "admin")

	id := submitBoth(t, app, token)
	resp, _ := notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/"+id+"/revoke", adminToken,
		[]byte(`{"reason":"mistake"}`))
	if resp.StatusCode != fiber.StatusConflict {
		t.Errorf("revoking a pending submission: status = %d, want %d", resp.StatusCode, fiber.StatusConflict)
	}
	if me := socialFollowMe(t, app, token); me["status"] != "pending" {
		t.Errorf("status = %v after a refused revocation, want it unchanged at pending", me["status"])
	}
}

// TestSocialFollow_ResubmissionReturnsToPendingAndKeepsTheHistory. A rejected
// contributor has to be able to try again, and the record of the earlier
// decision has to survive that.
func TestSocialFollow_ResubmissionReturnsToPendingAndKeepsTheHistory(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := socialFollowSuiteToken(t, userID, "contributor")
	adminID := adminSuiteInsertUser(t, d, "admin")
	adminToken := socialFollowSuiteToken(t, adminID, "admin")

	id := submitBoth(t, app, token)
	notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/"+id+"/reject", adminToken,
		[]byte(`{"reason":"screenshot does not show your account"}`))
	if me := socialFollowMe(t, app, token); me["status"] != "rejected" {
		t.Fatalf("status = %v, want rejected", me["status"])
	}

	again := submitBoth(t, app, token)
	if again != id {
		t.Errorf("resubmission created a second submission (%s vs %s); there is one per contributor", again, id)
	}
	me := socialFollowMe(t, app, token)
	if me["status"] != "pending" {
		t.Errorf("status = %v after resubmission, want pending", me["status"])
	}
	if me["decision_reason"] != nil {
		t.Errorf("decision_reason = %v after resubmission, want it cleared", me["decision_reason"])
	}

	// The append-only log is what makes overwriting the current status safe.
	var decisions int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM social_follow_decisions WHERE submission_id = $1`, id).Scan(&decisions); err != nil {
		t.Fatalf("count decisions: %v", err)
	}
	if decisions != 3 { // submitted, rejected, submitted
		t.Errorf("%d decisions logged, want 3 (submitted, rejected, submitted)", decisions)
	}
}

// TestSocialFollow_AdminListShowsBothScreenshotsForOneDecision: a reviewer
// judging one platform without the other in view is making half a decision.
func TestSocialFollow_AdminListShowsBothScreenshotsForOneDecision(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := socialFollowSuiteToken(t, userID, "contributor")
	adminID := adminSuiteInsertUser(t, d, "admin")
	adminToken := socialFollowSuiteToken(t, adminID, "admin")

	id := submitBoth(t, app, token)

	_, body := notifSuiteDo(t, app, "GET", "/admin/social-follow/submissions", adminToken, nil)
	var list struct {
		Submissions []struct {
			ID       string `json:"id"`
			LinkedIn string `json:"linkedin_screenshot"`
			X        string `json:"x_screenshot"`
			Status   string `json:"status"`
		} `json:"submissions"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, body)
	}
	var found bool
	for _, s := range list.Submissions {
		if s.ID != id {
			continue
		}
		found = true
		if s.LinkedIn == "" || s.X == "" {
			t.Error("a queued submission is missing one of its screenshots")
		}
		if s.Status != "pending" {
			t.Errorf("status = %s, want pending", s.Status)
		}
	}
	if !found {
		t.Error("submission missing from the pending review queue")
	}
}

// TestSocialFollow_AdminEndpointsRequireAdminRole.
func TestSocialFollow_AdminEndpointsRequireAdminRole(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := socialFollowSuiteToken(t, userID, "contributor")
	none := uuid.Nil.String()

	for _, path := range []string{
		"/admin/social-follow/submissions/" + none + "/approve",
		"/admin/social-follow/submissions/" + none + "/reject",
		"/admin/social-follow/submissions/" + none + "/revoke",
	} {
		resp, _ := notifSuiteDo(t, app, "POST", path, token, []byte(`{"reason":"x"}`))
		if resp.StatusCode != fiber.StatusForbidden {
			t.Errorf("%s: status = %d, want %d for a non-admin", path, resp.StatusCode, fiber.StatusForbidden)
		}
	}
}
