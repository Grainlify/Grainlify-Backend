package handlers

import (
	"context"
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

// A repo name contains a slash, so the signed-subject charset has to allow
// one. It must not allow anything that could end a line - that property is
// what stops a caller appending their own Expires.
func TestBountySubjectSafe_AllowsRepoNamesButStillNoNewlines(t *testing.T) {
	for _, ok := range []string{"Grainlify/grainlify-agent-sandbox", "Grainlify/Grainlify-Backend", "a/b"} {
		if !subjectSafe(ok) {
			t.Fatalf("subjectSafe(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"Grainlify/repo\nExpires: 2099-01-01T00:00:00Z", "a/b\r\nAction: run_draw"} {
		if subjectSafe(bad) {
			t.Fatalf("subjectSafe(%q) = true, want false", bad)
		}
	}
}

// The caller says which repo. This service decides whether it is a registered
// project, because a caller asserting that would defeat the point of the rule.
func TestBountyDraw_CallerCannotClaimARepoIsRegistered(t *testing.T) {
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
	app.Post("/admin/bounty-repos", func(c *fiber.Ctx) error { c.Locals(auth.LocalUserID, uid.String()); return c.Next() }, h.PostBountyRepo)

	req := httptest.NewRequest("POST", "/admin/bounty-repos",
		jsonBody(`{"full_name":"Someone/unverified","enabled":true,"registeredProject":true}`))
	req.Header.Set("Content-Type", "application/json")
	res, _ := app.Test(req, -1)
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	// No such project in our tables, so it is not registered, whatever the
	// body said.
	if seen["registeredProject"] != false {
		t.Fatalf("registeredProject = %v, want false for a repo we do not vouch for", seen["registeredProject"])
	}
	if !strings.Contains(seen["message"].(string), "Subject: Someone/unverified") {
		t.Fatalf("repo did not reach the signed message: %v", seen["message"])
	}
}

// A maintainer may look at their own repository and nobody else's. Owning the
// Grainlify project is one proof; GitHub permission, which the agent answers,
// is the other. Refusing needs BOTH to say no.
func TestBountyDraw_MaintainerViewRefusesSomebodyElsesRepo(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The agent has looked and this caller maintains nothing.
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"bounties":[]}`))
	}))
	defer agent.Close()

	d := dbtest.DB(t)
	uid := newUser(t, d)
	linkGitHub(t, d, uid, "Octocat")
	h := NewBountyDrawHandler(d, testSeedB64, agent.URL)
	app := fiber.New()
	app.Get("/maintainer/bounties/:bountyId/applications", func(c *fiber.Ctx) error { c.Locals(auth.LocalUserID, uid.String()); return c.Next() }, h.GetMaintainerBountyView)

	res, _ := app.Test(httptest.NewRequest("GET", "/maintainer/bounties/b1/applications?repo=Someone/else", nil), -1)
	if res.StatusCode != 403 {
		t.Fatalf("status = %d, want 403 for a repository the caller does not maintain", res.StatusCode)
	}
}

// An agent we cannot reach has not told us the caller lacks permission; it has
// told us nothing. Answering 403 there would report a refusal we never made,
// and send somebody to ask for access they may already have.
func TestBountyDraw_MaintainerViewCannotCheckIsNotARefusal(t *testing.T) {
	d := dbtest.DB(t)
	uid := newUser(t, d)
	linkGitHub(t, d, uid, "Octocat")
	h := NewBountyDrawHandler(d, testSeedB64, "http://127.0.0.1:1")
	app := fiber.New()
	app.Get("/maintainer/bounties/:bountyId/applications", func(c *fiber.Ctx) error { c.Locals(auth.LocalUserID, uid.String()); return c.Next() }, h.GetMaintainerBountyView)

	res, _ := app.Test(httptest.NewRequest("GET", "/maintainer/bounties/b1/applications?repo=Someone/else", nil), -1)
	if res.StatusCode != 502 {
		t.Fatalf("status = %d, want 502 when the agent could not be asked", res.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	if body["agent_url"] != "http://127.0.0.1:1" {
		t.Fatalf("agent_url = %v, want the address that was tried", body["agent_url"])
	}
}

// The agent saying the caller DOES maintain it is sufficient on its own - the
// project table is a shortcut, not the authority.
func TestBountyDraw_MaintainerViewAcceptsGitHubPermissionAlone(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"bounties":[{"bountyId":"b1","repo":"Someone/else","issueNumber":1}],"windowOpen":true,"canAssign":false}`))
	}))
	defer agent.Close()

	d := dbtest.DB(t)
	uid := newUser(t, d)
	linkGitHub(t, d, uid, "Octocat")
	h := NewBountyDrawHandler(d, testSeedB64, agent.URL)
	app := fiber.New()
	app.Get("/maintainer/bounties/:bountyId/applications", func(c *fiber.Ctx) error { c.Locals(auth.LocalUserID, uid.String()); return c.Next() }, h.GetMaintainerBountyView)

	res, _ := app.Test(httptest.NewRequest("GET", "/maintainer/bounties/b1/applications?repo=Someone/else", nil), -1)
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 when the agent says the caller maintains it", res.StatusCode)
	}
}

