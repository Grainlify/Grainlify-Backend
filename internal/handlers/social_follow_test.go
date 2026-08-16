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

	row, found := findSubmission(t, app, adminToken, "pending", id)
	if !found {
		t.Fatal("submission missing from the pending review queue")
	}
	if row["linkedin_screenshot"] == "" || row["x_screenshot"] == "" {
		t.Error("a queued submission is missing one of its screenshots")
	}
	if row["status"] != "pending" {
		t.Errorf("status = %v, want pending", row["status"])
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

// seedSocialFollowUser creates a user WITH a github_accounts row, because the
// review list resolves logins through that join - both the submitter's and,
// now, the deciding admin's. Seeding only the users row would make the login
// assertions pass vacuously against an empty string.
func seedSocialFollowUser(t *testing.T, d *db.DB, login string) uuid.UUID {
	t.Helper()
	userID := adminSuiteInsertUser(t, d, "contributor")
	ghID := time.Now().UnixNano() + int64(uuid.New().ID())
	if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO github_accounts (user_id, github_user_id, login, access_token, token_type, scope)
VALUES ($1, $2, $3, '\x00'::bytea, 'bearer', '')
`, userID, ghID, login); err != nil {
		t.Fatalf("seed github account for %s: %v", login, err)
	}
	return userID
}

// findSubmission pages through the queue looking for one specific submission.
//
// Necessary because the list is paginated and this suite shares a database
// with every other test in the package: a row is not guaranteed to be on the
// first page, and asserting against page one alone makes a test that passes
// or fails depending on how much unrelated data happens to exist. That is the
// same shared-fixture trap that has bitten this suite before.
func findSubmission(t *testing.T, app *fiber.App, token, status, id string) (map[string]any, bool) {
	t.Helper()
	for offset := 0; offset < 500; offset += 20 {
		page := adminList(t, app, token,
			fmt.Sprintf("?status=%s&limit=20&offset=%d", status, offset))
		rows := listRows(t, page)
		for _, r := range rows {
			row := r.(map[string]any)
			if row["id"] == id {
				return row, true
			}
		}
		if hasMore, _ := page["has_more"].(bool); !hasMore {
			break
		}
	}
	return nil, false
}

// adminList fetches one page of the review queue.
func adminList(t *testing.T, app *fiber.App, token, query string) map[string]any {
	t.Helper()
	_, body := notifSuiteDo(t, app, "GET", "/admin/social-follow/submissions"+query, token, nil)
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal list: %v, body = %s", err, body)
	}
	return out
}

func listRows(t *testing.T, m map[string]any) []any {
	t.Helper()
	rows, _ := m["submissions"].([]any)
	return rows
}

// The review queue must never return itself whole.
//
// This endpoint had no LIMIT. Every row carries both screenshots as base64
// data URLs - around 787kB per row in production - so 22 pending submissions
// was a 17MB JSON response that grew with the queue and would eventually time
// out. The page bound also gives "select all on this page" something true to
// mean: a reviewer can only act on what is on screen, so what is on screen has
// to be a bounded number rather than "everything pending".
func TestSocialFollow_ListIsPagedAndReportsWhatIsNotOnThePage(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	admin := socialFollowSuiteToken(t, seedSocialFollowUser(t, d, "sf-admin"), "admin")

	const submitted = 13
	for i := 0; i < submitted; i++ {
		u := seedSocialFollowUser(t, d, "sf-pager")
		submitBoth(t, app, socialFollowSuiteToken(t, u, "contributor"))
	}

	first := adminList(t, app, admin, "?status=pending")
	rows := listRows(t, first)
	if len(rows) != 10 {
		t.Errorf("default page returned %d rows, want the 10-row page size", len(rows))
	}
	if total, _ := first["total"].(float64); int(total) < submitted {
		t.Errorf("total = %v, want at least the %d just submitted", first["total"], submitted)
	}
	// The count that a bulk confirmation depends on: how many exist that the
	// admin cannot see. Without this the UI cannot honestly distinguish
	// "everything pending" from "this page".
	if hasMore, _ := first["has_more"].(bool); !hasMore {
		t.Error("has_more = false with more rows than fit on a page")
	}

	second := adminList(t, app, admin, "?status=pending&offset=10")
	if len(listRows(t, second)) == 0 {
		t.Error("offset returned nothing; the second page is unreachable")
	}

	// Distinct rows, not the same page twice - an off-by-one in the offset
	// would silently show page one forever.
	firstID := rows[0].(map[string]any)["id"]
	for _, r := range listRows(t, second) {
		if r.(map[string]any)["id"] == firstID {
			t.Error("page two contains a row from page one; offset is not being applied")
		}
	}
}

// No request may ask for the whole queue back.
func TestSocialFollow_PageSizeIsClampedNotObeyed(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	admin := socialFollowSuiteToken(t, seedSocialFollowUser(t, d, "sf-clamp-admin"), "admin")

	for i := 0; i < 3; i++ {
		u := seedSocialFollowUser(t, d, "sf-clamp")
		submitBoth(t, app, socialFollowSuiteToken(t, u, "contributor"))
	}

	for _, tc := range []struct {
		query   string
		wantMax int
	}{
		{"?status=pending&limit=100000", 20}, // capped, not honoured
		{"?status=pending&limit=-1", 10},     // nonsense means "unset", not "no rows"
		{"?status=pending&limit=0", 10},
	} {
		got := adminList(t, app, admin, tc.query)
		if l, _ := got["limit"].(float64); int(l) > tc.wantMax {
			t.Errorf("%s: limit = %v, want at most %d", tc.query, got["limit"], tc.wantMax)
		}
		if len(listRows(t, got)) > tc.wantMax {
			t.Errorf("%s: returned %d rows, want at most %d", tc.query, len(listRows(t, got)), tc.wantMax)
		}
	}
}

// The trail was already recorded; it just could not be read from here, so the
// admin page showed decisions with no author.
func TestSocialFollow_ListShowsWhoDecided(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)

	adminID := seedSocialFollowUser(t, d, "sf-decider")
	admin := socialFollowSuiteToken(t, adminID, "admin")
	contributor := seedSocialFollowUser(t, d, "sf-decided-on")
	id := submitBoth(t, app, socialFollowSuiteToken(t, contributor, "contributor"))

	if before, ok := findSubmission(t, app, admin, "pending", id); ok && before["decided_by"] != nil {
		t.Error("an undecided submission reports a decider")
	}

	resp, body := notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/"+id+"/approve", admin, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("approve status = %d, body = %s", resp.StatusCode, body)
	}

	row, found := findSubmission(t, app, admin, "approved", id)
	if !found {
		t.Fatal("the approved submission was not found in the approved list")
	}
	if row["decided_by"] != adminID.String() {
		t.Errorf("decided_by = %v, want the approving admin %s", row["decided_by"], adminID)
	}
	if row["decided_by_login"] != "sf-decider" {
		t.Errorf("decided_by_login = %v, want the admin's login", row["decided_by_login"])
	}
}
