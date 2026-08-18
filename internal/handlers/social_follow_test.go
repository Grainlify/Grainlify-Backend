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
	// Clear the queue first.
	//
	// dbtest.DB is shared and nothing truncates this table, so submissions
	// accumulate across every run - 550 pending rows had built up. The review
	// queue pages at socialFollowPageSize (50), so a freshly created
	// submission no longer appears on the first page and
	// TestSocialFollow_AdminListCarriesNoScreenshots fails looking for it.
	//
	// It failed in ISOLATION and passed in a full-suite run, which is the
	// wrong way round and why it went unnoticed: run alone the table is at its
	// dirtiest, and a full run happens to reorder things. A test that is green
	// only in company is not green.
	if d != nil && d.Pool != nil {
		_, _ = d.Pool.Exec(context.Background(), `TRUNCATE social_follow_decisions, social_follow_submissions CASCADE`)
	}
	app := fiber.New()
	h := handlers.NewSocialFollowHandler(d, nil)
	app.Post("/social-follow/submit", auth.RequireAuth(socialFollowSuiteJWTSecret), h.SubmitAll())
	app.Get("/social-follow/me", auth.RequireAuth(socialFollowSuiteJWTSecret), h.Me())

	admin := app.Group("/admin", auth.RequireAuth(socialFollowSuiteJWTSecret))
	admin.Get("/social-follow/submissions", auth.RequireRole("admin"), h.ListSubmissions())
	admin.Get("/social-follow/reason-codes", auth.RequireRole("admin"), h.ReasonCodes())
	admin.Get("/social-follow/submissions/:id/proofs", auth.RequireRole("admin"), h.Proofs())
	admin.Post("/social-follow/submissions/bulk-approve", auth.RequireRole("admin"), h.BulkApprove())
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