func TestBountyDraw_MaintainerViewNeedsARepo(t *testing.T) {
	d := dbtest.DB(t)
	uid := newUser(t, d)
	linkGitHub(t, d, uid, "Octocat")
	h := NewBountyDrawHandler(d, testSeedB64, "http://127.0.0.1:1")
	app := fiber.New()
	app.Get("/maintainer/bounties/:bountyId/applications", func(c *fiber.Ctx) error { c.Locals(auth.LocalUserID, uid.String()); return c.Next() }, h.GetMaintainerBountyView)
	res, _ := app.Test(httptest.NewRequest("GET", "/maintainer/bounties/b1/applications", nil), -1)
	if res.StatusCode != 400 {
		t.Fatalf("status = %d, want 400 without a repo", res.StatusCode)
	}
}

// GetBountyRepos must actually read the projects table.
//
// It could not. projects.github_app_installation_id is `text` and the struct
// scanned it into *int64, so every row failed to scan and the handler answered
// "lookup_failed" to every admin on every load. The Bounty Repositories screen
// has never listed a single project in production.
//
// Nothing caught it because nothing ran this handler against a database: the
// compiler cannot know a column's type, and the two only meet at runtime. So
// this test inserts a project and insists the handler returns it.
func TestBountyDraw_GetBountyReposReadsRealProjectRows(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"repos":[]}`))
	}))
	defer agent.Close()

	d := dbtest.DB(t)
	ctx := context.Background()
	owner := newUser(t, d)
	full := "Grainlify/scan-type-probe"
	if _, err := d.Pool.Exec(ctx, `
INSERT INTO projects (owner_user_id, github_full_name, status, verified_at, github_app_installation_id)
VALUES ($1, $2, 'verified', now(), '162850488')`, owner, full); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Pool.Exec(ctx, `DELETE FROM projects WHERE github_full_name = $1`, full)
	})

	// The handler relays a signed read to the agent, and signing needs the
	// caller's linked GitHub identity, so the admin is a real row here.
	admin := newUser(t, d)
	linkGitHub(t, d, admin, "Maintainer")

	h := NewBountyDrawHandler(d, testSeedB64, agent.URL)
	app := fiber.New()
	app.Get("/admin/bounty-repos", func(c *fiber.Ctx) error {
		c.Locals(auth.LocalUserID, admin.String())
		return c.Next()
	}, h.GetBountyRepos)

	res, err := app.Test(httptest.NewRequest("GET", "/admin/bounty-repos", nil), -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want 200: %s", res.StatusCode, b)
	}
	var out struct {
		Projects []struct {
			FullName   string `json:"full_name"`
			Verified   bool   `json:"verified"`
			Registered bool   `json:"registered_project"`
		} `json:"projects"`
	}
	b, _ := io.ReadAll(res.Body)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, b)
	}
	var found bool
	for _, p := range out.Projects {
		if p.FullName == full {
			found = true
			if !p.Verified || !p.Registered {
				t.Errorf("%s: verified=%v registered=%v, want both true", full, p.Verified, p.Registered)
			}
		}
	}
	if !found {
		t.Errorf("the handler returned %d projects and none was %s", len(out.Projects), full)
	}
}

// The POST decides whether a repository is a registered project and tells the
// agent. It read the same `text` column into an *int64 as the GET did, so the
// scan failed, `registered` fell to false, and it asserted "not a verified
// project" about a project that is verified. That is worse than the GET's
// failure: the GET showed an error, this one succeeded and wrote a wrong
// answer the payout gate then enforced.
func TestBountyDraw_PostBountyRepoTellsTheAgentTheProjectIsRegistered(t *testing.T) {
	var sent map[string]any
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The extra fields travel beside the signed message, not inside it.
		_ = json.NewDecoder(r.Body).Decode(&sent)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true,"repos":[]}`))
	}))
	defer agent.Close()

	d := dbtest.DB(t)
	ctx := context.Background()
	owner := newUser(t, d)
	full := "Grainlify/registered-probe"
	if _, err := d.Pool.Exec(ctx, `
INSERT INTO projects (owner_user_id, github_full_name, status, verified_at, github_app_installation_id)
VALUES ($1, $2, 'verified', now(), '162850488')`, owner, full); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Pool.Exec(ctx, `DELETE FROM projects WHERE github_full_name = $1`, full) })

	admin := newUser(t, d)
	linkGitHub(t, d, admin, "Admin")
	h := NewBountyDrawHandler(d, testSeedB64, agent.URL)
	app := fiber.New()
	app.Post("/admin/bounty-repos", func(c *fiber.Ctx) error {
		c.Locals(auth.LocalUserID, admin.String())
		return c.Next()
	}, h.PostBountyRepo)

	body := `{"full_name":"Grainlify/registered-probe","enabled":true}`
	req := httptest.NewRequest("POST", "/admin/bounty-repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d: %s", res.StatusCode, b)
	}

	// This is the judgement the agent records. Without it, registered_project
	// is written false and the payout gate refuses a repository the admin has
	// just switched on, with the screen showing it as on.
	if sent["registeredProject"] != true {
		t.Errorf("the agent was told registeredProject=%v, want true; whole body: %v", sent["registeredProject"], sent)
	}
	if sent["enabled"] != true {
		t.Errorf("the agent was told enabled=%v, want true", sent["enabled"])
	}
}

