package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/terms"
)

func termsApp(d *db.DB, uid uuid.UUID) *fiber.App {
	h := NewTermsHandler(d)
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(auth.LocalUserID, uid.String())
		return c.Next()
	})
	app.Get("/me/terms", h.Get)
	app.Post("/me/terms/accept", h.Accept)
	return app
}

func termsCall(t *testing.T, app *fiber.App, method, path, body string) (int, map[string]any) {
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

// Somebody who never accepted on the server - which is everybody, the day
// this ships, whatever their browser remembers - is asked to accept.
func TestTerms_NeverAcceptedNeedsAcceptance(t *testing.T) {
	d := dbtest.DB(t)
	app := termsApp(d, newUser(t, d))

	code, out := termsCall(t, app, "GET", "/me/terms", "")
	if code != 200 {
		t.Fatalf("GET /me/terms = %d %v", code, out)
	}
	if out["needs_acceptance"] != true {
		t.Errorf("needs_acceptance = %v, want true for somebody with no acceptance on record", out["needs_acceptance"])
	}
	if out["current_version"] != terms.Current() {
		t.Errorf("current_version = %v, want %s", out["current_version"], terms.Current())
	}
	if out["accepted_version"] != "" || out["accepted_at"] != nil {
		t.Errorf("accepted = %v at %v, want nothing", out["accepted_version"], out["accepted_at"])
	}
}

func TestTerms_AcceptCurrentIsRecordedWithTimestamp(t *testing.T) {
	d := dbtest.DB(t)
	uid := newUser(t, d)
	app := termsApp(d, uid)

	code, out := termsCall(t, app, "POST", "/me/terms/accept", `{"version":"`+terms.Current()+`"}`)
	if code != 200 {
		t.Fatalf("accept = %d %v", code, out)
	}
	if out["needs_acceptance"] != false || out["accepted_version"] != terms.Current() || out["accepted_at"] == nil {
		t.Errorf("after accepting: %v", out)
	}

	var n int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM terms_acceptances WHERE user_id = $1 AND version = $2`, uid, terms.Current()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}

	// Pressing Accept again (a second tab, a double click) is the same
	// acceptance, not a second one, and keeps the first timestamp.
	first := out["accepted_at"]
	_, again := termsCall(t, app, "POST", "/me/terms/accept", `{"version":"`+terms.Current()+`"}`)
	if again["accepted_at"] != first {
		t.Errorf("accepted_at moved from %v to %v on a repeat acceptance", first, again["accepted_at"])
	}
	_ = d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM terms_acceptances WHERE user_id = $1`, uid).Scan(&n)
	if n != 1 {
		t.Errorf("rows after repeat = %d, want 1", n)
	}
}

// Accepting an older version is recorded as exactly that, and still leaves
// the person needing to accept the current one. This is the stale-tab case:
// a page showing last month's text must not record agreement to this month's.
func TestTerms_AcceptingAnOlderVersionStillNeedsTheCurrentOne(t *testing.T) {
	if len(terms.Versions) < 2 {
		t.Skip("only one version published")
	}
	d := dbtest.DB(t)
	app := termsApp(d, newUser(t, d))
	old := terms.Versions[len(terms.Versions)-2]

	code, out := termsCall(t, app, "POST", "/me/terms/accept", `{"version":"`+old+`"}`)
	if code != 200 {
		t.Fatalf("accept old = %d %v", code, out)
	}
	if out["accepted_version"] != old || out["needs_acceptance"] != true {
		t.Errorf("after accepting %s: %v", old, out)
	}

	// Then the current one: the newer acceptance wins regardless of order.
	_, out = termsCall(t, app, "POST", "/me/terms/accept", `{"version":"`+terms.Current()+`"}`)
	if out["accepted_version"] != terms.Current() || out["needs_acceptance"] != false {
		t.Errorf("after accepting current: %v", out)
	}
	_, out = termsCall(t, app, "POST", "/me/terms/accept", `{"version":"`+old+`"}`)
	if out["accepted_version"] != terms.Current() {
		t.Errorf("an old tab re-accepting %s demoted the record to %v", old, out["accepted_version"])
	}
}

func TestTerms_UnknownVersionIsRefused(t *testing.T) {
	d := dbtest.DB(t)
	uid := newUser(t, d)
	app := termsApp(d, uid)

	for _, body := range []string{`{"version":"2099-01-01"}`, `{"version":""}`, `{}`, `not json`} {
		code, out := termsCall(t, app, "POST", "/me/terms/accept", body)
		if code != 400 {
			t.Errorf("accept %s = %d %v, want 400", body, code, out)
		}
	}
	var n int
	_ = d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM terms_acceptances WHERE user_id = $1`, uid).Scan(&n)
	if n != 0 {
		t.Errorf("%d rows recorded from refused requests", n)
	}
}

// The user comes from the token. A body naming somebody else is ignored, so
// nobody can accept on another person's behalf.
func TestTerms_CannotAcceptForSomebodyElse(t *testing.T) {
	d := dbtest.DB(t)
	caller := newUser(t, d)
	other := newUser(t, d)
	app := termsApp(d, caller)

	termsCall(t, app, "POST", "/me/terms/accept", `{"version":"`+terms.Current()+`","user_id":"`+other.String()+`"}`)
	var n int
	_ = d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM terms_acceptances WHERE user_id = $1`, other).Scan(&n)
	if n != 0 {
		t.Errorf("an acceptance was recorded for the user named in the body")
	}
}
