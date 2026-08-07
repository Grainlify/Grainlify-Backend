package handlers_test

import (
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

// socialFollowSuiteToken issues a token signed with this suite's own JWT
// secret (socialFollowSuiteApp's auth.RequireAuth middleware verifies
// against that secret specifically, not whatever another suite in this
// package happens to use).
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
	app.Post("/social-follow/:platform/submit", auth.RequireAuth(socialFollowSuiteJWTSecret), h.Submit())
	app.Get("/social-follow/me", auth.RequireAuth(socialFollowSuiteJWTSecret), h.Me())

	admin := app.Group("/admin", auth.RequireAuth(socialFollowSuiteJWTSecret))
	admin.Get("/social-follow/submissions", auth.RequireRole("admin"), h.ListSubmissions())
	admin.Post("/social-follow/submissions/:id/approve", auth.RequireRole("admin"), h.Approve())
	admin.Post("/social-follow/submissions/:id/reject", auth.RequireRole("admin"), h.Reject())
	return app
}

func TestSocialFollowHandler_RequiresAuth(t *testing.T) {
	app := socialFollowSuiteApp(&db.DB{})
	resp, _ := notifSuiteDo(t, app, "POST", "/social-follow/github/submit", "", []byte(`{"screenshot":"data:image/png;base64,x"}`))
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
	}
}

func TestSocialFollowHandler_Submit_RejectsInvalidPlatform(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := socialFollowSuiteToken(t, userID, "contributor")

	resp, _ := notifSuiteDo(t, app, "POST", "/social-follow/twitter/submit", token, []byte(`{"screenshot":"data:image/png;base64,x"}`))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want %d for an unsupported platform", resp.StatusCode, fiber.StatusBadRequest)
	}
}

func TestSocialFollowHandler_Submit_RejectsNonImageScreenshot(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := socialFollowSuiteToken(t, userID, "contributor")

	resp, _ := notifSuiteDo(t, app, "POST", "/social-follow/github/submit", token, []byte(`{"screenshot":"not-a-data-url"}`))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want %d for a non-image screenshot value", resp.StatusCode, fiber.StatusBadRequest)
	}
}

func TestSocialFollowHandler_SubmitThenMe_ReflectsPendingStatus(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := socialFollowSuiteToken(t, userID, "contributor")

	resp, body := notifSuiteDo(t, app, "POST", "/social-follow/github/submit", token, []byte(`{"screenshot":"data:image/png;base64,abc123"}`))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("submit status = %d, want %d, body = %s", resp.StatusCode, fiber.StatusOK, body)
	}

	_, meBody := notifSuiteDo(t, app, "GET", "/social-follow/me", token, nil)
	var got struct {
		Platforms []struct {
			Platform string  `json:"platform"`
			Status   *string `json:"status"`
		} `json:"platforms"`
		Completed bool `json:"completed"`
	}
	if err := json.Unmarshal(meBody, &got); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, meBody)
	}
	if got.Completed {
		t.Error("completed = true, want false with only one pending submission")
	}
	found := false
	for _, p := range got.Platforms {
		if p.Platform == "github" {
			found = true
			if p.Status == nil || *p.Status != "pending" {
				t.Errorf("github status = %v, want \"pending\"", p.Status)
			}
		}
	}
	if !found {
		t.Error("github platform missing from /social-follow/me response")
	}
}

func TestSocialFollowHandler_AdminEndpoints_RequireAdminRole(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := socialFollowSuiteToken(t, userID, "contributor")

	resp, _ := notifSuiteDo(t, app, "GET", "/admin/social-follow/submissions", token, nil)
	if resp.StatusCode != fiber.StatusForbidden {
		t.Errorf("list status = %d, want %d for a non-admin caller", resp.StatusCode, fiber.StatusForbidden)
	}

	resp, _ = notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/00000000-0000-0000-0000-000000000000/approve", token, nil)
	if resp.StatusCode != fiber.StatusForbidden {
		t.Errorf("approve status = %d, want %d for a non-admin caller", resp.StatusCode, fiber.StatusForbidden)
	}
}

