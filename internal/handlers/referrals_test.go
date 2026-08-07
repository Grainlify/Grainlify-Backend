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

const referralSuiteJWTSecret = "referrals-suite-test-secret"

func referralSuiteApp(d *db.DB) *fiber.App {
	app := fiber.New()
	h := handlers.NewReferralsHandler(d)
	app.Get("/referrals/me", auth.RequireAuth(referralSuiteJWTSecret), h.Me())
	return app
}

func referralSuiteInsertUser(t *testing.T, d *db.DB) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO users (display_name) VALUES ($1) RETURNING id
`, "referral-suite-user-"+uuid.NewString()).Scan(&id)
	if err != nil {
		t.Fatalf("insert referral-suite test user: %v", err)
	}
	return id
}

func referralSuiteToken(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	tok, err := auth.IssueJWT(referralSuiteJWTSecret, userID, "contributor", "", "", 0)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}
	return tok
}

func TestReferralsHandler_RequiresAuth(t *testing.T) {
	app := referralSuiteApp(&db.DB{})
	resp, _ := notifSuiteDo(t, app, "GET", "/referrals/me", "", nil)
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
	}
}

func TestReferralsHandler_Me_GeneratesCodeAndZeroStats(t *testing.T) {
	d := testDB(t)
	app := referralSuiteApp(d)
	userID := referralSuiteInsertUser(t, d)
	token := referralSuiteToken(t, userID)

	resp, body := notifSuiteDo(t, app, "GET", "/referrals/me", token, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", resp.StatusCode, fiber.StatusOK, body)
	}

	var got struct {
		Code          string `json:"code"`
		TotalReferred int    `json:"total_referred"`
		Pending       int    `json:"pending"`
		Completed     int    `json:"completed"`
		PointsEarned  int    `json:"points_earned"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal response: %v, body = %s", err, body)
	}
	if got.Code == "" {
		t.Error("code is empty, want a generated referral code")
	}
	if got.TotalReferred != 0 || got.Pending != 0 || got.Completed != 0 || got.PointsEarned != 0 {
		t.Errorf("stats = %+v, want all zero for a user with no referrals", got)
	}
}

func TestReferralsHandler_Me_ReturnsSameCodeOnRepeatedCalls(t *testing.T) {
	d := testDB(t)
	app := referralSuiteApp(d)
	userID := referralSuiteInsertUser(t, d)
	token := referralSuiteToken(t, userID)

	_, body1 := notifSuiteDo(t, app, "GET", "/referrals/me", token, nil)
	_, body2 := notifSuiteDo(t, app, "GET", "/referrals/me", token, nil)

	var r1, r2 struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body1, &r1); err != nil {
		t.Fatalf("unmarshal first response: %v", err)
	}
	if err := json.Unmarshal(body2, &r2); err != nil {
		t.Fatalf("unmarshal second response: %v", err)
	}
	if r1.Code != r2.Code {
		t.Errorf("code changed across calls: first = %q, second = %q", r1.Code, r2.Code)
	}
}

func TestReferralsHandler_Me_StatsReflectSeededReferrals(t *testing.T) {
	d := testDB(t)
	app := referralSuiteApp(d)
	referrerID := referralSuiteInsertUser(t, d)
	token := referralSuiteToken(t, referrerID)

	// One pending, two completed (100 points each) - seeded directly since
	// attach/completion are exercised in referrals_internal_test.go.
	referredA := referralSuiteInsertUser(t, d)
	referredB := referralSuiteInsertUser(t, d)
	referredC := referralSuiteInsertUser(t, d)

	seed := []struct {
		referred uuid.UUID
		status   string
		points   int
	}{
		{referredA, "pending", 0},
		{referredB, "completed", 100},
		{referredC, "completed", 100},
	}
	for _, s := range seed {
		if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO referrals (referrer_user_id, referred_user_id, status, points_awarded, completed_at)
VALUES ($1, $2, $3, $4, CASE WHEN $3 = 'completed' THEN now() ELSE NULL END)
`, referrerID, s.referred, s.status, s.points); err != nil {
			t.Fatalf("seed referrals row: %v", err)
		}
	}

	resp, body := notifSuiteDo(t, app, "GET", "/referrals/me", token, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", resp.StatusCode, fiber.StatusOK, body)
	}

	var got struct {
		TotalReferred int `json:"total_referred"`
		Pending       int `json:"pending"`
		Completed     int `json:"completed"`
		PointsEarned  int `json:"points_earned"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal response: %v, body = %s", err, body)
	}
	if got.TotalReferred != 3 {
		t.Errorf("total_referred = %d, want 3", got.TotalReferred)
	}
	if got.Pending != 1 {
		t.Errorf("pending = %d, want 1", got.Pending)
	}
	if got.Completed != 2 {
		t.Errorf("completed = %d, want 2", got.Completed)
	}
	if got.PointsEarned != 200 {
		t.Errorf("points_earned = %d, want 200", got.PointsEarned)
	}
}
