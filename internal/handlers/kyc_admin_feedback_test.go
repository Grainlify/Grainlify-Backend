package handlers_test

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// Telling a contributor what was wrong, and being able to prove afterwards
// what we told them.
//
// The reset used to send the ADMIN'S INTERNAL REASON to the contributor
// verbatim - one column doing two jobs - and recorded nothing about whether
// the message arrived. Three resets were applied by hand against production
// and notified nobody; the only reason anyone knows is that the operator
// typed it into the reason text.

func kycFeedbackApp(t *testing.T, d *db.DB, actorID uuid.UUID) *fiber.App {
	t.Helper()
	h := handlers.NewKYCAdminHandler(d, notifications.New(d, nil, "https://grainlify.com"))
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals("user_id", actorID.String())
		return c.Next()
	})
	app.Post("/admin/kyc/:id/reset", h.Reset())
	app.Get("/admin/kyc/:id/resets", h.History())
	app.Get("/admin/kyc/pending", h.Pending())
	app.Get("/admin/kyc/reason-codes", h.ReasonCodes())
	return app
}

func kycFeedbackGet(t *testing.T, app *fiber.App, path string) (int, []byte) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", path, nil), 10000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw
}

// The heart of it: the contributor is told the chosen reason, and is NOT told
// the admin's internal justification.
func TestKYCReset_SendsTheChosenReasonNotTheInternalNote(t *testing.T) {
	d := testDB(t)
	actor := leaderboardSuiteUser(t, d.Pool)
	subject := seedKYCUser(t, d, "rejected", "sess-"+uuid.New().String())
	app := kycFeedbackApp(t, d, actor)

	const internal = "third attempt, escalated by support, watch this account"
	code, out := kycAdminPost(t, app, subject,
		`{"reason_code":"document_is_a_screen_photo","note":"the upload was a screenshot both times","reason":"`+internal+`"}`)
	if code != fiber.StatusOK {
		t.Fatalf("reset returned %d: %v", code, out)
	}
	if out["notified"] != true {
		t.Errorf("notified = %v, want true", out["notified"])
	}

	var title, body, link string
	if err := d.Pool.QueryRow(t.Context(), `
SELECT title, body, link_path FROM notifications
WHERE user_id = $1 AND type = 'kyc_reset'
`, subject).Scan(&title, &body, &link); err != nil {
		t.Fatalf("no notification was created: %v", err)
	}

	// The written sentence for the chosen code.
	if !strings.Contains(strings.ToLower(body), "physical document") {
		t.Errorf("body does not carry the chosen reason's message:\n%s", body)
	}
	// The admin's note to the contributor.
	if !strings.Contains(body, "screenshot both times") {
		t.Errorf("body omits the admin's note:\n%s", body)
	}
	// And never the internal justification.
	if strings.Contains(body, internal) {
		t.Errorf("the admin's INTERNAL reason was sent to the contributor:\n%s", body)
	}
	if title == "" {
		t.Error("notification has no title")
	}
}

// The link must point at the screen where verification actually lives.
//
// It pointed at ?subtab=payout. Verification is in BillingTab; the payout
// screen's own copy tells you to go to Billing. Same class as the maintainer
// notification that pointed at the contributor view - a link to a page where
// the action is not.
func TestKYCReset_NotificationLinksToBillingNotPayout(t *testing.T) {
	d := testDB(t)
	actor := leaderboardSuiteUser(t, d.Pool)
	subject := seedKYCUser(t, d, "rejected", "sess-"+uuid.New().String())
	app := kycFeedbackApp(t, d, actor)

	if code, out := kycAdminPost(t, app, subject,
		`{"reason_code":"document_unreadable","reason":"blurry"}`); code != fiber.StatusOK {
		t.Fatalf("reset returned %d: %v", code, out)
	}

	var link string
	if err := d.Pool.QueryRow(t.Context(), `
SELECT link_path FROM notifications WHERE user_id = $1 AND type = 'kyc_reset'
`, subject).Scan(&link); err != nil {
		t.Fatalf("no notification: %v", err)
	}
	if want := notifications.SettingsLink(notifications.SubtabBilling); link != want {
		t.Errorf("link_path = %q, want %q", link, want)
	}
	if strings.Contains(link, "payout") {
		t.Errorf("link still points at the payout screen, where verification is not: %q", link)
	}
}

