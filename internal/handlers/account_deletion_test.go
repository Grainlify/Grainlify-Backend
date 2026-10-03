package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

func deletionApp(d *db.DB, uid uuid.UUID) *fiber.App {
	h := NewAccountDeletionHandler(d, nil)
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(auth.LocalUserID, uid.String())
		return c.Next()
	})
	app.Get("/me/deletion", h.Get)
	app.Post("/me/deletion", h.Request)
	app.Post("/me/deletion/cancel", h.Cancel)
	return app
}

func deletionCall(t *testing.T, app *fiber.App, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	out := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func deletionCleanup(t *testing.T, d *db.DB, ids ...uuid.UUID) {
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = d.Pool.Exec(context.Background(), `DELETE FROM account_deletion_requests WHERE user_id = $1`, id)
		}
	})
}

const confirmBody = `{"confirm":"delete my account"}`

func TestDeletion_RequiresTheConfirmationPhrase(t *testing.T) {
	d := dbtest.DB(t)
	uid := newUser(t, d)
	deletionCleanup(t, d, uid)
	app := deletionApp(d, uid)

	for _, body := range []string{`{}`, `{"confirm":"yes"}`, `not json`} {
		if code, out := deletionCall(t, app, "POST", "/me/deletion", body); code != 400 {
			t.Errorf("POST %s = %d %v, want 400", body, code, out)
		}
	}
	var n int
	_ = d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM account_deletion_requests WHERE user_id = $1`, uid).Scan(&n)
	if n != 0 {
		t.Errorf("%d requests recorded without confirmation", n)
	}
}

func TestDeletion_RequestIsRecordedForTheCallerOnly(t *testing.T) {
	d := dbtest.DB(t)
	caller := newUser(t, d)
	other := newUser(t, d)
	deletionCleanup(t, d, caller, other)
	app := deletionApp(d, caller)

	// A user id in the body is ignored: the account is the token's.
	code, out := deletionCall(t, app, "POST", "/me/deletion",
		`{"confirm":"Delete my account","user_id":"`+other.String()+`"}`)
	if code != 201 || out["created"] != true {
		t.Fatalf("POST = %d %v, want 201 created", code, out)
	}
	req, _ := out["request"].(map[string]any)
	if req["status"] != "scheduled" {
		t.Errorf("status = %v", req["status"])
	}
	after, _ := time.Parse(time.RFC3339Nano, req["execute_after"].(string))
	if d := time.Until(after); d < 6*24*time.Hour || d > 8*24*time.Hour {
		t.Errorf("execute_after is %s away, want about seven days", d)
	}

	var mine, theirs int
	_ = d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM account_deletion_requests WHERE user_id = $1`, caller).Scan(&mine)
	_ = d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM account_deletion_requests WHERE user_id = $1`, other).Scan(&theirs)
	if mine != 1 || theirs != 0 {
		t.Errorf("requests: caller %d, other %d; want 1 and 0", mine, theirs)
	}

	// Pressing again returns the same request.
	code, out = deletionCall(t, app, "POST", "/me/deletion", confirmBody)
	if code != 200 || out["created"] != false {
		t.Errorf("second POST = %d %v, want 200 not created", code, out)
	}
}

// Nobody can cancel somebody else's request: the other person's cancel
// finds nothing of their own, and the caller's request stays scheduled.
func TestDeletion_CancelOnlyTouchesTheCallersRequest(t *testing.T) {
	d := dbtest.DB(t)
	owner := newUser(t, d)
	other := newUser(t, d)
	deletionCleanup(t, d, owner, other)

	if code, _ := deletionCall(t, deletionApp(d, owner), "POST", "/me/deletion", confirmBody); code != 201 {
		t.Fatalf("request = %d", code)
	}
	if code, out := deletionCall(t, deletionApp(d, other), "POST", "/me/deletion/cancel", `{"user_id":"`+owner.String()+`"}`); code != 409 {
		t.Errorf("other's cancel = %d %v, want 409", code, out)
	}
	_, got := deletionCall(t, deletionApp(d, owner), "GET", "/me/deletion", "")
	req, _ := got["request"].(map[string]any)
	if req["status"] != "scheduled" {
		t.Errorf("owner's request = %v after somebody else cancelled", req["status"])
	}

	// The owner can cancel during the grace period.
	code, out := deletionCall(t, deletionApp(d, owner), "POST", "/me/deletion/cancel", "")
	req, _ = out["request"].(map[string]any)
	if code != 200 || req["status"] != "cancelled" {
		t.Errorf("owner's cancel = %d %v", code, out)
	}
}

// The confirmation screen gets the whole policy and any money in flight
// before the person confirms.
func TestDeletion_GetDescribesWhatHappens(t *testing.T) {
	d := dbtest.DB(t)
	uid := newUser(t, d)
	code, out := deletionCall(t, deletionApp(d, uid), "GET", "/me/deletion", "")
	if code != 200 {
		t.Fatalf("GET = %d %v", code, out)
	}
	if out["request"] != nil {
		t.Errorf("request = %v, want null before any request", out["request"])
	}
	pol, _ := out["policy"].(map[string]any)
	erased, _ := pol["erased"].([]any)
	retained, _ := pol["retained"].([]any)
	if pol["grace_days"] != float64(7) || pol["max_hold_days"] != float64(30) || len(erased) == 0 || len(retained) == 0 {
		t.Errorf("policy = %v", pol)
	}
	if mif, ok := out["money_in_flight"].([]any); !ok || len(mif) != 0 {
		t.Errorf("money_in_flight = %v, want []", out["money_in_flight"])
	}
	if out["confirm_phrase"] != DeletionConfirmPhrase {
		t.Errorf("confirm_phrase = %v", out["confirm_phrase"])
	}
}

func TestRefuseErasedAccounts(t *testing.T) {
	d := dbtest.DB(t)
	const secret = "test-secret"
	live := newUser(t, d)
	erased := newUser(t, d)
	if _, err := d.Pool.Exec(context.Background(), `UPDATE users SET erased_at = now() WHERE id = $1`, erased); err != nil {
		t.Fatal(err)
	}

	app := fiber.New()
	app.Use(RefuseErasedAccounts(secret, d))
	app.Get("/x", func(c *fiber.Ctx) error { return c.SendString("ok") })

	call := func(authz string) int {
		req := httptest.NewRequest("GET", "/x", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		res, err := app.Test(req, -1)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode
	}
	tok := func(id uuid.UUID) string {
		s, err := auth.IssueJWT(secret, id, "contributor", "", "", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return "Bearer " + s
	}

	if got := call(tok(erased)); got != 401 {
		t.Errorf("erased account's unexpired token = %d, want 401", got)
	}
	if got := call(tok(live)); got != 200 {
		t.Errorf("live account = %d, want 200", got)
	}
	// Not this middleware's business: RequireAuth and public routes decide.
	for _, h := range []string{"", "Bearer not-a-jwt", "Basic abc", tok(uuid.New())} {
		if got := call(h); got != 200 {
			t.Errorf("Authorization %q = %d, want passed through", h, got)
		}
	}
}