// The funded-bounty routes relay to the agent and decide nothing about money
// themselves. What they must get right is which action they name: a signature
// issued to read a quote must not be able to confirm a funding.
func TestBountyDraw_EscrowRoutesRelayTheRightAction(t *testing.T) {
	var seen []string
	var lastBody map[string]any
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		lastBody = body
		if msg, ok := body["message"].(string); ok {
			for _, line := range strings.Split(msg, "\n") {
				if strings.HasPrefix(line, "Action: ") {
					seen = append(seen, strings.TrimPrefix(line, "Action: "))
				}
			}
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer agent.Close()

	d := dbtest.DB(t)
	admin := newUser(t, d)
	linkGitHub(t, d, admin, "Funder")
	h := NewBountyDrawHandler(d, testSeedB64, agent.URL)
	app := fiber.New()
	withUser := func(c *fiber.Ctx) error { c.Locals(auth.LocalUserID, admin.String()); return c.Next() }
	app.Get("/bounties/escrow/quote", withUser, h.GetEscrowQuote)
	app.Get("/bounties/:bountyId/escrow", withUser, h.GetEscrow)
	app.Post("/bounties/:bountyId/escrow/confirm", withUser, h.PostEscrowConfirm)
	app.Get("/admin/escrows", withUser, h.GetEscrows)

	bounty := "b3f1a0be-27d4-4c85-9f9c-1a0be27d4c85"
	do := func(method, path, body string) int {
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest(method, path, nil)
		} else {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		}
		res, err := app.Test(r, -1)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		return res.StatusCode
	}

	if code := do("GET", "/bounties/escrow/quote?amount_minor=50000000", ""); code != 200 {
		t.Fatalf("quote: status = %d", code)
	}
	if lastBody["amountMinor"] != "50000000" {
		t.Errorf("the agent was sent amountMinor=%v", lastBody["amountMinor"])
	}
	if code := do("GET", "/bounties/"+bounty+"/escrow", ""); code != 200 {
		t.Fatalf("state: status = %d", code)
	}
	if code := do("POST", "/bounties/"+bounty+"/escrow/confirm", `{"signature":"5tRe9"}`); code != 200 {
		t.Fatalf("confirm: status = %d", code)
	}
	if lastBody["signature"] != "5tRe9" {
		t.Errorf("the agent was sent signature=%v", lastBody["signature"])
	}
	if code := do("GET", "/admin/escrows", ""); code != 200 {
		t.Fatalf("list: status = %d", code)
	}

	want := []string{"escrow_quote", "escrow_state", "escrow_confirm", "escrow_list"}
	if len(seen) != len(want) {
		t.Fatalf("actions sent = %v, want %v", seen, want)
	}
	for i, a := range want {
		if seen[i] != a {
			t.Errorf("action %d = %q, want %q", i, seen[i], a)
		}
	}
}