// The evidence behind the decision is copied at reset time, because it does
// not survive the contributor's next attempt.
func TestKYCReset_SnapshotsTheDecisionItRespondedTo(t *testing.T) {
	d := testDB(t)
	actor := leaderboardSuiteUser(t, d.Pool)
	subject := seedKYCUser(t, d, "rejected", "sess-"+uuid.New().String())
	if _, err := d.Pool.Exec(t.Context(), `
UPDATE users SET kyc_data = '{"id_verification":{"warnings":[{"risk":"SCREEN_CAPTURE_DETECTED"}]}}' WHERE id = $1
`, subject); err != nil {
		t.Fatalf("seed decision: %v", err)
	}
	app := kycFeedbackApp(t, d, actor)

	if code, out := kycAdminPost(t, app, subject,
		`{"reason_code":"document_is_a_screen_photo","reason":"screenshot"}`); code != fiber.StatusOK {
		t.Fatalf("reset returned %d: %v", code, out)
	}

	var snapshot []byte
	if err := d.Pool.QueryRow(t.Context(), `
SELECT previous_kyc_data FROM kyc_reset_audit WHERE subject_user_id = $1
`, subject).Scan(&snapshot); err != nil {
		t.Fatalf("audit read: %v", err)
	}
	if !strings.Contains(string(snapshot), "SCREEN_CAPTURE_DETECTED") {
		t.Fatalf("the decision behind the reset was not captured: %s", snapshot)
	}

	// Now destroy the live copy, exactly as the contributor's next attempt
	// does, and confirm the audit still answers "why did we tell them that?".
	if _, err := d.Pool.Exec(t.Context(), `
UPDATE users SET kyc_data = '{"session_url":"https://verify.example/new"}' WHERE id = $1
`, subject); err != nil {
		t.Fatalf("simulate retry: %v", err)
	}
	var after []byte
	if err := d.Pool.QueryRow(t.Context(), `
SELECT previous_kyc_data FROM kyc_reset_audit WHERE subject_user_id = $1
`, subject).Scan(&after); err != nil {
		t.Fatalf("audit read: %v", err)
	}
	if !strings.Contains(string(after), "SCREEN_CAPTURE_DETECTED") {
		t.Error("the snapshot did not survive the contributor's next attempt, which is the only reason it exists")
	}
}

// Whether the contributor was told is recorded, and readable without database
// access. Its absence is how three silent resets went unnoticed.
func TestKYCReset_RecordsWhetherTheContributorWasTold(t *testing.T) {
	d := testDB(t)
	actor := leaderboardSuiteUser(t, d.Pool)
	subject := seedKYCUser(t, d, "rejected", "sess-"+uuid.New().String())
	app := kycFeedbackApp(t, d, actor)

	if code, out := kycAdminPost(t, app, subject,
		`{"reason_code":"face_did_not_match","note":"try without the hat","reason":"low similarity"}`); code != fiber.StatusOK {
		t.Fatalf("reset returned %d: %v", code, out)
	}

	var notified bool
	var notifyErr *string
	if err := d.Pool.QueryRow(t.Context(), `
SELECT notified_at IS NOT NULL, notify_error FROM kyc_reset_audit WHERE subject_user_id = $1
`, subject).Scan(&notified, &notifyErr); err != nil {
		t.Fatalf("audit read: %v", err)
	}
	if !notified {
		t.Error("notified_at not set on a reset whose notification succeeded")
	}
	if notifyErr != nil {
		t.Errorf("notify_error = %q on a successful delivery", *notifyErr)
	}

	// And it comes back through the endpoint, so the answer does not require
	// database access - which is the state that made the silent resets
	// invisible in the first place.
	status, body := kycFeedbackGet(t, app, "/admin/kyc/"+subject.String()+"/resets")
	if status != fiber.StatusOK {
		t.Fatalf("history returned %d", status)
	}
	var hist struct {
		Resets []map[string]any `json:"resets"`
	}
	if err := json.Unmarshal(body, &hist); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(hist.Resets) != 1 {
		t.Fatalf("got %d resets, want 1", len(hist.Resets))
	}
	if hist.Resets[0]["notified"] != true {
		t.Errorf("history notified = %v, want true", hist.Resets[0]["notified"])
	}
	if hist.Resets[0]["reason_code"] != "face_did_not_match" {
		t.Errorf("history reason_code = %v", hist.Resets[0]["reason_code"])
	}
	if msg, _ := hist.Resets[0]["message_sent"].(string); !strings.Contains(msg, "try without the hat") {
		t.Errorf("history does not show what the contributor was told: %v", hist.Resets[0]["message_sent"])
	}
}

