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

const pointsSuiteJWTSecret = "points-suite-test-secret"

func pointsSuiteApp(d *db.DB) *fiber.App {
	app := fiber.New()
	h := handlers.NewPointsHandler(d)
	app.Get("/points/me", auth.RequireAuth(pointsSuiteJWTSecret), h.Me())
	return app
}

func pointsSuiteInsertUser(t *testing.T, d *db.DB) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO users (display_name) VALUES ($1) RETURNING id
`, "points-suite-user-"+uuid.NewString()).Scan(&id)
	if err != nil {
		t.Fatalf("insert points-suite test user: %v", err)
	}
	return id
}

func pointsSuiteToken(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	tok, err := auth.IssueJWT(pointsSuiteJWTSecret, userID, "contributor", "", "", 0)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}
	return tok
}

func TestPointsHandler_RequiresAuth(t *testing.T) {
	app := pointsSuiteApp(&db.DB{})
	resp, _ := notifSuiteDo(t, app, "GET", "/points/me", "", nil)
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
	}
}

func TestPointsHandler_Me_ReflectsLedgerBalance(t *testing.T) {
	d := testDB(t)
	app := pointsSuiteApp(d)
	userID := pointsSuiteInsertUser(t, d)
	token := pointsSuiteToken(t, userID)

	if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO point_ledger (user_id, amount, reason) VALUES ($1, 100, 'referral'), ($1, 500, 'social_follow')
`, userID); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	resp, body := notifSuiteDo(t, app, "GET", "/points/me", token, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", resp.StatusCode, fiber.StatusOK, body)
	}

	var got struct {
		Balance int `json:"balance"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, body)
	}
	if got.Balance != 600 {
		t.Errorf("balance = %d, want 600", got.Balance)
	}
}
