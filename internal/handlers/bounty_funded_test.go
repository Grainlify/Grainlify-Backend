package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// What the funded-bounty relay must get right is which channel and which
// action it signs, and the one judgement it makes itself: whether the
// repository is a verified project. Everything about money is the agent's and
// the chain's.
type relaySeen struct {
	path, headline, action, subject string
	body                            map[string]any
}

func fundedRelay(t *testing.T) (*fiber.App, *[]relaySeen, func(full string)) {
	t.Helper()
	var seen []relaySeen
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s := relaySeen{path: r.URL.Path, body: body}
		msg, _ := body["message"].(string)
		for _, line := range strings.Split(msg, "\n") {
			switch {
			case strings.HasPrefix(line, "Grainlify: "):
				s.headline = strings.TrimPrefix(line, "Grainlify: ")
			case strings.HasPrefix(line, "Action: "):
				s.action = strings.TrimPrefix(line, "Action: ")
			case strings.HasPrefix(line, "Subject: "):
				s.subject = strings.TrimPrefix(line, "Subject: ")
			}
		}
		seen = append(seen, s)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(agent.Close)

	d := dbtest.DB(t)
	user := newUser(t, d)
	linkGitHub(t, d, user, "owen")
	h := NewBountyDrawHandler(d, testSeedB64, agent.URL)
	app := fiber.New()
	withUser := func(c *fiber.Ctx) error { c.Locals(auth.LocalUserID, user.String()); return c.Next() }
	app.Post("/maintainer/funded", withUser, h.MaintainerFundedPrepare)
	app.Post("/maintainer/funded/:bountyId/draw", withUser, h.MaintainerFundedDraw)
	app.Post("/maintainer/funded-proposals/:proposalId/:answer", withUser, h.MaintainerFundedAnswer)
	app.Post("/bounties/:bountyId/unassign/propose", withUser, h.PostUnassignPropose)
	app.Post("/bounties/unassign-proposals/:proposalId/:answer", withUser, h.PostUnassignAnswer)
	app.Get("/admin/bounty-disputes", withUser, h.GetBountyDisputes)

	verify := func(full string) {
		ctx := context.Background()
		if _, err := d.Pool.Exec(ctx, `
INSERT INTO projects (owner_user_id, github_full_name, status, verified_at, github_app_installation_id)
VALUES ($1, $2, 'verified', now(), '162850488')`, user, full); err != nil {
			t.Fatalf("insert project: %v", err)
		}
		t.Cleanup(func() { _, _ = d.Pool.Exec(ctx, `DELETE FROM projects WHERE github_full_name = $1`, full) })
	}
	return app, &seen, verify
}

func send(t *testing.T, app *fiber.App, method, path, body string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	res, err := app.Test(r, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestFunded_PrepareRefusesAnUnverifiedProjectBeforeSigningAnything(t *testing.T) {
	app, seen, _ := fundedRelay(t)
	code, body := send(t, app, "POST", "/maintainer/funded",
		`{"repo":"acme/unverified","issue_number":41,"amount_minor":"50000000","currency":"USDC","mode":"draw","deadline":"2026-10-20T00:00:00Z","funder_wallet":"w"}`)
	if code != 403 || !strings.Contains(body, "not_a_verified_project") {
		t.Fatalf("status = %d, body = %s; want 403 not_a_verified_project", code, body)
	}
	if len(*seen) != 0 {
		t.Errorf("the agent was asked anyway: %v", *seen)
	}
}

func TestFunded_PrepareSignsOnTheMaintainerChannelWithTheIssueAsSubject(t *testing.T) {
	app, seen, verify := fundedRelay(t)
	verify("acme/funded-probe")
	code, body := send(t, app, "POST", "/maintainer/funded",
		`{"repo":"acme/funded-probe","issue_number":41,"amount_minor":"50000000","currency":"USDC","mode":"self_assign","deadline":"2026-10-20T00:00:00Z","funder_wallet":"w"}`)
	if code != 200 {
		t.Fatalf("status = %d: %s", code, body)
	}
	s := (*seen)[0]
	if s.path != "/maintainer/draw" || s.headline != "maintainer action" || s.action != "funded_prepare" || s.subject != "acme/funded-probe:41" {
		t.Errorf("relayed %+v", s)
	}
	if s.body["verifiedProject"] != true || s.body["mode"] != "self_assign" || s.body["amountMinor"] != "50000000" {
		t.Errorf("body = %v", s.body)
	}
}

func TestFunded_EachSideSignsOnItsOwnChannel(t *testing.T) {
	app, seen, _ := fundedRelay(t)
	bounty := "b3f1a0be-27d4-4c85-9f9c-1a0be27d4c85"
	proposal := "c4f1a0be-27d4-4c85-9f9c-1a0be27d4c85"
	for _, c := range []struct{ path, body string }{
		{"/maintainer/funded/" + bounty + "/draw", `{"simulate":true}`},
		{"/maintainer/funded-proposals/" + proposal + "/refuse", `{"reason":"no"}`},
		{"/bounties/" + bounty + "/unassign/propose", `{"reason":"I cannot finish"}`},
		{"/bounties/unassign-proposals/" + proposal + "/accept", `{}`},
	} {
		if code, body := send(t, app, "POST", c.path, c.body); code != 200 {
			t.Fatalf("%s: status = %d: %s", c.path, code, body)
		}
	}
	if code, _ := send(t, app, "POST", "/bounties/unassign-proposals/"+proposal+"/delete", `{}`); code != 404 {
		t.Errorf("an answer that is not accept, refuse or withdraw must not be relayed")
	}
	want := []relaySeen{
		{path: "/maintainer/draw", headline: "maintainer action", action: "funded_draw", subject: bounty},
		{path: "/maintainer/draw", headline: "maintainer action", action: "funded_refuse", subject: proposal},
		{path: "/bounties/apply", headline: "apply for a bounty", action: "unassign_propose", subject: bounty},
		{path: "/bounties/apply", headline: "apply for a bounty", action: "unassign_accept", subject: proposal},
	}
	if len(*seen) != len(want) {
		t.Fatalf("relayed %d, want %d: %+v", len(*seen), len(want), *seen)
	}
	for i, w := range want {
		g := (*seen)[i]
		if g.path != w.path || g.headline != w.headline || g.action != w.action || g.subject != w.subject {
			t.Errorf("relay %d = %+v, want %+v", i, g, w)
		}
	}
	if (*seen)[2].body["text"] != "I cannot finish" {
		t.Errorf("the contributor's reason was not sent: %v", (*seen)[2].body)
	}
}