// The list carries what a COLLAPSED row needs and nothing else.
//
// This test used to assert the opposite - that both screenshots were in the
// list - because the review UI rendered them inline. Rows are collapsed by
// default now and the proofs are fetched per submission when one is expanded.
func TestSocialFollow_AdminListCarriesNoScreenshots(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	token := socialFollowSuiteToken(t, seedSocialFollowUser(t, d, "sf-list-c"), "contributor")
	adminToken := socialFollowSuiteToken(t, seedSocialFollowUser(t, d, "sf-list-admin"), "admin")

	id := submitBoth(t, app, token)

	row, found := findSubmission(t, app, adminToken, "pending", id)
	if !found {
		t.Fatal("submission missing from the pending review queue")
	}
	if row["status"] != "pending" {
		t.Errorf("status = %v, want pending", row["status"])
	}

	// The inverted premise. This test used to assert both screenshots were
	// present in the list, because the review UI rendered them inline. Rows
	// are collapsed by default now and the proofs are fetched per submission,
	// so a screenshot appearing here is the 775kB-a-row payload coming back
	// rather than a feature working.
	for _, k := range []string{"linkedin_screenshot", "x_screenshot"} {
		if v, present := row[k]; present && v != nil && v != "" {
			t.Errorf("the list response still carries %s; that is ~775kB a row and the reason this endpoint was 7.7MB a page", k)
		}
	}
	// What a compact row does need, so it can be identified without expanding.
	if row["github_login"] == "" {
		t.Error("no github_login: a collapsed row has nothing else to identify the person by")
	}
	if _, present := row["avatar_url"]; !present {
		t.Error("no avatar_url in the list response")
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

	// Ask for a deliberately small page rather than relying on the default.
	// This test used to hardcode 10, which broke the moment the page size
	// changed - and it changed for a good reason. What it actually cares
	// about is that a page is bounded and that the caller is told what is
	// beyond it, neither of which is a specific number.
	const pageSize = 5
	const submitted = 13
	for i := 0; i < submitted; i++ {
		u := seedSocialFollowUser(t, d, "sf-pager")
		submitBoth(t, app, socialFollowSuiteToken(t, u, "contributor"))
	}

	first := adminList(t, app, admin, fmt.Sprintf("?status=pending&limit=%d", pageSize))
	rows := listRows(t, first)
	if len(rows) != pageSize {
		t.Errorf("page returned %d rows, want the %d requested", len(rows), pageSize)
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

	second := adminList(t, app, admin, fmt.Sprintf("?status=pending&limit=%d&offset=%d", pageSize, pageSize))
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

	// The ceiling is read back from the API rather than named here. Hardcoding
	// it made this test fail when the page size legitimately rose once the
	// screenshots left the response; what it is really asserting is that an
	// absurd request does not get what it asked for, and that is true at any
	// ceiling.
	const absurd = 100000
	capped := adminList(t, app, admin, fmt.Sprintf("?status=pending&limit=%d", absurd))
	ceiling, _ := capped["limit"].(float64)
	if int(ceiling) >= absurd {
		t.Errorf("limit = %v: an absurd page size was honoured rather than capped", capped["limit"])
	}
	if len(listRows(t, capped)) > int(ceiling) {
		t.Errorf("returned %d rows against a stated limit of %v", len(listRows(t, capped)), capped["limit"])
	}

	// Nonsense means "unset", not "no rows": both fall back to the default,
	// which has to be a real page rather than zero.
	for _, q := range []string{"?status=pending&limit=-1", "?status=pending&limit=0"} {
		got := adminList(t, app, admin, q)
		l, _ := got["limit"].(float64)
		if l <= 0 || int(l) > int(ceiling) {
			t.Errorf("%s: limit = %v, want the default (>0 and <= the %v ceiling)", q, got["limit"], ceiling)
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

// bulkApprove posts a selection and returns the decoded per-row report.
func bulkApprove(t *testing.T, app *fiber.App, token string, ids ...string) (int, map[string]any) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"ids": ids})
	resp, body := notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/bulk-approve", token, payload)
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	return resp.StatusCode, out
}

func countIn(m map[string]any, key string) int {
	v, _ := m[key].(float64)
	return int(v)
}

// A partial failure must be reported as a partial failure.
//
// The admin is told three separate facts: what was approved, what was skipped
// because the queue moved underneath them, and what actually failed. Collapsing
// skipped into failed sends somebody hunting a bug that is not there; reporting
// everything as approved is the lie this endpoint exists to avoid.
func TestSocialFollow_BulkApproveReportsEachRowsOutcome(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	adminID := seedSocialFollowUser(t, d, "sf-bulk-admin")
	admin := socialFollowSuiteToken(t, adminID, "admin")

	// One that will approve cleanly.
	fresh := submitBoth(t, app, socialFollowSuiteToken(t, seedSocialFollowUser(t, d, "sf-bulk-a"), "contributor"))

	// One already approved before the batch runs - the stale-row case.
	stale := submitBoth(t, app, socialFollowSuiteToken(t, seedSocialFollowUser(t, d, "sf-bulk-b"), "contributor"))
	if code, body := notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/"+stale+"/approve", admin, nil); code.StatusCode != fiber.StatusOK {
		t.Fatalf("pre-approve failed: %s", body)
	}

	// One already rejected - a different skip, and the one that matters most:
	// silently re-approving it would reverse a decision somebody made.
	rejected := submitBoth(t, app, socialFollowSuiteToken(t, seedSocialFollowUser(t, d, "sf-bulk-c"), "contributor"))
	if code, body := notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/"+rejected+"/reject", admin,
		[]byte(`{"reason_code":"unreadable"}`)); code.StatusCode != fiber.StatusOK {
		t.Fatalf("pre-reject failed: %s", body)
	}

	// One that does not exist at all.
	missing := uuid.NewString()

	status, out := bulkApprove(t, app, admin, fresh, stale, rejected, missing)
	if status != fiber.StatusOK {
		t.Fatalf("bulk approve status = %d, body = %+v", status, out)
	}

	if got := countIn(out, "approved_count"); got != 1 {
		t.Errorf("approved_count = %d, want 1", got)
	}
	if got := countIn(out, "skipped_count"); got != 3 {
		t.Errorf("skipped_count = %d, want 3 (already approved, already rejected, missing)", got)
	}
	if got := countIn(out, "failed_count"); got != 0 {
		t.Errorf("failed_count = %d, want 0 - none of these are failures", got)
	}

	// The skips must say WHY, and carry the status that caused them.
	reasons := map[string]string{}
	for _, raw := range out["skipped"].([]any) {
		row := raw.(map[string]any)
		reasons[row["id"].(string)] = row["reason"].(string)
		if row["reason"] == "not_pending" && row["current_status"] == nil {
			t.Errorf("skip for %v does not say what the row's status actually was", row["id"])
		}
	}
	if reasons[stale] != "not_pending" {
		t.Errorf("already-approved row skipped as %q, want not_pending", reasons[stale])
	}
	if reasons[rejected] != "not_pending" {
		t.Errorf("already-rejected row skipped as %q, want not_pending", reasons[rejected])
	}
	if reasons[missing] != "not_found" {
		t.Errorf("missing row skipped as %q, want not_found", reasons[missing])
	}

	// The rejected row must still be rejected. This is the whole point of the
	// guard: a bulk approve must not quietly reverse somebody's decision.
	row, found := findSubmission(t, app, admin, "rejected", rejected)
	if !found {
		t.Fatal("the rejected submission is no longer rejected after a bulk approve")
	}
	if row["status"] != "rejected" {
		t.Errorf("status = %v, want it left rejected", row["status"])
	}
}

