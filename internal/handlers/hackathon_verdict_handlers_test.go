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

func verdictSuiteApp(d *db.DB) *fiber.App {
	app := fiber.New()
	adminGroup := app.Group("/admin", auth.RequireAuth(hackathonSuiteJWTSecret))
	h := handlers.NewAdminHackathonVerdictsHandler(d)
	adminGroup.Get("/hackathons/:id/verdicts", auth.RequireRole("admin"), h.List())
	adminGroup.Get("/hackathon-verdicts/:id", auth.RequireRole("admin"), h.Get())
	adminGroup.Post("/hackathon-verdicts/:id/override", auth.RequireRole("admin"), h.Override())
	return app
}

// verdictFxSeed hand-seeds a verdict, which is how the review UI is built
// and tested before stages 3-5 exist to produce real ones.
func verdictFxSeed(t *testing.T, d *db.DB, hackathonID, projectID uuid.UUID, prNumber int, over map[string]any) uuid.UUID {
	t.Helper()
	judge := map[string]any{
		"criteria": []map[string]any{
			{"text": "Validates email", "met": true, "evidence": "src/auth/Login.tsx:44-61 adds regex validation"},
		},
		"criteria_met": 1, "criteria_total": 1, "bucket": "substantial", "confidence": "high",
	}
	judgeJSON, _ := json.Marshal(judge)
	stats, _ := json.Marshal(map[string]any{"files_changed": 3, "meaningful_lines": 90})

	needsReview, _ := over["needs_human_review"].(bool)
	prefilter := "passed"
	if s, ok := over["prefilter_status"].(string); ok {
		prefilter = s
	}

	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO hackathon_verdicts
  (hackathon_id, project_id, pr_number, github_login, prefilter_status, diff_stats,
   judge_bucket, judge_confidence, judge_payload, judge_model, needs_human_review)
VALUES ($1,$2,$3,'octocat',$4,$5,'substantial','high',$6,'claude-sonnet-5',$7)
RETURNING id`, hackathonID, projectID, prNumber, prefilter, stats, judgeJSON, needsReview).Scan(&id)
	if err != nil {
		t.Fatalf("verdictFxSeed: %v", err)
	}
	return id
}

func TestVerdicts_RequiresAdmin(t *testing.T) {
	d := testDB(t)
	app := verdictSuiteApp(d)
	hackathonID, _, _ := asmtFxLiveIssue(t, d)
	contributor := adminSuiteInsertUser(t, d, "contributor")

	resp, _ := notifSuiteDo(t, app, "GET", "/admin/hackathons/"+hackathonID.String()+"/verdicts",
		hackathonSuiteToken(t, contributor, "contributor"), nil)
	if resp.StatusCode != fiber.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestVerdicts_ListReportsShadowModeAndFilters(t *testing.T) {
	d := testDB(t)
	app := verdictSuiteApp(d)
	hackathonID, projectID, _ := asmtFxLiveIssue(t, d)
	admin := adminSuiteInsertUser(t, d, "admin")
	tok := hackathonSuiteToken(t, admin, "admin")

	verdictFxSeed(t, d, hackathonID, projectID, 1, map[string]any{"needs_human_review": true})
	verdictFxSeed(t, d, hackathonID, projectID, 2, nil)

	resp, body := notifSuiteDo(t, app, "GET", "/admin/hackathons/"+hackathonID.String()+"/verdicts", tok, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d (%s)", resp.StatusCode, body)
	}
	var all struct {
		Verdicts   []map[string]any `json:"verdicts"`
		ShadowMode bool             `json:"shadow_mode"`
	}
	if err := json.Unmarshal(body, &all); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(all.Verdicts) != 2 {
		t.Errorf("verdicts = %d, want 2", len(all.Verdicts))
	}
	// Shadow mode is the default, and the UI needs to be able to say so.
	if !all.ShadowMode {
		t.Error("shadow_mode = false by default")
	}

	resp, body = notifSuiteDo(t, app, "GET",
		"/admin/hackathons/"+hackathonID.String()+"/verdicts?status=needs_review", tok, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var filtered struct {
		Verdicts []map[string]any `json:"verdicts"`
	}
	_ = json.Unmarshal(body, &filtered)
	if len(filtered.Verdicts) != 1 {
		t.Errorf("needs_review verdicts = %d, want 1", len(filtered.Verdicts))
	}
}

// §5.7: an override with no written reason is a calibration example with no
// label. The API refuses it rather than storing a useless one.
func TestVerdicts_OverrideRequiresBucketAndReason(t *testing.T) {
	d := testDB(t)
	app := verdictSuiteApp(d)
	hackathonID, projectID, _ := asmtFxLiveIssue(t, d)
	admin := adminSuiteInsertUser(t, d, "admin")
	tok := hackathonSuiteToken(t, admin, "admin")
	verdictID := verdictFxSeed(t, d, hackathonID, projectID, 3, nil)
	path := "/admin/hackathon-verdicts/" + verdictID.String() + "/override"

	resp, body := notifSuiteDo(t, app, "POST", path, tok, []byte(`{"bucket":"accepted","reason":"   "}`))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("blank reason status = %d, want 400", resp.StatusCode)
	}
	var errBody struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &errBody)
	if errBody.Error != "reason_required" {
		t.Errorf("error = %q, want reason_required", errBody.Error)
	}

	resp, _ = notifSuiteDo(t, app, "POST", path, tok, []byte(`{"bucket":"not-a-bucket","reason":"why"}`))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Errorf("invalid bucket status = %d, want 400", resp.StatusCode)
	}
}

func TestVerdicts_OverrideRecordsWhoWhatAndWhy(t *testing.T) {
	d := testDB(t)
	app := verdictSuiteApp(d)
	hackathonID, projectID, _ := asmtFxLiveIssue(t, d)
	admin := adminSuiteInsertUser(t, d, "admin")
	tok := hackathonSuiteToken(t, admin, "admin")
	verdictID := verdictFxSeed(t, d, hackathonID, projectID, 4, map[string]any{"needs_human_review": true})

	resp, body := notifSuiteDo(t, app, "POST",
		"/admin/hackathon-verdicts/"+verdictID.String()+"/override", tok,
		[]byte(`{"bucket":"accepted","reason":"Routine change; the judge over-read the scope."}`))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d (%s)", resp.StatusCode, body)
	}

	var finalBucket, finalSource, reason string
	var overriddenBy uuid.UUID
	var needsReview bool
	if err := d.Pool.QueryRow(context.Background(), `
SELECT final_bucket, final_source, override_reason, overridden_by, needs_human_review
FROM hackathon_verdicts WHERE id = $1`, verdictID).
		Scan(&finalBucket, &finalSource, &reason, &overriddenBy, &needsReview); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if finalBucket != "accepted" || finalSource != "human_override" {
		t.Errorf("got (%s, %s), want (accepted, human_override)", finalBucket, finalSource)
	}
	if reason == "" {
		t.Error("reason not persisted - this is what the calibration set needs")
	}
	if overriddenBy != admin {
		t.Errorf("overridden_by = %s, want the acting admin", overriddenBy)
	}
	// A human decision settles the question.
	if needsReview {
		t.Error("still flagged for review after a human decided")
	}
}

// A verdict row existing must not be enough to release money.
func TestVerdicts_OverrideDoesNotWritePayout(t *testing.T) {
	d := testDB(t)
	app := verdictSuiteApp(d)
	hackathonID, projectID, _ := asmtFxLiveIssue(t, d)
	admin := adminSuiteInsertUser(t, d, "admin")
	verdictID := verdictFxSeed(t, d, hackathonID, projectID, 5, nil)

	notifSuiteDo(t, app, "POST", "/admin/hackathon-verdicts/"+verdictID.String()+"/override",
		hackathonSuiteToken(t, admin, "admin"),
		[]byte(`{"bucket":"exceptional","reason":"Unblocked three other issues."}`))

	var units *int
	var amount *string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT units, payout_amount::text FROM hackathon_verdicts WHERE id = $1`, verdictID).Scan(&units, &amount); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if units != nil || amount != nil {
		t.Errorf("an override wrote payout data (units=%v amount=%v) - releasing money must be a separate explicit action", units, amount)
	}
	var runs int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM hackathon_payout_runs WHERE hackathon_id = $1`, hackathonID).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Errorf("payout runs = %d, want 0", runs)
	}
}
