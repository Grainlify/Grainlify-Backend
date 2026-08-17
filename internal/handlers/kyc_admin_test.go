package handlers_test

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// The admin KYC reset, and the properties that make it safe to hand somebody.
//
// It exists because the alternative was an UPDATE against production, which
// happened for four contributors and left no record of who ran it or why.
//
// Its scope narrowed once canStartNewKYCSession began accepting "rejected" - a
// refused contributor retries without help now - but the reset still covers
// every state they cannot leave alone, chiefly in_review.

// kycAdminApp mounts the two routes with a fixed actor, standing in for
// RequireAuth + requireAdmin (both are exercised by their own tests; what
// matters here is the handler's behaviour once past them).
func kycAdminApp(d *db.DB, actorID uuid.UUID) *fiber.App {
	h := handlers.NewKYCAdminHandler(d, nil)
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals("user_id", actorID.String())
		return c.Next()
	})
	app.Post("/admin/kyc/:id/reset", h.Reset())
	app.Get("/admin/kyc/:id/resets", h.History())
	return app
}

func kycAdminPost(t *testing.T, app *fiber.App, id uuid.UUID, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/admin/kyc/"+id.String()+"/reset", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, 10000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// seedKYCUser makes a user with a github account and a given kyc state.
func seedKYCUser(t *testing.T, d *db.DB, status string, sessionID string) uuid.UUID {
	t.Helper()
	id := leaderboardSuiteUser(t, d.Pool)
	var sess any
	if sessionID != "" {
		sess = sessionID
	}
	_, err := d.Pool.Exec(t.Context(), `
UPDATE users SET kyc_status = $1, kyc_session_id = $2, kyc_data = '{"decision":"refused"}'::jsonb WHERE id = $3
`, status, sess, id)
	if err != nil {
		t.Fatalf("seed kyc state: %v", err)
	}
	return id
}

func TestKYCReset_UnblocksARejectedContributor(t *testing.T) {
	d := testDB(t)
	actor := leaderboardSuiteUser(t, d.Pool)
	subject := seedKYCUser(t, d, "rejected", "didit-session-"+uuid.New().String()[:8])
	app := kycAdminApp(d, actor)

	code, body := kycAdminPost(t, app, subject, `{"reason":"documents were blurry, contributor asked to retry"}`)
	if code != fiber.StatusOK {
		t.Fatalf("reset returned %d: %v", code, body)
	}
	if body["previous_status"] != "rejected" {
		t.Errorf("previous_status = %v, want rejected - the caller needs to see what it undid", body["previous_status"])
	}

	var status *string
	var sessionID *string
	var hasData bool
	if err := d.Pool.QueryRow(t.Context(),
		`SELECT kyc_status, kyc_session_id, kyc_data IS NOT NULL FROM users WHERE id = $1`, subject,
	).Scan(&status, &sessionID, &hasData); err != nil {
		t.Fatalf("read back: %v", err)
	}

	if status == nil || *status != "expired" {
		t.Errorf("kyc_status = %v, want expired", status)
	}
	if sessionID != nil {
		t.Errorf("kyc_session_id = %v, want NULL - Start() refuses while a session is attached", *sessionID)
	}
	// The decision payload is the only copy of WHY they were refused, and it is
	// not needed for a retry. Destroying it would be an irreversible loss of
	// the record a dispute turns on.
	if !hasData {
		t.Error("kyc_data was cleared; the reset must preserve Didit's decision")
	}
	// And the whole point: they can start again. canStartNewKYCSession is
	// unexported, so this asserts the value it accepts rather than calling it -
	// the coupling is named here so a change to that allow-list surfaces.
	if *status != "expired" && *status != "not_started" {
		t.Errorf("after reset kyc_status is %q, which canStartNewKYCSession does not accept", *status)
	}
}

func TestKYCReset_RequiresAReason(t *testing.T) {
	d := testDB(t)
	actor := leaderboardSuiteUser(t, d.Pool)
	subject := seedKYCUser(t, d, "rejected", "s1")
	app := kycAdminApp(d, actor)

	for _, body := range []string{`{}`, `{"reason":""}`, `{"reason":"   "}`} {
		code, out := kycAdminPost(t, app, subject, body)
		if code != fiber.StatusBadRequest {
			t.Errorf("body %s returned %d, want 400 - the audit row is the point of this endpoint", body, code)
		}
		if out["error"] != "reason_required" {
			t.Errorf("body %s: error = %v, want reason_required", body, out["error"])
		}
	}

	// And nothing was written.
	var status *string
	_ = d.Pool.QueryRow(t.Context(), `SELECT kyc_status FROM users WHERE id = $1`, subject).Scan(&status)
	if status == nil || *status != "rejected" {
		t.Errorf("status changed to %v despite the request being refused", status)
	}
}

func TestKYCReset_RefusesToUnverifySomebody(t *testing.T) {
	d := testDB(t)
	actor := leaderboardSuiteUser(t, d.Pool)
	subject := seedKYCUser(t, d, "verified", "s2")
	app := kycAdminApp(d, actor)

	code, out := kycAdminPost(t, app, subject, `{"reason":"misclick"}`)
	if code != fiber.StatusConflict {
		t.Fatalf("resetting a verified contributor returned %d, want 409", code)
	}
	if out["error"] != "already_verified" {
		t.Errorf("error = %v, want already_verified", out["error"])
	}

	var status *string
	_ = d.Pool.QueryRow(t.Context(), `SELECT kyc_status FROM users WHERE id = $1`, subject).Scan(&status)
	if status == nil || *status != "verified" {
		t.Errorf("a verified contributor was changed to %v; a reset must never remove a verification", status)
	}
}

func TestKYCReset_IsAudited(t *testing.T) {
	d := testDB(t)
	actor := leaderboardSuiteUser(t, d.Pool)
	sessionID := "didit-" + uuid.New().String()[:10]
	subject := seedKYCUser(t, d, "rejected", sessionID)
	app := kycAdminApp(d, actor)

	reason := "contributor " + uuid.New().String()[:6] + " asked to retry after a document upload error"
	if code, body := kycAdminPost(t, app, subject, fmt.Sprintf(`{"reason":%q}`, reason)); code != fiber.StatusOK {
		t.Fatalf("reset returned %d: %v", code, body)
	}

	var gotActor uuid.UUID
	var prevStatus, prevSession, gotReason *string
	if err := d.Pool.QueryRow(t.Context(), `
SELECT actor_user_id, previous_status, previous_session_id, reason
FROM kyc_reset_audit WHERE subject_user_id = $1 ORDER BY created_at DESC LIMIT 1
`, subject).Scan(&gotActor, &prevStatus, &prevSession, &gotReason); err != nil {
		t.Fatalf("no audit row written: %v", err)
	}

	if gotActor != actor {
		t.Errorf("actor_user_id = %v, want %v - the audit must say who reset whom", gotActor, actor)
	}
	if prevStatus == nil || *prevStatus != "rejected" {
		t.Errorf("previous_status = %v, want rejected; 'reset to expired' alone loses what was undone", prevStatus)
	}
	// The detached session id is the key to Didit's own record if the reset is
	// ever disputed, so it must survive the detach.
	if prevSession == nil || *prevSession != sessionID {
		t.Errorf("previous_session_id = %v, want %q", prevSession, sessionID)
	}
	if gotReason == nil || *gotReason != reason {
		t.Errorf("reason = %v, want %q", gotReason, reason)
	}

	// And it is readable without database access - which is exactly what the
	// manual UPDATE was not.
	resp, err := app.Test(httptest.NewRequest("GET", "/admin/kyc/"+subject.String()+"/resets", nil), 10000)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	defer resp.Body.Close()
	var hist struct {
		Resets []map[string]any `json:"resets"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&hist)
	if len(hist.Resets) != 1 {
		t.Fatalf("history returned %d rows, want 1", len(hist.Resets))
	}
	if hist.Resets[0]["reason"] != reason {
		t.Errorf("history reason = %v, want %q", hist.Resets[0]["reason"], reason)
	}
}

func TestKYCReset_RecordsEveryResetNotJustTheLast(t *testing.T) {
	d := testDB(t)
	actor := leaderboardSuiteUser(t, d.Pool)
	subject := seedKYCUser(t, d, "rejected", "s3")
	app := kycAdminApp(d, actor)

	// A contributor reset twice is a fact worth being able to see; a column on
	// users could only hold the most recent one.
	if code, _ := kycAdminPost(t, app, subject, `{"reason":"first attempt, blurry documents"}`); code != fiber.StatusOK {
		t.Fatalf("first reset failed")
	}
	_, _ = d.Pool.Exec(t.Context(), `UPDATE users SET kyc_status='rejected' WHERE id=$1`, subject)
	if code, _ := kycAdminPost(t, app, subject, `{"reason":"second attempt, wrong document type"}`); code != fiber.StatusOK {
		t.Fatalf("second reset failed")
	}

	var n int
	if err := d.Pool.QueryRow(t.Context(),
		`SELECT COUNT(*) FROM kyc_reset_audit WHERE subject_user_id = $1`, subject).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("audit rows = %d, want 2 - each reset is its own record", n)
	}
}

func TestKYCReset_UnknownUserIs404(t *testing.T) {
	d := testDB(t)
	actor := leaderboardSuiteUser(t, d.Pool)
	app := kycAdminApp(d, actor)

	code, _ := kycAdminPost(t, app, uuid.New(), `{"reason":"typo in the id"}`)
	if code != fiber.StatusNotFound {
		t.Errorf("unknown user returned %d, want 404", code)
	}
}