// A selection larger than a page is refused outright.
//
// Approval grants Founding Contributor Pool eligibility, so approving what you
// have not looked at is the thing to prevent. The UI only offers selection
// over rows on screen; this makes that a rule rather than a convention, since
// the endpoint is reachable without the UI.
func TestSocialFollow_BulkApproveRefusesMoreThanAPage(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	admin := socialFollowSuiteToken(t, seedSocialFollowUser(t, d, "sf-bulk-cap"), "admin")

	// One more than a full page, whatever a full page currently is.
	pageLimit, _ := adminList(t, app, admin, "?status=pending&limit=100000")["limit"].(float64)
	ids := make([]string, 0, int(pageLimit)+1)
	for i := 0; i < int(pageLimit)+1; i++ {
		ids = append(ids, uuid.NewString())
	}
	status, out := bulkApprove(t, app, admin, ids...)
	if status != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a selection larger than a page", status)
	}
	if out["error"] != "too_many_submissions" {
		t.Errorf("error = %v, want too_many_submissions", out["error"])
	}
}

func TestSocialFollow_BulkApproveRejectsAnEmptySelection(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	admin := socialFollowSuiteToken(t, seedSocialFollowUser(t, d, "sf-bulk-empty"), "admin")

	status, out := bulkApprove(t, app, admin)
	if status != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if out["error"] != "no_submissions_selected" {
		t.Errorf("error = %v", out["error"])
	}
}

// The same id twice is one submission, and must not consume two slots of the
// cap - otherwise a legitimate page-sized selection could be refused.
func TestSocialFollow_BulkApproveDedupesIDs(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	admin := socialFollowSuiteToken(t, seedSocialFollowUser(t, d, "sf-bulk-dupe"), "admin")
	id := submitBoth(t, app, socialFollowSuiteToken(t, seedSocialFollowUser(t, d, "sf-bulk-dupe-c"), "contributor"))

	status, out := bulkApprove(t, app, admin, id, id, id)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, body = %+v", status, out)
	}
	if got := countIn(out, "approved_count"); got != 1 {
		t.Errorf("approved_count = %d, want 1 - the same row three times is one approval", got)
	}
	if got := countIn(out, "skipped_count"); got != 0 {
		t.Errorf("skipped_count = %d, want 0 - the duplicates should never have been attempted", got)
	}
}

// Single-row approve and reject were unguarded: acting on a stale row silently
// overwrote the existing decision.
func TestSocialFollow_ApproveAndRejectRefuseStaleRows(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	admin := socialFollowSuiteToken(t, seedSocialFollowUser(t, d, "sf-guard-admin"), "admin")
	contributor := socialFollowSuiteToken(t, seedSocialFollowUser(t, d, "sf-guard-c"), "contributor")
	id := submitBoth(t, app, contributor)

	if resp, body := notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/"+id+"/reject", admin,
		[]byte(`{"reason_code":"wrong_account"}`)); resp.StatusCode != fiber.StatusOK {
		t.Fatalf("first reject failed: %s", body)
	}

	// Approving it now would reverse that rejection without a trace.
	resp, body := notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/"+id+"/approve", admin, nil)
	if resp.StatusCode != fiber.StatusConflict {
		t.Fatalf("approve on a rejected row = %d, want 409; body = %s", resp.StatusCode, body)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	if out["current_status"] != "rejected" {
		t.Errorf("conflict does not report the actual status: %+v", out)
	}

	// And rejecting twice must not stack a second decision either.
	if resp, _ := notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/"+id+"/reject", admin,
		[]byte(`{"reason_code":"unreadable"}`)); resp.StatusCode != fiber.StatusConflict {
		t.Errorf("second reject = %d, want 409", resp.StatusCode)
	}
}

