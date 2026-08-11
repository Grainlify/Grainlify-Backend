package handlers_test

import (
	"context"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

const bootstrapSecret = "bootstrap-suite-secret"
const bootstrapToken = "the-shared-bootstrap-token"

func bootstrapApp(d *db.DB) *fiber.App {
	cfg := config.Config{JWTSecret: bootstrapSecret, AdminBootstrapToken: bootstrapToken}
	h := handlers.NewAdminHandler(cfg, d)
	app := fiber.New()
	app.Post("/admin/bootstrap", auth.RequireAuth(bootstrapSecret), h.BootstrapAdmin())
	app.Put("/admin/users/:id/role", auth.RequireAuth(bootstrapSecret),
		auth.RequireLiveRole(handlers.NewRoleLookup(d), "admin"), h.SetUserRole())
	return app
}

func bootstrapToken4(t *testing.T, userID uuid.UUID, role string) string {
	t.Helper()
	tok, err := auth.IssueJWT(bootstrapSecret, userID, role, "", "", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}
	return tok
}

func clearAdmins(t *testing.T, d *db.DB) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(), `UPDATE users SET role = 'contributor' WHERE role = 'admin'`); err != nil {
		t.Fatalf("clear admins: %v", err)
	}
	if _, err := d.Pool.Exec(context.Background(), `DELETE FROM admin_role_audit`); err != nil {
		t.Fatalf("clear audit: %v", err)
	}
}

func doWithBootstrapToken(t *testing.T, app *fiber.App, jwt, token string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest("POST", "/admin/bootstrap", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	if token != "" {
		req.Header.Set("X-Admin-Bootstrap-Token", token)
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// TestBootstrap_GrantsTheFirstAdminAndRecordsItsOrigin. A first admin with no
// origin record is the same gap one step earlier - the one grant nobody can
// trace.
func TestBootstrap_GrantsTheFirstAdminAndRecordsItsOrigin(t *testing.T) {
	d := testDB(t)
	clearAdmins(t, d)
	app := bootstrapApp(d)

	userID := adminSuiteInsertUser(t, d, "contributor")
	code, body := doWithBootstrapToken(t, app, bootstrapToken4(t, userID, "contributor"), bootstrapToken)
	if code != fiber.StatusOK {
		t.Fatalf("status = %d, want 200 on a fresh install; body = %s", code, body)
	}

	var role string
	if err := d.Pool.QueryRow(context.Background(), `SELECT role FROM users WHERE id = $1`, userID).Scan(&role); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if role != "admin" {
		t.Errorf("role = %q, want admin", role)
	}

	var source, newRole string
	var actor uuid.UUID
	if err := d.Pool.QueryRow(context.Background(), `
SELECT source, new_role, actor_user_id FROM admin_role_audit WHERE subject_user_id = $1
`, userID).Scan(&source, &newRole, &actor); err != nil {
		t.Fatalf("no origin record for the first admin: %v", err)
	}
	if source != "bootstrap" || newRole != "admin" {
		t.Errorf("audit = %s/%s, want bootstrap/admin", source, newRole)
	}
	// actor == subject is the property worth being able to see: nobody else
	// authorised this.
	if actor != userID {
		t.Errorf("actor = %s, want the self-promoting user %s", actor, userID)
	}
}

// TestBootstrap_RefusesOnceAnAdminExists closes the self-service escalation.
func TestBootstrap_RefusesOnceAnAdminExists(t *testing.T) {
	d := testDB(t)
	clearAdmins(t, d)
	app := bootstrapApp(d)

	_ = adminSuiteInsertUser(t, d, "admin") // an admin already exists
	other := adminSuiteInsertUser(t, d, "contributor")

	code, body := doWithBootstrapToken(t, app, bootstrapToken4(t, other, "contributor"), bootstrapToken)
	if code != fiber.StatusForbidden {
		t.Fatalf("status = %d, want %d once an admin exists; body = %s", code, fiber.StatusForbidden, body)
	}

	var role string
	d.Pool.QueryRow(context.Background(), `SELECT role FROM users WHERE id = $1`, other).Scan(&role)
	if role == "admin" {
		t.Error("bootstrap promoted a user after it should have closed")
	}

	// A correct token presented after closure means somebody has the shared
	// secret and tried it. That is the signal worth recording.
	var n int
	d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM admin_role_audit WHERE subject_user_id = $1 AND source = 'bootstrap'`, other).Scan(&n)
	if n == 0 {
		t.Error("a refused bootstrap attempt was not recorded")
	}
}

// TestSetUserRole_RecordsBeforeAndAfter. "Alice changed Bob's role" is much
// less useful than "Alice promoted Bob from contributor to admin", and a
// demotion matters as much as a promotion.
func TestSetUserRole_RecordsBeforeAndAfter(t *testing.T) {
	d := testDB(t)
	clearAdmins(t, d)
	app := bootstrapApp(d)

	actor := adminSuiteInsertUser(t, d, "admin")
	subject := adminSuiteInsertUser(t, d, "contributor")
	jwt := bootstrapToken4(t, actor, "admin")

	for _, tc := range []struct{ to, wantOld string }{
		{"admin", "contributor"}, // promotion
		{"contributor", "admin"}, // demotion matters as much
	} {
		resp, respBody := notifSuiteDo(t, app, "PUT", "/admin/users/"+subject.String()+"/role", jwt,
			[]byte(`{"role":"`+tc.to+`","reason":"test"}`))
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("status = %d, want 200 setting role to %s; body = %s",
				resp.StatusCode, tc.to, respBody)
		}

		var oldRole, newRole, source string
		var gotActor uuid.UUID
		if err := d.Pool.QueryRow(context.Background(), `
SELECT old_role, new_role, source, actor_user_id FROM admin_role_audit
WHERE subject_user_id = $1 ORDER BY created_at DESC LIMIT 1
`, subject).Scan(&oldRole, &newRole, &source, &gotActor); err != nil {
			t.Fatalf("no audit row for role change to %s: %v", tc.to, err)
		}
		if oldRole != tc.wantOld || newRole != tc.to {
			t.Errorf("audit = %s -> %s, want %s -> %s", oldRole, newRole, tc.wantOld, tc.to)
		}
		if source != "admin_action" || gotActor != actor {
			t.Errorf("source/actor = %s/%s, want admin_action/%s", source, gotActor, actor)
		}
	}
}
