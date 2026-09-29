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
	"github.com/jagadeesh/grainlify/backend/internal/useremail"
)

func emailApp(t *testing.T, d *db.DB, uid uuid.UUID) *fiber.App {
	t.Helper()
	h := NewUserEmailHandler(d)
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(auth.LocalUserID, uid.String())
		return c.Next()
	})
	app.Get("/me/email", h.Get)
	app.Put("/me/email", h.Put)
	app.Delete("/me/email", h.Delete)
	return app
}

func emailCall(t *testing.T, app *fiber.App, method, body string) (int, useremail.Status) {
	t.Helper()
	var r *httptest.ResponseRecorder
	_ = r
	req := httptest.NewRequest(method, "/me/email", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	var out useremail.Status
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// Capture is what runs at sign-in.
func TestUserEmail_StoredAtSignIn(t *testing.T) {
	d := dbtest.DB(t)
	uid := newUser(t, d)

	changed, err := useremail.Capture(context.Background(), d.Pool, uid, "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("capture reported no change on the first write")
	}
	s, err := useremail.Get(context.Background(), d.Pool, uid)
	if err != nil {
		t.Fatal(err)
	}
	if s.Address != "ada@example.com" {
		t.Errorf("address = %q, want ada@example.com", s.Address)
	}
	if s.CapturedAt == "" {
		t.Error("captured_at is empty; the settings screen has nothing to show beside the address")
	}
	if !s.Enabled {
		t.Error("email should default to enabled")
	}
}

// Signing in again after changing it on GitHub must follow the change, and
// signing in with the same address must not churn the row.
func TestUserEmail_UpdatedWhenItChanges(t *testing.T) {
	d := dbtest.DB(t)
	uid := newUser(t, d)
	ctx := context.Background()

	if _, err := useremail.Capture(ctx, d.Pool, uid, "old@example.com"); err != nil {
		t.Fatal(err)
	}
	changed, err := useremail.Capture(ctx, d.Pool, uid, "old@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("re-capturing the same address reported a change")
	}
	if changed, err = useremail.Capture(ctx, d.Pool, uid, "new@example.com"); err != nil || !changed {
		t.Fatalf("capture of a changed address: changed=%v err=%v", changed, err)
	}
	s, _ := useremail.Get(ctx, d.Pool, uid)
	if s.Address != "new@example.com" {
		t.Errorf("address = %q, want new@example.com", s.Address)
	}
}

// An account whose GitHub address is private and unreadable must not blank out
// an address already on file.
func TestUserEmail_EmptyAddressDoesNotEraseTheOneOnFile(t *testing.T) {
	d := dbtest.DB(t)
	uid := newUser(t, d)
	ctx := context.Background()

	if _, err := useremail.Capture(ctx, d.Pool, uid, "ada@example.com"); err != nil {
		t.Fatal(err)
	}
	if changed, err := useremail.Capture(ctx, d.Pool, uid, ""); err != nil || changed {
		t.Fatalf("empty capture: changed=%v err=%v", changed, err)
	}
	s, _ := useremail.Get(ctx, d.Pool, uid)
	if s.Address != "ada@example.com" {
		t.Errorf("address = %q; an unreadable GitHub address erased the stored one", s.Address)
	}
}

// The one that matters: removal has to survive the next sign-in. A remove
// button that undoes itself within a day is worse than no button.
func TestUserEmail_RemovalSurvivesTheNextSignIn(t *testing.T) {
	d := dbtest.DB(t)
	uid := newUser(t, d)
	ctx := context.Background()
	app := emailApp(t, d, uid)

	if _, err := useremail.Capture(ctx, d.Pool, uid, "ada@example.com"); err != nil {
		t.Fatal(err)
	}
	code, s := emailCall(t, app, "DELETE", "")
	if code != 200 {
		t.Fatalf("DELETE status = %d", code)
	}
	if s.Address != "" || !s.Declined {
		t.Fatalf("after removal: address=%q declined=%v", s.Address, s.Declined)
	}

	// Sign in again.
	if changed, err := useremail.Capture(ctx, d.Pool, uid, "ada@example.com"); err != nil || changed {
		t.Fatalf("capture after removal: changed=%v err=%v", changed, err)
	}
	s2, _ := useremail.Get(ctx, d.Pool, uid)
	if s2.Address != "" {
		t.Errorf("address came back as %q after a removal; the remove button is a lie", s2.Address)
	}

	// And the person can change their mind.
	if code, s3 := emailCall(t, app, "PUT", `{"allow":true}`); code != 200 || s3.Declined {
		t.Fatalf("allow: status=%d declined=%v", code, s3.Declined)
	}
	if changed, err := useremail.Capture(ctx, d.Pool, uid, "ada@example.com"); err != nil || !changed {
		t.Fatalf("capture after allow: changed=%v err=%v", changed, err)
	}
}

// The master switch turns email off without losing the address, and without
// touching in-app.
func TestUserEmail_ToggleDoesNotTouchTheAddress(t *testing.T) {
	d := dbtest.DB(t)
	uid := newUser(t, d)
	ctx := context.Background()
	app := emailApp(t, d, uid)

	if _, err := useremail.Capture(ctx, d.Pool, uid, "ada@example.com"); err != nil {
		t.Fatal(err)
	}
	code, s := emailCall(t, app, "PUT", `{"enabled":false}`)
	if code != 200 {
		t.Fatalf("PUT status = %d", code)
	}
	if s.Enabled {
		t.Error("enabled is still true after turning it off")
	}
	if s.Address != "ada@example.com" {
		t.Errorf("address = %q; turning email off must not delete the address", s.Address)
	}
	if s.Declined {
		t.Error("turning email off must not count as declining; the two are different choices")
	}
}

// There is no way to set an address through the API. An address somebody typed
// is unverified, and an unverified address is a way to have Grainlify email a
// stranger.
func TestUserEmail_CannotBeSetThroughTheAPI(t *testing.T) {
	d := dbtest.DB(t)
	uid := newUser(t, d)
	app := emailApp(t, d, uid)

	code, s := emailCall(t, app, "PUT", `{"address":"victim@example.com","enabled":true}`)
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	if s.Address != "" {
		t.Errorf("address = %q; the API accepted an address it was never meant to", s.Address)
	}
}

func TestUserEmail_PutWithNothingToChangeIsRefused(t *testing.T) {
	d := dbtest.DB(t)
	uid := newUser(t, d)
	app := emailApp(t, d, uid)
	if code, _ := emailCall(t, app, "PUT", `{}`); code != 400 {
		t.Errorf("status = %d, want 400", code)
	}
}
