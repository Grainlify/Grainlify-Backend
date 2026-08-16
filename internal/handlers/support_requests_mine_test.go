package handlers_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// A contributor seeing their own report history.
//
// It exists because the only way to find out what you already sent is to ask
// in Telegram. The property that matters most is scoping: this returns the
// caller's reports and nobody else's, and the caller is taken from the
// verified token rather than a parameter.

func mineSuiteApp(d *db.DB) *fiber.App {
	app := fiber.New()
	h := handlers.NewSupportRequestsHandler(config.Config{JWTSecret: adminSuiteJWTSecret}, d)
	app.Get("/support-requests/mine", auth.RequireAuth(adminSuiteJWTSecret), h.Mine())
	return app
}

func mineSuiteSeed(t *testing.T, d *db.DB, userID *uuid.UUID, category, message string, delivered bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	q := `INSERT INTO support_requests (id, user_id, category, message, page_url, telegram_delivered_at)
	      VALUES ($1, $2, $3, $4, 'https://grainlify.com/dashboard', CASE WHEN $5 THEN now() ELSE NULL END)`
	if _, err := d.Pool.Exec(context.Background(), q, id, userID, category, message, delivered); err != nil {
		t.Fatalf("seed support request: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Pool.Exec(context.Background(), `DELETE FROM support_requests WHERE id = $1`, id) })
	return id
}

func mineSuiteList(t *testing.T, app *fiber.App, token string) []map[string]any {
	t.Helper()
	status, body := adminSuiteDo(t, app, "GET", "/support-requests/mine", token, nil)
	if status != fiber.StatusOK {
		t.Fatalf("GET /support-requests/mine: status = %d, want 200 (body=%v)", status, body)
	}
	raw, err := json.Marshal(body["support_requests"])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// The scoping property. A history endpoint that leaks is worse than no
// history endpoint: support messages describe verification problems, payment
// details and things people have got wrong.
func TestSupportMine_ReturnsOnlyTheCallersOwnReports(t *testing.T) {
	d := testDB(t)
	app := mineSuiteApp(d)

	me := adminSuiteInsertUser(t, d, "contributor")
	someoneElse := adminSuiteInsertUser(t, d, "contributor")

	mineID := mineSuiteSeed(t, d, &me, "bug", "my own report "+uuid.NewString()[:6], true)
	theirID := mineSuiteSeed(t, d, &someoneElse, "kyc", "somebody else's verification question", true)

	got := mineSuiteList(t, app, adminSuiteToken(t, me, "contributor"))

	var sawMine, sawTheirs bool
	for _, r := range got {
		if r["id"] == mineID.String() {
			sawMine = true
		}
		if r["id"] == theirID.String() {
			sawTheirs = true
		}
	}
	if !sawMine {
		t.Error("the caller's own report was missing from their history")
	}
	if sawTheirs {
		t.Fatal("another user's support request was returned - support messages describe " +
			"verification problems and payment details, and this endpoint is scoped by the token")
	}
}

// Anonymous reports have no owner, so they belong to nobody's history - and
// must not fall into the first person who asks.
func TestSupportMine_AnonymousReportsBelongToNobody(t *testing.T) {
	d := testDB(t)
	app := mineSuiteApp(d)

	me := adminSuiteInsertUser(t, d, "contributor")
	anonID := mineSuiteSeed(t, d, nil, "help", "sent while signed out "+uuid.NewString()[:6], true)

	for _, r := range mineSuiteList(t, app, adminSuiteToken(t, me, "contributor")) {
		if r["id"] == anonID.String() {
			t.Fatal("an anonymous report was attributed to a signed-in user")
		}
	}
}

// Status is honest about what the system actually knows.
func TestSupportMine_StatusDoesNotClaimSomebodyIsWorkingOnIt(t *testing.T) {
	d := testDB(t)
	app := mineSuiteApp(d)

	me := adminSuiteInsertUser(t, d, "contributor")
	delivered := mineSuiteSeed(t, d, &me, "bug", "delivered "+uuid.NewString()[:6], true)
	undelivered := mineSuiteSeed(t, d, &me, "bug", "not delivered "+uuid.NewString()[:6], false)

	byID := map[string]map[string]any{}
	for _, r := range mineSuiteList(t, app, adminSuiteToken(t, me, "contributor")) {
		byID[r["id"].(string)] = r
	}

	for _, id := range []uuid.UUID{delivered, undelivered} {
		r, ok := byID[id.String()]
		if !ok {
			t.Fatalf("report %s missing from history", id)
		}
		// Nothing in the system records that a report was read or answered, so
		// no value here may imply that it was. "in progress" from a timestamp
		// would be a claim the database cannot support.
		if s, _ := r["status"].(string); s != "received" {
			t.Errorf("status = %q; the only state the system actually knows is that the report arrived", s)
		}
	}

	// Delivered and undelivered are distinguishable: the row is written before
	// any sink is called, so "we have it" and "somebody was told" come apart.
	if byID[delivered.String()]["delivered_to_team"] != true {
		t.Error("a delivered report was not marked as delivered")
	}
	if byID[undelivered.String()]["delivered_to_team"] != false {
		t.Error("a report no sink accepted was reported as delivered to the team")
	}
}

// An empty history must serialise as [] and not null, or the page renders an
// error instead of an empty state.
func TestSupportMine_EmptyHistoryIsAnEmptyList(t *testing.T) {
	d := testDB(t)
	app := mineSuiteApp(d)
	fresh := adminSuiteInsertUser(t, d, "contributor")

	status, body := adminSuiteDo(t, app, "GET", "/support-requests/mine", adminSuiteToken(t, fresh, "contributor"), nil)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	list, ok := body["support_requests"].([]any)
	if !ok {
		t.Fatalf("support_requests is %T, want an array even when empty", body["support_requests"])
	}
	if len(list) != 0 {
		t.Errorf("a brand-new user has %d reports", len(list))
	}
}

// Unauthenticated callers get 401 rather than somebody's history.
func TestSupportMine_RequiresAToken(t *testing.T) {
	d := testDB(t)
	app := mineSuiteApp(d)
	status, _ := adminSuiteDo(t, app, "GET", "/support-requests/mine", "", nil)
	if status != fiber.StatusUnauthorized {
		t.Errorf("status = %d without a token, want 401", status)
	}
}
