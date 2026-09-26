package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// The exact string the agent's packages/gate/test/session-action.test.ts
// pins. If either side edits its message shape without the other, this fails
// here rather than as an opaque "malformed" in production - which is how the
// wallet read's column mismatch survived to a live deploy.
func TestBountyActionMessage_Shape(t *testing.T) {
	issued := time.Date(2026, 9, 27, 9, 30, 0, 0, time.UTC)
	got := BountyActionMessage("admin", "run_draw", "Jagadeeshftw", 583231,
		"b3f1a0be-27d4-4c85-9f9c-1a0be27d4c85", "3f9c1a0be27d4c853f9c1a0be27d4c85",
		issued, issued.Add(10*time.Minute))
	want := "Grainlify: admin action\n" +
		"Action: run_draw\n" +
		"GitHub: Jagadeeshftw (id 583231)\n" +
		"Subject: b3f1a0be-27d4-4c85-9f9c-1a0be27d4c85\n" +
		"Nonce: 3f9c1a0be27d4c853f9c1a0be27d4c85\n" +
		"Issued: 2026-09-27T09:30:00Z\n" +
		"Expires: 2026-09-27T09:40:00Z"
	if got != want {
		t.Fatalf("message shape drifted from the agent's parser\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestBountyActionMessage_ApplyHeadline(t *testing.T) {
	issued := time.Date(2026, 9, 27, 9, 30, 0, 0, time.UTC)
	got := BountyActionMessage("apply", "apply", "Octocat", 583231, "some-bounty-id", "3f9c1a0be27d4c853f9c1a0be27d4c85", issued, issued.Add(time.Minute))
	if want := "Grainlify: apply for a bounty\n"; len(got) < len(want) || got[:len(want)] != want {
		t.Fatalf("apply headline = %q", got)
	}
}

// A subject is interpolated into a signed message. If a newline could get in,
// a caller could append their own Expires line.
func TestBountySubjectSafe_RejectsAnythingThatCouldAddALine(t *testing.T) {
	for _, bad := range []string{
		"x\nExpires: 2099-01-01T00:00:00Z",
		"x\r\nAction: run_draw",
		"has space",
		"quote\"",
		string(make([]byte, 200)),
	} {
		if subjectSafe(bad) {
			t.Fatalf("subjectSafe(%q) = true, want false", bad)
		}
	}
	for _, ok := range []string{"", "b3f1a0be-27d4-4c85-9f9c-1a0be27d4c85", "application_window_hours", "a.b:c-d_e"} {
		if !subjectSafe(ok) {
			t.Fatalf("subjectSafe(%q) = false, want true", ok)
		}
	}
}

func TestBountyDraw_ApplyRelaysToTheAgent(t *testing.T) {
	var seen struct {
		Message          string `json:"message"`
		Countersignature string `json:"countersignature"`
	}
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bounties/apply" {
			t.Errorf("path = %s, want /bounties/apply", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&seen)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"applied":true}`))
	}))
	defer agent.Close()

	d := dbtest.DB(t)
	uid := newUser(t, d)
	linkGitHub(t, d, uid, "Octocat")
	h := NewBountyDrawHandler(d, testSeedB64, agent.URL)
	app := fiber.New()
	app.Post("/bounties/:bountyId/apply", func(c *fiber.Ctx) error { c.Locals(auth.LocalUserID, uid.String()); return c.Next() }, h.PostApply)

	res, _ := app.Test(httptest.NewRequest("POST", "/bounties/b3f1a0be-27d4-4c85-9f9c-1a0be27d4c85/apply", nil), -1)
	if res.StatusCode != 201 {
		t.Fatalf("status = %d, want 201", res.StatusCode)
	}
	if seen.Message == "" || seen.Countersignature == "" {
		t.Fatalf("the agent got no signed message: %+v", seen)
	}
	// The login comes from OUR database, not from anything the caller sent.
	if want := "GitHub: Octocat (id"; !strings.Contains(seen.Message, want) {
		t.Fatalf("message does not name the signed-in account: %q", seen.Message)
	}
	if !strings.Contains(seen.Message, "Subject: b3f1a0be-27d4-4c85-9f9c-1a0be27d4c85") {
		t.Fatalf("message does not carry the bounty id: %q", seen.Message)
	}
	if !strings.Contains(seen.Message, "Grainlify: apply for a bounty") {
		t.Fatalf("apply used the wrong headline: %q", seen.Message)
	}
}

func TestBountyDraw_ApplyRequiresSignedInUser(t *testing.T) {
	d := dbtest.DB(t)
	h := NewBountyDrawHandler(d, testSeedB64, "http://127.0.0.1:1")
	app := fiber.New()
	app.Post("/bounties/:bountyId/apply", h.PostApply)
	res, _ := app.Test(httptest.NewRequest("POST", "/bounties/x/apply", nil), -1)
	if res.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
}

// The caller supplies the body for set_setting. It must not be able to
// overwrite the two fields that carry the identity.
func TestBountyDraw_CallerCannotOverwriteTheSignedFields(t *testing.T) {
	var seen map[string]any
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seen)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer agent.Close()

	d := dbtest.DB(t)
	uid := newUser(t, d)
	linkGitHub(t, d, uid, "Admin")
	h := NewBountyDrawHandler(d, testSeedB64, agent.URL)
	app := fiber.New()
	app.Post("/admin/bounty-draw/settings", func(c *fiber.Ctx) error { c.Locals(auth.LocalUserID, uid.String()); return c.Next() }, h.PostSetting)

	req := httptest.NewRequest("POST", "/admin/bounty-draw/settings",
		jsonBody(`{"key":"application_window_hours","value":"12","message":"Grainlify: admin action","countersignature":"forged"}`))
	req.Header.Set("Content-Type", "application/json")
	res, _ := app.Test(req, -1)
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if seen["countersignature"] == "forged" {
		t.Fatalf("a caller-supplied countersignature reached the agent")
	}
	if m, _ := seen["message"].(string); m == "Grainlify: admin action" {
		t.Fatalf("a caller-supplied message reached the agent")
	}
	if !strings.Contains(seen["message"].(string), "Action: set_setting") {
		t.Fatalf("wrong action in the signed message: %v", seen["message"])
	}
}

func TestBountyDraw_AgentDownIsA502ThatNamesTheHost(t *testing.T) {
	d := dbtest.DB(t)
	uid := newUser(t, d)
	linkGitHub(t, d, uid, "Octocat")
	h := NewBountyDrawHandler(d, testSeedB64, "http://127.0.0.1:1")
	app := fiber.New()
	app.Post("/bounties/:bountyId/apply", func(c *fiber.Ctx) error { c.Locals(auth.LocalUserID, uid.String()); return c.Next() }, h.PostApply)
	res, _ := app.Test(httptest.NewRequest("POST", "/bounties/abc/apply", nil), -1)
	if res.StatusCode != 502 {
		t.Fatalf("status = %d, want 502", res.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	if body["agent_url"] != "http://127.0.0.1:1" {
		t.Fatalf("agent_url = %v, want the address that was tried", body["agent_url"])
	}
}

func jsonBody(s string) io.Reader { return strings.NewReader(s) }