// A quote with no amount is the caller's mistake and is refused here rather
// than relayed, so the agent is not asked to interpret an empty string.
func TestBountyDraw_EscrowQuoteNeedsAnAmount(t *testing.T) {
	d := dbtest.DB(t)
	admin := newUser(t, d)
	linkGitHub(t, d, admin, "Funder")
	h := NewBountyDrawHandler(d, testSeedB64, "http://127.0.0.1:1")
	app := fiber.New()
	app.Get("/bounties/escrow/quote", func(c *fiber.Ctx) error {
		c.Locals(auth.LocalUserID, admin.String())
		return c.Next()
	}, h.GetEscrowQuote)

	res, _ := app.Test(httptest.NewRequest("GET", "/bounties/escrow/quote", nil), -1)
	if res.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

// Confirming without a signature is refused before anything is relayed: an
// empty signature is not a claim the agent should have to evaluate.
func TestBountyDraw_EscrowConfirmNeedsASignature(t *testing.T) {
	d := dbtest.DB(t)
	admin := newUser(t, d)
	linkGitHub(t, d, admin, "Funder")
	h := NewBountyDrawHandler(d, testSeedB64, "http://127.0.0.1:1")
	app := fiber.New()
	app.Post("/bounties/:bountyId/escrow/confirm", func(c *fiber.Ctx) error {
		c.Locals(auth.LocalUserID, admin.String())
		return c.Next()
	}, h.PostEscrowConfirm)

	r := httptest.NewRequest("POST", "/bounties/b3f1a0be-27d4-4c85-9f9c-1a0be27d4c85/escrow/confirm", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	res, _ := app.Test(r, -1)
	if res.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

// The reason reaches the contributor, so a blank one is refused here rather
// than three hops away in the agent.
func TestBountyDraw_UnassignRequiresAReason(t *testing.T) {
	d := dbtest.DB(t)
	uid := newUser(t, d)
	linkGitHub(t, d, uid, "Maintainer")
	h := NewBountyDrawHandler(d, testSeedB64, "http://127.0.0.1:1")
	app := fiber.New()
	app.Post("/x/:bountyId/unassign", func(c *fiber.Ctx) error {
		c.Locals(auth.LocalUserID, uid.String())
		return c.Next()
	}, h.PostUnassign)

	for _, body := range []string{`{}`, `{"reason":"   "}`} {
		r := httptest.NewRequest("POST", "/x/b3f1a0be-27d4-4c85-9f9c-1a0be27d4c85/unassign", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		res, _ := app.Test(r, -1)
		if res.StatusCode != 400 {
			t.Fatalf("body %s: status = %d, want 400", body, res.StatusCode)
		}
	}
}

func TestBountyDraw_DeadlineChangeRequiresBothFields(t *testing.T) {
	d := dbtest.DB(t)
	uid := newUser(t, d)
	linkGitHub(t, d, uid, "Maintainer")
	h := NewBountyDrawHandler(d, testSeedB64, "http://127.0.0.1:1")
	app := fiber.New()
	app.Post("/x/:bountyId/deadline", func(c *fiber.Ctx) error {
		c.Locals(auth.LocalUserID, uid.String())
		return c.Next()
	}, h.PostAssignmentDeadline)

	for _, body := range []string{`{"reason":"more time"}`, `{"deadline":"2026-10-06T01:00:00Z"}`} {
		r := httptest.NewRequest("POST", "/x/b3f1a0be-27d4-4c85-9f9c-1a0be27d4c85/deadline", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		res, _ := app.Test(r, -1)
		if res.StatusCode != 400 {
			t.Fatalf("body %s: status = %d, want 400", body, res.StatusCode)
		}
	}
}

// A draw with no chosen deadline must not send one, or every draw would
// silently override the global setting with a zero.
func TestBountyDraw_RunDrawOnlySendsADeadlineWhenOneWasChosen(t *testing.T) {
	var bodies []map[string]any
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer agent.Close()

	d := dbtest.DB(t)
	uid := newUser(t, d)
	linkGitHub(t, d, uid, "Maintainer")
	h := NewBountyDrawHandler(d, testSeedB64, agent.URL)
	app := fiber.New()
	app.Post("/x/:bountyId/run", func(c *fiber.Ctx) error {
		c.Locals(auth.LocalUserID, uid.String())
		return c.Next()
	}, h.PostRunDraw)

	post := func(body string) {
		r := httptest.NewRequest("POST", "/x/b3f1a0be-27d4-4c85-9f9c-1a0be27d4c85/run", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if _, err := app.Test(r, -1); err != nil {
			t.Fatalf("app.Test: %v", err)
		}
	}
	post(`{}`)
	post(`{"stale_hours":96}`)

	if _, ok := bodies[0]["staleHours"]; ok {
		t.Errorf("a draw with no chosen deadline sent one: %v", bodies[0])
	}
	if bodies[1]["staleHours"] != float64(96) {
		t.Errorf("staleHours = %v, want 96", bodies[1]["staleHours"])
	}
}
