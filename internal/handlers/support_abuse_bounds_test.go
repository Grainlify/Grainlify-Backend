package handlers_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
)

// Bounds on an endpoint anyone can reach.
//
// /support is now a public route, and the per-client limiter guarding it does
// not work: it keys on Fiber's c.IP(), which behind Railway is the edge
// proxy's address. Every reporter_ip ever stored is a 100.64.0.x CGNAT
// address - six distinct ones across ten reports - and 14 rapid requests from
// one machine never triggered the 10/minute limit.
//
// So the bounds that do hold are the ones needing no key at all.

// The global hourly ceiling on ANONYMOUS submissions. Crude by design: under a
// flood it refuses legitimate reporters too, which is the lesser harm against
// an unbounded fan-out to Telegram and Discord.
func TestSupportCreate_AnonymousSubmissionsAreGloballyCapped(t *testing.T) {
	d := testDB(t)
	app := supportApp(d)

	// Fill the hour's allowance with rows that already exist, rather than by
	// sending 60 requests: the cap counts persisted anonymous rows, so this
	// exercises the same predicate the handler reads.
	for i := 0; i < 60; i++ {
		if _, err := d.Pool.Exec(t.Context(), `
INSERT INTO support_requests (id, user_id, category, message, created_at)
VALUES (gen_random_uuid(), NULL, 'bug', $1, now())
`, fmt.Sprintf("cap fixture %d", i)); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	t.Cleanup(func() {
		_, _ = d.Pool.Exec(t.Context(), `DELETE FROM support_requests WHERE message LIKE 'cap fixture %'`)
	})

	status, out := postSupport(t, app, `{"category":"bug","message":"one too many"}`, "")
	if status != fiber.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 once the hourly cap is reached (body %v)", status, out)
	}
	if out["error"] != "rate_limited" {
		t.Errorf("error = %v, want rate_limited", out["error"])
	}
	// Says what to do, not merely that it failed. Somebody hitting this is far
	// more likely to be a real person caught behind a flood.
	if msg, _ := out["message"].(string); !strings.Contains(strings.ToLower(msg), "sign in") {
		t.Errorf("message does not offer the way through: %q", msg)
	}

	// And nothing was written.
	var n int
	_ = d.Pool.QueryRow(t.Context(), `SELECT count(*)::int FROM support_requests WHERE message = 'one too many'`).Scan(&n)
	if n != 0 {
		t.Errorf("a refused submission still persisted %d rows", n)
	}
}

// The cap is on ANONYMOUS traffic only. A signed-in report is attributable, so
// the account bounds it - and the failure mode under a flood must be
// "anonymous reporting pauses", not "support stops".
func TestSupportCreate_SignedInReportsAreNotCapped(t *testing.T) {
	d := testDB(t)
	app := supportApp(d)
	userID := leaderboardSuiteUser(t, d.Pool)

	for i := 0; i < 60; i++ {
		if _, err := d.Pool.Exec(t.Context(), `
INSERT INTO support_requests (id, user_id, category, message, created_at)
VALUES (gen_random_uuid(), NULL, 'bug', $1, now())
`, fmt.Sprintf("cap fixture %d", i)); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	t.Cleanup(func() {
		_, _ = d.Pool.Exec(t.Context(), `DELETE FROM support_requests WHERE message LIKE 'cap fixture %' OR message = 'signed in, over the cap'`)
	})

	status, out := postSupport(t, app, `{"category":"bug","message":"signed in, over the cap"}`, supportToken(t, userID))
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200 for a signed-in reporter over the anonymous cap (body %v)", status, out)
	}
}

// The screenshot bound. Cut from 5MB to 2MB when this became a public route:
// per-request amplification is what an anonymous endpoint costs under load.
func TestSupportCreate_ScreenshotBoundIsTwoMegabytes(t *testing.T) {
	d := testDB(t)
	app := supportApp(d)

	// 2.5MB decoded: over the new 2MB bound and under the old 5MB one, so this
	// fails on the previous limit rather than passing on both. Kept under
	// Fiber's default 4MB request cap too - the test app does not set the
	// production BodyLimit, and a request rejected by the framework would
	// never reach the handler bound being tested.
	// 800k groups of 4 base64 chars = 2.4MB decoded: over the new 2MB bound,
	// under the old 5MB one, so this fails on the previous limit rather than
	// passing on both. Groups of 4 because a base64 body whose length is not a
	// multiple of 4 is rejected as malformed BEFORE the size check, which
	// passes the test for the wrong reason.
	oversized := "data:image/png;base64," + strings.Repeat("AAAA", 800_000)
	payload, _ := json.Marshal(map[string]string{
		"category": "bug", "message": "with a large screenshot", "screenshot": oversized,
	})
	status, out := postSupport(t, app, string(payload), "")
	if status != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a 3MB screenshot (body %v)", status, out)
	}
	if out["error"] != "screenshot_too_large" {
		t.Errorf("error = %v, want screenshot_too_large", out["error"])
	}
}

func supportToken(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	token, err := auth.IssueJWT(supportTestSecret, userID, "contributor", "", "", 3600_000_000_000)
	if err != nil {
		t.Fatalf("issue jwt: %v", err)
	}
	return token
}
