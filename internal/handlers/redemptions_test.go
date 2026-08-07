package handlers_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stellar/go/strkey"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

const redemptionsSuiteJWTSecret = "redemptions-suite-test-secret"

func redemptionsSuiteApp(d *db.DB) *fiber.App {
	app := fiber.New()
	h := handlers.NewRedemptionsHandler(d, nil)
	app.Post("/redemptions", auth.RequireAuth(redemptionsSuiteJWTSecret), h.Create())
	app.Get("/redemptions/me", auth.RequireAuth(redemptionsSuiteJWTSecret), h.Mine())

	admin := app.Group("/admin", auth.RequireAuth(redemptionsSuiteJWTSecret))
	admin.Get("/redemptions", auth.RequireRole("admin"), h.ListAdmin())
	admin.Post("/redemptions/:id/mark-paid", auth.RequireRole("admin"), h.MarkPaid())
	admin.Post("/redemptions/:id/reject", auth.RequireRole("admin"), h.Reject())
	return app
}

// redemptionsSuiteToken issues a token signed with this suite's own JWT
// secret (redemptionsSuiteApp's auth.RequireAuth middleware verifies
// against that secret specifically, not whatever another suite in this
// package happens to use).
func redemptionsSuiteToken(t *testing.T, userID uuid.UUID, role string) string {
	t.Helper()
	tok, err := auth.IssueJWT(redemptionsSuiteJWTSecret, userID, role, "", "", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}
	return tok
}

// redemptionsSuiteWallet returns a real, checksum-valid Stellar account
// address. StrKey validity depends only on the version byte + payload +
// checksum being internally consistent, not on the payload being an actual
// on-curve ed25519 key, so 32 random bytes through strkey.Encode is enough.
func redemptionsSuiteWallet(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	addr, err := strkey.Encode(strkey.VersionByteAccountID, raw)
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	return addr
}