// 'other' with no note would send a refusal with no content - the dead end,
// rebuilt one layer up.
func TestKYCReset_OtherRequiresANote(t *testing.T) {
	d := testDB(t)
	actor := leaderboardSuiteUser(t, d.Pool)
	subject := seedKYCUser(t, d, "rejected", "sess-"+uuid.New().String())
	app := kycFeedbackApp(t, d, actor)

	code, out := kycAdminPost(t, app, subject, `{"reason_code":"other","reason":"unusual case"}`)
	if code != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %v)", code, out)
	}
	if out["error"] != "note_required" {
		t.Errorf("error = %v, want note_required", out["error"])
	}

	// Nothing was written and nobody was told.
	var n int
	_ = d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM kyc_reset_audit WHERE subject_user_id = $1`, subject).Scan(&n)
	if n != 0 {
		t.Errorf("a rejected request still wrote %d audit rows", n)
	}
	var status string
	_ = d.Pool.QueryRow(t.Context(), `SELECT kyc_status FROM users WHERE id = $1`, subject).Scan(&status)
	if status != "rejected" {
		t.Errorf("kyc_status = %q; a refused request must not reset anybody", status)
	}
}

func TestKYCReset_RejectsAnUnlistedReasonCode(t *testing.T) {
	d := testDB(t)
	actor := leaderboardSuiteUser(t, d.Pool)
	subject := seedKYCUser(t, d, "rejected", "sess-"+uuid.New().String())
	app := kycFeedbackApp(t, d, actor)

	for _, body := range []string{
		`{"reason":"no code at all"}`,
		`{"reason_code":"","reason":"empty"}`,
		`{"reason_code":"made_up_code","reason":"invented"}`,
		// The shape that matters most: a fraud signal smuggled in as a code.
		`{"reason_code":"ip_country_mismatch","reason":"vpn"}`,
	} {
		code, out := kycAdminPost(t, app, subject, body)
		if code != fiber.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, code)
		}
		if out["error"] != "invalid_reason_code" {
			t.Errorf("body %s: error = %v, want invalid_reason_code", body, out["error"])
		}
	}
}

// The queue carries what is needed to choose, and nothing from the provider.
func TestKYCPending_ListsTheQueueWithoutProviderText(t *testing.T) {
	d := testDB(t)
	actor := leaderboardSuiteUser(t, d.Pool)
	app := kycFeedbackApp(t, d, actor)

	refused := seedKYCUser(t, d, "rejected", "sess-"+uuid.New().String())
	const warning = "Screen capture of document detected"
	if _, err := d.Pool.Exec(t.Context(), `