func TestSocialFollowHandler_ApproveAllThree_CompletesAndAwardsPoints(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := socialFollowSuiteToken(t, userID, "contributor")
	adminID := adminSuiteInsertUser(t, d, "admin")
	adminToken := socialFollowSuiteToken(t, adminID, "admin")

	for _, platform := range []string{"github", "telegram", "linkedin"} {
		resp, body := notifSuiteDo(t, app, "POST", "/social-follow/"+platform+"/submit", token, []byte(`{"screenshot":"data:image/png;base64,abc"}`))
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("submit(%s) status = %d, body = %s", platform, resp.StatusCode, body)
		}
	}

	resp, body := notifSuiteDo(t, app, "GET", "/admin/social-follow/submissions", adminToken, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("list status = %d, body = %s", resp.StatusCode, body)
	}
	var listResp struct {
		Submissions []struct {
			ID       string `json:"id"`
			UserID   string `json:"user_id"`
			Platform string `json:"platform"`
		} `json:"submissions"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("unmarshal list: %v, body = %s", err, body)
	}
	// The admin queue is global (by design), so filter to this test's own
	// user - a shared local Postgres instance can carry pending rows over
	// from other test runs.
	var mine []struct {
		ID       string `json:"id"`
		UserID   string `json:"user_id"`
		Platform string `json:"platform"`
	}
	for _, s := range listResp.Submissions {
		if s.UserID == userID.String() {
			mine = append(mine, s)
		}
	}
	if len(mine) != 3 {
		t.Fatalf("pending submissions for this test's user = %d, want 3", len(mine))
	}

	for _, s := range mine {
		resp, body := notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/"+s.ID+"/approve", adminToken, nil)
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("approve(%s) status = %d, body = %s", s.Platform, resp.StatusCode, body)
		}
	}

	_, meBody := notifSuiteDo(t, app, "GET", "/social-follow/me", token, nil)
	var meResp struct {
		Completed     bool `json:"completed"`
		PointsAwarded int  `json:"points_awarded"`
	}
	if err := json.Unmarshal(meBody, &meResp); err != nil {
		t.Fatalf("unmarshal me: %v, body = %s", err, meBody)
	}
	if !meResp.Completed {
		t.Error("completed = false after approving all three platforms")
	}
	if meResp.PointsAwarded != 500 {
		t.Errorf("points_awarded = %d, want 500", meResp.PointsAwarded)
	}
}

func TestSocialFollowHandler_Reject_SetsStatusAndReason(t *testing.T) {
	d := testDB(t)
	app := socialFollowSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := socialFollowSuiteToken(t, userID, "contributor")
	adminID := adminSuiteInsertUser(t, d, "admin")
	adminToken := socialFollowSuiteToken(t, adminID, "admin")

	notifSuiteDo(t, app, "POST", "/social-follow/github/submit", token, []byte(`{"screenshot":"data:image/png;base64,abc"}`))

	_, listBody := notifSuiteDo(t, app, "GET", "/admin/social-follow/submissions", adminToken, nil)
	var listResp struct {
		Submissions []struct {
			ID     string `json:"id"`
			UserID string `json:"user_id"`
		} `json:"submissions"`
	}
	json.Unmarshal(listBody, &listResp)
	var mySubmissionID string
	for _, s := range listResp.Submissions {
		if s.UserID == userID.String() {
			mySubmissionID = s.ID
		}
	}
	if mySubmissionID == "" {
		t.Fatalf("expected a pending submission for this test's user, found none among %d", len(listResp.Submissions))
	}

	resp, _ := notifSuiteDo(t, app, "POST", "/admin/social-follow/submissions/"+mySubmissionID+"/reject", adminToken, []byte(`{"reason":"screenshot unclear"}`))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("reject status = %d, want %d", resp.StatusCode, fiber.StatusOK)
	}

	_, meBody := notifSuiteDo(t, app, "GET", "/social-follow/me", token, nil)
	var meResp struct {
		Platforms []struct {
			Platform        string  `json:"platform"`
			Status          *string `json:"status"`
			RejectionReason *string `json:"rejection_reason"`
		} `json:"platforms"`
	}
	json.Unmarshal(meBody, &meResp)
	for _, p := range meResp.Platforms {
		if p.Platform == "github" {
			if p.Status == nil || *p.Status != "rejected" {
				t.Errorf("github status = %v, want \"rejected\"", p.Status)
			}
			if p.RejectionReason == nil || *p.RejectionReason != "screenshot unclear" {
				t.Errorf("rejection_reason = %v, want \"screenshot unclear\"", p.RejectionReason)
			}
		}
	}
}
