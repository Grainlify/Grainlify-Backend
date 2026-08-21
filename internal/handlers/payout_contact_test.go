package handlers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// The column has exactly one reader, and it is the one whose name states the
// purpose.
//
// This is the guard that stops payout_contact_email becoming a general mailing
// list. A comment asking future code not to use it for anything else would be a
// poor control - see docs/VERIFICATION-TRAPS.md on comments as guards, which
// records two instances of an author walking past their own warning within
// hours. So the rule is checked instead.
//
// Sources are globbed rather than listed: a file nobody adds to a slice is a
// file the check cannot see, which is a separate entry in the same file.
func TestPayoutContactColumnHasOneReader(t *testing.T) {
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var sources int
	var readers []string
	for _, name := range entries {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		sources++
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.Contains(line, "payout_contact_email") {
				continue
			}
			// Comments explain; they do not read the column.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			readers = append(readers, name+": "+strings.TrimSpace(line))
		}
	}
	// A glob returning nothing finds no readers, which is indistinguishable
	// from a clean package.
	if sources < 10 {
		t.Fatalf("globbed %d sources; the scan is not running where it thinks it is", sources)
	}

	// Three: the SELECT in payoutContactFor, and the two UPDATEs in Put that
	// set and clear it. Everything else must go through payoutContactFor.
	const wantReaders = 3
	if len(readers) != wantReaders {
		t.Errorf("payout_contact_email is touched in %d places, want %d — a new one means either "+
			"a reader that bypasses payoutContactFor, or a purpose this column was not collected for:\n  %s",
			len(readers), wantReaders, strings.Join(readers, "\n  "))
	}
	for _, r := range readers {
		if !strings.HasPrefix(r, "payout_contact.go:") {
			t.Errorf("payout_contact_email is touched outside payout_contact.go: %s", r)
		}
	}
}

// --- behaviour: optional means optional ---

func payoutContactApp(d *db.DB, uid uuid.UUID) *fiber.App {
	app := fiber.New()
	h := NewPayoutContactHandler(d)
	stub := func(c *fiber.Ctx) error { c.Locals("user_id", uid.String()); return c.Next() }
	app.Get("/me/payout-contact", stub, h.Get)
	app.Put("/me/payout-contact", stub, h.Put)
	return app
}

func pcUser(t *testing.T, d *db.DB) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO users (role, display_name) VALUES ('contributor',$1) RETURNING id`,
		"pc-"+uuid.NewString()).Scan(&id); err != nil {
		t.Fatalf("pcUser: %v", err)
	}
	return id
}

func pcDo(t *testing.T, app *fiber.App, method, body string) (int, string) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/me/payout-contact", nil)
	} else {
		r = httptest.NewRequest(method, "/me/payout-contact", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(r, -1)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Declining changes nothing. Asserted rather than read through, because
// "optional" is exactly the property that degrades quietly - and this lands on
// the screen every founding contributor is about to use for the first time.
func TestPayoutContact_DecliningChangesNothing(t *testing.T) {
	d := dbtest.DB(t)
	uid := pcUser(t, d)
	app := payoutContactApp(d, uid)

	// Never set: reads as empty, not as an error and not as a prompt.
	status, body := pcDo(t, app, "GET", "")
	if status != 200 || !strings.Contains(body, `"email":""`) {
		t.Fatalf("GET with nothing stored = %d %s, want 200 and an empty string", status, body)
	}

	// The stored row is untouched by having been asked. Nothing is written on
	// read, and no default is materialised.
	var stored *string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT payout_contact_email FROM users WHERE id=$1`, uid).Scan(&stored); err != nil {
		t.Fatalf("read column: %v", err)
	}
	if stored != nil {
		t.Errorf("declining wrote %q to the column", *stored)
	}
}

// Removal is self-serve, immediate, and cannot fail for a reason saving cannot.
// It is the whole removal story for this column - there is no account-deletion
// path to fall back on (#532) - so it has to work on the first attempt.
func TestPayoutContact_RemovalIsSelfServeAndUnconditional(t *testing.T) {
	d := dbtest.DB(t)
	uid := pcUser(t, d)
	app := payoutContactApp(d, uid)

	if status, body := pcDo(t, app, "PUT", `{"email":"someone@example.com"}`); status != 200 {
		t.Fatalf("save = %d %s", status, body)
	}
	if status, body := pcDo(t, app, "PUT", `{"email":""}`); status != 200 {
		t.Fatalf("clear = %d %s, want 200 - removal must not fail for a reason saving cannot", status, body)
	}

	var stored *string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT payout_contact_email FROM users WHERE id=$1`, uid).Scan(&stored); err != nil {
		t.Fatalf("read column: %v", err)
	}
	if stored != nil {
		t.Errorf("still stored after removal: %q", *stored)
	}

	// Clearing again is not an error. Somebody who is not sure whether it
	// worked must be able to press it twice.
	if status, _ := pcDo(t, app, "PUT", `{"email":""}`); status != 200 {
		t.Errorf("second clear = %d, want 200", status)
	}
}

// Shape only. An address we could never send to is worth refusing now rather
// than as a bounce nobody sees; deliverability is not probed, because a probe
// is a message they did not ask for.
func TestPayoutContact_RefusesAnAddressItCouldNotSendTo(t *testing.T) {
	d := dbtest.DB(t)
	uid := pcUser(t, d)
	app := payoutContactApp(d, uid)

	for _, bad := range []string{"not-an-address", "@example.com", "someone@", "someone@localhost"} {
		status, body := pcDo(t, app, "PUT", `{"email":"`+bad+`"}`)
		if status != 400 {
			t.Errorf("%q accepted with %d %s", bad, status, body)
		}
	}
	for _, ok := range []string{"someone@example.com", "a.b+tag@sub.example.co.uk"} {
		if status, body := pcDo(t, app, "PUT", `{"email":"`+ok+`"}`); status != 200 {
			t.Errorf("%q refused with %d %s", ok, status, body)
		}
	}
}