UPDATE users SET kyc_data = $1 WHERE id = $2
`, `{"session_number":"41",
     "extracted":{"full_name":"Fixture Person","document_number":"MUST-NOT-APPEAR","date_of_birth":"1990-01-01"},
     "id_verification":{"warnings":[{"risk":"SCREEN_CAPTURE_DETECTED","long_description":"`+warning+`"}]},
     "ip_analysis":{"warnings":[{"risk":"DUPLICATED_IP_ADDRESS","long_description":"Duplicated IP address from another session"}]}}`,
		refused); err != nil {
		t.Fatalf("seed: %v", err)
	}
	inReview := seedKYCUser(t, d, "in_review", "sess-"+uuid.New().String())
	verified := seedKYCUser(t, d, "verified", "sess-"+uuid.New().String())

	status, body := kycFeedbackGet(t, app, "/admin/kyc/pending")
	if status != fiber.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}

	// No provider prose anywhere in the payload, at any depth.
	if strings.Contains(string(body), warning) {
		t.Errorf("the queue leaks provider warning text:\n%s", body)
	}
	if strings.Contains(string(body), "Duplicated IP address") {
		t.Errorf("the queue leaks a fraud signal:\n%s", body)
	}
	// The strongest form of the narrowing: the fixture carries a document
	// number and a date of birth, and neither may appear anywhere in the
	// payload at any depth - not merely be absent from a named key.
	if strings.Contains(string(body), "MUST-NOT-APPEAR") {
		t.Errorf("the document number reached the response:\n%s", body)
	}
	if strings.Contains(string(body), "1990-01-01") {
		t.Errorf("the date of birth reached the response:\n%s", body)
	}

	var out struct {
		Pending []map[string]any `json:"pending"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]map[string]any{}
	for _, r := range out.Pending {
		byID[r["user_id"].(string)] = r
	}
	if _, ok := byID[refused.String()]; !ok {
		t.Error("a refused contributor is missing from the queue")
	}
	if _, ok := byID[inReview.String()]; !ok {
		t.Error("an in_review contributor is missing from the queue")
	}
	if _, ok := byID[verified.String()]; ok {
		t.Error("a verified contributor is in the queue; nothing is waiting on them")
	}

	// The name is carried, and it is the ONE piece of personal data this
	// endpoint returns. The provider console lists people by legal name, has no
	// documented search by session id and no URL that opens one session, so a
	// session id alone identifies a session that cannot be looked up.
	if got, _ := byID[refused.String()]["legal_name"].(string); got == "" {
		t.Error("legal_name is missing; a reviewer cannot match this row to a session in the console")
	}
	// And the exception stops at the name. Each of these is something a
	// reviewer already sees in the console, and each is a field somebody could
	// argue helps matching.
	for _, forbidden := range []string{
		"document_number", "date_of_birth", "nationality", "address",
		"place_of_birth", "document_type", "face_match_score",
	} {
		if _, present := byID[refused.String()][forbidden]; present {
			t.Errorf("%q is exposed; the name is a deliberate exception and it stops at the name", forbidden)
		}
	}
	// The provider's own session counter - not personal data, and better than
	// the name if their table shows it.
	if got, _ := byID[refused.String()]["session_number"].(string); got == "" {
		t.Error("session_number is missing")
	}

	// The session id is carried, because matching a queue row to a session in
	// the provider console by GitHub username does not work - the console does
	// not index by it. It is an identifier, not provider data.
	if got, _ := byID[refused.String()]["kyc_session_id"].(string); got == "" {
		t.Error("kyc_session_id is missing; a reviewer cannot match this row to a session")
	}

	// Suggestions name the screen photo and say nothing about the IP.
	sugg, _ := byID[refused.String()]["suggested_reason_codes"].([]any)
	var codes []string
	for _, s := range sugg {
		codes = append(codes, s.(string))
	}
	if len(codes) != 1 || codes[0] != "document_is_a_screen_photo" {
		t.Errorf("suggested_reason_codes = %v, want exactly [document_is_a_screen_photo]", codes)
	}
}

// The list is served, not duplicated in the frontend, so the picker can only
// offer what the send path accepts.
func TestKYCReasonCodes_ServesExactlyWhatResetAccepts(t *testing.T) {
	d := testDB(t)
	actor := leaderboardSuiteUser(t, d.Pool)
	app := kycFeedbackApp(t, d, actor)

	status, body := kycFeedbackGet(t, app, "/admin/kyc/reason-codes")
	if status != fiber.StatusOK {
		t.Fatalf("status = %d", status)
	}
	var out struct {
		ReasonCodes []struct {
			Code      string `json:"code"`
			Label     string `json:"label"`
			Message   string `json:"message"`
			NeedsNote bool   `json:"needs_note"`
		} `json:"reason_codes"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.ReasonCodes) < 6 {
		t.Fatalf("got %d codes, want at least the six actionable reasons plus 'other'", len(out.ReasonCodes))
	}

	// Every served code is one the reset endpoint will take.
	subject := seedKYCUser(t, d, "rejected", "sess-"+uuid.New().String())
	for _, r := range out.ReasonCodes {
		note := ""
		if r.NeedsNote {
			note = "a note, because this code requires one"
		}
		payload := `{"reason_code":"` + r.Code + `","note":"` + note + `","reason":"probe"}`
		code, res := kycAdminPost(t, app, subject, payload)
		if code == fiber.StatusBadRequest && res["error"] == "invalid_reason_code" {
			t.Errorf("code %q is offered by the picker but refused by the send path", r.Code)
		}
		// Put them back so the next code has something to reset.
		_, _ = d.Pool.Exec(t.Context(), `UPDATE users SET kyc_status = 'rejected' WHERE id = $1`, subject)
	}
}