func redemptionsSuiteGrantPoints(t *testing.T, d *db.DB, userID uuid.UUID, amount int) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO point_ledger (user_id, amount, reason) VALUES ($1, $2, 'referral')
`, userID, amount); err != nil {
		t.Fatalf("grant points: %v", err)
	}
}

func TestRedemptionsHandler_RequiresAuth(t *testing.T) {
	app := redemptionsSuiteApp(&db.DB{})
	resp, _ := notifSuiteDo(t, app, "POST", "/redemptions", "", []byte(`{"points":100,"stellar_wallet_address":"x"}`))
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
	}
}

func TestRedemptionsHandler_Create_RejectsBelowMinimum(t *testing.T) {
	d := testDB(t)
	app := redemptionsSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := redemptionsSuiteToken(t, userID, "contributor")
	redemptionsSuiteGrantPoints(t, d, userID, 1000)

	wallet := redemptionsSuiteWallet(t)
	resp, _ := notifSuiteDo(t, app, "POST", "/redemptions", token, []byte(`{"points":10,"stellar_wallet_address":"`+wallet+`"}`))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want %d for a below-minimum request", resp.StatusCode, fiber.StatusBadRequest)
	}
}

func TestRedemptionsHandler_Create_RejectsInvalidWalletAddress(t *testing.T) {
	d := testDB(t)
	app := redemptionsSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := redemptionsSuiteToken(t, userID, "contributor")
	redemptionsSuiteGrantPoints(t, d, userID, 1000)

	resp, _ := notifSuiteDo(t, app, "POST", "/redemptions", token, []byte(`{"points":200,"stellar_wallet_address":"not-a-real-address"}`))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want %d for an invalid wallet address", resp.StatusCode, fiber.StatusBadRequest)
	}
}

func TestRedemptionsHandler_Create_RejectsInsufficientBalance(t *testing.T) {
	d := testDB(t)
	app := redemptionsSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := redemptionsSuiteToken(t, userID, "contributor")
	redemptionsSuiteGrantPoints(t, d, userID, 100) // exactly the minimum, but we'll ask for more

	wallet := redemptionsSuiteWallet(t)
	resp, _ := notifSuiteDo(t, app, "POST", "/redemptions", token, []byte(`{"points":200,"stellar_wallet_address":"`+wallet+`"}`))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want %d when requesting more points than the balance", resp.StatusCode, fiber.StatusBadRequest)
	}
}

func TestRedemptionsHandler_Create_DeductsBalanceAndRecordsRequest(t *testing.T) {
	d := testDB(t)
	app := redemptionsSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := redemptionsSuiteToken(t, userID, "contributor")
	redemptionsSuiteGrantPoints(t, d, userID, 1000)

	wallet := redemptionsSuiteWallet(t)
	resp, body := notifSuiteDo(t, app, "POST", "/redemptions", token, []byte(`{"points":300,"stellar_wallet_address":"`+wallet+`"}`))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", resp.StatusCode, fiber.StatusOK, body)
	}

	pointsApp := pointsSuiteApp(d)
	_, balBody := notifSuiteDo(t, pointsApp, "GET", "/points/me", pointsSuiteToken(t, userID), nil)
	var bal struct {
		Balance int `json:"balance"`
	}
	json.Unmarshal(balBody, &bal)
	if bal.Balance != 700 {
		t.Errorf("balance after redeeming 300 of 1000 = %d, want 700", bal.Balance)
	}

	_, mineBody := notifSuiteDo(t, app, "GET", "/redemptions/me", token, nil)
	var mine struct {
		Redemptions []struct {
			PointsSpent int    `json:"points_spent"`
			Status      string `json:"status"`
		} `json:"redemptions"`
	}
	if err := json.Unmarshal(mineBody, &mine); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, mineBody)
	}
	if len(mine.Redemptions) != 1 || mine.Redemptions[0].PointsSpent != 300 || mine.Redemptions[0].Status != "pending" {
		t.Errorf("redemptions/me = %+v, want one pending 300-point redemption", mine.Redemptions)
	}
}

func TestRedemptionsHandler_AdminEndpoints_RequireAdminRole(t *testing.T) {
	d := testDB(t)
	app := redemptionsSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := redemptionsSuiteToken(t, userID, "contributor")

	resp, _ := notifSuiteDo(t, app, "GET", "/admin/redemptions", token, nil)
	if resp.StatusCode != fiber.StatusForbidden {
		t.Errorf("list status = %d, want %d for a non-admin caller", resp.StatusCode, fiber.StatusForbidden)
	}
}

func TestRedemptionsHandler_Reject_RefundsPoints(t *testing.T) {
	d := testDB(t)
	app := redemptionsSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := redemptionsSuiteToken(t, userID, "contributor")
	adminID := adminSuiteInsertUser(t, d, "admin")
	adminToken := redemptionsSuiteToken(t, adminID, "admin")
	redemptionsSuiteGrantPoints(t, d, userID, 1000)

	wallet := redemptionsSuiteWallet(t)
	_, createBody := notifSuiteDo(t, app, "POST", "/redemptions", token, []byte(`{"points":300,"stellar_wallet_address":"`+wallet+`"}`))
	var created struct {
		ID string `json:"id"`
	}
	json.Unmarshal(createBody, &created)

	resp, body := notifSuiteDo(t, app, "POST", "/admin/redemptions/"+created.ID+"/reject", adminToken, []byte(`{"reason":"suspicious"}`))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("reject status = %d, want %d, body = %s", resp.StatusCode, fiber.StatusOK, body)
	}

	pointsApp := pointsSuiteApp(d)
	_, balBody := notifSuiteDo(t, pointsApp, "GET", "/points/me", pointsSuiteToken(t, userID), nil)
	var bal struct {
		Balance int `json:"balance"`
	}
	json.Unmarshal(balBody, &bal)
	if bal.Balance != 1000 {
		t.Errorf("balance after rejected redemption = %d, want 1000 (fully refunded)", bal.Balance)
	}
}

func TestRedemptionsHandler_MarkPaid_TransitionsStatus(t *testing.T) {
	d := testDB(t)
	app := redemptionsSuiteApp(d)
	userID := adminSuiteInsertUser(t, d, "contributor")
	token := redemptionsSuiteToken(t, userID, "contributor")
	adminID := adminSuiteInsertUser(t, d, "admin")
	adminToken := redemptionsSuiteToken(t, adminID, "admin")
	redemptionsSuiteGrantPoints(t, d, userID, 1000)

	wallet := redemptionsSuiteWallet(t)
	_, createBody := notifSuiteDo(t, app, "POST", "/redemptions", token, []byte(`{"points":300,"stellar_wallet_address":"`+wallet+`"}`))
	var created struct {
		ID string `json:"id"`
	}
	json.Unmarshal(createBody, &created)

	resp, _ := notifSuiteDo(t, app, "POST", "/admin/redemptions/"+created.ID+"/mark-paid", adminToken, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("mark-paid status = %d, want %d", resp.StatusCode, fiber.StatusOK)
	}

	_, mineBody := notifSuiteDo(t, app, "GET", "/redemptions/me", token, nil)
	var mine struct {
		Redemptions []struct {
			Status string `json:"status"`
		} `json:"redemptions"`
	}
	json.Unmarshal(mineBody, &mine)
	if len(mine.Redemptions) != 1 || mine.Redemptions[0].Status != "paid" {
		t.Errorf("redemptions/me = %+v, want status \"paid\"", mine.Redemptions)
	}
}