// Codes are stored alongside the note, not instead of it.
func TestSocialFollow_RejectionStoresCodeAndNoteAndShowsBothToTheContributor(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	admin := socialFollowSuiteToken(t, seedSocialFollowUser(t, d, "sf-code-admin"), "admin")
	contributorID := seedSocialFollowUser(t, d, "sf-code-c")
	contributor := socialFollowSuiteToken(t, contributorID, "contributor")
	id := submitBoth(t, app, contributor)

	if resp, body := notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/"+id+"/reject", admin,
		[]byte(`{"reason_code":"unreadable","reason":"the second image is a profile page"}`)); resp.StatusCode != fiber.StatusOK {
		t.Fatalf("reject failed: %s", body)
	}

	me := socialFollowMe(t, app, contributor)
	if me["reason_code"] != "unreadable" {
		t.Errorf("reason_code = %v", me["reason_code"])
	}
	// The note survives alongside the code - that is why both columns exist.
	if me["decision_reason"] != "the second image is a profile page" {
		t.Errorf("decision_reason = %v", me["decision_reason"])
	}
	// And the contributor reads one resolved sentence, not a code.
	want := "Screenshot unreadable or wrong image - the second image is a profile page"
	if me["decision_text"] != want {
		t.Errorf("decision_text = %v, want %q", me["decision_text"], want)
	}
}

func TestSocialFollow_OtherRequiresANoteAndBadCodesAreRefused(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	admin := socialFollowSuiteToken(t, seedSocialFollowUser(t, d, "sf-other-admin"), "admin")
	id := submitBoth(t, app, socialFollowSuiteToken(t, seedSocialFollowUser(t, d, "sf-other-c"), "contributor"))

	// "Other" with no note tells the contributor their proof was rejected for
	// "Other", which reads as an answer while saying nothing.
	resp, body := notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/"+id+"/reject", admin,
		[]byte(`{"reason_code":"other"}`))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf(`"other" with no note = %d, want 400; body = %s`, resp.StatusCode, body)
	}

	resp, _ = notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/"+id+"/reject", admin,
		[]byte(`{"reason_code":"not_a_real_code","reason":"x"}`))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Errorf("unknown code = %d, want 400", resp.StatusCode)
	}

	// A bare note with no code still works, so an admin bundle cached across
	// the deploy keeps reviewing.
	if resp, body := notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/"+id+"/reject", admin,
		[]byte(`{"reason":"legacy free text"}`)); resp.StatusCode != fiber.StatusOK {
		t.Errorf("legacy reason-only reject = %d, want 200; body = %s", resp.StatusCode, body)
	}
}

// The proofs a reviewer sees when they expand a row.
//
// Both platforms in one response, always. A decision covers both, and the
// atomic submission model exists so nobody has to make half of one - an
// endpoint that could return a single platform's proof would put that back.
func TestSocialFollow_ProofsReturnsBothScreenshotsTogether(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	admin := socialFollowSuiteToken(t, seedSocialFollowUser(t, d, "sf-proofs-admin"), "admin")
	contributor := socialFollowSuiteToken(t, seedSocialFollowUser(t, d, "sf-proofs-c"), "contributor")
	id := submitBoth(t, app, contributor)

	resp, body := notifSuiteDo(t, app, "GET", "/admin/social-follow/submissions/"+id+"/proofs", admin, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("proofs status = %d, body = %s", resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["linkedin_screenshot"] != pngDataURL {
		t.Errorf("linkedin_screenshot = %v", out["linkedin_screenshot"])
	}
	if out["x_screenshot"] != pngDataURL {
		t.Errorf("x_screenshot = %v", out["x_screenshot"])
	}
}

func TestSocialFollow_ProofsRequiresAdminAndAValidID(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	contributorID := seedSocialFollowUser(t, d, "sf-proofs-guard")
	contributor := socialFollowSuiteToken(t, contributorID, "contributor")
	admin := socialFollowSuiteToken(t, seedSocialFollowUser(t, d, "sf-proofs-guard-admin"), "admin")
	id := submitBoth(t, app, contributor)

	// These are photographs of somebody's social accounts. An unguessable UUID
	// is not authorisation, and the submitter's own token is not an admin's.
	if resp, _ := notifSuiteDo(t, app, "GET", "/admin/social-follow/submissions/"+id+"/proofs", contributor, nil); resp.StatusCode == fiber.StatusOK {
		t.Error("a contributor could read the proofs endpoint")
	}
	if resp, _ := notifSuiteDo(t, app, "GET", "/admin/social-follow/submissions/"+uuid.NewString()+"/proofs", admin, nil); resp.StatusCode != fiber.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", resp.StatusCode)
	}
	if resp, _ := notifSuiteDo(t, app, "GET", "/admin/social-follow/submissions/not-a-uuid/proofs", admin, nil); resp.StatusCode != fiber.StatusBadRequest {
		t.Errorf("malformed id = %d, want 400", resp.StatusCode)
	}
}
